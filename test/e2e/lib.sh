# shellcheck shell=bash
# Helpers shared by the end-to-end scripts (test/e2e/reboot.sh and
# test/e2e/kurtosis/run.sh). Sourced, not run.
#
# The sourcing script defines api PATH (a GET of the controller, with its
# login where it has one) and may define e2e_state, which wait_until prints
# when it gives up. wait_poll is the seconds between two tries (default 3).

pass() { printf 'PASS: %s\n' "$*"; }
die() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

# wait_until DESCRIPTION SECONDS COMMAND...: poll until the command succeeds.
wait_until() {
    local what=$1 secs=$2 deadline
    shift 2
    deadline=$(($(date +%s) + secs))

    until "$@" >/dev/null 2>&1; do
        if [ "$(date +%s)" -ge "$deadline" ]; then
            printf 'state at timeout:\n'
            if declare -F e2e_state >/dev/null; then e2e_state || true; fi
            die "timed out after ${secs}s: $what"
        fi
        sleep "${wait_poll:-3}"
    done

    pass "$what"
}

# jq_true PATH [JQ-ARGS...] FILTER: the GET of PATH satisfies the jq filter.
jq_true() {
    local path=$1
    shift
    api "$path" | jq -e "$@" >/dev/null
}
