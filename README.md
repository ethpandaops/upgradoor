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
 │  - local EL/CL probes     │  lease,      │  - API + embedded web UI     │
 └───────────────────────────┘  directives  └──────────────────────────────┘
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

- **No secret in the cluster.** The controller verifies agents against one
  signer *public* key and users against the authenticatoor's public keys.
- **One seed per devnet**, `secret_upgradoor_seed` in the devnet's sops file.
  Ansible derives the signer key and one key per host from it. The seed never
  reaches a host or the cluster. A host holds only its own key.
- **Post-quantum host identity.** Signer and host keys are ML-DSA-44
  (FIPS 204, Go `crypto/mldsa`), and nothing else: no hybrid with a classical
  signature. Host certificates and request proofs are ML-DSA-44 signatures,
  because signatures of the elliptic-curve class are not safe against a
  quantum attacker. The derivation from the seed (HKDF-SHA256) is symmetric
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
  devnet's authenticatoor. `/metrics` is unauthenticated and carries host names
  and counts only; the helm chart keeps it off the Ingress.

Known limits of the model are listed in [docs/protocol.md](docs/protocol.md#limits).

## Documentation

| Document | Content |
|---|---|
| [docs/install.md](docs/install.md) | Rolling it out on a devnet: seed, Ansible roles, cluster config, watchtower, key rotation |
| [docs/configuration.md](docs/configuration.md) | Controller flags, `agent.yaml`, settings, file locations, helm values |
| [docs/operations.md](docs/operations.md) | Day-2 runbook: waiting reasons, halted rollouts, holds, manual actions, failure cases, metrics, troubleshooting |
| [docs/ui.md](docs/ui.md) | The web UI: tabs, what is public, actions and dialogs, mock mode |
| [docs/protocol.md](docs/protocol.md) | Identity, sync and inventory, needs, leases, directives, errors, limits |
| [docs/scheduler.md](docs/scheduler.md) | The scheduler's rule book: rollouts, budget, gate, ordering, readiness |
| [docs/development.md](docs/development.md) | Repository layout, tests, the end-to-end run, UI development, releasing |

## Build, test, lint

Needs Go 1.27 or newer (`crypto/mldsa`). The web UI needs Node 24.

```sh
make build        # bin/upgradoor with the web UI embedded (builds web/dist if missing)
make test         # go test -race ./... and the web UI tests
make lint         # golangci-lint and eslint + prettier
```

`go build ./cmd/upgradoor` and `go test ./...` work without Node: without the
`webui` build tag the controller serves a placeholder page instead of the UI.

More suites (real Docker, systemd containers that really reboot, a Kurtosis
devnet) are described in [docs/development.md](docs/development.md).

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
  syncInterval: 5s
keyFile: .tmp/dev/$h/host.key
certFile: .tmp/dev/$h/host.crt
holdFile: .tmp/dev/$h/hold
stateDir: .tmp/dev/$h/state
verbs: [roll, packages, reboot]
containers:
  - {name: geth, kind: el, image: "ethpandaops/geth:master", stopTimeout: 5s}
  - {name: lighthouse, kind: cl, image: "sigp/lighthouse:latest", stopTimeout: 5s}
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
  - {name: geth, image: "ethpandaops/geth:master", digest: "sha256:aaa"}
  - {name: lighthouse, image: "sigp/lighthouse:latest", digest: "sha256:bbb"}
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
# A minimal beacon API: a healthy chain, no liveness endpoint.
go run ./test/e2e/fakebeacon --listen 127.0.0.1:5052
```

```sh
bin/upgradoor controller \
    --listen 127.0.0.1:8080 --data-dir .tmp/dev/data \
    --network dev --signer-public-key "$(cat .tmp/dev/signer.pub)" \
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

Open <http://127.0.0.1:8080>. Without `--auth-provider-url` the API is open, so
every tab is visible without a login.

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

The fake hosts have no validators on purpose. The fake beacon node does not
serve the liveness endpoint, so a fleet with validator ranges runs on the
fallback, exactly as in production: the network gate judges finality only,
and readiness and the budget use the agents' probes instead of attestations
(the gate strip says so). Add `--validator-start 0 --validator-end 32` to a
`host-cert` call to see that. For validators that really attest, use the
Kurtosis run (`make e2e`).

The scenario file and the control endpoint are described in
[docs/development.md](docs/development.md#fake-hosts).

## License

No license file has been added to this repository yet.
