#!/bin/sh
# Container entrypoint: the "firmware" of the throwaway host. It runs once
# per container start, i.e. once per boot, and then hands over to systemd as
# PID 1.
set -eu

# A container shares the kernel of its host, so the kernel's boot id never
# changes when the container restarts. A real host gets a new one on every
# boot, and the agent relies on that to tell that a reboot happened. Give
# this "host" a fresh boot id per start by mounting a file over the kernel's.
boot_id=/run/upgradoor-test-boot-id
cat /proc/sys/kernel/random/uuid > "$boot_id"
mount --bind "$boot_id" /proc/sys/kernel/random/boot_id

# systemd needs a writable cgroup tree; without --privileged Docker mounts
# the container's (private) cgroup namespace read-only.
mount -o remount,rw /sys/fs/cgroup

exec /lib/systemd/systemd --log-target=console --show-status=false
