#!/usr/bin/env bash
# Reboot and package upgrade end to end (make test-reboot): the real agent
# binary under systemd in two throwaway Debian "hosts" (test/system), a real
# controller, and leases that must survive real reboots.
#
#   1. host-1 gets a pending blocked package upgrade and a reboot-required
#      flag. One lease serves both: the managed container is stopped, the
#      package upgraded, the host rebooted. After the boot the agent finds
#      the lease in its state file and finishes it; the controller releases
#      the slot.
#   2. A staggered reboot of the whole fleet from the API: both hosts
#      reboot, one after the other, never together.
#   3. A local hold (upgradoor hold) keeps a host out of a fleet reboot, and the
#      controller cannot lift it.
#
# Nothing on this machine is upgraded or rebooted: the hosts are containers
# (see test/system/lib.sh for how their reboot is realised). They do get the
# machine's Docker socket, to manage one throwaway container each.
#
# Needs docker, curl, jq and go. Everything is written under
# .tmp/reboot-test; containers, volumes and the network are removed on exit.
#
# Environment:
#   REBOOT_KEEP=1  keep everything for inspection
set -euo pipefail

# shellcheck source-path=SCRIPTDIR source=../system/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../system/lib.sh"
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

repo=$SYSBOX_REPO
id=upgradoor-reboot-$$
work=$repo/.tmp/reboot-test/$id
net=$id
network=reboot-e2e
hosts=(host-1 host-2)
ctl_port=18111
ctl=http://127.0.0.1:$ctl_port
export TMPDIR=$repo/.tmp

cleanup() {
    code=$?
    set +e

    if [ $code != 0 ]; then
        printf '\n--- controller log (tail)\n'
        docker logs --tail 60 "$id-controller" 2>&1
        for h in "${hosts[@]}"; do
            printf '\n--- agent journal %s (tail)\n' "$h"
            docker exec "$id-$h" journalctl -u upgradoor-agent --no-pager -n 60 2>&1
        done
    fi

    if [ "${REBOOT_KEEP:-}" != 1 ]; then
        for h in "${hosts[@]}"; do
            sysbox_remove "$id-$h" "$id-$h-state"
            docker rm -f "$id-$h-app" >/dev/null 2>&1
        done
        docker rm -f "$id-controller" "$id-beacon" >/dev/null 2>&1
        docker network rm "$net" >/dev/null 2>&1
        rm -rf "$work"
    fi

    exit $code
}
trap cleanup EXIT

api() { curl -fsS --max-time 10 "$ctl$1"; }
post() { curl -fsS --max-time 10 -H 'Content-Type: application/json' -X POST -d "$2" "$ctl$1"; }

wait_poll=2
e2e_state() {
    api /api/v1/plan | jq -c '{gate: .gate.reasons, budget, queue}'
    api /api/v1/rollouts |
        jq -c '(.open[], .recent[]) | {id, title, state, reason, hosts: [.hosts[] | {host, state, error}]}'
    api /api/v1/rollouts | jq -c '.leases[] | {id, host, state, phase: .operation.phase, readiness, error}'
}

# --- build -------------------------------------------------------------------

mkdir -p "$work"
(cd "$repo" && CGO_ENABLED=0 go build -o "$work/upgradoor" ./cmd/upgradoor &&
    CGO_ENABLED=0 go build -o "$work/upgradoor-controller" ./cmd/upgradoor-controller &&
    CGO_ENABLED=0 go build -o "$work/fakebeacon" ./test/e2e/fakebeacon)
sysbox_build_image
docker pull -q busybox:stable >/dev/null
pass "binaries and host image built"

# --- controller ----------------------------------------------------------------

docker network create --label upgradoor.test=reboot "$net" >/dev/null

docker run -d --name "$id-beacon" --network "$net" --label upgradoor.test=reboot \
    -v "$work/fakebeacon:/fakebeacon:ro" debian:13-slim /fakebeacon >/dev/null

seed=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
export UPGRADOOR_SEED=$seed
signer=$("$work/upgradoor-controller" keygen signer-pubkey)
# The controller signs its answers with a key it derives from the controller
# seed, a secret of its own; the agents accept only that key. The file is
# read by another user inside the container, and this is a test seed.
head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/controller.seed"
chmod 0444 "$work/controller.seed"
controller_pub=$("$work/upgradoor-controller" keygen controller-pubkey \
    --controller-seed-file "$work/controller.seed")

# Open API (no auth provider): this is a closed test network.
cat >"$work/controller.yaml" <<EOF
network:
  name: $network
  beaconEndpoints: [http://$id-beacon:5052]
agents:
  signerPublicKey: $signer
  controllerSeedFile: /controller.seed
  allowInsecureHttp: true
auth:
  allowOpenApi: true
deployWindow: 0s
settingDefaults:
  policyReboot: auto
  policyPackages: auto
  hostStaleAfter: 45s
  leaseTtl: 5m
  paused: true
EOF
docker run -d --name "$id-controller" --network "$net" --label upgradoor.test=reboot \
    -p "127.0.0.1:$ctl_port:8080" --tmpfs /data \
    -v "$work/upgradoor-controller:/upgradoor-controller:ro" \
    -v "$work/controller.seed:/controller.seed:ro" \
    -v "$work/controller.yaml:/controller.yaml:ro" \
    debian:13-slim /upgradoor-controller run --config /controller.yaml >/dev/null
wait_until "controller up" 30 curl -fsS "$ctl/healthz"

# --- hosts ---------------------------------------------------------------------

SYSBOX_NETWORK=$net
SYSBOX_RUN_ARGS=(--volume /var/run/docker.sock:/run/docker.sock)

for h in "${hosts[@]}"; do
    name=$id-$h
    dir=$work/$h
    mkdir -p "$dir"

    # The container this host manages: it must be stopped before the reboot
    # and running again after it.
    docker run -d --name "$name-app" --label upgradoor.test=reboot \
        --label io.ethpandaops.upgradoor.kind=app --stop-timeout 5 --restart unless-stopped \
        busybox:stable sleep 1000000 >/dev/null

    "$work/upgradoor" key --path "$dir/host.key" >"$dir/host.pub"
    "$work/upgradoor-controller" keygen host-cert --network "$network" --hostname "$h" \
        --public-key "$(cat "$dir/host.pub")" >"$dir/host.crt"

    cat >"$dir/agent.yaml" <<EOF
controller:
  url: http://$id-controller:8080
  allowInsecure: true
  publicKey: "$controller_pub"
  syncInterval: 3s
  leaseSyncInterval: 1s
  maxSyncInterval: 3s
overridesFile: /etc/upgradoor/overrides.yaml
verbs: [roll, packages, reboot]
docker:
  host: unix:///run/docker.sock
packages:
  blocked: [upgradoor-dummy]
verify:
  window: 5s
  timeout: 1m
EOF

    cat >"$dir/upgradoor-agent.service" <<'EOF'
[Unit]
Description=upgradoor agent
After=network-online.target

[Service]
ExecStart=/usr/local/bin/upgradoor agent --config /etc/upgradoor/agent.yaml
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
EOF

    sysbox_start "$name" "$name-state"
    sysbox_wait_ready "$name"
    sysbox_exec "$name" mkdir -p /etc/upgradoor
    sysbox_copy "$name" "$work/upgradoor" /usr/local/bin/upgradoor
    sysbox_copy "$name" "$dir/agent.yaml" /etc/upgradoor/agent.yaml
    sysbox_copy "$name" "$dir/host.key" /etc/upgradoor/host.key
    sysbox_copy "$name" "$dir/host.crt" /etc/upgradoor/host.crt
    sysbox_copy "$name" "$dir/upgradoor-agent.service" /etc/systemd/system/upgradoor-agent.service
    sysbox_exec "$name" chmod 600 /etc/upgradoor/host.key
    sysbox_exec "$name" systemctl enable --now upgradoor-agent >/dev/null 2>&1
done
unset UPGRADOOR_SEED

wait_until "both hosts registered" 60 jq_true /api/v1/hosts '.hosts | length == 2'
wait_until "network gate open" 60 jq_true /api/v1/plan '.gate.ok'
wait_until "an epoch settled from the per-slot data" 60 jq_true /api/v1/plan '.gate.network.livenessKnown'

# --- 1: packages + reboot in one slot, lease survives the reboot ----------------

# The controller starts paused, so the picture can be set up first. Both
# hosts already know of a newer version of the blocked package (their apt
# lists were refreshed when the image was built, as the apt timer does on a
# real host).
h1=$id-host-1
boots=$(sysbox_boot_count "$h1")
boot_id=$(sysbox_boot_id "$h1")

# What a kernel upgrade by unattended-upgrades leaves behind.
sysbox_exec "$h1" sh -c 'touch /var/run/reboot-required; echo linux-image-test > /var/run/reboot-required.pkgs'

wait_until "host-1 reports a packages need and a reboot need" 60 \
    jq_true /api/v1/hosts/host-1 '[.needs[].kind] | sort == ["packages", "reboot"]'
wait_until "host-2 reports only a packages need" 60 \
    jq_true /api/v1/hosts/host-2 '[.needs[].kind] == ["packages"]'
jq_true /api/v1/rollouts '(.leases | length) == 0 and (.recentLeases | length) == 0' ||
    die "a lease was granted while paused"
pass "nothing granted while paused"

# Watch for overlapping leases from here on.
(
    while sleep 0.5; do
        n=$(curl -fsS --max-time 5 "$ctl/api/v1/rollouts" 2>/dev/null | jq '.leases | length' 2>/dev/null) || continue
        [ "${n:-0}" -gt 1 ] && echo "$n" >"$work/overlap"
    done
) &
watch=$!

post /api/v1/actions/resume '{"reason":"reboot test: start"}' >/dev/null

lease_of() {
    api /api/v1/rollouts | jq -r --arg h "$1" '[.leases[], .recentLeases[]] | map(select(.host == $h)) | .[0].id // empty'
}
has_lease() { [ -n "$(lease_of "$1")" ]; }
wait_until "host-1 got a lease" 240 has_lease host-1
lease=$(lease_of host-1)
api /api/v1/rollouts | jq -e --arg l "$lease" \
    '[.leases[], .recentLeases[]] | map(select(.id == $l)) | .[0].needs | map(.kind) | sort == ["packages", "reboot"]' \
    >/dev/null || die "host-1's lease does not carry both needs"
pass "one lease ($lease) serves the packages need and the reboot need of host-1"

sysbox_wait_reboot "$h1" "$boots" 180
pass "host-1 really rebooted (boot $boots -> $(sysbox_boot_count "$h1"))"
[ "$(sysbox_boot_id "$h1")" != "$boot_id" ] || die "boot id did not change"

sysbox_exec "$h1" jq -e --arg l "$lease" '.. | objects | select(.leaseId? == $l)' \
    /data/upgradoor/state.json >/dev/null 2>&1 ||
    sysbox_exec "$h1" grep -q "$lease" /data/upgradoor/state.json ||
    die "the lease is not in the agent's state file after the reboot"
pass "lease $lease is in the agent's state file after the reboot"

wait_until "lease finished and released by the controller" 180 \
    jq_true /api/v1/rollouts --arg l "$lease" 'any(.recentLeases[]; .id == $l and .state == "released")'
api /api/v1/rollouts | jq --arg l "$lease" '.recentLeases[] | select(.id == $l)' >"$work/lease-1.json"
jq -e '.operation.phase == "done" and .operation.disrupted == true
       and ([.operation.steps[].status] | all(. == "done")) and (.operation.steps | length) == 2' \
    "$work/lease-1.json" >/dev/null || die "lease operation not done with two done steps: $(jq -c .operation "$work/lease-1.json")"

[ "$(sysbox_exec "$h1" dpkg-query -W -f '${Version}' upgradoor-dummy)" = 2.0 ] ||
    die "blocked package was not upgraded"
sysbox_exec "$h1" test ! -e /var/run/reboot-required || die "reboot-required flag still there"
[ "$(docker inspect -f '{{.State.Running}}' "$h1-app")" = true ] || die "managed container not running after the reboot"
# Read the journal first: grep -q stops early and, under pipefail, the
# journalctl it cut off would fail the check.
journal=$(sysbox_exec "$h1" journalctl -u upgradoor-agent --no-pager)
grep -qiE 'draining|stopp' <<<"$journal" || die "agent journal shows no drain before the reboot"
pass "package upgraded, flag gone, container back"

wait_until "packages and reboot rollouts completed" 60 \
    jq_true /api/v1/rollouts '(.open | length) == 0 and ([.recent[] | select(.state == "completed") | .kind] | sort) == ["packages", "reboot"]'
jq_true /api/v1/hosts 'all(.hosts[]; (.needs | length) == 0)' || die "hosts still report needs"
[ "$(sysbox_exec "$id-host-2" dpkg-query -W -f '${Version}' upgradoor-dummy)" = 2.0 ] ||
    die "host-2's blocked package was not upgraded"
[ "$(sysbox_boot_count "$id-host-2")" = 0 ] || die "host-2 rebooted without a reboot need"
[ ! -e "$work/overlap" ] || die "two leases were open at once"
pass "host-2 upgraded its package without a reboot, never at the same time as host-1"

# --- 2: staggered fleet reboot --------------------------------------------------

b1=$(sysbox_boot_count "$id-host-1")
b2=$(sysbox_boot_count "$id-host-2")
post /api/v1/actions/reboot '{"all":true,"staggered":true,"reason":"reboot test: fleet"}' >"$work/fleet.json"
fleet=$(jq -r .rollout "$work/fleet.json")
[ -n "$fleet" ] && [ "$fleet" != null ] || die "fleet reboot returned no rollout: $(cat "$work/fleet.json")"

wait_until "fleet reboot completed" 420 \
    jq_true /api/v1/rollouts --arg id "$fleet" 'any(.recent[]; .id == $id and .state == "completed")'
kill $watch 2>/dev/null || true

[ "$(sysbox_boot_count "$id-host-1")" -gt "$b1" ] || die "host-1 did not reboot in the fleet reboot"
[ "$(sysbox_boot_count "$id-host-2")" -gt "$b2" ] || die "host-2 did not reboot in the fleet reboot"
[ ! -e "$work/overlap" ] || die "two leases were open at once during the fleet reboot"
# A lease ends released, expired (a lease in which no step ran) or failed.
# Entries come newest first.
api "/api/v1/audit?limit=500" >"$work/audit.json"
jq -e '
    [.entries[] | select(.action | IN("lease.granted", "lease.released", "lease.expired", "lease.failed"))]
    | reverse
    | reduce .[] as $e ({open: [], ok: true};
        if $e.action == "lease.granted"
        then .ok = (.ok and (.open | length) == 0) | .open += [$e.detail.lease]
        else .open -= [$e.detail.lease] end)
    | .ok' "$work/audit.json" >/dev/null ||
    die "leases overlapped according to the audit log"
jq -e --arg id "$fleet" \
    '[.entries[] | select(.action == "lease.granted" and (.detail.rollouts | index($id)))] | length == 2' \
    "$work/audit.json" >/dev/null || die "the fleet reboot did not take exactly two leases"
for h in "${hosts[@]}"; do
    [ "$(docker inspect -f '{{.State.Running}}' "$id-$h-app")" = true ] || die "$h: container not running"
done
pass "staggered fleet reboot: both hosts rebooted, one after the other"

# --- 3: a local hold keeps a host out, and the controller cannot lift it ---------

sysbox_exec "$id-host-2" /usr/local/bin/upgradoor hold "debugging, hands off" >/dev/null
wait_until "host-2 reports the local hold" 30 \
    jq_true /api/v1/hosts/host-2 '.hold.local == true and .hold.localReason == "debugging, hands off"'
post /api/v1/actions/unhold '{"hosts":["host-2"],"reason":"reboot test: try to lift a local hold"}' >/dev/null
b1=$(sysbox_boot_count "$id-host-1")
b2=$(sysbox_boot_count "$id-host-2")
post /api/v1/actions/reboot '{"all":true,"staggered":true,"reason":"reboot test: with a held host"}' >"$work/held.json"
held=$(jq -r .rollout "$work/held.json")
wait_until "second fleet reboot completed without the held host" 300 \
    jq_true /api/v1/rollouts --arg id "$held" 'any(.recent[]; .id == $id and .state == "completed")'
[ "$(sysbox_boot_count "$id-host-1")" -gt "$b1" ] || die "host-1 did not reboot"
[ "$(sysbox_boot_count "$id-host-2")" = "$b2" ] || die "the held host rebooted"
jq_true /api/v1/hosts/host-2 '.hold.local == true' || die "the controller lifted a local hold"
pass "the held host was left alone and its local hold stayed"

pass "reboot test"
