#!/bin/sh
# Run an RKC development or inference workload in a deliberately subordinate
# cgroup. The defaults protect concurrent, higher-priority training workloads:
# at most one CPU core, no more than 4 GiB soft / 4.5 GiB hard memory, idle I/O
# scheduling, lowest CPU niceness, IOWeight=1 when the user manager delegates
# that controller, and a high OOM-kill preference. Operators may select a
# strictly smaller host profile with RKC_MEMORY_HIGH_MIB, RKC_MEMORY_MAX_MIB,
# RKC_MEMORY_SWAP_MAX_MIB, and RKC_GO_MEMORY_LIMIT_MIB. An optional
# RKC_HOST_AVAILABLE_MEMORY_MIN_MIB reserve makes direct RKC work yield when
# host-wide Linux MemAvailable falls below the selected floor.
# RKC_CPU_QUOTA_PERCENT may lower the CPU ceiling to 1..100 percent of one
# core. It cannot raise the default ceiling, and applies in both launch modes.
#
# Processes matching the bounded RKC_HIGHER_PRIORITY_MARKERS classes are
# treated as explicitly more important. The generic default is
# torchrun,lm_eval. RKC_HIGHER_PRIORITY_POLICY selects how visible
# higher-priority processes admit this workload:
#   refuse (strict) - refuse to start (exit 75) whenever one is visible;
#   yield  (default) - start inside this subordinate envelope and leave
#   continuous load monitoring to the guarded RKC binary, which refuses or
#   cancels promptly when higher-priority processes become measurably busy.
# The wrapper itself supervises host memory for every command, including
# build tools, and reaps only its fresh owned unit after exit or interruption.
set -eu

# These helpers run in the owner and a tiny detached shell watchdog. Neither
# path reports source directories, command arguments, environment values, or
# detailed service-manager errors.
guard_process_birth() {
    case "$1" in ''|*[!0123456789]*) return 1 ;; esac
    guard_stat_line=
    { IFS= read -r guard_stat_line < "/proc/$1/stat"; } 2>/dev/null || return 1
    case "$guard_stat_line" in *') '*) ;; *) return 1 ;; esac
    guard_stat_fields=${guard_stat_line##*) }
    # proc's comm field may contain spaces or parentheses. Strip through its
    # final closing parenthesis before splitting the numeric kernel fields.
    set -f
    set -- $guard_stat_fields
    [ "$#" -ge 20 ] || return 1
    [ "$1" != Z ] && [ "$1" != X ] || return 1
    guard_stat_shift=0
    while [ "$guard_stat_shift" -lt 19 ]; do
        shift
        guard_stat_shift=$((guard_stat_shift + 1))
    done
    case "$1" in ''|*[!0123456789]*) return 1 ;; esac
    printf '%s\n' "$1"
}

guard_process_matches() {
    guard_current_birth=$(guard_process_birth "$1") || return 1
    [ "$guard_current_birth" = "$2" ]
}

guard_process_parent() {
    case "$1" in ''|*[!0123456789]*) return 1 ;; esac
    guard_parent_line=
    { IFS= read -r guard_parent_line < "/proc/$1/stat"; } 2>/dev/null || return 1
    case "$guard_parent_line" in *') '*) ;; *) return 1 ;; esac
    guard_parent_fields=${guard_parent_line##*) }
    set -f
    set -- $guard_parent_fields
    [ "$#" -ge 2 ] || return 1
    case "$2" in ''|*[!0123456789]*) return 1 ;; esac
    printf '%s\n' "$2"
}

guard_host_memory_available() {
    [ "$1" -gt 0 ] || return 0
    awk -v minimum_mib="$1" '
        NR > 256 { invalid=1; exit }
        $1 == "MemAvailable:" || $1 == "MemTotal:" {
            if (NF != 3 || $2 !~ /^(0|[1-9][0-9]*)$/ || length($2) > 15 || $3 != "kB") invalid=1
            if ($1 == "MemAvailable:") { available_count++; available=$2 }
            else { total_count++; total=$2 }
        }
        END {
            if (invalid || available_count != 1 || total_count != 1 ||
                total <= 0 || available > total || available < minimum_mib * 1024) exit 1
        }
    ' /proc/meminfo 2>/dev/null
}

guard_owned_state() {
    guard_state=$1
    guard_owner_pid=$2
    guard_owner_birth=$3
    guard_mode=$4
    case "$guard_owner_pid" in ''|*[!0123456789]*|0*) return 1 ;; esac
    case "$guard_owner_birth" in ''|*[!0123456789]*) return 1 ;; esac
    case "$guard_mode" in scope|service) ;; *) return 1 ;; esac
    guard_state_base=${guard_state##*/}
    guard_token=${guard_state_base#rkc-guard.}
    [ "$guard_state_base" != "$guard_token" ] && [ "${#guard_token}" -eq 16 ] || return 1
    case "$guard_token" in *[!abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789]*) return 1 ;; esac
    [ -d "$guard_state" ] && [ ! -L "$guard_state" ] || return 1
    guard_state_owner=$(stat -c '%u:%a' "$guard_state" 2>/dev/null) || return 1
    guard_user_id=$(id -u) || return 1
    [ "$guard_state_owner" = "$guard_user_id:700" ] || return 1
    guard_state_identity=$(stat -c '%d:%i' "$guard_state" 2>/dev/null) || return 1
    case "$guard_state_identity" in ''|*[!0123456789:]*) return 1 ;; esac
    guard_description="rkc-guard-$guard_owner_pid-$guard_owner_birth-$guard_state_identity"
    [ -f "$guard_state/owner" ] && [ ! -L "$guard_state/owner" ] || return 1
    guard_unit="rkc-low-$guard_owner_pid-$guard_token.$guard_mode"
    guard_record_pid= guard_record_birth= guard_record_mode= guard_record_unit= guard_record_floor= guard_record_umask= guard_record_extra=
    { read -r guard_record_pid guard_record_birth guard_record_mode guard_record_unit guard_record_floor guard_record_umask guard_record_extra < "$guard_state/owner"; } 2>/dev/null || return 1
    [ "$guard_record_pid" = "$guard_owner_pid" ] && [ "$guard_record_birth" = "$guard_owner_birth" ] &&
        [ "$guard_record_mode" = "$guard_mode" ] && [ "$guard_record_unit" = "$guard_unit" ] && [ -z "$guard_record_extra" ] || return 1
    case "$guard_record_floor" in ''|*[!0123456789]*|0?*) return 1 ;; esac
    [ "${#guard_record_floor}" -le 5 ] && [ "$guard_record_floor" -le 65536 ] || return 1
    case "$guard_record_umask" in ''|*[!01234567]*) return 1 ;; esac
    [ "${#guard_record_umask}" -le 4 ] || return 1
}

guard_observe_owned_unit() {
    # A collected unit may already have disappeared. Only an explicit
    # not-found load state proves absence; empty output is not evidence.
    guard_load_state=$(timeout --signal=KILL 1s systemctl --user show --property=LoadState --value "$guard_unit" 2>/dev/null) || return 1
    if [ "$guard_load_state" = not-found ]; then
        guard_active_state=inactive
        guard_unit_exists=0
        return 0
    fi
    [ "$guard_load_state" = loaded ] || return 1
    guard_unit_description=$(timeout --signal=KILL 1s systemctl --user show --property=Description --value "$guard_unit" 2>/dev/null) || return 1
    [ "$guard_unit_description" = "$guard_description" ] || return 1
    guard_active_state=$(timeout --signal=KILL 1s systemctl --user show --property=ActiveState --value "$guard_unit" 2>/dev/null) || return 1
    case "$guard_active_state" in active|activating|deactivating|inactive|failed|reloading) ;; *) return 1 ;; esac
    guard_unit_exists=1
}

guard_cleanup_owned_unit() {
    # Stop the launcher first: it must not enqueue another unit start while we
    # prove the owned unit quiescent. Birth times avoid killing a reused PID.
    guard_launcher_pid= guard_launcher_birth= guard_launcher_extra=
    if { read -r guard_launcher_pid guard_launcher_birth guard_launcher_extra < "$guard_state/launcher"; } 2>/dev/null &&
        [ -z "$guard_launcher_extra" ] && guard_process_matches "$guard_launcher_pid" "$guard_launcher_birth"; then
        kill -TERM "$guard_launcher_pid" 2>/dev/null || true
        kill -KILL "$guard_launcher_pid" 2>/dev/null || true
    fi
    guard_quiet_samples=0
    guard_cleanup_round=0
    while [ "$guard_cleanup_round" -lt 4 ]; do
        if ! guard_observe_owned_unit; then
            # Never touch an existing unit whose ownership is unproven.
            return 1
        fi
        if [ "$guard_unit_exists" -eq 1 ]; then
            timeout --signal=KILL 1s systemctl --user kill --kill-whom=all --signal=TERM "$guard_unit" >/dev/null 2>&1 || true
            timeout --signal=KILL 2s systemctl --user stop "$guard_unit" >/dev/null 2>&1 || true
            timeout --signal=KILL 1s systemctl --user kill --kill-whom=all --signal=KILL "$guard_unit" >/dev/null 2>&1 || true
            guard_observe_owned_unit || return 1
        fi
        case "$guard_active_state" in inactive|failed) guard_quiet_samples=$((guard_quiet_samples + 1)) ;; *) guard_quiet_samples=0 ;; esac
        [ "$guard_quiet_samples" -lt 2 ] || return 0
        # Reobserve a queued late creation before touching it. Its payload
        # gate also refuses work after closing or owner death.
        guard_cleanup_round=$((guard_cleanup_round + 1))
        [ "$guard_cleanup_round" -ge 4 ] || sleep 1
    done
    return 1
}

guard_launch_unit() {
    guard_script=$0
    guard_cpu_quota_percent=$RKC_CPU_QUOTA_PERCENT
    guard_memory_high_mib=$RKC_MEMORY_HIGH_MIB
    guard_memory_max_mib=$RKC_MEMORY_MAX_MIB
    guard_memory_swap_max_mib=$RKC_MEMORY_SWAP_MAX_MIB
    guard_go_memory_limit_mib=$RKC_GO_MEMORY_LIMIT_MIB
    guard_host_available_memory_min_mib=$RKC_HOST_AVAILABLE_MEMORY_MIN_MIB
    guard_cgo_enabled=${CGO_ENABLED-}
    guard_require_io_controller=${RKC_REQUIRE_IO_CONTROLLER:-0}
    higher_priority_policy=$RKC_HIGHER_PRIORITY_POLICY
    higher_priority_markers=$RKC_HIGHER_PRIORITY_MARKERS
    guard_priority_load_max=${RKC_HIGHER_PRIORITY_LOAD_MAX-}
case "$guard_mode" in
    scope)
        exec systemd-run \
            --user \
            --scope \
            --collect \
            --quiet \
            --expand-environment=no \
            --unit "$guard_unit" \
            --setenv="RKC_RESOURCE_GUARD_UNIT=$guard_unit" \
            --setenv="RKC_HOST_AVAILABLE_MEMORY_MIN_MIB=$guard_host_available_memory_min_mib" \
            --property "Description=$guard_description" \
            --property CPUWeight=1 \
            --property IOWeight=1 \
            --property "CPUQuota=${guard_cpu_quota_percent}%" \
            --property "MemoryHigh=${guard_memory_high_mib}M" \
            --property "MemoryMax=${guard_memory_max_mib}M" \
            --property "MemorySwapMax=${guard_memory_swap_max_mib}M" \
            --property TasksMax=128 \
            --property OOMPolicy=stop \
            choom -n 750 -- ionice -c 3 nice -n 19 \
            /bin/sh "$guard_script" --rkc-internal-payload \
            "$guard_state" "$guard_owner_pid" "$guard_owner_birth" "$guard_mode" "$@"
        ;;
    service)
        [ -n "${XDG_RUNTIME_DIR:-}" ] || { echo "rkc resource guard: XDG_RUNTIME_DIR is required in service mode" >&2; exit 1; }
        [ -n "${DBUS_SESSION_BUS_ADDRESS:-}" ] || { echo "rkc resource guard: DBUS_SESSION_BUS_ADDRESS is required in service mode" >&2; exit 1; }
        exec systemd-run \
            --user \
            --wait \
            --pipe \
            --collect \
            --quiet \
            --service-type=exec \
            --same-dir \
            --expand-environment=no \
            --unit "$guard_unit" \
            --setenv="HOME=${HOME:-/nonexistent}" \
            --setenv="PATH=$PATH" \
            --setenv="XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR" \
            --setenv="DBUS_SESSION_BUS_ADDRESS=$DBUS_SESSION_BUS_ADDRESS" \
            --setenv="RKC_RESOURCE_GUARD_UNIT=$guard_unit" \
            --setenv="GOMAXPROCS=$GOMAXPROCS" \
            --setenv="OMP_NUM_THREADS=$OMP_NUM_THREADS" \
            --setenv="OPENBLAS_NUM_THREADS=$OPENBLAS_NUM_THREADS" \
            --setenv="MKL_NUM_THREADS=$MKL_NUM_THREADS" \
            --setenv="NUMEXPR_NUM_THREADS=$NUMEXPR_NUM_THREADS" \
            --setenv="CMAKE_BUILD_PARALLEL_LEVEL=$CMAKE_BUILD_PARALLEL_LEVEL" \
            --setenv="CARGO_BUILD_JOBS=$CARGO_BUILD_JOBS" \
            --setenv="GOFLAGS=$GOFLAGS" \
            --setenv="GOMEMLIMIT=$GOMEMLIMIT" \
            --setenv="CGO_ENABLED=$guard_cgo_enabled" \
            --setenv="RKC_REQUIRE_IO_CONTROLLER=$guard_require_io_controller" \
            --setenv="RKC_CPU_QUOTA_PERCENT=$guard_cpu_quota_percent" \
            --setenv="RKC_MEMORY_HIGH_MIB=$guard_memory_high_mib" \
            --setenv="RKC_MEMORY_MAX_MIB=$guard_memory_max_mib" \
            --setenv="RKC_MEMORY_SWAP_MAX_MIB=$guard_memory_swap_max_mib" \
            --setenv="RKC_GO_MEMORY_LIMIT_MIB=$guard_go_memory_limit_mib" \
            --setenv="RKC_HOST_AVAILABLE_MEMORY_MIN_MIB=$guard_host_available_memory_min_mib" \
            --setenv="RKC_HIGHER_PRIORITY_POLICY=$higher_priority_policy" \
            --setenv="RKC_HIGHER_PRIORITY_MARKERS=$higher_priority_markers" \
            --setenv="RKC_HIGHER_PRIORITY_LOAD_MAX=$guard_priority_load_max" \
            --property "Description=$guard_description" \
            --property "UMask=$guard_record_umask" \
            --property CPUWeight=1 \
            --property IOWeight=1 \
            --property "CPUQuota=${guard_cpu_quota_percent}%" \
            --property "MemoryHigh=${guard_memory_high_mib}M" \
            --property "MemoryMax=${guard_memory_max_mib}M" \
            --property "MemorySwapMax=${guard_memory_swap_max_mib}M" \
            --property TasksMax=128 \
            --property OOMPolicy=stop \
            --property KillMode=control-group \
            --property TimeoutStopSec=5s \
            -- \
            choom -n 750 -- ionice -c 3 nice -n 19 \
            /bin/sh "$guard_script" --rkc-internal-payload \
            "$guard_state" "$guard_owner_pid" "$guard_owner_birth" "$guard_mode" "$@"
        ;;
    *)
        echo "rkc resource guard: RKC_RESOURCE_GUARD_MODE must be scope or service" >&2
        exit 2
        ;;
esac
}

case "${1-}" in
    --rkc-internal-watchdog|--rkc-internal-payload|--rkc-internal-launcher)
        guard_internal_mode=$1
        shift
        # Internal receipts stay private even when the caller permits public
        # files. Only the actual payload regains its original creation mask.
        umask 077
        [ "$#" -ge 4 ] || exit 75
        guard_owned_state "$1" "$2" "$3" "$4" || exit 75
        shift 4
        if [ "$guard_internal_mode" = --rkc-internal-launcher ]; then
            [ "$#" -gt 0 ] || exit 75
            guard_actual_parent=$(guard_process_parent "$$") || exit 75
            [ "$guard_actual_parent" = "$guard_owner_pid" ] || exit 75
            [ ! -f "$guard_state/closing" ] && guard_process_matches "$guard_owner_pid" "$guard_owner_birth" || exit 75
            guard_child_birth=$(guard_process_birth "$$") || exit 75
            { printf '%s %s\n' "$$" "$guard_child_birth" > "$guard_state/launcher"; } 2>/dev/null || exit 75
            [ ! -f "$guard_state/closing" ] && guard_process_matches "$guard_owner_pid" "$guard_owner_birth" || exit 75
            exec 3<&-
            umask "$guard_record_umask"
            guard_launch_unit "$@"
            exit 75
        fi
        if [ "$guard_internal_mode" = --rkc-internal-payload ]; then
            [ "$#" -gt 0 ] || exit 75
            [ "${RKC_RESOURCE_GUARD_UNIT:-}" = "$guard_unit" ] || exit 75
            # This gate also rejects a delayed unit creation after its owner
            # has exited and removed private state; no workload may start late.
            [ ! -f "$guard_state/closing" ] && guard_process_matches "$guard_owner_pid" "$guard_owner_birth" || exit 75
            if ! guard_host_memory_available "$guard_record_floor"; then
                { : > "$guard_state/memory-violation"; } 2>/dev/null || true
                exit 75
            fi
            umask "$guard_record_umask"
            exec "$@"
        fi
        [ "$#" -eq 0 ] || exit 75
        guard_actual_parent=$(guard_process_parent "$$") || exit 75
        [ "$guard_actual_parent" = "$guard_owner_pid" ] || exit 75
        # setsid gave the watchdog its own session/process group. It retains
        # no caller stdin/stdout descriptors and survives launcher-group death.
        : > "$guard_state/ready"
        while [ ! -f "$guard_state/closing" ] && guard_process_matches "$guard_owner_pid" "$guard_owner_birth"; do
            if ! guard_host_memory_available "$guard_record_floor"; then
                : > "$guard_state/memory-violation"
                break
            fi
            sleep 1
        done
        : > "$guard_state/closing"
        if ! guard_cleanup_owned_unit; then
            : > "$guard_state/cleanup-failed"
        fi
        : > "$guard_state/done"
        if [ ! -f "$guard_state/cleanup-failed" ] && ! guard_process_matches "$guard_owner_pid" "$guard_owner_birth"; then
            rm -rf -- "$guard_state"
        fi
        exit 0
        ;;
esac

if [ "$#" -eq 0 ]; then
    echo "usage: scripts/with-rkc-limits.sh command [args ...]" >&2
    exit 2
fi

# Keep language runtimes and build tools from manufacturing parallel work that
# merely contends inside the one-core quota. These are safety policy values, not
# tuning hints, so an ambient high-parallelism environment cannot override them.
GOMAXPROCS=1
OMP_NUM_THREADS=1
OPENBLAS_NUM_THREADS=1
MKL_NUM_THREADS=1
NUMEXPR_NUM_THREADS=1
CMAKE_BUILD_PARALLEL_LEVEL=1
CARGO_BUILD_JOBS=1
GOFLAGS="${GOFLAGS:+$GOFLAGS }-p=1"
export GOMAXPROCS OMP_NUM_THREADS OPENBLAS_NUM_THREADS MKL_NUM_THREADS
export NUMEXPR_NUM_THREADS CMAKE_BUILD_PARALLEL_LEVEL CARGO_BUILD_JOBS GOFLAGS

guard_cpu_quota_percent=${RKC_CPU_QUOTA_PERCENT:-100}
case "$guard_cpu_quota_percent" in
    ''|*[!0123456789]*|0?*)
        echo "rkc resource guard: CPU quota must be an integer between 1 and 100 percent of one core" >&2
        exit 2
        ;;
esac
if [ "${#guard_cpu_quota_percent}" -gt 3 ] || [ "$guard_cpu_quota_percent" -lt 1 ] || [ "$guard_cpu_quota_percent" -gt 100 ]; then
    echo "rkc resource guard: CPU quota must be an integer between 1 and 100 percent of one core" >&2
    exit 2
fi
RKC_CPU_QUOTA_PERCENT=$guard_cpu_quota_percent
export RKC_CPU_QUOTA_PERCENT

# Transient services start with the user manager's clean environment rather
# than the caller's environment. Preserve only the caller-controlled build and
# controller policy values that the guarded command must observe.
guard_cgo_enabled=${CGO_ENABLED-}
case "$guard_cgo_enabled" in
    ''|0|1) ;;
    *)
        echo "rkc resource guard: CGO_ENABLED must be empty, 0, or 1" >&2
        exit 2
        ;;
esac
guard_require_io_controller=${RKC_REQUIRE_IO_CONTROLLER:-0}
case "$guard_require_io_controller" in
    0|1) ;;
    *)
        echo "rkc resource guard: RKC_REQUIRE_IO_CONTROLLER must be 0 or 1" >&2
        exit 2
        ;;
esac

guard_memory_high_mib=${RKC_MEMORY_HIGH_MIB:-4096}
guard_memory_max_mib=${RKC_MEMORY_MAX_MIB:-4608}
guard_memory_swap_max_mib=${RKC_MEMORY_SWAP_MAX_MIB:-256}
guard_go_memory_limit_mib=${RKC_GO_MEMORY_LIMIT_MIB:-$guard_memory_high_mib}
guard_host_available_memory_min_mib=${RKC_HOST_AVAILABLE_MEMORY_MIN_MIB:-0}
invalid_memory_profile() {
    echo "rkc resource guard: memory profile must use integer MiB values with high=64..4096, max=high..4608, swap=0..256, Go limit=64..high, and host reserve=0..65536" >&2
    exit 2
}
for guard_memory_value in "$guard_memory_high_mib" "$guard_memory_max_mib" "$guard_memory_swap_max_mib" "$guard_go_memory_limit_mib"; do
    case "$guard_memory_value" in
        ''|*[!0123456789]*) invalid_memory_profile ;;
    esac
    [ "${#guard_memory_value}" -le 4 ] || invalid_memory_profile
    case "$guard_memory_value" in
        0?*) invalid_memory_profile ;;
    esac
done
[ "$guard_memory_high_mib" -ge 64 ] && [ "$guard_memory_high_mib" -le 4096 ] || invalid_memory_profile
[ "$guard_memory_max_mib" -ge "$guard_memory_high_mib" ] && [ "$guard_memory_max_mib" -le 4608 ] || invalid_memory_profile
[ "$guard_memory_swap_max_mib" -le 256 ] || invalid_memory_profile
[ "$guard_go_memory_limit_mib" -ge 64 ] && [ "$guard_go_memory_limit_mib" -le "$guard_memory_high_mib" ] || invalid_memory_profile
case "$guard_host_available_memory_min_mib" in
    ''|*[!0123456789]*) invalid_memory_profile ;;
    0?*) invalid_memory_profile ;;
esac
[ "${#guard_host_available_memory_min_mib}" -le 5 ] || invalid_memory_profile
[ "$guard_host_available_memory_min_mib" -le 65536 ] || invalid_memory_profile
RKC_MEMORY_HIGH_MIB=$guard_memory_high_mib
RKC_MEMORY_MAX_MIB=$guard_memory_max_mib
RKC_MEMORY_SWAP_MAX_MIB=$guard_memory_swap_max_mib
RKC_GO_MEMORY_LIMIT_MIB=$guard_go_memory_limit_mib
RKC_HOST_AVAILABLE_MEMORY_MIN_MIB=$guard_host_available_memory_min_mib
GOMEMLIMIT=${guard_go_memory_limit_mib}MiB
export RKC_MEMORY_HIGH_MIB RKC_MEMORY_MAX_MIB RKC_MEMORY_SWAP_MAX_MIB
export RKC_GO_MEMORY_LIMIT_MIB RKC_HOST_AVAILABLE_MEMORY_MIN_MIB GOMEMLIMIT

# Configured workload classes are explicitly higher priority on shared hosts.
# The strict policy refuses to start new RKC work while one is visible (callers
# receive EX_TEMPFAIL (75) and can retry later). The default yield policy starts
# inside the subordinate envelope below and leaves continuous CPU-load
# monitoring to the guarded RKC binary.
higher_priority_policy=${RKC_HIGHER_PRIORITY_POLICY:-yield}
case "$higher_priority_policy" in
    refuse|yield) ;;
    *)
        echo "rkc resource guard: RKC_HIGHER_PRIORITY_POLICY must be refuse or yield" >&2
        exit 2
        ;;
esac
higher_priority_markers=${RKC_HIGHER_PRIORITY_MARKERS:-torchrun,lm_eval}
invalid_priority_markers() {
    echo "rkc resource guard: RKC_HIGHER_PRIORITY_MARKERS must contain 1-16 unique lower-case ASCII markers (1-32 bytes each, 255 bytes total), separated by commas" >&2
    exit 2
}
if [ "${#higher_priority_markers}" -gt 255 ]; then
    invalid_priority_markers
fi
case "$higher_priority_markers" in
    ''|,*|*,|*,,*|*[!abcdefghijklmnopqrstuvwxyz0123456789_,]*) invalid_priority_markers ;;
esac
marker_count=0
seen_markers=,
saved_ifs=$IFS
IFS=,
for priority_class in $higher_priority_markers; do
    marker_count=$((marker_count + 1))
    if [ "$marker_count" -gt 16 ] || [ "${#priority_class}" -gt 32 ]; then
        invalid_priority_markers
    fi
    case "$priority_class" in
        [abcdefghijklmnopqrstuvwxyz0123456789]*) ;;
        *) invalid_priority_markers ;;
    esac
    case "$seen_markers" in
        *",$priority_class,"*) invalid_priority_markers ;;
    esac
    seen_markers="${seen_markers}${priority_class},"
done
IFS=$saved_ifs
RKC_HIGHER_PRIORITY_POLICY=$higher_priority_policy
RKC_HIGHER_PRIORITY_MARKERS=$higher_priority_markers
guard_priority_load_max=${RKC_HIGHER_PRIORITY_LOAD_MAX-}
RKC_HIGHER_PRIORITY_LOAD_MAX=$guard_priority_load_max
export RKC_HIGHER_PRIORITY_POLICY RKC_HIGHER_PRIORITY_MARKERS
export RKC_HIGHER_PRIORITY_LOAD_MAX
for required in pgrep ps readlink tr systemd-run systemctl ionice nice choom awk stat id mktemp setsid sleep timeout rm; do
    if ! command -v "$required" >/dev/null 2>&1; then
        echo "rkc resource guard: required command not found: $required" >&2
        exit 1
    fi
done
priority_classes=$(printf '%s' "$higher_priority_markers" | tr ',' ' ')

ancestry=" $$ "
ancestor=$$
while [ "$ancestor" -gt 1 ]; do
    ancestor=$(ps -o ppid= -p "$ancestor" 2>/dev/null | tr -d '[:space:]')
    [ -n "$ancestor" ] || break
    ancestry="$ancestry$ancestor "
done
higher_priority=$(
    seen=' '
    for priority_class in $priority_classes; do
        priority_first=${priority_class%"${priority_class#?}"}
        priority_rest=${priority_class#?}
        # Bracketing the first byte prevents this pgrep invocation from
        # matching its own regular-expression argument.
        priority_pattern="[$priority_first]$priority_rest"
        # Ask pgrep for PIDs only. Reading `pgrep -a` output would ingest an
        # unrelated process's raw argv, whose embedded newlines could be
        # mistaken for records and violate the guard's fixed PID/class output.
        for process_id in $(pgrep -f "$priority_pattern" || true); do
            case "$process_id" in
                ''|*[!0-9]*) continue ;;
            esac
            case "$ancestry" in
                *" $process_id "*) ;;
                *)
                    case "$seen" in
                        *" $process_id "*) continue ;;
                    esac
                    seen="$seen$process_id "
                    printf 'pid=%s class=%s\n' "$process_id" "$priority_class"
                    ;;
            esac
        done
    done
    # A common training launch uses a relative interpreter and relative script
    # from inside a marked checkout, leaving no workload marker in argv.
    # Inspect only interpreter PIDs, reduce cwd immediately to a fixed class,
    # and never print the raw path.
    interpreter_pattern='(^|/)(python([0-9.]+)?|pypy([0-9.]*)?|sh|bash|dash|ksh|mksh|zsh)([[:space:]]|$)'
    for process_id in $(pgrep -f "$interpreter_pattern" || true); do
        case "$process_id" in
            ''|*[!0-9]*) continue ;;
        esac
        case "$ancestry" in
            *" $process_id "*) continue ;;
        esac
        case "$seen" in
            *" $process_id "*) continue ;;
        esac
        process_cwd=$(readlink "/proc/$process_id/cwd" 2>/dev/null || true)
        normalized_cwd=$(printf '%s' "$process_cwd" | tr '[:upper:]' '[:lower:]')
        path_tokens=$(printf '%s' "$normalized_cwd" | tr -cs 'abcdefghijklmnopqrstuvwxyz0123456789_' ' ')
        priority_class=
        for configured_marker in $priority_classes; do
            for path_token in $path_tokens; do
                if [ "$path_token" = "$configured_marker" ]; then
                    priority_class=$configured_marker
                    break 2
                fi
            done
        done
        unset process_cwd normalized_cwd path_tokens
        [ -n "$priority_class" ] || continue
        seen="$seen$process_id "
        printf 'pid=%s class=%s\n' "$process_id" "$priority_class"
    done
)
if [ -n "$higher_priority" ]; then
    case "$higher_priority_policy" in
        refuse)
            echo "rkc resource guard: configured higher-priority work is active; refusing to start" >&2
            echo "$higher_priority" >&2
            exit 75
            ;;
        yield)
            echo "rkc resource guard: configured higher-priority work is visible; yield policy keeps this workload subordinate (one core, minimum weight, idle I/O, OOM-first)" >&2
            echo "$higher_priority" >&2
            ;;
    esac
fi

guard_mode=${RKC_RESOURCE_GUARD_MODE:-scope}
case "$guard_mode" in
    scope) ;;
    service)
        [ -n "${XDG_RUNTIME_DIR:-}" ] || { echo "rkc resource guard: XDG_RUNTIME_DIR is required in service mode" >&2; exit 1; }
        [ -n "${DBUS_SESSION_BUS_ADDRESS:-}" ] || { echo "rkc resource guard: DBUS_SESSION_BUS_ADDRESS is required in service mode" >&2; exit 1; }
        ;;
    *)
        echo "rkc resource guard: RKC_RESOURCE_GUARD_MODE must be scope or service" >&2
        exit 2
        ;;
esac

if ! guard_host_memory_available "$guard_host_available_memory_min_mib"; then
    echo "rkc resource guard: host memory reserve is unavailable or cannot be verified; refusing to start" >&2
    exit 75
fi

guard_script=$(readlink -f "$0" 2>/dev/null) || { echo "rkc resource guard: cannot locate the supervisor" >&2; exit 1; }
[ -f "$guard_script" ] || { echo "rkc resource guard: cannot locate the supervisor" >&2; exit 1; }
guard_owner_pid=$$
guard_owner_birth=$(guard_process_birth "$$") || { echo "rkc resource guard: cannot establish launcher identity" >&2; exit 1; }
guard_payload_umask=$(umask)
umask 077
guard_state=$(mktemp -d "${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}}/rkc-guard.XXXXXXXXXXXXXXXX" 2>/dev/null) || { echo "rkc resource guard: cannot create private supervisor state" >&2; exit 1; }
guard_token=${guard_state##*/rkc-guard.}
guard_unit="rkc-low-$guard_owner_pid-$guard_token.$guard_mode"
if ! { printf '%s %s %s %s %s %s\n' "$guard_owner_pid" "$guard_owner_birth" "$guard_mode" "$guard_unit" "$guard_host_available_memory_min_mib" "$guard_payload_umask" > "$guard_state/owner"; } 2>/dev/null ||
    ! guard_owned_state "$guard_state" "$guard_owner_pid" "$guard_owner_birth" "$guard_mode"; then
    rm -rf -- "$guard_state" 2>/dev/null
    echo "rkc resource guard: cannot bind private supervisor state" >&2
    exit 1
fi
umask "$guard_payload_umask"

guard_finish() {
    guard_exit_status=$?
    trap - 0 HUP INT TERM
    set +e
    umask 077
    { : > "$guard_state/closing"; } 2>/dev/null
    guard_finish_wait=0
    while [ ! -f "$guard_state/done" ] && [ "$guard_finish_wait" -lt 45 ]; do
        if [ -z "${guard_watchdog_birth:-}" ] || ! guard_process_matches "$guard_watchdog_pid" "$guard_watchdog_birth"; then
            break
        fi
        sleep 1
        guard_finish_wait=$((guard_finish_wait + 1))
    done
    if [ ! -f "$guard_state/done" ] || [ -f "$guard_state/cleanup-failed" ]; then
        # The owner is a second cleanup path if the watchdog itself fails.
        if [ -n "${guard_watchdog_birth:-}" ] && guard_process_matches "$guard_watchdog_pid" "$guard_watchdog_birth"; then
            kill -KILL "$guard_watchdog_pid" 2>/dev/null || true
        fi
        if guard_cleanup_owned_unit; then
            rm -f -- "$guard_state/cleanup-failed" 2>/dev/null
            { : > "$guard_state/done"; } 2>/dev/null
        fi
    fi
    if [ -f "$guard_state/done" ] && [ ! -f "$guard_state/cleanup-failed" ]; then
        rm -rf -- "$guard_state" 2>/dev/null
    else
        echo "rkc resource guard: could not verify owned workload cleanup" >&2
        [ "$guard_exit_status" -ne 0 ] || guard_exit_status=1
    fi
    if [ -n "${guard_watchdog_pid:-}" ]; then
        if [ -n "${guard_watchdog_birth:-}" ] && guard_process_matches "$guard_watchdog_pid" "$guard_watchdog_birth"; then
            kill -KILL "$guard_watchdog_pid" 2>/dev/null || true
        fi
        wait "$guard_watchdog_pid" 2>/dev/null || true
    fi
    exit "$guard_exit_status"
}
trap guard_finish 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# POSIX asynchronous commands otherwise inherit /dev/null as stdin. Keep a
# deliberate descriptor for the real launcher, but give none to the watchdog.
exec 3<&0
choom -n 750 -- ionice -c 3 nice -n 19 \
    setsid /bin/sh "$guard_script" --rkc-internal-watchdog \
    "$guard_state" "$guard_owner_pid" "$guard_owner_birth" "$guard_mode" \
    </dev/null >/dev/null 2>&1 3<&- &
guard_watchdog_pid=$!
guard_watchdog_birth=$(guard_process_birth "$guard_watchdog_pid") || guard_watchdog_birth=
guard_ready_wait=0
while [ ! -f "$guard_state/ready" ]; do
    if [ -z "$guard_watchdog_birth" ] || ! guard_process_matches "$guard_watchdog_pid" "$guard_watchdog_birth" || [ "$guard_ready_wait" -ge 5 ]; then
        echo "rkc resource guard: independent supervisor did not start" >&2
        exit 1
    fi
    sleep 1
    guard_ready_wait=$((guard_ready_wait + 1))
done
/bin/sh "$guard_script" --rkc-internal-launcher \
    "$guard_state" "$guard_owner_pid" "$guard_owner_birth" "$guard_mode" "$@" <&3 &
guard_launcher_pid=$!
exec 3<&-
guard_launcher_birth=$(guard_process_birth "$guard_launcher_pid") || guard_launcher_birth=
while [ -n "$guard_launcher_birth" ] && guard_process_matches "$guard_launcher_pid" "$guard_launcher_birth"; do
    if [ -f "$guard_state/memory-violation" ]; then
        echo "rkc resource guard: host memory reserve is unavailable or cannot be verified; cancelling workload" >&2
        exit 75
    fi
    if ! guard_process_matches "$guard_watchdog_pid" "$guard_watchdog_birth"; then
        echo "rkc resource guard: independent supervisor stopped; cancelling workload" >&2
        exit 75
    fi
    sleep 1
done
guard_command_status=0
wait "$guard_launcher_pid" || guard_command_status=$?
if [ -f "$guard_state/memory-violation" ]; then
    echo "rkc resource guard: host memory reserve is unavailable or cannot be verified; cancelling workload" >&2
    exit 75
fi
exit "$guard_command_status"
