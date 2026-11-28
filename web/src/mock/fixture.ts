// The mock fleet: a devnet with 24 hosts in the states an operator meets.
//
//   - a group rollout in progress (lighthouse cl): the canary
//     and a host without validators are done, one host is verifying, the
//     rest waits, one of them to pull the new image again after a failed
//     pull;
//   - a halted group rollout (nethermind el) with a failed host, which is
//     broken for it;
//   - a reboot rollout that waits for approval (policy manual);
//   - a stale host, a host on local hold and a host with a custom image.
//
// Everything is deterministic: the same fixture is used for development,
// screenshots and tests.

import type {
  AuditEntry,
  Container,
  ContainerOverride,
  ContainerStats,
  ContainerKind,
  HostDetail,
  Lease,
  Need,
  Package,
  Resources,
  Rollout,
  RolloutHost,
  RolloutTarget,
  Settings,
  Upgradable,
} from '../api/types';

/** hex returns `len` deterministic hex characters for a seed. */

/** MockRollout is a rollout as the mock stores it; its policy follows from the settings when it is served. */
export type MockRollout = Omit<Rollout, 'policy'>;

export function hex(seed: string, len = 64): string {
  let h = 1779033703 ^ seed.length;
  for (let i = 0; i < seed.length; i++) {
    h = Math.imul(h ^ seed.charCodeAt(i), 3432918353);
    h = (h << 13) | (h >>> 19);
  }

  let out = '';
  while (out.length < len) {
    h = Math.imul(h ^ (h >>> 16), 2246822507);
    h = Math.imul(h ^ (h >>> 13), 3266489909);
    h ^= h >>> 16;
    out += (h >>> 0).toString(16).padStart(8, '0');
  }
  return out.slice(0, len);
}

/** hash returns a small deterministic number for a seed. */
export function hash(seed: string): number {
  return parseInt(hex(seed, 8), 16);
}

const digest = (seed: string) => `sha256:${hex(`digest/${seed}`)}`;
const revision = (seed: string) => hex(`revision/${seed}`, 40);

const NETWORK = 'mock-devnet-0';

const VALIDATORS_PER_HOST = 128;
export const SECONDS_PER_SLOT = 12;
export const SLOTS_PER_EPOCH = 32;
export const EPOCH_MS = SECONDS_PER_SLOT * SLOTS_PER_EPOCH * 1000;

interface ImageSpec {
  /** container is the container name Ansible deploys. */
  container: string;
  image: string;
  /** kind is the kind label; absent for an unlabelled container. */
  kind?: ContainerKind;
}

const EL: Record<string, ImageSpec> = {
  geth: { container: 'geth', image: 'ethpandaops/geth:master', kind: 'el' },
  nethermind: { container: 'nethermind', image: 'nethermindeth/nethermind:master', kind: 'el' },
  besu: { container: 'besu', image: 'hyperledger/besu:develop', kind: 'el' },
  reth: { container: 'reth', image: 'ghcr.io/paradigmxyz/reth:nightly', kind: 'el' },
  erigon: { container: 'erigon', image: 'erigontech/erigon:main', kind: 'el' },
  ethrex: { container: 'ethrex', image: 'ghcr.io/lambdaclass/ethrex:main', kind: 'el' },
};

const CL: Record<string, ImageSpec> = {
  lighthouse: { container: 'lighthouse', image: 'sigp/lighthouse:unstable', kind: 'cl' },
  prysm: { container: 'prysm', image: 'gcr.io/offchainlabs/prysm/beacon-chain:latest', kind: 'cl' },
  teku: { container: 'teku', image: 'consensys/teku:develop', kind: 'cl' },
  nimbus: { container: 'nimbus', image: 'statusim/nimbus-eth2:multiarch-latest', kind: 'cl' },
  lodestar: { container: 'lodestar', image: 'chainsafe/lodestar:next', kind: 'cl' },
  grandine: { container: 'grandine', image: 'sifrai/grandine:unstable', kind: 'cl' },
};

const VC_IMAGE: Record<string, string> = {
  prysm: 'gcr.io/offchainlabs/prysm/validator:latest',
};

const CL_NAMES = Object.keys(CL);
const EL_NAMES = Object.keys(EL);

export const LIGHTHOUSE_IMAGE = (CL.lighthouse as ImageSpec).image;
const NETHERMIND_IMAGE = (EL.nethermind as ImageSpec).image;
const GRANDINE_IMAGE = (CL.grandine as ImageSpec).image;

/** currentDigest is the digest a tag points at in the registry. */
export const currentDigest = (image: string) => digest(`${image}/current`);
const previousDigest = (image: string) => digest(`${image}/previous`);

interface HostSpec {
  name: string;
  cl?: string;
  el?: string;
  validators: boolean;
  roles: string[];
  /** behind lists image references the host still runs at the previous digest. */
  behind?: string[];
  reboot?: boolean;
  stale?: boolean;
  /** agentProtocol is the protocol version the agent speaks; 1 when absent. */
  agentProtocol?: number;
  localHold?: string;
  customEL?: string;
  failedEL?: boolean;
  security?: boolean;
  oldDocker?: boolean;
  /** overrides are entries of the host's overrides file, by container name; until is in seconds from now. */
  overrides?: Record<string, Omit<ContainerOverride, 'until'> & { until?: number }>;
  /** fixed: the agent configuration lets the controller override nothing on this host. */
  fixed?: boolean;
  /** pullFailed: the last lease could not pull the images of the host's image needs. */
  pullFailed?: boolean;
}

function pair(cl: string, el: string, count: number): HostSpec[] {
  return Array.from({ length: count }, (_, i) => ({
    name: `${cl}-${el}-${i + 1}`,
    cl,
    el,
    validators: true,
    roles: ['ethereum_node'],
  }));
}

function specs(): HostSpec[] {
  const all: HostSpec[] = [
    ...pair('lighthouse', 'geth', 4),
    ...pair('lighthouse', 'nethermind', 1),
    ...pair('prysm', 'geth', 3),
    ...pair('prysm', 'erigon', 1),
    ...pair('teku', 'besu', 3),
    ...pair('teku', 'reth', 1),
    ...pair('nimbus', 'nethermind', 3),
    ...pair('lodestar', 'reth', 3),
    ...pair('grandine', 'erigon', 2),
    { name: 'rpc-1', cl: 'lighthouse', el: 'geth', validators: false, roles: ['ethereum_node', 'rpc'] },
    { name: 'bootnode-1', cl: 'lighthouse', el: 'geth', validators: false, roles: ['bootnode'] },
    { name: 'tooling-1', validators: false, roles: ['tooling'] },
  ];

  const patch = (name: string, p: Partial<HostSpec>) => {
    const spec = all.find(s => s.name === name);
    if (spec) Object.assign(spec, p);
  };

  patch('lighthouse-geth-3', { behind: [LIGHTHOUSE_IMAGE] });
  patch('lighthouse-geth-4', { behind: [LIGHTHOUSE_IMAGE], pullFailed: true });
  patch('lighthouse-nethermind-1', { behind: [LIGHTHOUSE_IMAGE, NETHERMIND_IMAGE] });
  patch('bootnode-1', { behind: [LIGHTHOUSE_IMAGE] });
  patch('nimbus-nethermind-1', { failedEL: true });
  patch('nimbus-nethermind-2', { behind: [NETHERMIND_IMAGE] });
  patch('nimbus-nethermind-3', { behind: [NETHERMIND_IMAGE] });
  patch('teku-besu-1', { reboot: true });
  patch('teku-besu-2', { reboot: true });
  patch('teku-besu-3', { reboot: true });
  patch('lodestar-reth-1', { reboot: true });
  patch('lodestar-reth-3', { localHold: 'debugging reth OOM, do not touch (pk)', oldDocker: true });
  patch('prysm-geth-3', { stale: true, oldDocker: true });
  patch('grandine-erigon-1', { agentProtocol: 0 });
  patch('grandine-erigon-2', { customEL: 'erigontech/erigon:pr-17012-debug', behind: [GRANDINE_IMAGE] });
  patch('teku-reth-1', { security: true });
  patch('prysm-erigon-1', { security: true });

  // Overrides, as the hosts' overrides files state them: one that is applied
  // and ends in a few hours, one whose entry is gone and that waits to be put
  // back, and one the agent cannot apply as written.
  patch('prysm-geth-1', {
    overrides: {
      prysm: {
        addFlags: ['--verbosity=debug'],
        by: 'alice@example.org',
        reason: 'debugging attestation misses',
        until: 4 * 3600 + 20 * 60,
        applied: true,
      },
    },
  });
  patch('lodestar-reth-2', {
    overrides: { reth: { removeFlags: ['--full'], by: 'bob@example.org', applied: true, restoring: true } },
  });
  patch('teku-besu-2', {
    overrides: {
      besu: {
        image: 'ghcr.io/somebody/besu:pr-9321',
        by: 'carol@example.org',
        reason: 'try the fix for the trie log',
        applied: false,
        error: 'image ghcr.io/somebody/besu:pr-9321 is not allowed for this container (allowed: hyperledger/besu)',
      },
    },
  });
  patch('prysm-geth-2', { fixed: true });

  return all;
}

const KERNEL_OLD = '6.12.48+deb13-amd64';
const KERNEL_NEW = '6.12.57+deb13-amd64';

/**
 * Libraries every host has, each in a few parts: enough packages for the
 * Packages tab to need more than one page.
 */
const FILLER_LIBRARIES = [
  'libacl1',
  'libapt-pkg7.0',
  'libattr1',
  'libaudit1',
  'libblkid1',
  'libbz2-1.0',
  'libcap2',
  'libcom-err2',
  'libcrypt1',
  'libdb5.3',
  'libdebconfclient0',
  'libext2fs2',
  'libffi8',
  'libgcrypt20',
  'libgmp10',
  'libgnutls30',
  'libgpg-error0',
  'libhogweed6',
  'libidn2-0',
  'liblz4-1',
  'liblzma5',
  'libmd0',
  'libmount1',
  'libnettle8',
  'libp11-kit0',
  'libpam0g',
  'libpcre2-8-0',
  'libseccomp2',
  'libselinux1',
  'libsemanage2',
  'libsepol2',
  'libsmartcols1',
  'libstdc++6',
  'libsystemd0',
  'libtasn1-6',
  'libtinfo6',
  'libudev1',
  'libunistring5',
  'libuuid1',
  'libxxhash0',
  'libzstd1',
  'zlib1g',
];

const FILLER_PACKAGES: [string, string][] = FILLER_LIBRARIES.flatMap((name, i): [string, string][] => {
  const version = `${1 + (i % 4)}.${i % 10}.${(i * 7) % 12}-${1 + (i % 3)}`;
  return [
    [name, version],
    [`${name}-dbgsym`, version],
    [`${name}-doc`, version],
  ];
});

const BASE_PACKAGES: [string, string][] = [
  ['apt', '3.0.3'],
  ['base-files', '13.8+deb13u1'],
  ['bash', '5.2.37-2+b5'],
  ['ca-certificates', '20250419'],
  ['containerd.io', '1.7.28-1~debian.13~trixie'],
  ['coreutils', '9.7-3'],
  ['curl', '8.14.1-2+deb13u2'],
  ['docker-buildx-plugin', '0.29.1-1~debian.13~trixie'],
  ['docker-ce', '5:29.0.0-1~debian.13~trixie'],
  ['docker-ce-cli', '5:29.0.0-1~debian.13~trixie'],
  ['docker-compose-plugin', '2.40.3-1~debian.13~trixie'],
  ['git', '1:2.47.3-0+deb13u1'],
  ['gnupg', '2.4.7-21+b3'],
  ['htop', '3.4.1-5'],
  ['iproute2', '6.15.0-1'],
  ['jq', '1.7.1-6+deb13u1'],
  ['libc6', '2.41-12'],
  ['libssl3t64', '3.5.4-1~deb13u1'],
  ['linux-image-amd64', '6.12.57-1'],
  ['openssh-server', '1:10.0p1-7'],
  ['openssl', '3.5.4-1~deb13u1'],
  ['python3', '3.13.5-1'],
  ['rsync', '3.4.1+ds1-5'],
  ['sudo', '1.9.16p2-3'],
  ['systemd', '257.8-1~deb13u2'],
  ['tmux', '3.5a-3'],
  ['unattended-upgrades', '2.12'],
  ['vim', '2:9.1.1230-2'],
  ['zstd', '1.5.7+dfsg-1'],
];

const OPENSSL_OLD = '3.5.1-1+deb13u1';
const LIBC_OLD = '2.41-11';
const DOCKER_OLD = '5:28.5.1-1~debian.13~trixie';
const DOCKER_NEW = '5:29.0.0-1~debian.13~trixie';

/** Everything the mock controller keeps in memory. */
export interface MockData {
  network: string;
  version: string;
  /** createdAt anchors every relative time of the fixture. */
  createdAt: number;
  genesis: number;
  hosts: Map<string, HostDetail>;
  /** seenAgo is the constant part of how long ago a host's agent synced, in seconds. */
  seenAgo: Map<string, number>;
  rollouts: MockRollout[];
  leases: Lease[];
  audit: AuditEntry[];
  settings: Settings;
  defaults: Settings;
  gateFailing: boolean;
  /** deployWindow is the initial deployment window (fleet.DeployWindowState), in ms. */
  deployWindow: { startedAt: number; armedAt?: number };
  /** quiet: every host attests, whatever its containers say. */
  quiet: boolean;
  /** offline are the times a host's validators do not attest without upgradoor having touched it, by host. */
  offline: Map<string, [number, number][]>;
  seq: number;
}

export function defaultSettings(): Settings {
  return {
    maxUnavailablePercent: 10,
    maxLeasedPercent: 10,
    maxIndependentPercent: 10,
    minParticipationPercent: 90,
    maxFinalityLagEpochs: 3,
    readyAttestations: 2,
    readyStableFor: '1m0s',
    minPeers: 3,
    leaseTtl: '30m0s',
    hostStaleAfter: '2m0s',
    readinessTimeout: '30m0s',
    baselineGrace: '15m0s',
    maxConcurrentPulls: 10,
    policyImage: 'auto',
    policyPackages: 'auto',
    policyReboot: 'manual',
    policyClients: {},
    policyKinds: {},
    actionPolicies: {},
    pauseAfterCanaryImage: false,
    pauseAfterCanaryPackages: false,
    pauseAfterCanaryReboot: false,
    readyPeerShare: 0,
    lookupInterval: '5m0s',
    paused: false,
  };
}

function container(
  host: string,
  spec: ImageSpec,
  at: (secondsAgo: number) => string,
  o: { behind: boolean; name?: string; image?: string; kind?: ContainerKind; fixed?: boolean }
): Container {
  const image = o.image ?? spec.image;
  const cur = currentDigest(image);
  const running = o.behind ? previousDigest(image) : cur;
  const name = o.name ?? spec.container;
  // A client's containers carry its name as their team label; a validator
  // client the team of its consensus client. Tooling has no team.
  const team = spec.kind === 'el' || spec.kind === 'cl' ? spec.container : undefined;
  const kind = o.kind ?? spec.kind;

  return {
    name,
    id: hex(`container/${host}/${name}`),
    image,
    imageId: `sha256:${hex(`imageid/${running}`)}`,
    digest: running,
    revision: revision(running),
    version: 'v0.0.0-dev',
    state: 'running',
    health: 'healthy',
    restartCount: 0,
    startedAt: at(o.behind ? 3 * 86400 + (hash(host) % 40000) : 1800 + (hash(host + name) % 5000)),
    managed: true,
    kind,
    team,
    // Labelled: part of the node; unlabelled (defaultPolicy roll): independent.
    impact: kind ? 'node' : 'independent',
    configuredImage: image,
    registry: {
      digest: cur,
      revision: revision(cur),
      checkedAt: at(20 + (hash(`check/${host}/${name}`) % 240)),
      pulled: true,
    },
    // What follows is in a host's details and in the rows of a client's host
    // list; the rows of the host table do not carry it (MockController.summary).
    command: team ? baseCommand(kind, name) : undefined,
    overridable: team && !o.fixed ? true : undefined,
  };
}

/** baseCommand is the configured command line of a client container: what its kind usually takes. */
function baseCommand(kind: string | undefined, name: string): string[] {
  const common = [`--network=${NETWORK}`, '--datadir=/data'];
  const metrics = `--metrics-port=${9000 + (hash(name) % 90)}`;
  switch (kind) {
    case 'el':
      return [
        ...common,
        '--http',
        '--http.addr=0.0.0.0',
        '--http.api=eth,net,web3,debug,txpool',
        '--authrpc.jwtsecret=/jwt/jwtsecret',
        '--syncmode=snap',
        '--metrics',
        metrics,
      ];
    case 'cl':
      return [
        ...common,
        '--execution-endpoint=http://execution:8551',
        '--jwt-secret=/jwt/jwtsecret',
        `--checkpoint-sync-url=https://checkpoint-sync.${NETWORK}.ethpandaops.io`,
        '--http',
        '--http-address=0.0.0.0',
        '--metrics',
        metrics,
      ];
    default:
      return [...common, '--beacon-nodes=http://beacon:5052', '--graffiti=upgradoor', metrics];
  }
}

function unmanaged(host: string, name: string, image: string, at: (s: number) => string): Container {
  return {
    name,
    id: hex(`container/${host}/${name}`),
    image,
    imageId: `sha256:${hex(`imageid/${image}`)}`,
    digest: digest(image),
    state: 'running',
    restartCount: 0,
    startedAt: at(9 * 86400 + (hash(host + name) % 80000)),
    managed: false,
  };
}

const PULL_ERROR = 'toomanyrequests: You have reached your unauthenticated pull rate limit';

function imageNeed(c: Container, pullFailed = false): Need {
  const to = currentDigest(c.image);
  return {
    id: `image/${c.name}/${to}`,
    kind: 'image',
    ...(pullFailed ? { pullError: PULL_ERROR } : {}),
    image: {
      container: c.name,
      kind: c.kind,
      team: c.team,
      image: c.image,
      fromDigest: c.digest,
      toDigest: to,
      toRevision: revision(to),
    },
  };
}

/** enr is a made-up bootnode record of about the length of a real one. */
const enr = (seed: string) => `enr:-${hex(`enr/${seed}`, 64).repeat(5)}`;

/** specNeed moves a container that runs another image than agent.yaml names back onto it; Ansible changed its bootnodes too. */
function specNeed(c: Container): Need {
  const image = c.configuredImage ?? c.image;
  const to = currentDigest(image);
  const key = to.slice(7, 19);
  return {
    id: `spec/${c.name}/${key}`,
    kind: 'spec',
    spec: {
      container: c.name,
      kind: c.kind,
      team: c.team,
      key,
      image,
      fromImage: c.image,
      toDigest: to,
      toRevision: revision(to),
      flags: true,
      command: {
        added: [`--bootnodes=${['a', 'b', 'd'].map(enr).join(',')}`, '--maxpeers=80'],
        removed: [`--bootnodes=${['a', 'b', 'c'].map(enr).join(',')}`, '--maxpeers=50', '--nat=any'],
      },
    },
  };
}

function buildHost(spec: HostSpec, index: number, at: (s: number) => string): HostDetail {
  const behind = new Set(spec.behind ?? []);
  const containers: Container[] = [];

  const el = spec.el ? EL[spec.el] : undefined;
  const cl = spec.cl ? CL[spec.cl] : undefined;

  if (el) {
    const c = container(spec.name, el, at, { behind: behind.has(el.image), fixed: spec.fixed });
    if (spec.customEL) {
      c.image = spec.customEL;
      c.digest = undefined;
      c.revision = undefined;
      c.registry = undefined;
      c.imageId = `sha256:${hex(`imageid/${spec.customEL}`)}`;
      c.startedAt = at(5 * 3600);
    }
    if (spec.failedEL) {
      c.state = 'restarting';
      c.health = 'unhealthy';
      c.restartCount = 14;
      c.startedAt = at(40);
    }
    containers.push(c);
  }

  if (cl) {
    containers.push(container(spec.name, cl, at, { behind: behind.has(cl.image), fixed: spec.fixed }));
    if (spec.validators) {
      const vcImage = VC_IMAGE[spec.cl as string] ?? cl.image;
      containers.push(
        container(spec.name, cl, at, {
          behind: behind.has(vcImage),
          name: `${cl.container}-validator`,
          image: vcImage,
          kind: 'vc',
          fixed: spec.fixed,
        })
      );
    }
  }

  // An RPC host has a snooper in front of its EL: a kind without a team, which nobody may override.
  if (spec.roles.includes('rpc')) {
    containers.push(
      container(spec.name, { container: 'snooper', image: 'ethpandaops/rpc-snooper:latest', kind: 'snooper-el' }, at, {
        behind: false,
      })
    );
  }

  if (!el && !cl) {
    containers.push(
      container(spec.name, { container: 'dora', image: 'ethpandaops/dora:master' }, at, {
        behind: false,
      }),
      container(spec.name, { container: 'spamoor', image: 'ethpandaops/spamoor:latest' }, at, {
        behind: false,
      })
    );
  }

  for (const c of containers) {
    const override = spec.overrides?.[c.name];
    if (override) {
      c.override = {
        ...override,
        until: override.until === undefined ? undefined : at(-override.until),
      };
      // A container that runs its override has the added flags on its command line.
      if (override.applied) c.command = [...(c.command ?? []), ...(override.addFlags ?? [])];
    }
  }

  containers.push(unmanaged(spec.name, 'node-exporter', 'prom/node-exporter:v1.9.1', at));
  if (index % 7 === 3) containers.push(unmanaged(spec.name, 'tcpdump-debug', 'nicolaka/netshoot:latest', at));

  const needs: Need[] = [];
  if (!spec.localHold) {
    for (const c of containers) {
      if (c.managed && c.configuredImage && c.image !== c.configuredImage) needs.push(specNeed(c));
      else if (c.managed && c.digest !== c.registry?.digest) {
        needs.push(imageNeed(c, spec.pullFailed));
        if (spec.pullFailed && c.registry)
          Object.assign(c.registry, { pulled: false, pullFailed: true, error: PULL_ERROR });
      }
    }
    if (spec.reboot) needs.push({ id: 'reboot', kind: 'reboot', reboot: { reasons: ['flag', 'kernel'] } });
  }

  const openssl = spec.security ? OPENSSL_OLD : (BASE_PACKAGES.find(p => p[0] === 'openssl') as [string, string])[1];
  const packages: Package[] = [...BASE_PACKAGES, ...FILLER_PACKAGES].map(([name, version]) => {
    let v = version;
    if (spec.security && (name === 'openssl' || name === 'libssl3t64')) v = OPENSSL_OLD;
    if (spec.security && name === 'libc6') v = LIBC_OLD;
    if (spec.oldDocker && (name === 'docker-ce' || name === 'docker-ce-cli')) v = DOCKER_OLD;
    return { name, version: v, arch: name === 'ca-certificates' || name === 'unattended-upgrades' ? 'all' : 'amd64' };
  });
  packages.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));

  const upgradable: Upgradable[] = [];
  if (spec.security) {
    const newest = (BASE_PACKAGES.find(p => p[0] === 'openssl') as [string, string])[1];
    for (const name of ['libssl3t64', 'openssl']) {
      upgradable.push({
        name,
        arch: 'amd64',
        current: openssl,
        candidate: newest,
        origin: 'Debian-Security:13/stable-security',
        security: true,
        blocked: false,
      });
    }
    upgradable.push({
      name: 'libc6',
      arch: 'amd64',
      current: LIBC_OLD,
      candidate: '2.41-12',
      origin: 'Debian-Security:13/stable-security',
      security: true,
      blocked: false,
    });
  }
  if (spec.oldDocker) {
    for (const name of ['docker-ce', 'docker-ce-cli']) {
      upgradable.push({
        name,
        arch: 'amd64',
        current: DOCKER_OLD,
        candidate: DOCKER_NEW,
        origin: 'Docker:trixie',
        security: false,
        blocked: true,
      });
    }
  }
  if (index % 3 === 0) {
    upgradable.push({
      name: 'tmux',
      arch: 'amd64',
      current: '3.5a-3',
      candidate: '3.5a-3+deb13u1',
      origin: 'Debian:13/stable-updates',
      security: false,
      blocked: false,
    });
  }
  upgradable.sort((a, b) => a.name.localeCompare(b.name));

  const resources = hostResources(spec.name, index);
  for (const c of containers) {
    if (c.managed && c.state === 'running') c.stats = containerStats(spec.name, c, resources.cpu.busy ?? 0);
  }

  const runningKernel = spec.reboot ? KERNEL_OLD : KERNEL_NEW;
  const uptime = spec.reboot ? 41 * 86400 + (hash(spec.name) % 80000) : 6 * 86400 + (hash(spec.name) % 300000);
  const headSlot = 181_000 + index;

  return {
    name: spec.name,
    roles: spec.roles,
    validators: spec.validators ? VALIDATORS_PER_HOST : 0,
    lastSeen: at(0),
    stale: spec.stale === true,
    agentVersion: spec.agentProtocol === 0 ? '0.2.9' : index % 9 === 4 ? '0.3.1' : '0.3.2',
    agentProtocol: spec.agentProtocol ?? 1,
    ...(spec.agentProtocol === undefined || spec.agentProtocol === 1
      ? {}
      : { incompatible: `agent protocol ${spec.agentProtocol} not supported (controller supports 1..1)` }),
    containers,
    node: {
      el: el
        ? spec.failedEL
          ? {
              reachable: false,
              syncing: false,
              peers: 0,
              head: 0,
              error: 'dial tcp 127.0.0.1:8545: connection refused',
            }
          : { reachable: true, syncing: false, peers: 18 + (hash(spec.name) % 30), head: headSlot - 40 }
        : undefined,
      cl: cl
        ? {
            reachable: true,
            syncing: false,
            optimistic: spec.failedEL === true,
            elOffline: spec.failedEL === true,
            peers: 30 + (hash(`cl/${spec.name}`) % 40),
            headSlot,
            syncDistance: 0,
          }
        : undefined,
    },
    os: {
      id: 'debian',
      versionId: '13',
      prettyName: 'Debian GNU/Linux 13 (trixie)',
      arch: 'amd64',
      runningKernel,
      newestKernel: KERNEL_NEW,
      rebootRequired: spec.reboot === true,
      rebootPackages: spec.reboot ? ['linux-image-6.12.57+deb13-amd64', 'libc6'] : undefined,
    },
    pending: {
      upgradable: upgradable.length,
      security: upgradable.filter(u => u.security).length,
      blocked: upgradable.filter(u => u.blocked).length,
    },
    uptimeSeconds: uptime,
    resources,
    hold:
      spec.localHold === undefined
        ? { local: false }
        : {
            local: true,
            localReason: spec.localHold,
            holds: [{ holder: 'host', by: 'pk', reason: spec.localHold, at: at(3600) }],
          },
    holders: spec.localHold === undefined ? [] : ['host'],
    verbs: spec.name === 'tooling-1' ? ['roll', 'packages'] : ['roll', 'packages', 'reboot'],
    // The mock's host actions are added by seedHostActions (hostActions.ts).
    actions: [],
    needs,
    // The scheduler view is computed per request by the mock controller.
    plan: { slot: 'idle', weight: 0, live: 0, liveKnown: false },

    certHash: hex(`cert/${spec.name}`),
    firstSeen: at(19 * 86400 + (hash(`first/${spec.name}`) % 3600)),
    remoteAddr: `203.0.113.${10 + index}`,
    assignedValidators: spec.validators ? VALIDATORS_PER_HOST : 0,
    inventory: {
      hash: hex(`inventory/${spec.name}`),
      collectedAt: at(3600 + (hash(`inv/${spec.name}`) % 7200)),
      os: {
        id: 'debian',
        versionId: '13',
        codename: 'trixie',
        prettyName: 'Debian GNU/Linux 13 (trixie)',
        arch: 'amd64',
      },
      kernels: {
        running: runningKernel,
        newest: KERNEL_NEW,
        installed: spec.reboot ? [KERNEL_OLD, KERNEL_NEW] : [KERNEL_NEW],
      },
      packages,
      upgradable,
    },
    leases: [],
    history: [],
  };
}

const GIB = 2 ** 30;

/**
 * hostResources is a host's resource use: most hosts are comfortable, every
 * eleventh is busy and every thirteenth is short of memory, so the fleet
 * summary and the colours have something to show.
 */
function hostResources(host: string, index: number): Resources {
  const cores = [8, 16, 32][hash(`cores/${host}`) % 3] ?? 16;
  const total = [32, 64, 128][hash(`mem/${host}`) % 3] ?? 64;
  const busy = index % 11 === 5 ? 93.4 : index % 7 === 2 ? 82.1 : 8 + (hash(`busy/${host}`) % 450) / 10;
  const memShare = index % 13 === 7 ? 0.94 : index % 9 === 4 ? 0.85 : 0.35 + (hash(`used/${host}`) % 40) / 100;
  const swapTotal = hash(`swap/${host}`) % 2 === 0 ? 4 * GIB : 0;
  const load = (busy / 100) * cores;

  return {
    cpu: { cores, load1: load, load5: load * 0.9, load15: load * 0.8, busy },
    memory: {
      total: total * GIB,
      available: Math.round(total * GIB * (1 - memShare)),
      swapTotal,
      swapUsed: swapTotal > 0 ? Math.round(swapTotal * (memShare > 0.9 ? 0.6 : 0.02)) : 0,
    },
  };
}

/** containerStats is a running managed container's share of its host: the EL takes the most. */
function containerStats(host: string, c: Container, busy: number): ContainerStats {
  const seed = hash(`stats/${host}/${c.name}`);
  switch (c.kind) {
    case 'el':
      return { cpu: Math.round(busy * 5.5) / 10, memory: (8 + (seed % 16)) * GIB };
    case 'cl':
      return { cpu: Math.round(busy * 3) / 10, memory: (4 + (seed % 8)) * GIB };
    case 'vc':
      return { cpu: 0.4, memory: (0.5 + (seed % 10) / 10) * GIB, memoryLimit: 4 * GIB };
    default:
      return { cpu: 0.2, memory: (seed % 400) * 2 ** 20 };
  }
}

/** A group rollout's key and title, as the scheduler writes them (groupKey, groupTitle); kind '' is unlabelled. */
function groupRollout(
  kind: ContainerKind,
  team: string,
  targets: RolloutTarget[]
): Pick<Rollout, 'kind' | 'key' | 'title' | 'containerKind' | 'team' | 'targets'> {
  const parts = targets.map(t => {
    const part = `${t.image} to ${t.digest.slice(7, 19)}`;
    if (!t.spec) return part;
    return `${t.container ?? ''} to ${part}${t.flags ? ', command line changed' : ''}`;
  });
  const unit = !kind ? `unit:/${targets[0]?.image ?? ''}` : team ? `unit:${kind}#${team}` : `unit:${kind}`;
  const name = !kind ? 'unlabelled' : team ? `${team} ${kind}` : kind;
  return {
    kind: 'group',
    key: `${unit}@${hex(JSON.stringify(targets), 16)}`,
    title: `${name}: ${parts.join('; ')}`,
    ...(kind ? { containerKind: kind } : {}),
    ...(team ? { team } : {}),
    targets,
  };
}

/** imageTarget is the target of a tag that moved. */
const imageTarget = (image: string, to: string): RolloutTarget => ({ image, digest: to, revision: revision(to) });

function member(
  host: string,
  state: RolloutHost['state'],
  stage: RolloutHost['stage'],
  updatedAt: string
): RolloutHost {
  return { host, state, stage, updatedAt };
}

/** buildMockData creates the fixture, with every time relative to `now`. */
/** The fleets of the mock: the fixture's two dozen hosts, the same with nothing going on, or three hundred in a rolling update. */
export type MockFleet = 'small' | 'quiet' | 'large' | 'huge';

export function buildMockData(now: number = Date.now(), fleet: MockFleet = 'small'): MockData {
  const at = (secondsAgo: number) => new Date(now - secondsAgo * 1000).toISOString();
  // The readiness note of a validator host that attested again after an operation done then.
  const attested = (doneSecondsAgo: number) =>
    `ready: node at head with peers, validators attesting: 2 of 2 slots since ${at(doneSecondsAgo).slice(11, 19)}`;

  const hosts = new Map<string, HostDetail>();
  const seenAgo = new Map<string, number>();

  specs().forEach((spec, index) => {
    const host = buildHost(spec, index, at);
    hosts.set(host.name, host);
    seenAgo.set(host.name, spec.stale ? 14 * 60 + 12 : hash(`seen/${spec.name}`) % 28);
  });

  // One client kept broken on purpose: known offline, left out of the gate and the budget.
  const offline = hosts.get('teku-reth-1');
  if (offline) offline.offline = { reason: 'reth crash under investigation, keep as is', until: at(-3 * 86400) };

  const need = (host: string, prefix: string): string => {
    const n = hosts.get(host)?.needs?.find(x => x.id.startsWith(prefix));
    return n?.id ?? '';
  };

  const lhDigest = currentDigest(LIGHTHOUSE_IMAGE);
  const nmDigest = currentDigest(NETHERMIND_IMAGE);
  const lhNeed = (c: string): Need => ({
    id: `image/${c}/${lhDigest}`,
    kind: 'image',
    image: {
      container: c,
      kind: c.endsWith('-validator') ? 'vc' : 'cl',
      team: 'lighthouse',
      image: LIGHTHOUSE_IMAGE,
      fromDigest: previousDigest(LIGHTHOUSE_IMAGE),
      toDigest: lhDigest,
      toRevision: revision(lhDigest),
    },
  });
  const nmNeed: Need = {
    id: `image/nethermind/${nmDigest}`,
    kind: 'image',
    image: {
      container: 'nethermind',
      kind: 'el',
      team: 'nethermind',
      image: NETHERMIND_IMAGE,
      fromDigest: previousDigest(NETHERMIND_IMAGE),
      toDigest: nmDigest,
      toRevision: revision(nmDigest),
    },
  };

  const grDigest = currentDigest(GRANDINE_IMAGE);
  const grNeed = (c: string): Need => ({
    id: `image/${c}/${grDigest}`,
    kind: 'image',
    image: {
      container: c,
      kind: c.endsWith('-validator') ? 'vc' : 'cl',
      team: 'grandine',
      image: GRANDINE_IMAGE,
      fromDigest: previousDigest(GRANDINE_IMAGE),
      toDigest: grDigest,
      toRevision: revision(grDigest),
    },
  });

  const grandineRollout = `ro-${hex('rollout/grandine', 16)}`;
  const leaseGrandine = `ls-${hex('lease/grandine-erigon-1', 16)}`;
  const lighthouseRollout = `ro-${hex('rollout/lighthouse', 16)}`;
  const nethermindRollout = `ro-${hex('rollout/nethermind', 16)}`;
  const rebootRollout = `ro-${hex('rollout/reboot', 16)}`;
  const dockerRollout = `ro-${hex('rollout/docker', 16)}`;
  const oldLighthouseRollout = `ro-${hex('rollout/lighthouse-old', 16)}`;
  const abortedRollout = `ro-${hex('rollout/aborted', 16)}`;
  // Unlabelled tooling images: long references without a space to break at.
  const toolingRollout = `ro-${hex('rollout/tooling', 16)}`;
  const exporterRollout = `ro-${hex('rollout/exporter', 16)}`;
  const exporterOldRollout = `ro-${hex('rollout/exporter-old', 16)}`;
  const TOOLING_IMAGE = 'ghcr.io/ethpandaops/devnet-tooling-assertoor-test-runner/with-path:master-latest';
  const EXPORTER_IMAGE = 'ghcr.io/ethpandaops/ethereum-metrics-exporter-for-devnets/with-path:master-latest';

  const leaseVerifying = `ls-${hex('lease/lighthouse-geth-2', 16)}`;
  const leaseCanary = `ls-${hex('lease/lighthouse-geth-1', 16)}`;
  const leaseRpc = `ls-${hex('lease/rpc-1', 16)}`;
  const leaseFailed = `ls-${hex('lease/nimbus-nethermind-1', 16)}`;

  // The lease that failed on nimbus-nethermind-1 left its mark on the host.
  const brokenHost = hosts.get('nimbus-nethermind-1');
  if (brokenHost) brokenHost.broken = { 'unit:el#nethermind': leaseFailed };

  const withLease = (m: RolloutHost, leaseId: string, needId: string): RolloutHost => ({ ...m, leaseId, needId });
  const pending = (host: string, stage: RolloutHost['stage'], prefix: string, ago: number): RolloutHost => ({
    ...member(host, 'pending', stage, at(ago)),
    needId: need(host, prefix),
  });

  const rollouts: MockRollout[] = [
    {
      id: lighthouseRollout,
      ...groupRollout('cl', 'lighthouse', [imageTarget(LIGHTHOUSE_IMAGE, lhDigest)]),
      state: 'active',
      paused: false,
      approved: false,
      force: false,
      ignoreBudget: false,
      ignoreGate: false,
      canary: 'lighthouse-geth-1',
      createdBy: 'scheduler',
      createdAt: at(47 * 60),
      updatedAt: at(4 * 60),
      hosts: [
        withLease(member('lighthouse-geth-1', 'done', 2, at(38 * 60)), leaseCanary, 'image/lighthouse/' + lhDigest),
        // Syncing before its lease and after it: released as no worse, no success of the rollout.
        {
          ...withLease(member('rpc-1', 'done', 3, at(21 * 60)), leaseRpc, 'image/lighthouse/' + lhDigest),
          noWorse: true,
        },
        withLease(member('lighthouse-geth-2', 'leased', 3, at(9 * 60)), leaseVerifying, 'image/lighthouse/' + lhDigest),
        pending('lighthouse-geth-3', 3, 'image/lighthouse/', 46 * 60),
        pending('lighthouse-geth-4', 3, 'image/lighthouse/', 46 * 60),
        pending('lighthouse-nethermind-1', 3, 'image/lighthouse/', 46 * 60),
        pending('bootnode-1', 3, 'image/lighthouse/', 46 * 60),
      ],
    },
    {
      id: nethermindRollout,
      ...groupRollout('el', 'nethermind', [imageTarget(NETHERMIND_IMAGE, nmDigest)]),
      state: 'halted',
      reason: 'nimbus-nethermind-1: container nethermind restarted 14 times while verifying',
      paused: false,
      approved: false,
      force: false,
      ignoreBudget: false,
      ignoreGate: false,
      canary: 'nimbus-nethermind-1',
      createdBy: 'scheduler',
      createdAt: at(3 * 3600 + 12 * 60),
      updatedAt: at(2 * 3600 + 50 * 60),
      hosts: [
        {
          ...member('nimbus-nethermind-1', 'failed', 2, at(2 * 3600 + 50 * 60)),
          leaseId: leaseFailed,
          needId: nmNeed.id,
          error: 'container nethermind restarted 14 times while verifying',
        },
        pending('lighthouse-nethermind-1', 3, 'image/nethermind/', 3 * 3600 + 11 * 60),
        pending('nimbus-nethermind-2', 3, 'image/nethermind/', 3 * 3600 + 11 * 60),
        pending('nimbus-nethermind-3', 3, 'image/nethermind/', 3 * 3600 + 11 * 60),
      ],
    },
    {
      // The canary is done and ready; with pauseAfterCanaryImage on, the
      // rollout waits for a person before the second host goes.
      id: grandineRollout,
      ...groupRollout('cl', 'grandine', [imageTarget(GRANDINE_IMAGE, grDigest)]),
      state: 'active',
      paused: true,
      pauseReason: 'paused after the canary; resume to continue',
      approved: false,
      force: false,
      ignoreBudget: false,
      ignoreGate: false,
      canary: 'grandine-erigon-1',
      createdBy: 'scheduler',
      createdAt: at(95 * 60),
      updatedAt: at(78 * 60),
      hosts: [
        withLease(member('grandine-erigon-1', 'done', 2, at(78 * 60)), leaseGrandine, grNeed('grandine').id),
        pending('grandine-erigon-2', 3, 'image/grandine/', 94 * 60),
      ],
    },
    {
      id: toolingRollout,
      ...groupRollout('', '', [imageTarget(TOOLING_IMAGE, digest('tooling/next'))]),
      state: 'active',
      paused: true,
      approved: false,
      force: false,
      ignoreBudget: false,
      ignoreGate: false,
      createdBy: 'scheduler',
      createdAt: at(6 * 3600),
      updatedAt: at(5 * 3600),
      hosts: [member('tooling-1', 'pending', 3, at(6 * 3600))],
    },
    {
      id: rebootRollout,
      kind: 'reboot',
      key: 'reboot',
      title: 'Reboot (kernel 6.12.57, reboot-required flag)',
      state: 'active',
      paused: false,
      approved: false,
      force: false,
      ignoreBudget: false,
      ignoreGate: false,
      canary: 'lodestar-reth-1',
      createdBy: 'scheduler',
      createdAt: at(26 * 3600),
      updatedAt: at(26 * 3600),
      hosts: [
        pending('lodestar-reth-1', 2, 'reboot', 26 * 3600),
        pending('teku-besu-1', 3, 'reboot', 26 * 3600),
        pending('teku-besu-2', 3, 'reboot', 25 * 3600),
        pending('teku-besu-3', 3, 'reboot', 25 * 3600),
      ],
    },
    {
      id: dockerRollout,
      kind: 'packages',
      key: `packages:${hex('packages/docker', 16)}`,
      title: 'Packages: docker-ce, docker-ce-cli → 5:29.0.0',
      packages: [
        { name: 'docker-ce', from: DOCKER_OLD, to: DOCKER_NEW },
        { name: 'docker-ce-cli', from: DOCKER_OLD, to: DOCKER_NEW },
      ],
      state: 'completed',
      paused: false,
      approved: false,
      force: false,
      ignoreBudget: false,
      ignoreGate: false,
      canary: 'grandine-erigon-1',
      createdBy: 'scheduler',
      createdAt: at(3 * 86400 + 5 * 3600),
      updatedAt: at(2 * 86400 + 2 * 3600),
      endedAt: at(2 * 86400 + 2 * 3600),
      hosts: [...hosts.keys()]
        .filter(h => h !== 'lodestar-reth-3' && h !== 'prysm-geth-3')
        .map(h => ({
          ...member(h, 'done', h === 'grandine-erigon-1' ? 2 : 3, at(2 * 86400 + 3 * 3600)),
          leaseId: `ls-${hex(`lease/docker/${h}`, 16)}`,
        })),
    },
    {
      id: exporterRollout,
      ...groupRollout('', '', [imageTarget(EXPORTER_IMAGE, digest('exporter/current'))]),
      state: 'completed',
      paused: false,
      approved: false,
      force: false,
      ignoreBudget: false,
      ignoreGate: false,
      createdBy: 'scheduler',
      createdAt: at(2 * 86400 + 40 * 60),
      updatedAt: at(2 * 86400),
      endedAt: at(2 * 86400),
      hosts: [
        {
          ...member('tooling-1', 'done', 3, at(2 * 86400)),
          leaseId: `ls-${hex('lease/exporter/tooling-1', 16)}`,
        },
      ],
    },
    {
      id: exporterOldRollout,
      ...groupRollout('', '', [imageTarget(EXPORTER_IMAGE, digest('exporter/older'))]),
      state: 'superseded',
      reason: `the target changed: unlabelled: ${EXPORTER_IMAGE} to ${digest('exporter/current').slice(7, 19)}`,
      paused: false,
      approved: false,
      force: false,
      ignoreBudget: false,
      ignoreGate: false,
      createdBy: 'scheduler',
      createdAt: at(2 * 86400 + 3 * 3600),
      updatedAt: at(2 * 86400 + 40 * 60),
      endedAt: at(2 * 86400 + 40 * 60),
      hosts: [member('tooling-1', 'pending', 3, at(2 * 86400 + 3 * 3600))],
    },
    {
      id: oldLighthouseRollout,
      ...groupRollout('cl', 'lighthouse', [{ image: LIGHTHOUSE_IMAGE, digest: digest('lighthouse/older') }]),
      state: 'superseded',
      reason: `the target changed: lighthouse cl: ${LIGHTHOUSE_IMAGE} to ${previousDigest(LIGHTHOUSE_IMAGE).slice(7, 19)}`,
      paused: false,
      approved: false,
      force: false,
      ignoreBudget: false,
      ignoreGate: false,
      canary: 'lighthouse-geth-1',
      createdBy: 'scheduler',
      createdAt: at(4 * 86400),
      updatedAt: at(4 * 86400 - 3000),
      endedAt: at(4 * 86400 - 3000),
      hosts: [
        {
          ...member('lighthouse-geth-1', 'done', 2, at(4 * 86400 - 1800)),
          leaseId: `ls-${hex('lease/lighthouse-old/lighthouse-geth-1', 16)}`,
        },
        { ...member('rpc-1', 'done', 3, at(4 * 86400 - 600)), leaseId: `ls-${hex('lease/lighthouse-old/rpc-1', 16)}` },
        member('lighthouse-geth-2', 'pending', 3, at(4 * 86400)),
        member('lighthouse-geth-3', 'pending', 3, at(4 * 86400)),
      ],
    },
    {
      id: abortedRollout,
      kind: 'manual',
      key: `manual:${abortedRollout}`,
      title: 'Manual reboot of 3 hosts',
      state: 'aborted',
      reason: 'wrong selection',
      paused: false,
      approved: true,
      force: false,
      ignoreBudget: false,
      ignoreGate: false,
      createdBy: 'alice@example.org',
      createdAt: at(5 * 86400),
      updatedAt: at(5 * 86400 - 240),
      endedAt: at(5 * 86400 - 240),
      // Aborted before anybody was served: the members are as they were then.
      hosts: [
        member('prysm-geth-1', 'pending', 3, at(5 * 86400 - 300)),
        member('prysm-geth-2', 'pending', 3, at(5 * 86400 - 300)),
        member('prysm-erigon-1', 'pending', 3, at(5 * 86400 - 300)),
      ],
    },
  ];

  const leases: Lease[] = [
    {
      id: leaseVerifying,
      host: 'lighthouse-geth-2',
      state: 'verifying',
      needs: [lhNeed('lighthouse'), lhNeed('lighthouse-validator')],
      rollouts: {
        [lhNeed('lighthouse').id]: lighthouseRollout,
        [lhNeed('lighthouse-validator').id]: lighthouseRollout,
      },
      weight: VALIDATORS_PER_HOST,
      charged: VALIDATORS_PER_HOST,
      force: false,
      grantedBy: 'scheduler',
      grantedAt: at(9 * 60),
      expiresAt: at(9 * 60 - 30 * 60),
      // Done two minutes ago, at head with peers for 30 s: the node part of
      // the readiness check waits for that to last a minute.
      agentDoneAt: at(2 * 60),
      operation: {
        leaseId: leaseVerifying,
        phase: 'done',
        startedAt: at(9 * 60 - 8),
        updatedAt: at(2 * 60),
        steps: [
          { needId: lhNeed('lighthouse').id, status: 'done' },
          { needId: lhNeed('lighthouse-validator').id, status: 'done' },
        ],
        disrupted: true,
      },
      readiness: 'node at head with peers, waiting for it to stay so for 1m0s',
      readyAt: at(-30),
      baseline: {
        at: at(9 * 60 + 10),
        node: {
          el: { reachable: true, syncing: false, peers: 27, head: 9_140_200 },
          cl: {
            reachable: true,
            syncing: false,
            optimistic: false,
            elOffline: false,
            peers: 48,
            headSlot: 181_950,
            syncDistance: 0,
          },
        },
        duty: VALIDATORS_PER_HOST,
        live: VALIDATORS_PER_HOST,
        liveKnown: true,
      },
    },
    {
      id: leaseGrandine,
      host: 'grandine-erigon-1',
      state: 'released',
      needs: [grNeed('grandine'), grNeed('grandine-validator')],
      rollouts: {
        [grNeed('grandine').id]: grandineRollout,
        [grNeed('grandine-validator').id]: grandineRollout,
      },
      weight: VALIDATORS_PER_HOST,
      charged: VALIDATORS_PER_HOST,
      force: false,
      grantedBy: 'scheduler',
      grantedAt: at(93 * 60),
      expiresAt: at(63 * 60),
      agentDoneAt: at(92 * 60),
      endedAt: at(78 * 60),
      operation: {
        leaseId: leaseGrandine,
        phase: 'done',
        startedAt: at(93 * 60 - 6),
        updatedAt: at(92 * 60),
        steps: [
          { needId: grNeed('grandine').id, status: 'done' },
          { needId: grNeed('grandine-validator').id, status: 'done' },
        ],
        disrupted: true,
      },
      readiness: attested(92 * 60),
      // What it was released on: a quarter of the fleet's median (readyPeerShare 0.25).
      peerChecks: [
        { layer: 'EL', peers: 27, median: 31, needs: 8 },
        { layer: 'CL', peers: 49, median: 52, needs: 13 },
      ],
    },
    {
      id: leaseCanary,
      host: 'lighthouse-geth-1',
      state: 'released',
      needs: [lhNeed('lighthouse'), lhNeed('lighthouse-validator')],
      rollouts: {
        [lhNeed('lighthouse').id]: lighthouseRollout,
        [lhNeed('lighthouse-validator').id]: lighthouseRollout,
      },
      weight: VALIDATORS_PER_HOST,
      charged: VALIDATORS_PER_HOST,
      force: false,
      grantedBy: 'scheduler',
      grantedAt: at(46 * 60),
      expiresAt: at(16 * 60),
      // Released 7 minutes after done.
      agentDoneAt: at(45 * 60),
      endedAt: at(38 * 60),
      operation: {
        leaseId: leaseCanary,
        phase: 'done',
        startedAt: at(46 * 60 - 6),
        updatedAt: at(45 * 60),
        steps: [
          { needId: lhNeed('lighthouse').id, status: 'done' },
          { needId: lhNeed('lighthouse-validator').id, status: 'done' },
        ],
        disrupted: true,
      },
      readiness: attested(45 * 60),
    },
    {
      id: leaseRpc,
      host: 'rpc-1',
      state: 'released',
      needs: [lhNeed('lighthouse')],
      rollouts: { [lhNeed('lighthouse').id]: lighthouseRollout },
      weight: 0,
      charged: 0,
      force: false,
      grantedBy: 'scheduler',
      grantedAt: at(36 * 60),
      expiresAt: at(6 * 60),
      agentDoneAt: at(33 * 60),
      endedAt: at(21 * 60),
      operation: {
        leaseId: leaseRpc,
        phase: 'done',
        startedAt: at(36 * 60 - 5),
        updatedAt: at(33 * 60),
        steps: [{ needId: lhNeed('lighthouse').id, status: 'done' }],
        disrupted: true,
      },
      readiness: 'not ready before the lease: node is syncing; no worse after',
      // The EL was still catching up when the lease was granted, and was after the grace.
      baseline: {
        at: at(36 * 60 + 20),
        node: {
          el: { reachable: true, syncing: true, peers: 22, head: 9_102_377 },
          cl: {
            reachable: true,
            syncing: false,
            optimistic: true,
            elOffline: false,
            peers: 51,
            headSlot: 181_700,
            syncDistance: 0,
          },
        },
        duty: 0,
        live: 0,
        liveKnown: false,
        containers: [
          { name: 'geth', kind: 'el', state: 'running', restartCount: 2 },
          { name: 'lighthouse', kind: 'cl', state: 'running', restartCount: 0 },
        ],
        unready: 'node is syncing',
        root: 'el',
      },
      noWorseSince: at(31 * 60),
      noWorseUntil: at(21 * 60),
      outcome: 'no-worse',
    },
    {
      id: leaseFailed,
      host: 'nimbus-nethermind-1',
      state: 'failed',
      needs: [nmNeed],
      rollouts: { [nmNeed.id]: nethermindRollout },
      weight: VALIDATORS_PER_HOST,
      charged: VALIDATORS_PER_HOST,
      force: false,
      grantedBy: 'scheduler',
      grantedAt: at(3 * 3600 + 10 * 60),
      expiresAt: at(2 * 3600 + 40 * 60),
      agentDoneAt: at(2 * 3600 + 50 * 60),
      endedAt: at(2 * 3600 + 50 * 60),
      operation: {
        leaseId: leaseFailed,
        phase: 'failed',
        startedAt: at(3 * 3600 + 10 * 60 - 7),
        updatedAt: at(2 * 3600 + 50 * 60),
        error: 'container nethermind restarted 14 times while verifying',
        steps: [
          { needId: nmNeed.id, status: 'failed', error: 'container nethermind restarted 14 times while verifying' },
        ],
        disrupted: true,
      },
      error: 'container nethermind restarted 14 times while verifying',
    },
  ];

  const defaults = defaultSettings();
  const audit = buildAudit({
    at,
    hosts,
    defaults,
    rollouts: { abortedRollout, dockerRollout, rebootRollout, nethermindRollout, grandineRollout, lighthouseRollout },
    leases: { leaseFailed, leaseGrandine, leaseCanary, leaseRpc, leaseVerifying },
    attested,
    targets: {
      nethermind: imageTarget(NETHERMIND_IMAGE, nmDigest),
      grandine: imageTarget(GRANDINE_IMAGE, grDigest),
      lighthouse: imageTarget(LIGHTHOUSE_IMAGE, lhDigest),
    },
  });

  return {
    network: NETWORK,
    version: '0.3.2-mock',
    createdAt: now,
    genesis: now - 5656 * EPOCH_MS - 7 * SECONDS_PER_SLOT * 1000,
    hosts,
    seenAgo,
    // A quiet fleet: nothing is rolling out and nobody is off.
    rollouts: fleet === 'quiet' ? [] : rollouts,
    leases: fleet === 'quiet' ? [] : leases,
    quiet: fleet === 'quiet',
    audit,
    // One client has an image policy of its own; no rollout of it is open.
    settings: {
      ...defaults,
      // The large fleets take more hosts at once than they may pull images for.
      ...(fleet === 'large' || fleet === 'huge' ? { maxLeasedPercent: 15 } : {}),
      pauseAfterCanaryImage: true,
      readyPeerShare: 0.25,
      policyClients: { el: { besu: 'manual' } },
    },
    defaults,
    gateFailing: false,
    // The quiet fleet is a fresh devnet in its initial deployment window; the others were armed long ago.
    deployWindow:
      fleet === 'quiet'
        ? { startedAt: now - 5 * 3600_000 }
        : { startedAt: now - 30 * 86400_000, armedAt: now - 29 * 86400_000 },
    offline:
      fleet === 'large' || fleet === 'huge'
        ? largeFleet(now, hosts, seenAgo, leases, fleet === 'huge' ? HUGE_FLEET_HOSTS : LARGE_FLEET_HOSTS)
        : new Map<string, [number, number][]>(),
    seq: 1000,
  };
}

/** How many hosts the large fleet adds to the fixture's two dozen. */
const LARGE_FLEET_HOSTS = 276;
/** The huge fleet is the large one with more hosts after its waves: twelve hundred in all. */
const HUGE_FLEET_HOSTS = 1176;

/**
 * largeFleet turns the fixture into a devnet of three hundred hosts in the
 * middle of a rolling update: a new geth image went out in waves of thirty
 * hosts over the last hours, two of the rolls failed, a wave is running. A
 * few hosts have been offline all along and two were for a while, without
 * upgradoor having touched them. It returns those times.
 */
function largeFleet(
  now: number,
  hosts: Map<string, HostDetail>,
  seenAgo: Map<string, number>,
  leases: Lease[],
  count: number
): Map<string, [number, number][]> {
  const at = (secondsAgo: number) => new Date(now - secondsAgo * 1000).toISOString();
  const names: string[] = [];
  const WAVE = 30;
  const WAVES = 8;

  for (let i = 0; i < count; i++) {
    const name = `node-${String(i + 1).padStart(4, '0')}`;
    // The hosts of the waves run lighthouse and geth; the ones after them are a mix of every client.
    const mixed = i >= WAVE * WAVES;
    const spec: HostSpec = {
      name,
      // Every consensus client paired with every execution client in turn.
      cl: mixed ? CL_NAMES[i % CL_NAMES.length] : 'lighthouse',
      el: mixed ? EL_NAMES[Math.floor(i / CL_NAMES.length) % EL_NAMES.length] : 'geth',
      validators: true,
      roles: ['ethereum_node'],
    };
    hosts.set(name, buildHost(spec, 100 + i, at));
    seenAgo.set(name, hash(`seen/${name}`) % 28);
    names.push(name);
  }

  const rollout = `ro-${hex('rollout/large-geth', 16)}`;
  const need: Need = {
    id: 'image/geth/large',
    kind: 'image',
    image: {
      container: 'geth',
      kind: 'el',
      team: 'geth',
      image: 'ethpandaops/geth:master',
      fromDigest: 'sha256:0ld',
      toDigest: 'sha256:new',
    },
  };
  for (let wave = 0; wave < WAVES; wave++) {
    // A wave every twenty minutes; the last one is running.
    const granted = (WAVES - 1 - wave) * 20 * 60 + 4 * 60;
    const running = wave === WAVES - 1;

    for (let i = 0; i < WAVE; i++) {
      const host = names[wave * WAVE + i];
      if (!host) continue;
      const took = 9 * 60 + (hash(`took/${host}`) % 300);
      const failed = !running && hash(`fail/${host}`) % 60 === 0;
      const id = `ls-${hex(`lease/${host}`, 16)}`;

      // The last ten of the running wave were granted and wait for the agent: as many as may pull at once.
      const waiting = running && i >= WAVE - 10;
      leases.push({
        id,
        host,
        state: waiting ? 'granted' : running ? 'running' : failed ? 'failed' : 'released',
        needs: [need],
        rollouts: { [need.id]: rollout },
        weight: VALIDATORS_PER_HOST,
        charged: VALIDATORS_PER_HOST,
        force: false,
        grantedBy: 'scheduler',
        grantedAt: at(granted),
        expiresAt: at(granted - 30 * 60),
        ...(running ? {} : { agentDoneAt: at(granted - took + 120), endedAt: at(granted - took) }),
        ...(failed ? { error: 'container exited 137 after the roll' } : {}),
      });
    }
  }

  // Not rolled yet, and off: five for good, two for forty minutes.
  const offline = new Map<string, [number, number][]>();
  names.slice(-5).forEach(host => offline.set(host, [[now - 30 * 3600_000, now]]));
  names
    .slice(-7, -5)
    .forEach((host, i) => offline.set(host, [[now - (95 - i * 12) * 60_000, now - (55 - i * 12) * 60_000]]));

  return offline;
}

/** What buildAudit writes about: the fixture's hosts, rollouts and leases. */
interface AuditInput {
  at: (secondsAgo: number) => string;
  hosts: ReadonlyMap<string, HostDetail>;
  defaults: Settings;
  rollouts: Record<
    | 'abortedRollout'
    | 'dockerRollout'
    | 'rebootRollout'
    | 'nethermindRollout'
    | 'grandineRollout'
    | 'lighthouseRollout',
    string
  >;
  leases: Record<'leaseFailed' | 'leaseGrandine' | 'leaseCanary' | 'leaseRpc' | 'leaseVerifying', string>;
  attested: (doneSecondsAgo: number) => string;
  targets: Record<'nethermind' | 'grandine' | 'lighthouse', RolloutTarget>;
}

/** The scheduler's readiness line of a lease that was released. */
const READY = 'ready: node at head with peers, validators attesting';

/**
 * buildAudit writes the audit log of the fixture as the controller does,
 * with the details its actions record: mostly the scheduler's rollouts and
 * leases over three weeks, the agents' registrations, and what a handful of
 * people did of every kind.
 */
function buildAudit(input: AuditInput): AuditEntry[] {
  const { at, hosts, defaults, rollouts: r, leases: l } = input;
  const pending: [number, string, string, string, Record<string, unknown> | undefined][] = [];
  const log = (secondsAgo: number, actor: string, action: string, target: string, detail?: Record<string, unknown>) => {
    pending.push([secondsAgo, actor, action, target, detail]);
  };
  // What allowed a person's request, as the controller stamps it.
  const staff = { authorizedBy: 'manage', teams: [] };
  const team = (t: string) => ({ authorizedBy: 'team_manage', teams: [t] });
  const admin = { authorizedBy: 'admin', teams: [] };
  const names = [...hosts.keys()];
  const H = 3600;
  const D = 86400;

  for (const [i, name] of names.entries()) {
    const h = hosts.get(name);
    log(19 * D - i * 40, `agent:${name}`, 'host.registered', name, {
      certHash: h?.certHash,
      roles: h?.roles,
      validators: h?.validators ?? 0,
      remoteAddr: `10.0.${Math.floor(i / 200)}.${(i % 200) + 10}`,
    });
  }
  log(11 * D, 'agent:prysm-geth-2', 'host.certificate-changed', 'prysm-geth-2', {
    before: { certHash: hex('cert/prysm-geth-2/old') },
    after: { certHash: hex('cert/prysm-geth-2') },
    remoteAddr: '10.0.0.24',
  });

  // Rollouts the scheduler ran on its own: a lease per host, granted and released.
  let leaseSeq = 0;
  const rolloutRun = (
    id: string,
    key: string,
    title: string,
    startAgo: number,
    members: readonly string[],
    end: 'completed' | 'superseded'
  ) => {
    log(startAgo, 'scheduler', 'rollout.created', id, { key, title });
    let t = startAgo - 60;
    for (const [i, host] of members.entries()) {
      leaseSeq += 1;
      const lease = `ls-${hex(`audit/lease/${leaseSeq}`, 16)}`;
      log(t, 'scheduler', 'lease.granted', host, {
        lease,
        rollouts: [id],
        cost: 1,
        impact: 'node',
        unavailable: i % 3,
        allowed: 3,
        force: false,
        ignoreBudget: false,
        ignoredGate: false,
        closedGate: false,
      });
      log(t - 540, 'scheduler', 'lease.released', host, {
        lease,
        force: false,
        readiness: `${READY}: ${3 + (i % 4)} of 4 attestations`,
      });
      if (i === 0 && end === 'completed') {
        log(t - 541, 'scheduler', 'rollout.paused', id, {
          key,
          reason: 'paused after the canary; resume to continue',
          lease,
        });
        log(t - 900, 'alice@example.org', 'action.resume', id, {
          before: { paused: true },
          after: { paused: false, approved: false },
          reason: 'canary is attesting',
          ...staff,
        });
        t -= 900;
      }
      t -= 120;
    }
    log(t, 'scheduler', `rollout.${end}`, id, {
      key,
      reason: end === 'superseded' ? 'newer package versions appeared' : '',
    });
  };

  const fleetHosts = names.filter(n => n !== 'tooling-1' && n !== 'bootnode-1');
  rolloutRun(
    `ro-${hex('audit/rollout/libssl', 16)}`,
    `packages:${hex('packages/libssl', 16)}`,
    'packages: libssl3 3.0.13-0ubuntu3.5, openssl 3.0.13-0ubuntu3.5',
    17 * D,
    fleetHosts,
    'completed'
  );
  rolloutRun(
    `ro-${hex('audit/rollout/geth-old', 16)}`,
    `unit:el/geth:${hex('geth/old', 12)}`,
    'geth el: ethpandaops/geth:master to sha256:5f0c2e9a1b7d',
    14 * D,
    fleetHosts.filter(n => n.includes('geth')),
    'completed'
  );
  rolloutRun(
    `ro-${hex('audit/rollout/curl', 16)}`,
    `packages:${hex('packages/curl', 16)}`,
    'packages: curl 8.5.0-2ubuntu10.5',
    12 * D,
    fleetHosts.slice(0, 6),
    'superseded'
  );
  rolloutRun(
    `ro-${hex('audit/rollout/deploy', 16)}`,
    `unit:cl/lighthouse:${hex('deploy/lighthouse', 12)}`,
    'lighthouse cl: lighthouse to sigp/lighthouse:v8.0.0 to sha256:8a1d0c7e44f2, command line changed',
    10 * D,
    fleetHosts.filter(n => n.startsWith('lighthouse')),
    'completed'
  );
  rolloutRun(
    `ro-${hex('audit/rollout/curl2', 16)}`,
    `packages:${hex('packages/curl2', 16)}`,
    'packages: curl 8.5.0-2ubuntu10.6',
    12 * D - 4 * H,
    fleetHosts,
    'completed'
  );
  const dropped = `ro-${hex('audit/rollout/dropped', 16)}`;
  log(9 * D, 'scheduler', 'rollout.created', dropped, {
    key: 'local:teku-besu-2',
    title: 'teku-besu-2: overrides file',
  });
  log(9 * D - 300, 'scheduler', 'rollout.dropped', dropped, {
    key: 'local:teku-besu-2',
    title: 'teku-besu-2: overrides file',
    reason: 'every host resolved on its own',
  });
  log(8 * D, 'scheduler', 'lease.granted', 'teku-besu-1', {
    lease: `ls-${hex('audit/expired', 16)}`,
    rollouts: [`ro-${hex('audit/rollout/curl2', 16)}`],
    cost: 1,
    impact: 'node',
    force: false,
  });
  log(8 * D - 1800, 'scheduler', 'lease.expired', 'teku-besu-1', {
    lease: `ls-${hex('audit/expired', 16)}`,
    reason: 'the agent did not report the lease done in time',
  });

  // People: the settings.
  log(18 * D, 'bob@example.org', 'action.arm-deploy-window', 'settings', {
    before: { armedAt: null },
    after: { armedAt: at(18 * D) },
    reason: 'fleet is deployed',
    ...admin,
  });
  log(6 * D, 'bob@example.org', 'settings.update', 'settings', {
    before: { ...defaults, maxUnavailablePercent: 5, readyAttestations: 4 },
    after: { ...defaults },
    ...admin,
  });
  log(4 * D, 'bob@example.org', 'action.client-policy', 'settings', {
    before: { policyClients: {} },
    after: { policyClients: { el: { besu: 'manual' } } },
    reason: 'besu releases need a look first',
    ...admin,
  });
  log(96 * 60, 'bob@example.org', 'settings.update', 'settings', {
    before: { ...defaults },
    after: { ...defaults, pauseAfterCanaryImage: true, readyPeerShare: 0.25 },
    ...admin,
  });
  log(15 * D, 'carol@example.org', 'action.pause', 'settings', {
    before: { paused: false },
    after: { paused: true },
    reason: 'devnet maintenance',
    ...staff,
  });
  log(15 * D - 2 * H, 'carol@example.org', 'action.resume', 'settings', {
    before: { paused: true },
    after: { paused: false },
    reason: 'maintenance done',
    ...staff,
  });
  log(2 * D, 'bob@example.org', 'sources.validator-ranges.refresh', 'validator-ranges', { changed: true, ...admin });

  // People: rollouts.
  log(5 * D, 'alice@example.org', 'action.reboot', r.abortedRollout, {
    hosts: ['prysm-erigon-1', 'prysm-geth-1', 'prysm-geth-2'],
    rejected: {},
    containers: [],
    staggered: true,
    force: false,
    ignoreBudget: false,
    ignoreGate: false,
    reason: 'kernel test',
    ...staff,
  });
  log(5 * D - 240, 'alice@example.org', 'action.abort', r.abortedRollout, {
    before: { state: 'active' },
    after: { state: 'aborted' },
    reason: 'wrong selection',
    ...staff,
  });
  const manualRoll = `ro-${hex('audit/rollout/manual-roll', 16)}`;
  log(13 * D, 'dave@example.org', 'action.roll', manualRoll, {
    hosts: ['teku-besu-1', 'teku-besu-2', 'teku-besu-3'],
    rejected: { 'teku-reth-1': 'the host runs no container of the team' },
    containers: ['teku'],
    staggered: true,
    force: false,
    ignoreBudget: false,
    ignoreGate: false,
    reason: 'pick up the new jwt secret',
    ...team('teku'),
  });
  log(13 * D - 1800, 'dave@example.org', 'action.skip', manualRoll, {
    hosts: ['teku-besu-3'],
    reason: 'that one is being debugged',
    ...team('teku'),
  });
  const hostAction = `ro-${hex('audit/rollout/host-action', 16)}`;
  log(7 * D, 'carol@example.org', 'action.host-action', hostAction, {
    action: 'prune-docker',
    hosts: ['rpc-1', 'tooling-1', 'bootnode-1'],
    rejected: {},
    basis: { 'rpc-1': 'disk 91% full' },
    pauseAfterCanary: true,
    ignoreGate: false,
    reason: 'disks are filling up',
    ...staff,
  });
  log(16 * D, 'alice@example.org', 'action.ignore-gate', `ro-${hex('audit/rollout/libssl', 16)}`, {
    before: { ignoreGate: false },
    after: { ignoreGate: true },
    reason: 'the gate is closed by a known lodestar bug',
    ...staff,
  });
  log(16 * D - 600, 'alice@example.org', 'action.ignore-budget', `ro-${hex('audit/rollout/libssl', 16)}`, {
    before: { ignoreBudget: false },
    after: { ignoreBudget: true },
    reason: 'security fix, roll it now',
    ...staff,
  });
  log(2 * H + 20 * 60, 'alice@example.org', 'action.pause', r.nethermindRollout, {
    before: { paused: false },
    after: { paused: true },
    reason: 'looking at the crash loop',
    ...staff,
  });
  log(2 * H + 10 * 60, 'alice@example.org', 'action.resume', r.nethermindRollout, {
    before: { paused: true },
    after: { paused: false, approved: false },
    reason: 'still halted; leave it to nethermind',
    ...staff,
  });
  log(3 * D, 'erin@example.org', 'action.retry', `ro-${hex('audit/rollout/geth-old', 16)}`, {
    hosts: ['lighthouse-geth-3'],
    before: { state: 'halted' },
    after: { state: 'active' },
    reason: 'the disk was full, fixed',
    ...team('geth'),
  });

  // People: hosts.
  log(8 * H, 'alice@example.org', 'action.hold', 'grandine-erigon-2', {
    holder: 'staff',
    directive: `dv-${hex('audit/hold', 12)}`,
    before: { held: false },
    after: { held: true, by: 'alice@example.org', reason: 'testing a debug build' },
    reason: 'testing a debug build',
    ...staff,
  });
  log(8 * H - 20, 'scheduler', 'directive.accepted', 'grandine-erigon-2', {
    directive: `dv-${hex('audit/hold', 12)}`,
    kind: 'hold',
    holder: 'staff',
  });
  log(7 * H, 'alice@example.org', 'action.unhold', 'grandine-erigon-2', {
    holder: 'staff',
    directive: `dv-${hex('audit/unhold', 12)}`,
    before: { held: true, by: 'alice@example.org', reason: 'testing a debug build' },
    after: { held: false, by: 'alice@example.org', reason: 'custom image is enough' },
    reason: 'custom image is enough',
    ...staff,
  });
  log(7 * H - 20, 'scheduler', 'directive.accepted', 'grandine-erigon-2', {
    directive: `dv-${hex('audit/unhold', 12)}`,
    kind: 'release-hold',
    holder: 'staff',
  });
  log(9 * D, 'carol@example.org', 'action.mark-offline', 'prysm-erigon-1', {
    before: { offline: false },
    after: { offline: true, reason: 'hardware swap', until: at(8 * D) },
    reason: 'hardware swap',
    ...staff,
  });
  log(8 * D, 'carol@example.org', 'action.unmark-offline', 'prysm-erigon-1', {
    before: { offline: true, reason: 'hardware swap' },
    after: { offline: false },
    reason: 'back',
    ...staff,
  });
  log(30 * H, 'frank@example.org', 'action.image-override', 'nimbus-nethermind-2', {
    container: 'nethermind',
    directive: `dv-${hex('audit/override', 12)}`,
    reason: 'debug build for the sync issue',
    part: 'image',
    image: 'nethermindeth/nethermind:debug-sync',
    until: at(-2 * D),
    ...team('nethermind'),
  });
  log(30 * H - 30, 'scheduler', 'directive.accepted', 'nimbus-nethermind-2', {
    directive: `dv-${hex('audit/override', 12)}`,
    kind: 'override',
    container: 'nethermind',
  });
  log(29 * H, 'frank@example.org', 'action.flag-override', 'nimbus-nethermind-3', {
    container: 'nethermind',
    directive: `dv-${hex('audit/flags', 12)}`,
    reason: 'more logs',
    part: 'flags',
    addFlags: ['--log=debug', '--Sync.VerboseLogging=true'],
    removeFlags: ['--log=info'],
    until: at(-D),
    ...team('nethermind'),
  });
  log(29 * H - 30, 'scheduler', 'directive.rejected', 'nimbus-nethermind-3', {
    directive: `dv-${hex('audit/flags', 12)}`,
    kind: 'override',
    container: 'nethermind',
    error: 'the agent configuration allows no flag overrides',
  });
  log(20 * H, 'frank@example.org', 'action.image-override', 'nimbus-nethermind-2', {
    container: 'nethermind',
    directive: `dv-${hex('audit/override-clear', 12)}`,
    reason: 'fixed upstream',
    part: 'image',
    clear: true,
    ...team('nethermind'),
  });
  log(20 * H - 600, 'scheduler', 'directive.cancelled', 'nimbus-nethermind-2', {
    directive: `dv-${hex('audit/override-clear', 12)}`,
    kind: 'override',
    container: 'nethermind',
    error: 'the agent did not answer in time',
  });
  log(6 * D, 'alice@example.org', 'action.approve-lease', 'lodestar-reth-2', {
    lease: `ls-${hex('audit/approve', 16)}`,
    rollouts: [`ro-${hex('audit/rollout/curl2', 16)}`],
    readiness: 'not ready: node is syncing',
    reason: 'it was syncing before, roll it anyway',
    ...staff,
  });
  log(6 * D - 400, 'scheduler', 'lease.released', 'lodestar-reth-2', {
    lease: `ls-${hex('audit/approve', 16)}`,
    force: false,
    readiness: 'not ready before the lease: node is syncing; no worse after',
    outcome: 'no-worse',
    approvedBy: 'alice@example.org',
  });
  log(4 * D, 'erin@example.org', 'action.end-lease', 'lodestar-reth-3', {
    lease: `ls-${hex('audit/end', 16)}`,
    state: 'verifying',
    reason: 'stuck verifying for an hour',
    ...staff,
  });
  log(4 * D - 60, 'scheduler', 'lease.cancelled', 'lodestar-reth-3', {
    lease: `ls-${hex('audit/end', 16)}`,
    by: 'erin@example.org',
    rollouts: [],
  });
  log(11 * D, 'carol@example.org', 'action.forget', 'old-bootnode-2', { reason: 'decommissioned', ...staff });
  log(11 * D - 120, 'carol@example.org', 'action.forget', 'old-bootnode-3', { reason: 'decommissioned', ...staff });
  log(11 * D - 121, 'carol@example.org', 'action.forget.failed', 'old-bootnode-3', {
    error: 'scheduler: delete host old-bootnode-3: database is locked',
  });
  log(2 * D + 3 * H, 'ci-deployer', 'auth.refused', '/api/v1/actions/roll', {
    method: 'POST',
    reason: 'the login lacks the feature manage',
    teams: ['lighthouse'],
  });
  log(30 * 60, 'dave@example.org', 'auth.refused', '/api/v1/settings', {
    method: 'PUT',
    reason: 'the login lacks the feature admin',
    teams: ['teku'],
  });

  // The open rollouts of the fixture.
  log(3 * D + 5 * H, 'scheduler', 'rollout.created', r.dockerRollout, {
    key: `packages:${hex('packages/docker', 16)}`,
    title: 'Packages: docker-ce, docker-ce-cli → 5:29.0.0',
  });
  log(2 * D + 2 * H, 'scheduler', 'rollout.completed', r.dockerRollout, {
    key: `packages:${hex('packages/docker', 16)}`,
    reason: '',
  });
  log(26 * H, 'scheduler', 'rollout.created', r.rebootRollout, { key: 'reboot', title: 'reboot' });
  const created = (t: RolloutTarget, unit: string) => ({
    key: `unit:${unit}:${hex(t.digest, 12)}`,
    title: `${unit.replace('/', ' ')}: ${t.image} to ${t.digest.slice(0, 19)}`,
  });
  log(
    3 * H + 12 * 60,
    'scheduler',
    'rollout.created',
    r.nethermindRollout,
    created(input.targets.nethermind, 'nethermind/el')
  );
  log(3 * H + 10 * 60, 'scheduler', 'lease.granted', 'nimbus-nethermind-1', {
    lease: l.leaseFailed,
    rollouts: [r.nethermindRollout],
    cost: 1,
    impact: 'node',
    force: false,
  });
  log(2 * H + 50 * 60, 'scheduler', 'lease.failed', 'nimbus-nethermind-1', {
    lease: l.leaseFailed,
    reason: 'container nethermind restarted 14 times while verifying',
    rollouts: [r.nethermindRollout],
  });
  log(2 * H + 50 * 60 - 1, 'scheduler', 'rollout.halted', r.nethermindRollout, {
    host: 'nimbus-nethermind-1',
    lease: l.leaseFailed,
    reason: 'container nethermind restarted 14 times while verifying',
  });
  log(95 * 60, 'scheduler', 'rollout.created', r.grandineRollout, created(input.targets.grandine, 'grandine/cl'));
  log(93 * 60, 'scheduler', 'lease.granted', 'grandine-erigon-1', {
    lease: l.leaseGrandine,
    rollouts: [r.grandineRollout],
    cost: 1,
    impact: 'node',
    force: false,
  });
  log(78 * 60, 'scheduler', 'lease.released', 'grandine-erigon-1', {
    lease: l.leaseGrandine,
    force: false,
    readiness: input.attested(92 * 60),
  });
  log(78 * 60 - 1, 'scheduler', 'rollout.paused', r.grandineRollout, {
    reason: 'paused after the canary; resume to continue',
    lease: l.leaseGrandine,
  });
  log(47 * 60, 'scheduler', 'rollout.created', r.lighthouseRollout, created(input.targets.lighthouse, 'lighthouse/cl'));
  log(46 * 60, 'scheduler', 'lease.granted', 'lighthouse-geth-1', {
    lease: l.leaseCanary,
    rollouts: [r.lighthouseRollout],
    cost: 1,
    impact: 'node',
    force: false,
  });
  log(38 * 60, 'scheduler', 'lease.released', 'lighthouse-geth-1', {
    lease: l.leaseCanary,
    force: false,
    readiness: input.attested(45 * 60),
  });
  // The lighthouse rollout paused after its canary too, and was resumed.
  log(38 * 60 - 1, 'scheduler', 'rollout.paused', r.lighthouseRollout, {
    reason: 'paused after the canary; resume to continue',
    lease: l.leaseCanary,
  });
  log(37 * 60, 'alice@example.org', 'action.resume', r.lighthouseRollout, {
    before: { paused: true },
    after: { paused: false, approved: false },
    reason: 'the canary looks good',
    ...staff,
  });
  log(36 * 60, 'scheduler', 'lease.granted', 'rpc-1', {
    lease: l.leaseRpc,
    rollouts: [r.lighthouseRollout],
    cost: 0,
    impact: 'node',
    force: false,
  });
  log(21 * 60, 'scheduler', 'lease.released', 'rpc-1', {
    lease: l.leaseRpc,
    force: false,
    readiness: 'not ready before the lease: node is syncing; no worse after',
    outcome: 'no-worse',
  });
  log(9 * 60, 'scheduler', 'lease.granted', 'lighthouse-geth-2', {
    lease: l.leaseVerifying,
    rollouts: [r.lighthouseRollout],
    cost: 1,
    impact: 'node',
    force: false,
  });

  // Oldest first, so IDs grow with time; a stable sort keeps the order of one moment.
  return pending
    .map((p, i) => ({ p, i }))
    .sort((a, b) => b.p[0] - a.p[0] || a.i - b.i)
    .map(({ p: [secondsAgo, actor, action, target, detail] }, i) => ({
      id: i + 1,
      time: at(secondsAgo),
      actor,
      action,
      target,
      detail,
    }));
}
