#!/bin/sh
# Run an RKC development or inference workload in a deliberately subordinate
# cgroup. The defaults protect concurrent, higher-priority training workloads:
# at most one CPU core, 2 GiB soft / 2.5 GiB hard memory, idle I/O scheduling,
# lowest CPU niceness, idle I/O scheduling (plus IOWeight=1 when the user
# manager delegates that controller), and a high OOM-kill preference.
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

# ERAIS training and evaluation are explicitly higher-priority workloads on
# shared development hosts. Refuse to start new RKC work while one is visible;
# callers receive EX_TEMPFAIL (75) and can retry later. The bracketed regular
# expressions do not match this pgrep command's own argv.
for required in pgrep ps tr systemd-run ionice nice choom; do
    if ! command -v "$required" >/dev/null 2>&1; then
        echo "rkc resource guard: required command not found: $required" >&2
        exit 1
    fi
done

ancestry=" $$ "
ancestor=$$
while [ "$ancestor" -gt 1 ]; do
    ancestor=$(ps -o ppid= -p "$ancestor" 2>/dev/null | tr -d '[:space:]')
    [ -n "$ancestor" ] || break
    ancestry="$ancestry$ancestor "
done
matches=$(pgrep -af '[e]rais|[t]orchrun|[l]m_eval' || true)
higher_priority=$(
    printf '%s\n' "$matches" |
        while IFS=' ' read -r process_id command_line; do
            [ -n "$process_id" ] || continue
            case "$ancestry" in
                *" $process_id "*) ;;
                *) printf '%s %s\n' "$process_id" "$command_line" ;;
            esac
        done
)
if [ -n "$higher_priority" ]; then
    echo "rkc resource guard: higher-priority ERAIS work is active; refusing to start" >&2
    echo "$higher_priority" >&2
    exit 75
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
            --unit "$unit" \
            --property CPUWeight=1 \
            --property IOWeight=1 \
            --property CPUQuota=100% \
            --property MemoryHigh=2048M \
            --property MemoryMax=2560M \
            --property MemorySwapMax=256M \
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
            --unit "$unit" \
            --setenv="HOME=${HOME:-/nonexistent}" \
            --setenv="PATH=$PATH" \
            --setenv="XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR" \
            --setenv="DBUS_SESSION_BUS_ADDRESS=$DBUS_SESSION_BUS_ADDRESS" \
            --property CPUWeight=1 \
            --property IOWeight=1 \
            --property CPUQuota=100% \
            --property MemoryHigh=2048M \
            --property MemoryMax=2560M \
            --property MemorySwapMax=256M \
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
