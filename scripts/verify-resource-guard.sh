#!/bin/sh
# Prove that the local low-priority wrapper is attached to a delegated cgroup
# whose effective controls match RKC's documented safety envelope.
set -eu

fail() {
    echo "rkc resource guard verification: $*" >&2
    exit 1
}

[ "$(uname -s)" = "Linux" ] || fail "Linux cgroup v2 is required"

for required in systemctl systemd-run awk cat grep ionice nice choom pgrep ps tr; do
    command -v "$required" >/dev/null 2>&1 || fail "required command not found: $required"
done

[ -n "${XDG_RUNTIME_DIR:-}" ] || fail "XDG_RUNTIME_DIR is not set"
[ -n "${DBUS_SESSION_BUS_ADDRESS:-}" ] || fail "DBUS_SESSION_BUS_ADDRESS is not set"
[ -S "$XDG_RUNTIME_DIR/bus" ] || fail "user-systemd bus is unavailable"
systemctl --user is-active --quiet default.target || fail "user-systemd default target is not active"

controllers_file=/sys/fs/cgroup/cgroup.controllers
[ -r "$controllers_file" ] || fail "unified cgroup v2 controllers are unavailable"
controllers=$(cat "$controllers_file")
for controller in cpu memory pids; do
    case " $controllers " in
        *" $controller "*) ;;
        *) fail "required cgroup v2 controller is unavailable: $controller" ;;
    esac
done

guard_require_io_controller=${RKC_REQUIRE_IO_CONTROLLER:-0}
guard_memory_high_mib=${RKC_MEMORY_HIGH_MIB:-4096}
guard_memory_max_mib=${RKC_MEMORY_MAX_MIB:-4608}
guard_memory_swap_max_mib=${RKC_MEMORY_SWAP_MAX_MIB:-256}
guard_go_memory_limit_mib=${RKC_GO_MEMORY_LIMIT_MIB:-$guard_memory_high_mib}
guard_host_available_memory_min_mib=${RKC_HOST_AVAILABLE_MEMORY_MIN_MIB:-0}
CGO_ENABLED=0 \
GOFLAGS=-mod=readonly \
RKC_REQUIRE_IO_CONTROLLER="$guard_require_io_controller" \
sh scripts/with-rkc-limits.sh sh -c '
    set -eu
    fail() {
        echo "rkc resource guard verification: $*" >&2
        exit 1
    }
    [ "${GOMAXPROCS:-}" = 1 ] || fail "GOMAXPROCS did not survive guard entry"
    [ "${OMP_NUM_THREADS:-}" = 1 ] || fail "OMP_NUM_THREADS did not survive guard entry"
    [ "${OPENBLAS_NUM_THREADS:-}" = 1 ] || fail "OPENBLAS_NUM_THREADS did not survive guard entry"
    [ "${MKL_NUM_THREADS:-}" = 1 ] || fail "MKL_NUM_THREADS did not survive guard entry"
    [ "${NUMEXPR_NUM_THREADS:-}" = 1 ] || fail "NUMEXPR_NUM_THREADS did not survive guard entry"
    [ "${CMAKE_BUILD_PARALLEL_LEVEL:-}" = 1 ] || fail "CMAKE_BUILD_PARALLEL_LEVEL did not survive guard entry"
    [ "${CARGO_BUILD_JOBS:-}" = 1 ] || fail "CARGO_BUILD_JOBS did not survive guard entry"
    [ "${GOFLAGS:-}" = "-mod=readonly -p=1" ] || fail "GOFLAGS did not survive guard entry"
    [ "${GOMEMLIMIT:-}" = "$5"MiB ] || fail "Go memory limit did not survive guard entry"
    [ "${CGO_ENABLED:-}" = 0 ] || fail "CGO_ENABLED did not survive guard entry"
    [ "${RKC_REQUIRE_IO_CONTROLLER:-}" = "$1" ] || fail "RKC_REQUIRE_IO_CONTROLLER did not survive guard entry"
    [ "${RKC_MEMORY_HIGH_MIB:-}" = "$2" ] || fail "soft-memory profile did not survive guard entry"
    [ "${RKC_MEMORY_MAX_MIB:-}" = "$3" ] || fail "hard-memory profile did not survive guard entry"
    [ "${RKC_MEMORY_SWAP_MAX_MIB:-}" = "$4" ] || fail "swap profile did not survive guard entry"
    [ "${RKC_GO_MEMORY_LIMIT_MIB:-}" = "$5" ] || fail "Go-memory profile did not survive guard entry"
    [ "${RKC_HOST_AVAILABLE_MEMORY_MIN_MIB:-}" = "$6" ] || fail "host-memory reserve did not survive guard entry"
    expected_high=$(( $2 * 1024 * 1024 ))
    expected_max=$(( $3 * 1024 * 1024 ))
    expected_swap=$(( $4 * 1024 * 1024 ))
    relative=
    while IFS=: read -r hierarchy controllers path; do
        [ "$hierarchy" = 0 ] && relative=$path
    done < /proc/self/cgroup
    [ -n "$relative" ] || fail "guarded process has no unified cgroup path"
    unit=${relative##*/}
    case "$unit" in
        rkc-low-*.scope|rkc-low-*.service) ;;
        *)
            # A hosted runner may place the transient service in a cgroup
            # namespace whose visible root is `/`. In that case, prove the
            # exact service identity through both its injected name and
            # systemd MainPID instead of trusting a hidden host-relative path.
            [ "$relative" = / ] || fail "guarded process is not inside an RKC unit: $unit"
            unit=${RKC_RESOURCE_GUARD_UNIT:-}
            case "$unit" in
                rkc-low-*.service) ;;
                *) fail "guarded cgroup namespace has no bound RKC service identity" ;;
            esac
            main_pid=$(systemctl --user show --property=MainPID --value "$unit")
            [ "$main_pid" = "$$" ] || fail "guarded service MainPID does not match the probe"
            ;;
    esac
    cgroup=/sys/fs/cgroup$relative
control_group=$(systemctl --user show --property=ControlGroup --value "$unit")
case "$control_group" in
    */"$unit") ;;
    *) fail "systemd control group is not bound to the guarded unit: $control_group" ;;
esac
if [ "$relative" = / ] && [ -d "/sys/fs/cgroup$control_group" ]; then
    cgroup=/sys/fs/cgroup$control_group
fi
    [ -d "$cgroup" ] || fail "guard cgroup is unavailable: $cgroup"

    [ "$(cat "$cgroup/cpu.weight")" = "1" ] || fail "CPUWeight is not 1"
    set -- $(cat "$cgroup/cpu.max")
    [ "$1" != "max" ] || fail "CPUQuota is unlimited"
    [ "$1" -le "$2" ] || fail "CPUQuota exceeds one core"
    if [ -r "$cgroup/io.weight" ]; then
        grep -Eq "^default[[:space:]]+1$" "$cgroup/io.weight" || fail "IOWeight is not 1"
    else
        [ "${RKC_REQUIRE_IO_CONTROLLER:-0}" != "1" ] || fail "I/O controller is not delegated to the user manager"
        echo "rkc resource guard verification: user manager lacks I/O-controller delegation; enforcing idle ionice"
    fi
    [ "$(cat "$cgroup/memory.high")" = "$expected_high" ] || fail "MemoryHigh does not match the selected profile"
    [ "$(cat "$cgroup/memory.max")" = "$expected_max" ] || fail "MemoryMax does not match the selected profile"
    [ "$(cat "$cgroup/memory.swap.max")" = "$expected_swap" ] || fail "MemorySwapMax does not match the selected profile"
    [ "$(cat "$cgroup/pids.max")" = "128" ] || fail "TasksMax is not 128"
    [ "$(cat /proc/$$/oom_score_adj)" = "750" ] || fail "OOM score adjustment is not 750"
    nice_value=$(ps -o ni= -p $$ | tr -d "[:space:]")
    [ "$nice_value" = "19" ] || fail "process nice value is not 19"
    ionice -p $$ | grep -Eq "^idle" || fail "I/O scheduling class is not idle"
    [ "$(systemctl --user show --property=OOMPolicy --value "$unit")" = "stop" ] || fail "OOMPolicy is not stop"
' guard-probe "$guard_require_io_controller" "$guard_memory_high_mib" "$guard_memory_max_mib" \
    "$guard_memory_swap_max_mib" "$guard_go_memory_limit_mib" "$guard_host_available_memory_min_mib"

echo "rkc resource guard verification: passed"
