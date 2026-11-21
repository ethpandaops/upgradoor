// Badges for the domain states, the image cell and need descriptions.

import {
  list,
  type ClientHostState,
  type HostState,
  type Impact,
  type LeaseState,
  type Need,
  type OfflineMark,
  type PublicImageRef,
  type RolloutKind,
  type RolloutState,
  type Stage,
  type StepStatus,
} from '../api/types';
import { displayImage, shortDigest, shortRevision } from '../lib/format';
import { CopyValue, Digest } from './Digest';
import { Badge, Dash, type Tone } from './primitives';

const IMPACT_TITLE: Record<Impact, string> = {
  node: 'Impact node: counts against the validator budget, the host limit and the network gate',
  slot: 'Impact slot: counts against the host limit and the network gate, not the validator budget',
  independent: 'Impact independent: no gate and no validator budget; a limit of its own',
};

/** ImpactBadge names the impact class of a managed container. */
export function ImpactBadge({ impact }: { impact: Impact }) {
  return (
    <Badge tone={impact === 'node' ? 'neutral' : 'info'} title={IMPACT_TITLE[impact]}>
      {impact}
    </Badge>
  );
}

const ROLLOUT_TONE: Record<RolloutState, Tone> = {
  active: 'info',
  halted: 'danger',
  completed: 'success',
  superseded: 'neutral',
  aborted: 'warning',
};

/**
 * RolloutKindBadge names the kind of a rollout. A local rollout is what a
 * host asked for itself (an override, a roll from its command line): one
 * host, no policy, no canary.
 */
export function RolloutKindBadge({ kind }: { kind: RolloutKind }) {
  if (kind === 'spec') {
    return (
      <Badge title="agent.yaml names another image or command line for a container: Ansible's soft deploy, rolled under the image policy">
        spec
      </Badge>
    );
  }
  return kind === 'local' ? (
    <Badge title="Asked for on the host itself (an override, or a roll from its command line): one host, no policy, no canary">
      host request
    </Badge>
  ) : (
    <Badge>{kind}</Badge>
  );
}

export function RolloutStateBadge({ state }: { state: RolloutState }) {
  return <Badge tone={ROLLOUT_TONE[state]}>{state}</Badge>;
}

const HOST_STATE_TONE: Record<HostState, Tone> = {
  expected: 'neutral',
  pending: 'info',
  leased: 'accent',
  done: 'success',
  failed: 'danger',
  skipped: 'neutral',
};

export function HostStateBadge({ state }: { state: HostState }) {
  return <Badge tone={HOST_STATE_TONE[state]}>{state}</Badge>;
}

const CLIENT_STATE_TONE: Record<ClientHostState, Tone> = {
  failed: 'danger',
  rolling: 'accent',
  held: 'warning',
  overridden: 'info',
  pending: 'info',
  current: 'success',
};

/** CLIENT_STATE_LABEL names the state of a host on a client's page, as its badge, its filter and its count do. */
export const CLIENT_STATE_LABEL: Record<ClientHostState, string> = {
  failed: 'failed',
  rolling: 'rolling',
  held: 'held',
  overridden: 'overridden',
  pending: 'behind',
  current: 'up to date',
};

/** What each state means, for tooltips. Where several apply to a host the first of the list is shown. */
export const CLIENT_STATE_MEANING: Record<ClientHostState, string> = {
  failed: 'The last lease failed, or a container of the client is down that should be up',
  rolling: 'The host holds a lease: it is being updated or verified',
  held: 'A hold is in effect or wanted',
  overridden: 'A container of the client has an override',
  pending: 'A container of the client is behind its tag, or has an open need',
  current: 'Runs what the tag points at',
};

/** ClientStateBadge is the one state of a host on a client's page. */
export function ClientStateBadge({ state }: { state: ClientHostState }) {
  return (
    <Badge tone={CLIENT_STATE_TONE[state]} title={CLIENT_STATE_MEANING[state]}>
      {CLIENT_STATE_LABEL[state]}
    </Badge>
  );
}

const LEASE_TONE: Record<LeaseState, Tone> = {
  granted: 'info',
  running: 'accent',
  verifying: 'accent',
  released: 'success',
  failed: 'danger',
  expired: 'neutral',
};

export function LeaseStateBadge({ state }: { state: LeaseState }) {
  return <Badge tone={LEASE_TONE[state]}>{state}</Badge>;
}

const STEP_TONE: Record<StepStatus, Tone> = {
  pending: 'neutral',
  running: 'accent',
  done: 'success',
  failed: 'danger',
  superseded: 'neutral',
  refused: 'warning',
  cancelled: 'neutral',
};

export function StepBadge({ status, title }: { status: StepStatus; title?: string }) {
  return (
    <Badge tone={STEP_TONE[status]} title={title}>
      {status}
    </Badge>
  );
}

const STAGE_LABEL: Record<Stage, string> = {
  1: 'repair',
  2: 'canary',
  3: 'rest',
};

/** stageLabel names a fleet.Stage. */
export function stageLabel(stage: Stage): string {
  return STAGE_LABEL[stage];
}

/**
 * ImageCell shows an image as tag + short digest + revision. stacked puts the
 * digest below the tag to keep wide tables narrow.
 */
export function ImageCell({
  image,
  stacked = false,
  compact = false,
}: {
  image: PublicImageRef | undefined;
  stacked?: boolean;
  /** compact is one line: the tag without its registry path and the digest. */
  compact?: boolean;
}) {
  if (!image) return <Dash />;

  if (compact) {
    return (
      <span className="inline-flex items-center gap-2">
        <span className="max-w-32 truncate font-mono text-xs/5 2xl:max-w-56" title={image.image}>
          {displayImage(image.image)}
        </span>
        <span className="text-xs/5">
          <Digest value={image.digest} />
        </span>
      </span>
    );
  }

  const ids = (
    <>
      <Digest value={image.digest} />
      {image.revision && <CopyValue value={image.revision} short={shortRevision(image.revision)} label="revision" />}
    </>
  );

  if (stacked) {
    return (
      <span className="flex flex-col">
        <span className="max-w-40 truncate font-mono" title={image.image}>
          {displayImage(image.image)}
        </span>
        <span className="flex items-center gap-1.5">{ids}</span>
      </span>
    );
  }

  return (
    <span className="inline-flex items-center gap-1.5">
      <span className="font-mono" title={image.image}>
        {image.image}
      </span>
      {ids}
    </span>
  );
}

/** needSummary renders a need as one plain line (used for titles and sorting). */
export function needSummary(need: Need): string {
  switch (need.kind) {
    case 'image':
      return need.image
        ? `image ${need.image.container}: ${shortDigest(need.image.fromDigest) || '?'} → ${shortDigest(need.image.toDigest)}`
        : 'image';
    case 'override':
      return need.override
        ? `override ${need.override.container}: ${need.override.restore ? 'back to' : 'onto'} ${need.override.image}` +
            (need.override.flags ? ', flags change' : '')
        : 'override';
    case 'spec':
      return need.spec
        ? `spec ${need.spec.container}: onto ${need.spec.image}` + (need.spec.flags ? ', command line changes' : '')
        : 'spec';
    case 'packages':
      return `packages: ${list(need.packages?.packages)
        .map(p => `${p.name} ${p.from} → ${p.to}`)
        .join(', ')}`;
    case 'reboot':
      return `reboot (${list(need.reboot?.reasons).join(', ') || 'unknown reason'})`;
  }
}

/** NeedLine renders a need with copyable digests. */
export function NeedLine({ need }: { need: Need }) {
  return (
    <span className="inline-flex flex-wrap items-center gap-1.5">
      <Badge tone={need.kind === 'reboot' ? 'warning' : 'info'}>{need.kind}</Badge>
      {need.kind === 'image' && need.image && (
        <>
          <span className="font-mono">{need.image.container}</span>
          <span className="font-mono text-muted">{need.image.image}</span>
          <Digest value={need.image.fromDigest} />
          <span aria-hidden="true">→</span>
          <Digest value={need.image.toDigest} />
        </>
      )}
      {need.kind === 'override' && need.override && (
        <>
          <span className="font-mono">{need.override.container}</span>
          <span className="text-muted">{need.override.restore ? 'back to' : 'onto'}</span>
          <span className="font-mono text-muted">{need.override.image}</span>
          <Digest value={need.override.toDigest} />
          {need.override.flags && <span className="text-muted">flags change</span>}
        </>
      )}
      {need.kind === 'spec' && need.spec && (
        <>
          <span className="font-mono">{need.spec.container}</span>
          <span className="text-muted">onto agent.yaml's</span>
          <span className="font-mono text-muted">{need.spec.image}</span>
          <Digest value={need.spec.toDigest} />
          {need.spec.flags && <span className="text-muted">command line changes</span>}
        </>
      )}
      {need.kind === 'packages' &&
        list(need.packages?.packages).map(p => (
          <span key={p.name} className="font-mono">
            {p.name} <span className="text-muted">{p.from}</span> → {p.to}
          </span>
        ))}
      {need.kind === 'reboot' && <span>{list(need.reboot?.reasons).join(', ')}</span>}
      {need.directiveId && <Badge title={`directive ${need.directiveId}`}>manual</Badge>}
    </span>
  );
}

export const WORK_LABEL = { image: 'Image roll', packages: 'Package upgrade', reboot: 'Reboot' } as const;
const WORK_DOT = { image: 'bg-series-image', packages: 'bg-series-packages', reboot: 'bg-series-reboot' } as const;

/** WorkTag names what is done to a host, keyed with the timeline's colour. */
export function WorkTag({ kind }: { kind: keyof typeof WORK_LABEL }) {
  return (
    <span className="inline-flex items-center gap-1.5 text-xs/5 whitespace-nowrap text-muted">
      <span aria-hidden="true" className={`size-2 rounded-xs ${WORK_DOT[kind]}`} />
      {WORK_LABEL[kind]}
    </span>
  );
}

/** OfflineBadge says that a host is marked known offline, with why and until when. */
export function OfflineBadge({ mark }: { mark: OfflineMark | undefined }) {
  if (!mark) return null;
  const until = mark.until ? `, until ${new Date(mark.until).toLocaleString()}` : '';
  return (
    <Badge tone="neutral" title={`Known offline${until}: ${mark.reason}. Left out of the gate and the budget.`}>
      known offline
    </Badge>
  );
}
