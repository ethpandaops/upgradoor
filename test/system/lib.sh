#!/usr/bin/env bash
# Reusable part of the upgradoor system test harness: a throwaway Debian 13
# "host" in a container, with systemd as PID 1, that can really be rebooted.
# Source this file; run.sh is the example user.
#
#   source test/system/lib.sh
#   sysbox_build_image                       # builds $SYSBOX_IMAGE, sets $SYSBOX_IMAGE_ID
#   sysbox_start "$name" "$volume"           # boots the host
#   sysbox_wait_ready "$name"                # systemd finished booting
#   sysbox_copy "$name" bin/upgradoor /usr/local/bin/upgradoor
#   sysbox_exec "$name" systemctl start upgradoor-agent
#   boots=$(sysbox_boot_count "$name")
#   ...something inside calls "systemctl reboot"...
#   sysbox_wait_reboot "$name" "$boots"      # down, up again, booted
#   sysbox_remove "$name" "$volume"
#
# What the host is
#   Image       $SYSBOX_IMAGE (default upgradoor-system-test:local), built
#               from test/system/Dockerfile. Debian 13, systemd, dbus, apt.
#               Its only apt sources are two local repositories under
#               /opt/upgradoor-repo (see build-repo.sh for the packages);
#               the container runs with --network none.
#               sysbox_build_image also sets SYSBOX_IMAGE_ID to the id of
#               the image it built, and sysbox_start boots that one. The tag
#               is shared by every checkout on the machine; the id is not,
#               so runs in parallel (other worktrees, other suites) each get
#               the image built from their own files.
#   Privileges  Deliberately not --privileged: CAP_SYS_ADMIN (mounts,
#               cgroups) and CAP_SYS_BOOT (reboot) on top of Docker's
#               defaults, in its own PID and cgroup namespaces. That is all
#               systemd, apt and a real reboot need, and /proc/sys stays
#               read-only, so nothing in the container (systemd-sysctl, a
#               test) can change a sysctl of the machine it runs on.
#
# How to get a binary in
#   sysbox_copy (docker cp) into the container's root filesystem. Build it
#   on the host with CGO_ENABLED=0; the image has no toolchain. The root
#   filesystem survives reboots, so copy once.
#
# How a reboot is realised
#   "systemctl reboot" inside makes systemd shut down and call reboot(2).
#   In a PID namespace that does not reboot the machine: the kernel kills
#   the namespace's init and the container exits. The container has
#   --restart=always, so Docker starts it again, which is the "power-on":
#   boot.sh runs again and execs a fresh systemd. sysbox_wait_reboot waits
#   for Docker's restart counter to move and for the new systemd to finish
#   booting; sysbox_boot_count reads that counter.
#   Because a container shares its host's kernel, the real boot id would not
#   change; boot.sh therefore mounts a fresh random id over
#   /proc/sys/kernel/random/boot_id on every start. /proc/uptime is the
#   machine's and does not reset.
#
# Where state lives
#   Survives a reboot:  the container's root filesystem (installed packages,
#                       /etc, copied binaries) and the named volume mounted
#                       at $SYSBOX_STATE_DIR (/var/lib/upgradoor), which is
#                       where the agent keeps state.json and the lease.
#   Lost on a reboot:   /run (so /var/run/reboot-required), /tmp, every
#                       process, the boot id.
#   Removed with sysbox_remove: all of it. The image stays as a build cache.

SYSBOX_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SYSBOX_REPO=$(cd "$SYSBOX_DIR/../.." && pwd)

: "${SYSBOX_IMAGE:=upgradoor-system-test:local}"
# Build contexts and artifacts; inside the repository, git-ignored. Callers
# keep their own files in a directory of their own below it (run.sh:
# $SYSBOX_WORKDIR/<container name>), so that parallel runs do not collide.
: "${SYSBOX_WORKDIR:=$SYSBOX_REPO/.tmp/system-test}"
# Set by sysbox_build_image: the id of the image this run built.
: "${SYSBOX_IMAGE_ID:=}"
: "${SYSBOX_STATE_DIR:=/var/lib/upgradoor}"
: "${SYSBOX_BOOT_TIMEOUT:=120}"
# Optional: the Docker network to attach the host to (default: none) and
# extra "docker run" arguments, for tests that need more than an isolated
# host (test/e2e/reboot.sh: a controller to talk to and the Docker socket).
: "${SYSBOX_NETWORK:=none}"
if ! declare -p SYSBOX_RUN_ARGS >/dev/null 2>&1; then
    SYSBOX_RUN_ARGS=()
fi

sysbox_log() {
    printf '[system-test] %s\n' "$*" >&2
}

# sysbox_build_image: build the host image from a private context under the
# workdir, tag it $SYSBOX_IMAGE and remember its id in SYSBOX_IMAGE_ID. Call
# it directly, not in a subshell, or the id is lost.
sysbox_build_image() {
    local context status=0

    mkdir -p "$SYSBOX_WORKDIR"
    context=$(mktemp -d "$SYSBOX_WORKDIR/context.XXXXXX") || return 1
    cp "$SYSBOX_DIR/Dockerfile" "$SYSBOX_DIR/boot.sh" "$SYSBOX_DIR/build-repo.sh" "$context/"

    sysbox_log "building image $SYSBOX_IMAGE"
    SYSBOX_IMAGE_ID=$(docker build --quiet --tag "$SYSBOX_IMAGE" "$context") || status=$?
    rm -rf "$context"

    if ((status != 0)) || [[ -z "$SYSBOX_IMAGE_ID" ]]; then
        SYSBOX_IMAGE_ID=

        return 1
    fi

    sysbox_log "image id $SYSBOX_IMAGE_ID"
}

# sysbox_start <container> <volume>: create the state volume and boot the
# host from the image sysbox_build_image built (or, without a build in this
# shell, from the tag $SYSBOX_IMAGE). Both names must be unique to the caller.
sysbox_start() {
    local name=$1 volume=$2

    docker volume create --label upgradoor.test=system "$volume" >/dev/null
    docker run --detach \
        --name "$name" \
        --hostname upgradoor-test \
        --label upgradoor.test=system \
        --restart always \
        --network "${SYSBOX_NETWORK:-none}" \
        "${SYSBOX_RUN_ARGS[@]}" \
        --cap-add SYS_ADMIN \
        --cap-add SYS_BOOT \
        --security-opt apparmor=unconfined \
        --cgroupns private \
        --tmpfs /run:exec \
        --tmpfs /run/lock \
        --tmpfs /tmp:exec \
        --volume "$volume:$SYSBOX_STATE_DIR" \
        "${SYSBOX_IMAGE_ID:-$SYSBOX_IMAGE}" >/dev/null
}

# sysbox_exec <container> <command...>: run a command inside, as root.
sysbox_exec() {
    local name=$1
    shift

    docker exec "$name" "$@"
}

# sysbox_copy <container> <host path> <path inside>: copy a file in.
sysbox_copy() {
    docker cp --quiet "$2" "$1:$3"
}

# sysbox_boot_count <container>: how often Docker restarted the container.
sysbox_boot_count() {
    docker inspect --format '{{.RestartCount}}' "$1"
}

# sysbox_boot_id <container>: the host's current boot id.
sysbox_boot_id() {
    docker exec "$1" cat /proc/sys/kernel/random/boot_id
}

# sysbox_wait_ready <container> [timeout seconds]: wait until systemd has
# finished booting ("running", or "degraded" when an irrelevant unit failed).
sysbox_wait_ready() {
    local name=$1 timeout=${2:-$SYSBOX_BOOT_TIMEOUT} deadline state=unknown

    deadline=$((SECONDS + timeout))
    while ((SECONDS < deadline)); do
        state=$(docker exec "$name" systemctl is-system-running 2>/dev/null) || true
        case "$state" in
        running | degraded)
            return 0
            ;;
        esac
        sleep 0.5
    done

    sysbox_log "host $name did not finish booting within ${timeout}s (state: ${state:-none})"
    docker logs --tail 40 "$name" >&2 || true

    return 1
}

# sysbox_wait_reboot <container> <boot count before> [timeout seconds]: wait
# until the host has gone down and booted again since the given count.
sysbox_wait_reboot() {
    local name=$1 before=$2 timeout=${3:-$SYSBOX_BOOT_TIMEOUT} deadline count=$2

    deadline=$((SECONDS + timeout))
    while ((SECONDS < deadline)); do
        count=$(sysbox_boot_count "$name" 2>/dev/null) || count=$before
        if ((count > before)); then
            sysbox_wait_ready "$name" "$((deadline - SECONDS))"

            return
        fi
        sleep 0.5
    done

    sysbox_log "host $name did not reboot within ${timeout}s"
    docker logs --tail 40 "$name" >&2 || true

    return 1
}

# sysbox_remove <container> <volume>: destroy the host and its state.
sysbox_remove() {
    docker rm --force --volumes "$1" >/dev/null 2>&1 || true
    docker volume rm --force "$2" >/dev/null 2>&1 || true
}
