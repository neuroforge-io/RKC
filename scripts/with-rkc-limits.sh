#!/bin/sh
# Run an RKC development or inference workload in a deliberately subordinate
# cgroup. The defaults protect concurrent, higher-priority training workloads:
# at most one CPU core, no more than 4 GiB soft / 4.5 GiB hard memory, idle I/O
# scheduling, lowest CPU niceness, IOWeight=1 when the user manager delegates
# that controller, and a high OOM-kill preference. Operators may select a
# strictly smaller host profile with RKC_MEMORY_HIGH_MIB, RKC_MEMORY_MAX_MIB,
# RKC_MEMORY_SWAP_MAX_MIB, and RKC_GO_MEMORY_LIMIT_MIB.
#
# Processes matching the bounded RKC_HIGHER_PRIORITY_MARKERS classes are
# treated as explicitly more important. The generic default is
# torchrun,lm_eval. RKC_HIGHER_PRIORITY_POLICY selects how visible
# higher-priority processes admit this workload:
#   refuse (strict) - refuse to start (exit 75) whenever one is visible;
#   yield  (default) - start inside this subordinate envelope and leave
#   continuous load monitoring to the guarded RKC binary, which refuses or
#   cancels promptly when higher-priority processes become measurably busy.
set -eu

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
invalid_memory_profile() {
    echo "rkc resource guard: memory profile must use integer MiB values with high=64..4096, max=high..4608, swap=0..256, and Go limit=64..high" >&2
    exit 2
}
for guard_memory_value in "$guard_memory_high_mib" "$guard_memory_max_mib" "$guard_memory_swap_max_mib" "$guard_go_memory_limit_mib"; do
    case "$guard_memory_value" in
        ''|*[!0123456789]*) invalid_memory_profile ;;
    esac
    [ "${#guard_memory_value}" -le 4 ] || invalid_memory_profile
done
[ "$guard_memory_high_mib" -ge 64 ] && [ "$guard_memory_high_mib" -le 4096 ] || invalid_memory_profile
[ "$guard_memory_max_mib" -ge "$guard_memory_high_mib" ] && [ "$guard_memory_max_mib" -le 4608 ] || invalid_memory_profile
[ "$guard_memory_swap_max_mib" -le 256 ] || invalid_memory_profile
[ "$guard_go_memory_limit_mib" -ge 64 ] && [ "$guard_go_memory_limit_mib" -le "$guard_memory_high_mib" ] || invalid_memory_profile
RKC_MEMORY_HIGH_MIB=$guard_memory_high_mib
RKC_MEMORY_MAX_MIB=$guard_memory_max_mib
RKC_MEMORY_SWAP_MAX_MIB=$guard_memory_swap_max_mib
RKC_GO_MEMORY_LIMIT_MIB=$guard_go_memory_limit_mib
GOMEMLIMIT=${guard_go_memory_limit_mib}MiB
export RKC_MEMORY_HIGH_MIB RKC_MEMORY_MAX_MIB RKC_MEMORY_SWAP_MAX_MIB
export RKC_GO_MEMORY_LIMIT_MIB GOMEMLIMIT

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
for required in pgrep ps readlink tr systemd-run ionice nice choom; do
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

mode=${RKC_RESOURCE_GUARD_MODE:-scope}
case "$mode" in
    scope)
        unit="rkc-low-$$.scope"
        exec systemd-run \
            --user \
            --scope \
            --collect \
            --quiet \
            --expand-environment=no \
            --unit "$unit" \
            --setenv="RKC_RESOURCE_GUARD_UNIT=$unit" \
            --property CPUWeight=1 \
            --property IOWeight=1 \
            --property CPUQuota=100% \
            --property "MemoryHigh=${guard_memory_high_mib}M" \
            --property "MemoryMax=${guard_memory_max_mib}M" \
            --property "MemorySwapMax=${guard_memory_swap_max_mib}M" \
            --property TasksMax=128 \
            --property OOMPolicy=stop \
            choom -n 750 -- ionice -c 3 nice -n 19 "$@"
        ;;
    service)
        [ -n "${XDG_RUNTIME_DIR:-}" ] || { echo "rkc resource guard: XDG_RUNTIME_DIR is required in service mode" >&2; exit 1; }
        [ -n "${DBUS_SESSION_BUS_ADDRESS:-}" ] || { echo "rkc resource guard: DBUS_SESSION_BUS_ADDRESS is required in service mode" >&2; exit 1; }
        unit="rkc-low-$$.service"
        exec systemd-run \
            --user \
            --wait \
            --pipe \
            --collect \
            --quiet \
            --service-type=exec \
            --same-dir \
            --expand-environment=no \
            --unit "$unit" \
            --setenv="HOME=${HOME:-/nonexistent}" \
            --setenv="PATH=$PATH" \
            --setenv="XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR" \
            --setenv="DBUS_SESSION_BUS_ADDRESS=$DBUS_SESSION_BUS_ADDRESS" \
            --setenv="RKC_RESOURCE_GUARD_UNIT=$unit" \
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
            --setenv="RKC_MEMORY_HIGH_MIB=$guard_memory_high_mib" \
            --setenv="RKC_MEMORY_MAX_MIB=$guard_memory_max_mib" \
            --setenv="RKC_MEMORY_SWAP_MAX_MIB=$guard_memory_swap_max_mib" \
            --setenv="RKC_GO_MEMORY_LIMIT_MIB=$guard_go_memory_limit_mib" \
            --setenv="RKC_HIGHER_PRIORITY_POLICY=$higher_priority_policy" \
            --setenv="RKC_HIGHER_PRIORITY_MARKERS=$higher_priority_markers" \
            --setenv="RKC_HIGHER_PRIORITY_LOAD_MAX=$guard_priority_load_max" \
            --property CPUWeight=1 \
            --property IOWeight=1 \
            --property CPUQuota=100% \
            --property "MemoryHigh=${guard_memory_high_mib}M" \
            --property "MemoryMax=${guard_memory_max_mib}M" \
            --property "MemorySwapMax=${guard_memory_swap_max_mib}M" \
            --property TasksMax=128 \
            --property OOMPolicy=stop \
            --property KillMode=control-group \
            --property TimeoutStopSec=5s \
            -- \
            choom -n 750 -- ionice -c 3 nice -n 19 "$@"
        ;;
    *)
        echo "rkc resource guard: RKC_RESOURCE_GUARD_MODE must be scope or service" >&2
        exit 2
        ;;
esac
