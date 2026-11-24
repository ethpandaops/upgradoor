#!/usr/bin/env bash
# End-to-end run on a local Kurtosis devnet (make e2e).
#
# Four nodes (geth + lighthouse beacon + lighthouse validator, 32 validators
# each), one real upgradoor agent per node and one real controller. The run
# publishes lighthouse builds A, B, C (broken) and D under one tag and
# asserts what upgradoor does with them:
#
#   A  hosts and images appear in the public view; details and the package
#      search need a token
#   B  rolled host by host under the budget, behind the network gate, each
#      slot released only after the host's validators attest again
#   C  breaks the first host: the rollout halts, the digest is blocked,
#      nobody else is touched
#   D  supersedes C, repairs the broken host first, then the rest
#   then manual actions (roll one host now, staggered roll of all, hold,
#   pause) and a settings change, all found in the audit log
#
# The agents run in containers ("hosts") that share this machine's Docker
# daemon but have their own Debian userland and no systemd: they are
# configured for the roll verb only, and could not upgrade or reboot this
# machine if they tried. Reboots and package upgrades are covered by
# test/system and test/e2e/reboot.sh.
#
# Needs docker, kurtosis, curl, jq, go and node. Takes about half an hour.
# Everything it writes is under .tmp/e2e in the repository.
#
# Environment:
#   E2E_KEEP=1    leave the devnet, agents and controller running at the end
#                 (UI on http://127.0.0.1:8099, log in with the button)
#   E2E_PORT      controller port (default 8099)
set -euo pipefail

# shellcheck source-path=SCRIPTDIR source=../lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"

cd "$(dirname "${BASH_SOURCE[0]}")/../../.."
repo=$PWD
work=$repo/.tmp/e2e
export TMPDIR=$repo/.tmp

enclave=upgradoor-e2e
package=github.com/ethpandaops/ethereum-package@6.1.0
base=sigp/lighthouse:v8.2.2
registry=localhost:5055
image=$registry/lighthouse:example
network=kurtosis-e2e
port=${E2E_PORT:-8099}
auth_port=18099
ranges_port=18100
ctl=http://127.0.0.1:$port
auth=http://127.0.0.1:$auth_port
nodes=4
validators=32
prefix=upgradoor-e2e

say() { printf '\n=== %s\n' "$*"; }

cleanup() {
    code=$?
    set +e

    [ -n "${sampler_pid:-}" ] && kill "$sampler_pid" 2>/dev/null

    if [ "${E2E_KEEP:-}" = 1 ] && [ $code = 0 ]; then
        printf '\nLeft running (E2E_KEEP=1). UI: %s\nStop with: %s/test/e2e/kurtosis/stop.sh\n' "$ctl" "$repo"
        exit 0
    fi

    if [ $code != 0 ]; then
        printf '\n--- controller log (tail)\n'
        tail -n 60 "$work/controller.log" 2>/dev/null
        for i in $(seq 1 $nodes); do
            printf '\n--- agent node-%s log (tail)\n' "$i"
            docker logs --tail 40 "$prefix-node-$i" 2>&1
        done
    fi

    "$repo/test/e2e/kurtosis/stop.sh" >/dev/null 2>&1
    exit $code
}
trap cleanup EXIT

# --- helpers ---------------------------------------------------------------

pub() { curl -fsS --max-time 10 "$ctl$1"; }
api() { curl -fsS --max-time 10 -H "Authorization: Bearer $token" "$ctl$1"; }
post() {
    curl -fsS --max-time 10 -H "Authorization: Bearer $token" \
        -H 'Content-Type: application/json' -X "${3:-POST}" -d "$2" "$ctl$1"
}

# expect_status STATUS TOKEN PATH BODY [METHOD]: a request by that login must
# be answered with STATUS. The answer is left in $work/answer.json.
expect_status() {
    local got
    got=$(curl -s -o "$work/answer.json" -w '%{http_code}' --max-time 10 \
        -H "Authorization: Bearer $2" -H 'Content-Type: application/json' \
        -X "${5:-POST}" -d "$4" "$ctl$3")
    [ "$got" = "$1" ] || die "${5:-POST} $3 $4 answered $got, want $1: $(cat "$work/answer.json")"
}

e2e_state() {
    api /api/v1/plan 2>/dev/null | jq -c '{gate: .gate.reasons, budget, queue}'
    api /api/v1/rollouts 2>/dev/null |
        jq -c '.open[] | {id, title, state, reason, hosts: [.hosts[] | {host, state, error}]}'
}
pub_true() { pub "$1" | jq -e "$2" >/dev/null; }

container() { docker ps -a --format '{{.Names}}' | grep "^$1--" | head -n1; }
ip_of() { docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$1"; }

# publish NAME DOCKERFILE-BODY: build a lighthouse variant, move the tag to
# it and print its manifest digest.
#
# The agents share this machine's Docker daemon and pull the followed tag
# whenever the registry moves it, which re-points the local tag. So the build
# never uses that tag locally: it is pushed under a tag of its own and the
# followed tag is moved inside the registry.
publish() {
    local name=$1 dir=$work/build-$1 build=$registry/lighthouse:build-$1
    local accept='application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'
    local type
    mkdir -p "$dir"
    printf 'FROM %s\n%s\n' "$base" "$2" >"$dir/Dockerfile"
    docker build -q -t "$build" "$dir" >/dev/null
    docker push -q "$build" >/dev/null
    docker image rm "$build" >/dev/null 2>&1 || true

    type=$(curl -fsS -D - -o "$dir/manifest" -H "Accept: $accept" \
        "http://$registry/v2/lighthouse/manifests/build-$name" |
        tr -d '\r' | awk 'tolower($1) == "content-type:" {print $2}')
    curl -fsS -X PUT -H "Content-Type: $type" --data-binary "@$dir/manifest" \
        "http://$registry/v2/lighthouse/manifests/example" >/dev/null
    curl -fsSI -H "Accept: $accept" "http://$registry/v2/lighthouse/manifests/example" |
        tr -d '\r' | awk 'tolower($1) == "docker-content-digest:" {print $2}'
}

# rollout_of DIGEST: the id of the beacon node rollout for a digest.
rollout_for() {
    jq --arg d "$1" '[.open[], .recent[]] |
        map(select(.containerKind == "cl" and any(.targets[]?; .digest == $d))) | .[0]'
}
rollout_of() { api /api/v1/rollouts | rollout_for "$1" | jq -r '.id // empty'; }
rollout_state() { api /api/v1/rollouts | rollout_for "$1" | jq -r '.state // "none"'; }
rollout_is() { [ "$(rollout_state "$1")" = "$2" ]; }
rollout_exists() { [ -n "$(rollout_of "$1")" ]; }

# hosts_on DIGEST: how many hosts run the beacon node at that digest.
hosts_on() { pub /api/v1/public/hosts | jq --arg d "$1" '[.hosts[] | select(.cl.digest == $d)] | length'; }
all_on() { [ "$(hosts_on "$1")" = "$nodes" ]; }

# --- build -----------------------------------------------------------------

say "build"
"$repo/test/e2e/kurtosis/stop.sh" >/dev/null 2>&1 || true
rm -rf "$work"
mkdir -p "$work"

make -s build
go build -o "$work/authstub" ./test/e2e/authstub
go build -o "$work/relabel" ./test/e2e/relabel
cp bin/upgradoor "$work/upgradoor"
cp bin/upgradoor-controller "$work/upgradoor-controller"
docker build -q -t "$prefix-host" -f test/e2e/kurtosis/Dockerfile.host "$work" >/dev/null
pass "controller, agent image and auth stub built"

# --- registry and build A --------------------------------------------------

say "registry"
docker run -d --name "$prefix-registry" -p 127.0.0.1:5055:5000 registry:2 >/dev/null
wait_until "registry up" 30 curl -fsS "http://$registry/v2/"

digest_a=$(publish a 'LABEL upgradoor.e2e.build=a org.opencontainers.image.revision=aaaaaaa')
[ -n "$digest_a" ] || die "no digest for build A"
docker pull -q "$image" >/dev/null
pass "build A published: $digest_a"

# --- devnet ----------------------------------------------------------------

say "devnet"
kurtosis run --enclave "$enclave" "$package" --args-file test/e2e/kurtosis/network.yaml \
    >"$work/kurtosis.log" 2>&1 || {
    tail -n 40 "$work/kurtosis.log"
    die "kurtosis run failed"
}
pass "devnet started"

# The controller watches the network through the devnet's own beacon nodes.
# A real controller has the in-cluster node, which upgradoor never touches;
# here every beacon node is also a managed host and one of them is broken on
# purpose, so all four are given and the controller fails over.
beacon_args=()
for i in $(seq 1 $nodes); do
    beacon_args+=(--beacon-endpoint "http://$(ip_of "$(container "cl-$i-lighthouse-geth")"):4000")
done

# --- identities, controller, agents ----------------------------------------

say "controller and agents"
seed=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
export UPGRADOOR_SEED=$seed
signer=$("$work/upgradoor-controller" keygen signer-pubkey)
# The controller signs its answers with a key it derives from the controller
# seed, a secret of its own; the agents accept only that key.
(umask 077 && head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/controller.seed")
controller_pub=$("$work/upgradoor-controller" keygen controller-pubkey \
    --controller-seed-file "$work/controller.seed")

"$work/authstub" --listen "127.0.0.1:$auth_port" >"$work/authstub.log" 2>&1 &
echo $! >"$work/authstub.pid"
wait_until "auth stub up" 20 curl -fsS "$auth/jwks.json"

# The validator ranges inventory that a devnet's config service publishes:
# inclusive validator index ranges by hostname.
mkdir -p "$work/ranges"
ranges=$(for i in $(seq 1 $nodes); do
    printf '"%d-%d": "node-%d"\n' $(((i - 1) * validators)) $((i * validators - 1)) "$i"
done | paste -sd, -)
echo "{\"ranges\": {$ranges}}" >"$work/ranges/validator-ranges.json"
python3 -m http.server --bind 127.0.0.1 --directory "$work/ranges" "$ranges_port" \
    >"$work/ranges.log" 2>&1 &
echo $! >"$work/ranges.pid"
wait_until "validator ranges served" 20 \
    curl -fsS "http://127.0.0.1:$ranges_port/validator-ranges.json"

mkdir -p "$work/data"
"$work/upgradoor-controller" run \
    --listen "127.0.0.1:$port" --data-dir "$work/data" \
    --network "$network" --signer-public-key "$signer" \
    --controller-seed-file "$work/controller.seed" \
    --allow-insecure-agent-http \
    --auth-provider-url "$auth" \
    --registry-plain-http "$registry" --registry-allow-private \
    "${beacon_args[@]}" \
    --validator-ranges-url "http://127.0.0.1:$ranges_port/validator-ranges.json" \
    --default-setting maxUnavailablePercent=25 \
    --default-setting readyAttestations=2 \
    --default-setting leaseTtl=5m \
    --default-setting readinessTimeout=6m \
    --default-setting hostStaleAfter=1m \
    --default-setting lookupInterval=30s \
    >"$work/controller.log" 2>&1 &
echo $! >"$work/controller.pid"
wait_until "controller up" 30 curl -fsS "$ctl/healthz"

# Three logins. The stub mints a staff token (every feature) unless told
# otherwise; a member of the lighthouse team gets the team features only, and
# a reader a token without any feature.
token=$(curl -fsS "$auth/mint?email=e2e@example.com" | jq -r .token)
team_token=$(curl -fsS -G "$auth/mint" --data-urlencode "email=lh@example.com" \
    --data-urlencode "services=-all,+upgradoor,+upgradoor:team_*" \
    --data-urlencode "teams=lighthouse" | jq -r .token)
reader_token=$(curl -fsS -G "$auth/mint" --data-urlencode "email=reader@example.com" \
    --data-urlencode "services=none" | jq -r .token)

for i in $(seq 1 $nodes); do
    dir=$work/node-$i
    mkdir -p "$dir"
    el=$(container "el-$i-geth-lighthouse")
    cl=$(container "cl-$i-lighthouse-geth")
    vc=$(container "vc-$i-geth-lighthouse")
    [ -n "$el" ] && [ -n "$cl" ] && [ -n "$vc" ] || die "containers of node $i not found"
    # Kurtosis created them: give them the labels Ansible would set.
    # All nodes share one Docker daemon: each agent only sees its node's containers.
    "$work/relabel" "+e2e.node=$i" "$el=el/geth" "$cl=cl/lighthouse" "$vc=vc/lighthouse" || die "relabel node $i"

    "$work/upgradoor" key --path "$dir/host.key" >"$dir/host.pub"
    "$work/upgradoor-controller" keygen host-cert --network "$network" --hostname "node-$i" \
        --public-key "$(cat "$dir/host.pub")" >"$dir/host.crt"

    cat >"$dir/agent.yaml" <<EOF
controller:
  url: $ctl
  allowInsecure: true
  publicKey: "$controller_pub"
  syncInterval: 5s
  leaseSyncInterval: 2s
  maxSyncInterval: 5s
keyFile: /etc/upgradoor/host.key
certFile: /etc/upgradoor/host.crt
stateDir: /data/upgradoor
roles: [ethereum_node]
# Only image rolls: this "host" shares the real machine's kernel.
verbs: [roll]
# The containers carry the kind and team labels (relabel above).
docker:
  labels:
    e2e.node: "$i"
probes:
  el: http://$(ip_of "$el"):8545
  cl: http://$(ip_of "$cl"):4000
verify:
  window: 15s
  timeout: 2m
EOF

    docker run -d --name "$prefix-node-$i" --hostname "node-$i" --network host \
        -v /var/run/docker.sock:/var/run/docker.sock \
        -v "$dir:/etc/upgradoor:ro" \
        -v "$prefix-node-$i-state:/data/upgradoor" \
        "$prefix-host" agent --config /etc/upgradoor/agent.yaml >/dev/null
done
unset UPGRADOOR_SEED
pass "four agents started"

# Sample how many leases are open at once, for the whole run.
(
    max=0
    while sleep 2; do
        n=$(curl -fsS --max-time 5 -H "Authorization: Bearer $token" "$ctl/api/v1/rollouts" 2>/dev/null |
            jq '.leases | length' 2>/dev/null) || continue
        [ "${n:-0}" -gt "$max" ] && max=$n && echo "$max" >"$work/max-open-leases"
    done
) &
sampler_pid=$!

# --- A: visibility -----------------------------------------------------------

say "A: public and authenticated views"
wait_until "all hosts registered and on build A" 240 all_on "$digest_a"
wait_until "inventories uploaded" 120 \
    jq_true "/api/v1/packages?q=libc6" '[.matches[] | select(.name == "libc6") | .versions[].hosts[]] | length == 4'
pass "package search finds libc6 on four hosts (authenticated)"

pub /api/v1/public/hosts >"$work/public-hosts.json"
jq -e '.hosts | length == 4 and all(.[]; .el.image != null and .cl.image != null and .cl.digest != null)' \
    "$work/public-hosts.json" >/dev/null || die "public hosts view incomplete"
jq -e '[.hosts[] | keys[]] | unique - ["name","el","cl","updatePending"] == []' \
    "$work/public-hosts.json" >/dev/null || die "public hosts view carries extra fields"
pub /api/v1/public/images >"$work/public-images.json"
jq -e --arg d "$digest_a" '.images | any(.[]; .digest == $d and (.hostsOn | length) == 4)' \
    "$work/public-images.json" >/dev/null || die "public images view does not show build A on four hosts"

kernel=$(uname -r)
libc=$(api "/api/v1/packages?q=libc6" | jq -r '.matches[] | select(.name == "libc6") | .versions[0].version')
for f in public-hosts.json public-images.json; do
    if grep -qF -e "$kernel" -e "$libc" "$work/$f"; then
        die "$f leaks a kernel or package version"
    fi
done
pub /api/v1/public/network >"$work/public-network.json"
grep -qF -e "$kernel" -e "$libc" "$work/public-network.json" && die "public network view leaks"
pass "public views: hosts, images, nothing else (no kernel, no package version)"

for path in /api/v1/hosts /api/v1/hosts/node-1 "/api/v1/packages?q=libc" /api/v1/rollouts \
    /api/v1/plan /api/v1/settings /api/v1/audit; do
    code=$(curl -s -o /dev/null -w '%{http_code}' "$ctl$path")
    [ "$code" = 401 ] || die "GET $path without a token answered $code, want 401"
done
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    -d '{"hosts":["node-1"]}' "$ctl/api/v1/actions/reboot")
[ "$code" = 401 ] || die "action without a token answered $code, want 401"
pass "authenticated routes and actions refuse requests without a token"

jq_true /api/v1/hosts/node-1 --arg k "$kernel" \
    '.os.runningKernel == $k and (.inventory.packages | length) > 50 and .validators == 32' ||
    die "authenticated host detail incomplete"
pass "authenticated host detail has kernel, packages and validator range"

# --- permissions ---------------------------------------------------------------

say "permissions: reader, team member, staff"
jq_true /api/v1/hosts 'all(.hosts[];
        ([.containers[] | select(.kind == "el") | .team] == ["geth"]) and
        ([.containers[] | select(.kind == "cl") | .team] == ["lighthouse"]))' ||
    die "the authenticated host view does not carry the team labels"
grep -q '"team"' "$work/public-hosts.json" "$work/public-images.json" &&
    die "a public view carries teams"
pass "teams reach the authenticated view and no public view"

el1=$(container "el-1-geth-lighthouse")
cl1=$(container "cl-1-lighthouse-geth")
rollouts_before=$(api /api/v1/rollouts | jq '(.open + .recent) | length')
audit_before=$(api "/api/v1/audit?action=action.&limit=1" | jq '.entries[0].id // 0')

# A reader reads everything and changes nothing.
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $reader_token" "$ctl/api/v1/hosts/node-1")" = 200 ] ||
    die "a login without features cannot read"
expect_status 403 "$reader_token" /api/v1/actions/hold '{"hosts":["node-3"]}'
expect_status 403 "$reader_token" /api/v1/actions/roll '{"hosts":["node-3"]}'
expect_status 403 "$reader_token" /api/v1/settings "$(api /api/v1/settings | jq .settings)" PUT

# A lighthouse member rolls lighthouse, not geth, and not the host as a whole.
expect_status 403 "$team_token" /api/v1/actions/roll "{\"hosts\":[\"node-1\"],\"containers\":[\"$el1\"]}"
jq -e --arg c "$el1" '.error == "forbidden: upgradoor:team_manage does not cover container \($c) on host node-1 (owners: geth)"' \
    "$work/answer.json" >/dev/null || die "the refusal does not name the container: $(cat "$work/answer.json")"
expect_status 403 "$team_token" /api/v1/actions/roll '{"hosts":["node-1"]}'
expect_status 403 "$team_token" /api/v1/actions/roll "{\"hosts\":[\"node-1\"],\"containers\":[\"$el1\"],\"force\":true}"
expect_status 403 "$team_token" /api/v1/actions/roll '{"all":true}'
expect_status 403 "$team_token" /api/v1/actions/pause '{}'
expect_status 403 "$team_token" /api/v1/actions/forget '{"hosts":["node-1"]}'
expect_status 403 "$team_token" /api/v1/settings "$(api /api/v1/settings | jq .settings)" PUT

[ "$(api /api/v1/rollouts | jq '(.open + .recent) | length')" = "$rollouts_before" ] ||
    die "a refused request created a rollout"
[ "$(api "/api/v1/audit?action=action.&limit=1" | jq '.entries[0].id // 0')" = "$audit_before" ] ||
    die "a refused request was audited"
jq_true /api/v1/hosts/node-1 '(.hold.holds // []) == [] and (.plan.holdRequests // []) == []' ||
    die "a refused request changed a host"
pass "refusals: 403, nothing changed, nothing audited"

# One owned container is enough for a host-level action.
expect_status 200 "$team_token" /api/v1/actions/hold '{"hosts":["node-3"],"reason":"e2e: team hold"}'
wait_until "node-3 reports the team hold" 60 jq_true /api/v1/hosts/node-3 '[.hold.holds[]?.holder] == ["lighthouse"]'
jq_true "/api/v1/audit?action=action.hold&limit=1" '.entries[0] |
        .actor == "lh@example.com" and .target == "node-3" and
        .detail.authorizedBy == "team_manage" and .detail.teams == ["lighthouse"]' ||
    die "the team hold is not audited with the team and the feature"
expect_status 200 "$team_token" /api/v1/actions/unhold '{"hosts":["node-3"],"reason":"e2e: team unhold"}'
wait_until "node-3 hold lifted" 60 jq_true /api/v1/hosts/node-3 '(.hold.holds // []) == []'
pass "a team member holds and releases a host of its team; the audit log says through which feature"

wait_until "liveness known and gate open" 300 \
    jq_true /api/v1/plan '.gate.ok and .gate.network.livenessKnown and .gate.network.tracked == 128'

# --- B: a healthy roll -------------------------------------------------------

say "B: staged roll"
digest_b=$(publish b 'LABEL upgradoor.e2e.build=b org.opencontainers.image.revision=bbbbbbb')
pass "build B published: $digest_b"

wait_until "rollout for B opened" 120 rollout_exists "$digest_b"
wait_until "rollout for B completed" 1500 rollout_is "$digest_b" completed
all_on "$digest_b" || die "not every host runs build B"
pass "every host runs build B"

rb=$(rollout_of "$digest_b")
api /api/v1/rollouts | jq --arg id "$rb" '.recent[] | select(.id == $id)' >"$work/rollout-b.json"
jq -e '.canary != "" and ([.hosts[] | select(.state == "done")] | length) == 4' \
    "$work/rollout-b.json" >/dev/null || die "rollout B: not four done hosts with a canary"

# Leases for B, oldest first: one host at a time (the host limit is 1 for
# four hosts), the canary first, each released (node at head with peers,
# validators attesting again) before the next was granted.
api "/api/v1/audit?limit=500" | jq '[.entries[] | select(.action | startswith("lease."))] | reverse' \
    >"$work/audit-leases.json"
jq -e --arg c "$(jq -r .canary "$work/rollout-b.json")" '
    [.[] | select(.action == "lease.granted" or .action == "lease.released" or .action == "lease.failed")] as $e
    | ($e | map(select(.action == "lease.granted")) | .[0].target) == $c
    and ([range(0; $e | length) | select($e[.].action == "lease.granted") | . as $i
          | ($i == 0 or $e[$i - 1].action == "lease.released")] | all)' \
    "$work/audit-leases.json" >/dev/null ||
    die "leases for B were not strictly one after another with the canary first"
[ "$(cat "$work/max-open-leases" 2>/dev/null || echo 0)" -le 1 ] || die "more than one lease was open at once"
pass "B rolled host by host: canary first, never more than one lease, each released before the next"

jq -e '[.[] | select(.action == "lease.released") | .detail.readiness
        | startswith("ready: node at head with peers, validators attesting: ")] | length == 4 and all' \
    "$work/audit-leases.json" >/dev/null ||
    die "a lease was released without the node and attestation readiness check: $(jq -c '[.[] | select(.action == "lease.released") | .detail]' "$work/audit-leases.json")"
pass "every slot was released with the node at head with peers and the validators attesting"

# --- C: a broken build -------------------------------------------------------

say "C: broken build"
digest_c=$(publish c 'RUN printf "#!/bin/sh\necho broken build >&2\nexit 1\n" > /usr/local/bin/lighthouse && chmod +x /usr/local/bin/lighthouse
LABEL upgradoor.e2e.build=c org.opencontainers.image.revision=ccccccc')
pass "build C published: $digest_c"

wait_until "rollout for C halted" 900 rollout_is "$digest_c" halted
rc=$(rollout_of "$digest_c")
api /api/v1/rollouts | jq --arg id "$rc" '.open[] | select(.id == $id)' >"$work/rollout-c.json"
jq -e '([.hosts[] | select(.state == "failed")] | length) == 1
       and ([.hosts[] | select(.state == "done" or .state == "leased")] | length) == 0' \
    "$work/rollout-c.json" >/dev/null || die "rollout C: expected exactly one failed host and nobody else touched"
broken=$(jq -r '.hosts[] | select(.state == "failed") | .host' "$work/rollout-c.json")
pass "rollout C halted after $broken failed: $(jq -r .reason "$work/rollout-c.json")"

[ "$(hosts_on "$digest_b")" = 3 ] || die "expected three hosts still on build B"
granted_before=$(api "/api/v1/audit?action=lease.granted&limit=500" | jq '.entries | length')
sleep 90
granted_after=$(api "/api/v1/audit?action=lease.granted&limit=500" | jq '.entries | length')
[ "$granted_before" = "$granted_after" ] || die "a lease was granted while the digest was blocked"
[ "$(hosts_on "$digest_b")" = 3 ] || die "a host left build B while the digest was blocked"
rollout_is "$digest_c" halted || die "rollout C did not stay halted"
pass "digest C is blocked: no lease for 90s, three hosts still on B"

# --- D: recovery -------------------------------------------------------------

say "D: recovery"
digest_d=$(publish d 'LABEL upgradoor.e2e.build=d org.opencontainers.image.revision=ddddddd')
[ "$digest_d" != "$digest_c" ] || die "build D has the digest of build C"
pass "build D published: $digest_d"

wait_until "rollout C superseded" 180 rollout_is "$digest_c" superseded
wait_until "rollout for D completed" 1800 rollout_is "$digest_d" completed
all_on "$digest_d" || die "not every host runs build D"
rd=$(rollout_of "$digest_d")
first=$(api "/api/v1/audit?action=lease.granted&limit=500" |
    jq -r --arg id "$rd" '[.entries[] | select(.detail | tostring | contains($id))] | last | .target')
[ "$first" = "$broken" ] || die "D went to $first first, expected the broken host $broken"
wait_until "gate open again" 300 jq_true /api/v1/plan '.gate.ok'
pass "D superseded C, repaired $broken first, then rolled everyone"

# --- manual actions, settings, audit ------------------------------------------

say "manual actions and settings"
post /api/v1/actions/roll '{"hosts":["node-2"],"staggered":false,"reason":"e2e: roll one host now"}' \
    >"$work/action-roll-now.json"
rn=$(jq -r .rollout "$work/action-roll-now.json")
[ -n "$rn" ] && [ "$rn" != null ] || die "roll now returned no rollout"
wait_until "manual roll of node-2 completed" 600 \
    jq_true /api/v1/rollouts --arg id "$rn" 'any(.recent[]; .id == $id and .state == "completed")'

post /api/v1/actions/hold '{"hosts":["node-4"],"reason":"e2e: hold"}' >/dev/null
wait_until "node-4 reports staff's hold" 60 \
    jq_true /api/v1/hosts/node-4 '[.hold.holds[]?.holder] == ["staff"] and (.plan.holdRequests // []) == []'

# A hold of staff is not a team's to lift, also on a host of the team.
jq_true /api/v1/hosts/node-4 '[.hold.holds[]?.holder] == ["staff"]' || die "the staff hold is not shown as staff's"
expect_status 403 "$team_token" /api/v1/actions/unhold '{"hosts":["node-4"],"holders":["staff"]}'
jq -e '.error == "forbidden: upgradoor:team_manage does not lift the hold of staff on host node-4"' \
    "$work/answer.json" >/dev/null || die "the refusal does not say whose hold it is: $(cat "$work/answer.json")"
jq_true /api/v1/hosts/node-4 '[.hold.holds[]?.holder] == ["staff"] and (.plan.holdRequests // []) == []' ||
    die "a team member lifted a staff hold"

post /api/v1/actions/roll '{"all":true,"staggered":true,"reason":"e2e: staggered roll"}' \
    >"$work/action-roll-staggered.json"
rs=$(jq -r .rollout "$work/action-roll-staggered.json")
jq -e '.rejected["node-4"] != null or (.hosts | index("node-4") | not)' "$work/action-roll-staggered.json" \
    >/dev/null || die "the held host was accepted for the staggered roll"

post /api/v1/actions/pause '{"reason":"e2e: pause"}' >/dev/null
wait_until "global pause in effect" 30 jq_true /api/v1/plan '.paused == true'
post /api/v1/actions/resume '{"reason":"e2e: resume"}' >/dev/null

wait_until "staggered manual roll completed" 1500 \
    jq_true /api/v1/rollouts --arg id "$rs" 'any(.recent[]; .id == $id and .state == "completed")'
post /api/v1/actions/unhold '{"hosts":["node-4"],"reason":"e2e: unhold"}' >/dev/null
wait_until "node-4 hold lifted" 60 jq_true /api/v1/hosts/node-4 '(.hold.holds // []) == []'
[ "$(cat "$work/max-open-leases" 2>/dev/null || echo 0)" -le 1 ] || die "more than one lease was open at once"
pass "roll now, hold, pause/resume and a staggered roll all worked, still one lease at a time"

api /api/v1/settings | jq '.settings | .readyAttestations = 3 | .minPeers = 2 | .policyReboot = "auto"' >"$work/settings.json"
post /api/v1/settings "$(cat "$work/settings.json")" PUT >/dev/null
jq_true /api/v1/settings '.settings.readyAttestations == 3 and .defaults.readyAttestations == 2 and .settings.minPeers == 2 and .defaults.minPeers == 3' || die "settings change not stored"

api "/api/v1/audit?limit=500" >"$work/audit.json"
for action in settings.update action.roll action.hold action.unhold action.pause action.resume; do
    jq -e --arg a "$action" 'any(.entries[]; .action == $a and .actor == "e2e@example.com")' \
        "$work/audit.json" >/dev/null || die "audit log has no $action by e2e@example.com"
done
jq -e 'any(.entries[]; .action == "settings.update"
        and .detail.before.readyAttestations == 2 and .detail.after.readyAttestations == 3
        and .detail.before.minPeers == 3 and .detail.after.minPeers == 2)' \
    "$work/audit.json" >/dev/null || die "settings change is not audited with before and after"
for action in host.registered rollout.created rollout.halted rollout.completed lease.granted lease.released lease.failed; do
    jq -e --arg a "$action" 'any(.entries[]; .action == $a)' "$work/audit.json" >/dev/null ||
        die "audit log has no $action"
done
pass "settings change and actions are in the audit log with actor, before and after"

say "done"
pass "end-to-end run"
