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

sh scripts/with-rkc-limits.sh sh -c '
    set -eu
    fail() {
        echo "rkc resource guard verification: $*" >&2
        exit 1
    }
    relative=$(awk -F: '\''$1 == "0" { print $3 }'\'' /proc/self/cgroup)
    [ -n "$relative" ] || fail "guarded process has no unified cgroup path"
    unit=${relative##*/}
    case "$unit" in
        rkc-low-*.scope|rkc-low-*.service) ;;
        *) fail "guarded process is not inside an RKC unit: $unit" ;;
    esac
    cgroup=/sys/fs/cgroup$relative
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
    [ "$(cat "$cgroup/memory.high")" = "2147483648" ] || fail "MemoryHigh is not 2 GiB"
    [ "$(cat "$cgroup/memory.max")" = "2684354560" ] || fail "MemoryMax is not 2.5 GiB"
    [ "$(cat "$cgroup/memory.swap.max")" = "268435456" ] || fail "MemorySwapMax is not 256 MiB"
    [ "$(cat "$cgroup/pids.max")" = "128" ] || fail "TasksMax is not 128"
    [ "$(cat /proc/$$/oom_score_adj)" = "750" ] || fail "OOM score adjustment is not 750"
    nice_value=$(ps -o ni= -p $$ | tr -d "[:space:]")
    [ "$nice_value" = "19" ] || fail "process nice value is not 19"
    ionice -p $$ | grep -Eq "^idle" || fail "I/O scheduling class is not idle"
    [ "$(systemctl --user show --property=OOMPolicy --value "$unit")" = "stop" ] || fail "OOMPolicy is not stop"
'

echo "rkc resource guard verification: passed"
