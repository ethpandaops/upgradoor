// Badges for the domain states, the image cell and need descriptions.

import {
  list,
  type HostState,
  type LeaseState,
  type Need,
  type PublicImageRef,
  type RolloutState,
  type SlotState,
  type Stage,
  type StepStatus,
} from '../api/types';
import { displayImage, shortDigest, shortRevision } from '../lib/format';
import { CopyValue, Digest } from './Digest';
import { Badge, Dash, type Tone } from './primitives';

const SLOT_TONE: Record<SlotState, Tone> = {
  idle: 'neutral',
  queued: 'info',
  leased: 'accent',
  verifying: 'accent',
  failed: 'danger',
  held: 'warning',
  stale: 'warning',
};

export function SlotBadge({ slot }: { slot: SlotState }) {
  return <Badge tone={SLOT_TONE[slot]}>{slot}</Badge>;
}

const ROLLOUT_TONE: Record<RolloutState, Tone> = {
  active: 'info',
  halted: 'danger',
  completed: 'success',
  superseded: 'neutral',
  aborted: 'warning',
};

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

const CUSTOM_BADGE = (
  <Badge tone="warning" title="The image was swapped by hand; this container is never rolled">
    custom
  </Badge>
);

/**
 * ImageCell shows an image as tag + short digest + revision, with a "custom"
 * badge. stacked puts the digest below the tag to keep wide tables narrow.
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
        {image.custom && CUSTOM_BADGE}
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
        <span className="flex items-center gap-1.5">
          {ids}
          {image.custom && CUSTOM_BADGE}
        </span>
      </span>
    );
  }

  return (
    <span className="inline-flex items-center gap-1.5">
      <span className="font-mono" title={image.image}>
        {image.image}
      </span>
      {ids}
      {image.custom && CUSTOM_BADGE}
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

const WORK_LABEL = { image: 'Image roll', packages: 'Package upgrade', reboot: 'Reboot' } as const;
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
