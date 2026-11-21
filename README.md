# upgradoor

Staged, health-gated image rolls, package upgrades and reboots for the Debian
hosts of an ethpandaops devnet. One controller per devnet, one small agent per
host.

- The **agent** runs on every host. It watches the Docker containers, the
  registries, the package state and the reboot flag, and reports them. It
  restarts nothing by itself.
- The **controller** runs in the devnet's service cluster. It groups what the
  hosts need into rollouts and hands out slots ("leases") a few hosts at a
  time, only while the network is healthy, and stops when a host fails.

It replaces watchtower for the node containers and adds what watchtower never
had: coordination across hosts, reboots, upgrades of the Docker packages, and
one place to see which host runs what.

## What it is not

- No remote shell, no arbitrary command execution, no configuration
  management. Ansible stays the tool for that.
- No per-package vetting, no file-integrity monitoring, no intrusion
  detection.
- One controller per devnet. No multi-devnet controller, no high availability:
  exactly one replica with one SQLite file.
- It does not install security updates. `unattended-upgrades` does (the
  `unattended_upgrades` Ansible role configures it); upgradoor only upgrades a
  short list of *blocked* packages (Docker) and reboots.
- It never moves a container to another tag or another image. It recreates a
  container on the newer digest of the tag the container already uses.

## Architecture

```
 host (Debian + Docker)                     service cluster (k8s, per devnet)
 ┌───────────────────────────┐              ┌──────────────────────────────┐
 │ upgradoor agent (systemd) │  HTTPS poll  │ upgradoor controller         │
 │  - docker socket          │ ───────────> │  - scheduler / leases        │
 │  - registry checks        │  cert+proof  │  - SQLite on a PVC           │
 │  - apt / kernel / reboot  │ <─────────── │  - beacon liveness + gate    │
 │  - local EL/CL probes     │ signed lease │  - API + embedded web UI     │
 └───────────────────────────┘  + directives └──────────────────────────────┘
                                                 ▲                ▲
                                   in-cluster beacon node   authenticatoor JWKS
```

- One Go module, one binary: `upgradoor agent`, `upgradoor controller` and
  `upgradoor keygen`.
- The agent opens no port. Every exchange is an outbound HTTPS request from the
  agent to `https://upgradoor.<network_subdomain>`: a sync every 30 s, every
  5 s while it holds a lease.
- The protocol is level-triggered. Every sync restates the host's full state
  and open needs, every answer restates the host's lease and directives. A lost
  message or a restart on either side loses nothing.

## Security model

- **Both sides prove who they are, and not through DNS or TLS.** Every agent
  request carries a host certificate and a proof signed with the host key.
  Every controller answer an agent acts on carries a signature of the devnet's
  controller key over the answer, the host it is for and the request it
  answers. An agent takes no lease and no directive from an answer without
  that signature: not from whoever took over the DNS name, obtained a TLS
  certificate for it or sits in a proxy in front of the controller, and not
  from a replayed answer or one meant for another host.
- **One secret in the cluster: the controller seed.** It is one entry per
  devnet group in the cluster's secrets file, mounted into the controller as
  a file. The controller derives its signing key from it at start, for its
  own network and a generation; nothing is generated at run time and nothing
  has to be fetched from a running controller. Everything else the controller
  holds is public: the signer public key for agents, the authenticatoor's
  public keys for users. Whoever holds the controller seed of a group can
  sign answers for the devnets of that group, which lets them order the
  closed verb set below, and nothing else.
- **Two seeds, two purposes.** The *devnet seed*, `secret_upgradoor_seed` in
  the devnet's sops file, yields the signer key and one key per host. It
  never reaches a host or the cluster. The *controller seed*,
  `secret_upgradoor_controller_seed`, yields the controller key of each
  devnet of a group. It is in the cluster and in the devnet repository's
  sops file, never on a host: a host holds only its own key and the
  controller's public key, so no host can sign for the controller.
- **Post-quantum identity.** Signer, controller and host keys are ML-DSA-44
  (FIPS 204, Go `crypto/mldsa`), and nothing else: no hybrid with a classical
  signature. Host certificates, request proofs and answer signatures are
  ML-DSA-44 signatures, because signatures of the elliptic-curve class are
  not safe against a quantum attacker. The derivation from the seeds (HKDF-SHA256) is symmetric
  and not affected. Two things are outside upgradoor's control and are not
  post-quantum: the user-login tokens, which the authenticatoor issues and
  signs (RS256), and TLS, which is the ingress's.
- **Closed verb set.** An agent can do three things, each only under a lease
  and only if its local configuration allows the verb: `roll` a managed
  container, upgrade the configured blocked `packages`, `reboot`. The
  controller cannot widen that set.
- **Digest verification.** A roll happens only if the locally pulled image for
  the container's own tag has exactly the digest named in the lease.
- **Hold file.** `touch /etc/upgradoor/hold` on a host and the agent requests
  nothing and refuses all work. Nothing the controller sends can remove that
  file.
- **Custom images are left alone.** A managed container that runs another image
  reference than the configured one is reported as `custom` and never rolled.
- **Public vs authenticated data.** Without a login the API serves host names,
  the EL and CL image of each host (tag, digest, revision), an update-pending
  marker and chain health. Kernels, OS details, package versions, addresses,
  rollouts, settings, the audit log and all actions need a token from the
  devnet's authenticatoor. `/metrics` is unauthenticated and carries fleet-wide
  counts only; the helm chart keeps it off the Ingress.
- **A token reads; a feature acts.** Every valid token may read. Each action
  needs a feature of the service `upgradoor` in the token (`roll`, `reboot`,
  `hold`, `recover`, `supervise`, `admin`), which the
  authenticatoor's grants hand out and which nobody has by default. A client
  team gets the team twins instead (`upgradoor:team_*`): its members may roll
  the containers their team owns, and reboot, hold and recover the hosts on
  which it owns one, always staggered and through the budget and the gate.
  A team lifts only the holds it set itself, and retries its hosts, not
  rollouts. `force`, "ignore budget", acting on the whole fleet, forgetting
  hosts, the global pause and the settings need `admin`. Who owns a container is what the host's own
  agent configuration says. The audit log records the teams of the login and
  the feature that allowed each action.

## Build, test, lint

Needs Go 1.27 or newer (`crypto/mldsa`). The web UI needs Node 24.

```sh
make build        # bin/upgradoor with the web UI embedded (rebuilds web/dist when a web source changed)
make test         # go test -race ./... and the web UI tests
make lint         # golangci-lint and eslint + prettier
make wire         # regenerates web/src/api/wire.schema.json and wire.golden.json from the Go types
```

`go build ./cmd/upgradoor` and `go test ./...` work without Node: without the
`webui` build tag the controller serves a placeholder page instead of the UI.

More suites need Docker: `make test-docker` (container recreate against real
containers), `make test-system` (package upgrades and reboots in a systemd
container), `make test-reboot` (agents under systemd whose leases survive real
reboots) and `make e2e` (a Kurtosis devnet with real beacons and tokens; CI
runs it nightly and on pull requests labelled `e2e`).

## Quick start for local development

This runs a controller and three agents against **fake hosts**, plus a fake
beacon node. With `--fake` the agent works on an in-memory host described by a
scenario file. It does not talk to Docker, apt or systemd and touches nothing
on the machine.

Run everything from the repository root. All files go to `.tmp/dev`, which is
ignored by git.

```sh
make build
mkdir -p .tmp/dev/data

# A throwaway seed. keygen reads it from the environment, never from a flag.
export UPGRADOOR_SEED=$(openssl rand -hex 32)
bin/upgradoor keygen signer-pubkey --network dev > .tmp/dev/signer.pub

# The controller seed is a second secret, not the seed above. The controller
# derives the key it signs its answers with from it at start. The agents get
# the public half and act on nothing that is not signed with it.
(umask 077 && openssl rand -hex 32 > .tmp/dev/controller.seed)
controller_pub=$(bin/upgradoor keygen controller-pubkey --network dev \
    --controller-seed-file .tmp/dev/controller.seed)

port=7771
for h in node-1 node-2 bootnode-1; do
  mkdir -p .tmp/dev/$h
  role=ethereum_node; [ $h = bootnode-1 ] && role=bootnode

  bin/upgradoor keygen host-key  --network dev --hostname $h > .tmp/dev/$h/host.key
  bin/upgradoor keygen host-cert --network dev --hostname $h --role $role > .tmp/dev/$h/host.crt

  cat > .tmp/dev/$h/agent.yaml <<EOF
controller:
  url: http://127.0.0.1:8080
  allowInsecure: true
  publicKeys: ["$controller_pub"]
  syncInterval: 5s
keyFile: .tmp/dev/$h/host.key
certFile: .tmp/dev/$h/host.crt
holdFile: .tmp/dev/$h/hold
stateDir: .tmp/dev/$h/state
overridesFile: .tmp/dev/$h/overrides.yaml
socket: {path: .tmp/dev/$h/agent.sock}
verbs: [roll, packages, reboot]
containers:
  - name: geth
    kind: el
    client: geth
    image: "ethpandaops/geth:master"
    stopTimeout: 5s
    owners: [geth]
    overrides: {images: ["ethpandaops/geth", "ethpandaops/geth-*"], flags: true}
  - name: lighthouse
    kind: cl
    client: lighthouse
    image: "sigp/lighthouse:latest"
    stopTimeout: 5s
    owners: [lighthouse]
    overrides: {images: ["sigp/*"]}
registry:
  pollInterval: 10s
packages:
  blocked: [docker-ce]
verify:
  window: 5s
  timeout: 1m
EOF

  cat > .tmp/dev/$h/host.yaml <<EOF
os: {id: debian, versionId: "13", prettyName: Debian GNU/Linux 13, arch: amd64}
runningKernel: 6.12.41-amd64
newestKernel: 6.12.41-amd64
containers:
  - {name: geth, image: "ethpandaops/geth:master", digest: "sha256:aaa", command: ["--http", "--maxpeers=50"]}
  - {name: lighthouse, image: "sigp/lighthouse:latest", digest: "sha256:bbb", command: ["bn", "--http"]}
registry:
  "ethpandaops/geth:fix": {digest: "sha256:fff", revision: f1f1f1}
packages:
  blocked: [docker-ce]
  installed:
    - {name: docker-ce, version: "28.0", arch: amd64}
control: 127.0.0.1:$port
EOF
  port=$((port + 1))
done
unset UPGRADOOR_SEED
```

Start the processes, each in its own terminal:

```sh
# A minimal beacon API: a healthy chain on which validators 0..63 attest.
go run ./test/e2e/fakebeacon --listen 127.0.0.1:5052
```

```sh
bin/upgradoor controller \
    --listen 127.0.0.1:8080 --data-dir .tmp/dev/data \
    --network dev --signer-public-key "$(cat .tmp/dev/signer.pub)" \
    --controller-seed-file .tmp/dev/controller.seed \
    --allow-insecure-agent-http \
    --beacon-endpoint http://127.0.0.1:5052
```

```sh
bin/upgradoor agent --config .tmp/dev/node-1/agent.yaml --fake .tmp/dev/node-1/host.yaml
```

```sh
bin/upgradoor agent --config .tmp/dev/node-2/agent.yaml --fake .tmp/dev/node-2/host.yaml
```

```sh
bin/upgradoor agent --config .tmp/dev/bootnode-1/agent.yaml --fake .tmp/dev/bootnode-1/host.yaml
```

`allowInsecure` and `--allow-insecure-agent-http` only permit plain http on
the loopback interface. The answers are signed and verified as in production:
an agent started against a controller with another controller key (another
controller seed, or `--controller-generation 1`) logs
`refused the controller's answer` and does nothing. (The opt-outs
`--allow-unsigned-agent-answers` and `controller.allowUnsignedAnswers` exist
for development without keys; the quick start does not need them.)

Open <http://127.0.0.1:8080>. Without `--auth-provider-url` the API is open, so
every tab is visible and every action allowed without a login. To try the
permissions, run the auth stub and start the controller with
`--auth-provider-url http://127.0.0.1:18099`; `--services` and `--teams` of
the stub choose who the login button logs in.

Make something happen through the fake hosts' control endpoints:

```sh
# The lighthouse tag moves to a new digest on all three hosts.
for p in 7771 7772 7773; do
  curl -s -X POST -d '{"ref":"sigp/lighthouse:latest","digest":"sha256:ccc","revision":"1a2b3c"}' \
      http://127.0.0.1:$p/registry > /dev/null
done

# A reboot becomes necessary on node-1.
curl -s -X POST -d '{"required":true}' http://127.0.0.1:7771/reboot-required > /dev/null
```

What to expect in the Rollouts tab within about a minute:

- An image rollout opens. One host goes first as the canary (none of the
  fake hosts has validators, so it is the first host that reports the new
  digest, or the first of those by name); when
  it is ready the other two follow one after the other (the host limit is 1
  for three hosts).
- A reboot rollout opens for `node-1` and waits: `waiting for approval (policy
  manual)`. Approve it on the rollout card to see a (simulated) reboot.

Overrides and the host commands work on the fake hosts too. The **Clients**
tab lists the hosts per client; "Image…" on a geth row sets an image override
(`ethpandaops/geth:fix` is in the fake registry and in the host's allowed
repositories), which the agent writes into the host's `overrides.yaml` and
applies under a lease. On the "node", the same through the agent's socket:

```sh
alias h1='bin/upgradoor host --socket .tmp/dev/node-1/agent.sock'
h1 init-overrides      # the generated block of .tmp/dev/node-1/overrides.yaml
h1 list                # containers, images, flags, overrides
h1 override set geth --add-flag --pprof --remove-flag --maxpeers --until 10m --reason "profiling"
h1 roll lighthouse     # waits and prints: queued, recreated, verified, done
h1 override clear geth
```

Editing `.tmp/dev/node-1/overrides.yaml` by hand does the same; the agent
notices within a sync. What the host recorded of all this is in
`.tmp/dev/node-1/state/audit.log`. (A Unix socket path is limited to about
100 characters; from a deeply nested checkout give `socket.path` a shorter
one.)

The fake hosts have no validators on purpose. Add
`validators: {start: 0, end: 32}` to an `agent.yaml` to give one some: the
fake beacon node's blocks carry a full attestation of every committee, so the
controller counts those validators as attesting, slot by slot and per epoch,
and the gate opens on that participation (a host reports its validator range
itself; on a real host the agent reads it from the marker file of the
`validator_keys` Ansible role, `validators.markerFile`). For validators that
really attest, use the Kurtosis run (`make e2e`).

## License

No license file has been added to this repository yet.
