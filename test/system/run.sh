#!/usr/bin/env bash
# System test of the OS part of pkg/agent/host (make test-system).
#
# Boots a throwaway Debian 13 container with systemd as PID 1 and a local
# apt repository, copies in a test binary built from this directory
# (build tag system_integration) and lets it drive the real thing: read the
# inventory, upgrade blocked packages, refuse others, see the reboot flag,
# and reboot. The container really goes down and comes back; the boot id
# recorded on the state volume before must differ after.
#
# Nothing on this machine is installed, upgraded or rebooted. Needs Docker
# and Go. Everything it writes is under .tmp/system-test in the repository,
# in a directory of this run's own, so several runs can go on at once; that
# directory, the container and its volume are removed on exit, also after a
# failure.
#
# The host image, the reboot mechanism and the state volume are reusable for
# other tests (the full agent binary with a lease that survives the reboot):
# see lib.sh.
#
# Environment:
#   SYSBOX_IMAGE   image tag to build and use (default upgradoor-system-test:local)
#   SYSBOX_KEEP=1  keep the container and volume for inspection
set -euo pipefail

# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

NAME="upgradoor-system-test-$$-$RANDOM"
VOLUME="$NAME-state"
# This run's own artifacts.
RUN_DIR="$SYSBOX_WORKDIR/$NAME"
TEST_BIN="$RUN_DIR/upgradoor-system.test"
TEST_BIN_INSIDE=/usr/local/bin/upgradoor-system.test
FAILED=0

pass() {
    printf 'PASS: %s\n' "$*"
}

fail() {
    printf 'FAIL: %s\n' "$*"
    FAILED=1
}

# check <description> <command...>: one PASS/FAIL line per step.
check() {
    local what=$1
    shift

    if "$@"; then
        pass "$what"
    else
        fail "$what"

        return 1
    fi
}

cleanup() {
    local status=$?

    trap - EXIT INT TERM

    if [[ "${SYSBOX_KEEP:-}" == 1 ]]; then
        sysbox_log "keeping container $NAME and volume $VOLUME"
    else
        sysbox_remove "$NAME" "$VOLUME"
    fi

    rm -rf "$RUN_DIR"

    if ((status != 0 || FAILED != 0)); then
        printf 'FAIL: system test\n'
        exit 1
    fi

    printf 'PASS: system test\n'
    exit 0
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

build_test_binary() {
    mkdir -p "$RUN_DIR/gotmp"
    (
        cd "$SYSBOX_REPO"
        # Go's scratch space stays inside the repository, off /tmp.
        GOTMPDIR="$RUN_DIR/gotmp" CGO_ENABLED=0 GOOS=linux \
            go test -c -tags system_integration -o "$TEST_BIN" ./test/system/
    )
}

# run_phase <go test name>: run one test function inside the host.
run_phase() {
    sysbox_exec "$NAME" env UPGRADOOR_SYSTEM_TEST=1 "UPGRADOOR_STATE_DIR=$SYSBOX_STATE_DIR" \
        "$TEST_BIN_INSIDE" -test.v -test.count=1 -test.run "^$1\$"
}

state_file() {
    sysbox_exec "$NAME" cat "$SYSBOX_STATE_DIR/$1"
}

mkdir -p "$RUN_DIR"

check "test binary built (CGO_ENABLED=0, tag system_integration)" build_test_binary
check "host image built" sysbox_build_image
check "host booted (systemd is PID 1)" sysbox_start "$NAME" "$VOLUME"
check "host finished booting" sysbox_wait_ready "$NAME"
check "test binary copied in" sysbox_copy "$NAME" "$TEST_BIN" "$TEST_BIN_INSIDE"

check "inventory, status, refusal, lists lock, upgrade, reboot flag (TestPreReboot)" run_phase TestPreReboot

boots_before=$(sysbox_boot_count "$NAME")
boot_id_before=$(sysbox_boot_id "$NAME")

# The shutdown may kill the test process right after Reboot() returned, so
# its exit status proves nothing either way; the marker it writes to the
# state volume is checked after the reboot.
if run_phase TestReboot; then
    pass "Reboot() accepted (TestReboot)"
else
    sysbox_log "TestReboot ended with status $? (expected when the shutdown wins the race)"
fi

check "host went down and came back" sysbox_wait_reboot "$NAME" "$boots_before"

boot_id_after=$(sysbox_boot_id "$NAME")
boot_id_recorded=$(state_file boot-id-before-reboot)

sysbox_log "boot id before: $boot_id_before"
sysbox_log "boot id after:  $boot_id_after"

check "state volume survived the reboot" test -n "$boot_id_recorded"
check "state volume holds the boot id from before the reboot" \
    test "$boot_id_recorded" = "$boot_id_before"
check "boot id changed" test "$boot_id_after" != "$boot_id_before"
check "Reboot() returned without error before the host went down" \
    sysbox_exec "$NAME" test -s "$SYSBOX_STATE_DIR/reboot-accepted"

check "state after the reboot (TestPostReboot)" run_phase TestPostReboot
