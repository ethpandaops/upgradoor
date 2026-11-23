# upgradoor

Staggered container rolls, package upgrades and reboots for the hosts of an
ethpandaops devnet, gated on network health. One controller per devnet, one
agent per host. upgradoor never creates containers: Ansible deploys them,
upgradoor only recreates them on a newer digest of their tag (or an override).

## How it works

- The **agent** (`upgradoor agent`, systemd) reports the host's containers,
  blocked packages and reboot flag to the controller over outbound HTTPS. It
  opens no port and changes nothing without a lease.
- The **controller** (`upgradoor-controller run`) looks up the image tags at
  their registries, groups the needs into rollouts and grants leases a few
  hosts at a time:
  - budget: validator weight (`maxUnavailablePercent`) and a host limit
    (`maxLeasedPercent`);
  - network gate: participation and finality lag from the beacon node;
  - canary first, then the rest; a slot is released once the host is ready
    (at head, peers, validators attesting).
- Policies `auto` or `manual` per verb (`policyImage`, `policyPackages`,
  `policyReboot`), per client (`policyClients`) and per container kind
  (`policyKinds`, `other` for unlabelled containers); an image rollout answers
  to its client's, then its kind's, then `policyImage`. Settings are edited in
  the UI; `--default-setting key=value` sets their defaults.
- Web UI and API with logins from the devnet's authenticatoor. Every login
  reads a reduced view; more needs a feature of the service `upgradoor`:

  | feature | allows |
  |---|---|
  | `details` | host details: OS, kernel, packages, unmanaged containers, addresses, host actions |
  | `manage` | roll, reboot, hold, mark-offline, skip, retry, end-lease, client policy, image refresh, pause/resume/approve/abort of a rollout, `force`, `ignoreBudget`, all hosts; host actions without a `feature:` |
  | `manage_image` | image overrides |
  | `manage_flags` | flag overrides |
  | `admin` | all of the above, forget, global pause, settings, data sources |

  Each except `admin` has a `team_` twin limited to what the token's teams
  own (`team_manage`: no reboot, mark-offline or image refresh). A plain feature includes its twin; a
  grant `manage*` covers all three `manage` features. A host action in
  `agent.yaml` names its `feature:` (default `manage`); members of its
  `teams:` may start it without one.

## Containers

The containers carry two labels, set by Ansible:

- `io.ethpandaops.upgradoor.kind`: any kind (`el`, `cl`, `vc`, ...) makes the
  container managed with node impact; `none` excludes it.
- `io.ethpandaops.upgradoor.team`: the owning team, the client name (`geth`).
  Rollouts are per team and kind. Containers without a team form one rollout
  per kind across the fleet, which only staff may act on.

Unlabelled containers follow `defaultPolicy` in `agent.yaml`: `ignore`
(default) or `roll` (rolled as independent, without gate or budget).

## Host files

`/etc/upgradoor/agent.yaml`:

```yaml
controller:
  url: https://upgradoor.<network_subdomain>
  publicKey: <output of keygen controller-pubkey>
roles: [ethereum_node]
verbs: [roll, packages, reboot]
defaultPolicy: ignore
overridesFile: /home/devops/upgradoor-overrides.yaml  # omit to disable overrides
packages:
  blocked: [docker-ce, containerd.io]
probes:
  el: http://127.0.0.1:8545
  cl: http://127.0.0.1:5052
commands:
  upgradePackages: [/usr/bin/sudo, -n, /usr/local/lib/upgradoor/upgrade-packages]
  reboot: [/usr/bin/sudo, -n, /usr/bin/systemctl, reboot]
```

Other keys (unknown keys are an error): `keyFile`, `certFile`, `stateDir`,
`entryUser`, `managedFile`, `socket`, `auditLog`, `docker`, `verify`, and
`kindPolicies` + `fallback` (rolls while the controller is unreachable).

- `/etc/upgradoor/managed.yaml`: what Ansible deployed each container as
  (image, command), written by `upgradoor deploy`.
- Overrides file (only with `overridesFile`): a per-container image and/or
  command for a while. A command override replaces the whole command.
- Holds (local and controller) are kept in the agent's state in `stateDir`.

On-host commands (via the agent's socket, root and the entry user's group):

```
upgradoor list                  managed containers, images, commands, overrides
upgradoor status                hold, open needs, lease, requested rolls
upgradoor roll <container>...   request a roll and follow it
upgradoor override set|clear    set or clear an override (--image, --add-flag, --remove-flag, --until)
upgradoor init-overrides        lay out the overrides file
upgradoor hold [reason]         hold the host; upgradoor release lifts it
upgradoor deploy <container>    record what Ansible deployed (root only)
upgradoor key                   create the host key, print its public key
upgradoor verify-cert           print what a host certificate certifies
```

## Validators

The controller reads the devnet's validator name inventory from
`--validator-ranges-url` (e.g.
`https://config.<network_subdomain>/api/v1/nodes/validator-ranges`). Only
validators with an attestation duty count toward the budget and readiness.
Without the URL no host has validators and the budget is only the host limit.

## Security

- Each host creates its own ML-DSA key (`upgradoor key`). Its certificate is
  issued with the devnet seed: `upgradoor-controller keygen host-cert
  --network <n> --hostname <h> --public-key <key>`.
- The controller signs its answers with a key derived from the controller
  seed (`--controller-seed-file`); agents pin its public key
  (`controller.publicKey`) and act on nothing else.
- Neither public key depends on the network: the controller refuses
  certificates and agents refuse answers of another network.
- The agent runs as user `upgradoor` with two sudo rules (package upgrade,
  reboot) and does only the verbs its `agent.yaml` allows.
- Hosts install only signed releases (below).

## Releases and signing

Hosts install only releases whose `upgradoor-manifest.txt` is signed by a key
in `deploy/allowed_signers`. CI builds but never signs.

1. Run the `Build release` workflow with version `X.Y.Z`: reproducible
   binaries, the controller image by digest, the manifest, a draft release.
2. A signer runs `scripts/sign-release.sh vX.Y.Z` with a FIDO key plugged in.
   It rebuilds from the tag, checks binaries and image against the manifest,
   signs with one touch and uploads the signature.
3. Publish the draft. CI verifies the signature, then tags the image
   `vX.Y.Z` and `latest`.

New signers run `scripts/enroll-signer.sh "<name>" --add` and get the line
into `deploy/allowed_signers` here and in
`roles/upgradoor_agent/files/allowed_signers` of ansible-collection-general.

## Development

Needs Go 1.27+ and, for the web UI, Node 24.

```sh
make build       # bin/upgradoor and bin/upgradoor-controller (UI embedded)
make test        # Go tests (-race) and web tests
make lint        # golangci-lint, eslint, prettier
make wire        # regenerate the web UI's wire schema from the Go types
make test-docker # container recreate against a real Docker daemon
make test-system # package upgrades and reboots in a systemd container
make test-reboot # agents under systemd across real reboots
make e2e         # Kurtosis devnet with real clients
```

Local quick start with a fake beacon, a fake registry and one fake host
(touches nothing on the machine):

```sh
make build && mkdir -p .tmp/dev/data && cd .tmp/dev
export UPGRADOOR_SEED=$(openssl rand -hex 32)
openssl rand -hex 32 > controller.seed
../../bin/upgradoor-controller keygen signer-pubkey > signer.pub
cpub=$(../../bin/upgradoor-controller keygen controller-pubkey --controller-seed-file controller.seed)
pub=$(../../bin/upgradoor key --path host.key)
../../bin/upgradoor-controller keygen host-cert --network dev --hostname node-1 --public-key "$pub" > host.crt
cat > agent.yaml <<EOF
controller: {url: http://127.0.0.1:8080, allowInsecure: true, publicKey: "$cpub"}
keyFile: host.key
certFile: host.crt
stateDir: state
managedFile: managed.yaml
socket: {path: agent.sock}
verbs: [roll, packages, reboot]
EOF
cat > host.yaml <<EOF
os: {id: debian, versionId: "13", arch: amd64}
containers:
  - {name: geth, image: "127.0.0.1:5050/ethpandaops/geth:master", digest: "sha256:$(printf '%064d' 1)", kind: el, team: geth}
EOF
```

Then, each in its own terminal from `.tmp/dev`:

```sh
go run ../../test/e2e/fakebeacon --listen 127.0.0.1:5052
go run ../../test/fakeregistry --scenario host.yaml
../../bin/upgradoor-controller run --listen 127.0.0.1:8080 --data-dir data --network dev \
  --signer-public-key "$(cat signer.pub)" --controller-seed-file controller.seed \
  --allow-insecure-agent-http --allow-open-api --beacon-endpoint http://127.0.0.1:5052 \
  --registry-plain-http 127.0.0.1:5050 --registry-allow-private --default-setting lookupInterval=30s
go run ../../test/fakeagent --config agent.yaml --scenario host.yaml --registry 127.0.0.1:5050
```

Open <http://127.0.0.1:8080> and move the tag to see a rollout:

```sh
curl -X POST http://127.0.0.1:5050/tags \
  -d "{\"ref\":\"ethpandaops/geth:master\",\"digest\":\"sha256:$(printf '%064d' 2)\"}"
```

## License

No license file has been added to this repository yet.
