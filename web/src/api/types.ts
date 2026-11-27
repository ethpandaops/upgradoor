// Hand-written mirror of the controller's JSON contract. Every type names the
// Go type it mirrors; field names are the Go json names.
//
// Conventions:
//   - time.Time is an RFC 3339 string; the Go zero time is "0001-01-01T00:00:00Z".
//   - uint64 / int64 / int / float64 are numbers.
//   - A Go slice or map without omitempty can arrive as null (nil slice), so
//     those fields are typed `T[] | null`; read them through list() / dict().
//   - omitempty fields are optional.

/** A Go slice without omitempty: an array, or null for a nil slice. */
type List<T> = T[] | null;
/** A Go map without omitempty. */
type Dict<T> = Record<string, T> | null;

const EMPTY: readonly never[] = Object.freeze([]);

/** list returns the slice, or an empty array for null / undefined. */
export function list<T>(v: readonly T[] | null | undefined): readonly T[] {
  return v ?? EMPTY;
}

/** dict returns the map, or an empty object for null / undefined. */
export function dict<T>(v: Record<string, T> | null | undefined): Record<string, T> {
  return v ?? {};
}

// ---------------------------------------------------------------- protocol

/** protocol.Verb */
type Verb = 'roll' | 'packages' | 'reboot' | 'action';

/** protocol.ActionCondition: when an action is due by itself. */
export type ActionCondition = 'securityUpdates';

/** protocol.ActionInfo: one configured host action as the agent reports it. */
export interface ActionInfo {
  name: string;
  description?: string;
  impact: Impact;
  /** feature is the controller feature that allows starting the action, next to admin; manage when unset. */
  feature?: string;
  /** teams are the teams whose members may start the action. */
  teams?: string[];
  when?: ActionCondition;
  /** due: when holds now. */
  due?: boolean;
}

/** protocol.Hold: the holds the agent reports; it owns them. */
export interface Hold {
  local: boolean;
  localReason?: string;
  /** holds in order: the host's own ("host") first, then staff's, then the teams by name. */
  holds?: List<HostHold>;
}

/** protocol.HostHold: one hold of a host. */
export interface HostHold {
  /** holder is "host" (the local hold), "staff" or the team the hold is of. */
  holder: string;
  by?: string;
  reason?: string;
  at: string;
}

/**
 * protocol.ContainerKind: the io.ethpandaops.upgradoor.kind label, a free-form
 * name; el, cl and vc are the node's clients.
 */
export type ContainerKind = string;

/**
 * protocol.Impact: how the controller schedules work on a container. node (a
 * labelled container): the validator budget, the host limit and the gate;
 * independent (an unlabelled one): a limit of its own.
 */
export type Impact = 'node' | 'independent';

/** protocol.RegistryCheck */
interface RegistryCheck {
  digest?: string;
  revision?: string;
  checkedAt: string;
  error?: string;
  pulled: boolean;
  /** Set after a lease could not pull the image, until the agent lets it be tried again; error says why. */
  pullFailed?: boolean;
}

/** protocol.Container */
export interface Container {
  name: string;
  id: string;
  image: string;
  imageId: string;
  digest?: string;
  revision?: string;
  version?: string;
  state: string;
  exitCode?: number;
  health?: string;
  restartCount: number;
  startedAt: string;
  managed: boolean;
  /** kind is the kind label of a managed container; absent for an unlabelled one. */
  kind?: ContainerKind;
  /**
   * team is the team label of a managed container: the client ("geth",
   * "lighthouse") that owns it; absent for a container without one (staff's).
   */
  team?: string;
  impact?: Impact;
  configuredImage?: string;
  registry?: RegistryCheck;
  /**
   * command is the command line of a managed container: what a flag
   * override changes. This and the two fields below are in a host's details
   * and in the rows of a client's host list, never in the rows of the host
   * table.
   */
  command?: string[];
  /** override is the entry of the host's overrides file for the container, or what is left to undo of one. */
  override?: ContainerOverride;
  /** overridable: image and command line may be overridden (a container with a team, overrides on). */
  overridable?: boolean;
  /** notRolled says why the agent rolls this container to nothing (its updates are still reported). */
  notRolled?: string;
  /** stats is the resource use of a running managed container; in a host's details and a client's host list. */
  stats?: ContainerStats;
}

/** protocol.ContainerStats: one container's resource use, from its cgroup. */
export interface ContainerStats {
  /** Percentage of the host's CPU capacity since the agent's previous reading; absent on its first. */
  cpu?: number;
  /** Bytes in use without the inactive page cache. */
  memory: number;
  /** The memory limit in bytes; absent without one. */
  memoryLimit?: number;
}

/** protocol.Resources: the host's resource use at its last sync. */
export interface Resources {
  cpu: {
    cores: number;
    load1: number;
    load5: number;
    load15: number;
    /** Percentage of CPU time not idle since the agent's previous reading; absent on its first. */
    busy?: number;
  };
  /** Bytes; total is 0 when unknown. */
  memory: { total: number; available: number; swapTotal: number; swapUsed: number };
}

/** api.HostUsage: a host's resource use in whole percents. */
export interface HostUsage {
  cpu?: number;
  memory?: number;
}

/** protocol.ContainerOverride: the override of one managed container, as the host's overrides file states it. */
export interface ContainerOverride {
  /** The image reference the container runs instead of the configured one. */
  image?: string;
  addFlags?: string[];
  removeFlags?: string[];
  /** Who set the override and why, as the file says. */
  by?: string;
  reason?: string;
  /** When the override ends; absent for one without an end. */
  until?: string;
  /** The container runs what the override asks for. */
  applied: boolean;
  /** The entry is gone and the container still runs it: it waits for a lease to be put back. */
  restoring?: boolean;
  /** Why the override cannot be applied as written. */
  error?: string;
}

/** protocol.ELHealth */
export interface ELHealth {
  reachable: boolean;
  syncing: boolean;
  peers: number;
  head: number;
  error?: string;
}

/** protocol.CLHealth */
export interface CLHealth {
  reachable: boolean;
  syncing: boolean;
  optimistic: boolean;
  elOffline: boolean;
  peers: number;
  headSlot: number;
  syncDistance: number;
  error?: string;
}

/** protocol.KindHealth: the HTTP probe of one kind; a 2xx answer is healthy. */
export interface KindHealth {
  kind: ContainerKind;
  healthy: boolean;
  error?: string;
}

/** protocol.NodeHealth */
interface NodeHealth {
  el?: ELHealth;
  cl?: CLHealth;
  /** The probes of the other kinds, sorted by kind. */
  probes?: KindHealth[];
}

/** protocol.OSSummary */
interface OSSummary {
  id: string;
  versionId: string;
  prettyName: string;
  arch: string;
  runningKernel: string;
  newestKernel: string;
  rebootRequired: boolean;
  rebootPackages?: string[];
}

/** protocol.Pending */
export interface Pending {
  upgradable: number;
  security: number;
  blocked: number;
}

/** protocol.NeedKind */
export type NeedKind = 'image' | 'override' | 'spec' | 'packages' | 'reboot' | 'action';

/** protocol.ImageNeed */
interface ImageNeed {
  container: string;
  /** The kind and team of the container: the needs of one team and kind are one rollout. */
  kind?: ContainerKind;
  team?: string;
  image: string;
  fromDigest?: string;
  toDigest: string;
  toRevision?: string;
}

/** protocol.OverrideNeed: recreate one container so that it runs what the host's overrides file says. */
interface OverrideNeed {
  container: string;
  /** The reference the container is recreated from. */
  image: string;
  fromImage?: string;
  fromDigest?: string;
  toDigest: string;
  toRevision?: string;
  /** The command line changes. */
  flags?: boolean;
  /** The need takes an override away. */
  restore?: boolean;
}

/**
 * protocol.SpecNeed: recreate one container with the image and command line
 * agent.yaml names now (the soft deploy of an Ansible change), overrides
 * applied. Hosts with the same key share one rollout.
 */
interface SpecNeed {
  container: string;
  /** The kind and team of the container: the needs of one team and kind are one rollout. */
  kind?: ContainerKind;
  team?: string;
  key: string;
  image: string;
  fromImage?: string;
  fromDigest?: string;
  toDigest: string;
  toRevision?: string;
  /** The command line changes. */
  flags?: boolean;
}

/** protocol.PackageChange */
interface PackageChange {
  name: string;
  from: string;
  to: string;
}

/** protocol.PackagesNeed */
interface PackagesNeed {
  packages: List<PackageChange>;
}

/** protocol.RebootNeed */
interface RebootNeed {
  reasons: List<string>;
}

/** protocol.ActionNeed: run one configured action. */
interface ActionNeed {
  name: string;
  impact: Impact;
  /** when is the condition that made the need; absent for a person's start. */
  when?: ActionCondition;
}

/** protocol.Need */
export interface Need {
  id: string;
  kind: NeedKind;
  directiveId?: string;
  /** local: the need was asked for on the host itself (every override need, a roll from the local command line). */
  local?: boolean;
  /** Why the last lease could not pull the need's image; set until the agent lets the pull be tried again. A need with it is not granted. */
  pullError?: string;
  image?: ImageNeed;
  override?: OverrideNeed;
  spec?: SpecNeed;
  packages?: PackagesNeed;
  reboot?: RebootNeed;
  action?: ActionNeed;
}

/** protocol.Phase. `pulling`: the images are pulled first; nothing is stopped yet. */
export type Phase =
  | 'pulling'
  | 'draining'
  | 'applying'
  | 'rebooting'
  | 'starting'
  | 'action'
  | 'verifying'
  | 'done'
  | 'failed';

/**
 * protocol.StepStatus. `cancelled`: the lease was cancelled before the step
 * had begun; nothing was disrupted for it and it is not a failure.
 * `deferred`: an image of the lease could not be pulled, so nothing was
 * begun; not a failure, the need stays open.
 */
export type StepStatus =
  | 'pending'
  | 'running'
  | 'done'
  | 'failed'
  | 'superseded'
  | 'refused'
  | 'deferred'
  | 'cancelled';

/** protocol.ActionResult: how one action run ended. */
export interface ActionRun {
  exitCode: number;
  /** output is the tail of the output, at most 16 KiB. */
  output?: string;
  timedOut?: boolean;
  /** unknown: the agent restarted while the action ran; the outcome is not known. */
  unknown?: boolean;
}

/** protocol.Step */
export interface Step {
  needId: string;
  status: StepStatus;
  error?: string;
  action?: ActionRun;
}

/** protocol.Operation */
interface Operation {
  leaseId: string;
  phase: Phase;
  startedAt: string;
  updatedAt: string;
  error?: string;
  steps: List<Step>;
  disrupted: boolean;
  /** The image pull in flight while the phase is pulling; older agents omit it. */
  pull?: PullProgress;
}

/**
 * protocol.PullProgress: how far one image pull got. The totals are those of
 * the layers the daemon has seen so far.
 */
export interface PullProgress {
  image?: string;
  /** Counts from 1; a stalled or transiently failed pull is tried again within the lease. */
  attempt?: number;
  bytesDone?: number;
  bytesTotal?: number;
  layersDone?: number;
  layersTotal?: number;
  /** The start of the first attempt. */
  startedAt?: string;
  /** When the daemon last reported progress. */
  lastProgressAt?: string;
}

/** protocol.OSInfo */
interface OSInfo {
  id: string;
  versionId: string;
  codename: string;
  prettyName: string;
  arch: string;
}

/** protocol.KernelInfo */
interface KernelInfo {
  running: string;
  newest: string;
  installed: List<string>;
}

/** protocol.Package */
export interface Package {
  name: string;
  version: string;
  arch: string;
}

/** protocol.Upgradable */
export interface Upgradable {
  name: string;
  arch: string;
  current: string;
  candidate: string;
  origin: string;
  security: boolean;
  blocked: boolean;
}

/** protocol.Inventory */
export interface Inventory {
  hash: string;
  collectedAt: string;
  os: OSInfo;
  kernels: KernelInfo;
  packages: List<Package>;
  upgradable: List<Upgradable>;
}

// ------------------------------------------------------------------- fleet

/** fleet.NetworkHealth */
/** fleet.SlotPhase: a stretch of the chain with one slot duration. */
interface SlotPhase {
  /** The first epoch of the phase and its start. */
  epoch: number;
  start: string;
  slotMs: number;
}

export interface NetworkHealth {
  updatedAt: string;
  error?: string;
  genesisTime: string;
  /** The current slot duration rounded down to whole seconds, for display. Epoch times come from phases. */
  secondsPerSlot: number;
  slotsPerEpoch: number;
  /** The chain's slot durations; a fork that changes the duration starts a new phase. */
  phases?: List<SlotPhase>;
  /**
   * Why the beacon node's chain timing is invalid as a whole. The network gate
   * is then closed with it as the reason ("invalid chain timing: ...").
   */
  timingError?: string;
  headSlot: number;
  currentEpoch: number;
  finalizedEpoch: number;
  finalityLagEpochs: number;
  /**
   * How the in-sync beacon endpoints divide over chains when no chain has a
   * strict majority ("2 chains, 3/5 and 2/5 endpoints"); the gate is closed while set.
   */
  split?: string;
  livenessKnown: boolean;
  livenessEpoch: number;
  /** Why the beacon service's last count of the per-slot data failed, if it did. */
  livenessError?: string;
  tracked: number;
  live: number;
  /** Why the last fetch of the validator ranges inventory failed; the last good version stays in use. */
  validatorRangesError?: string;
}

/**
 * fleet.RolloutKind. `group` moves the containers of one kind of one team (of
 * the kind alone without a team, of one image when unlabelled) onto what the
 * hosts' image and spec needs of it ask for. `local` is a rollout of one host for what was asked for
 * on the host itself (an override, a roll from its command line): it has one
 * member, answers to no policy and has no canary.
 */
export type RolloutKind = 'group' | 'packages' | 'reboot' | 'manual' | 'local' | 'action';

/** fleet.RolloutState */
export type RolloutState = 'active' | 'halted' | 'completed' | 'superseded' | 'aborted';

/** fleet.HostState */
export type HostState = 'expected' | 'pending' | 'leased' | 'done' | 'failed' | 'cancelled' | 'skipped' | 'aborted';

/**
 * fleet.Stage, in serving order: 1 repair (a member whose host is broken for
 * what the rollout is about, the image or the packages: HostSummary.broken;
 * it is served first), 2 the canary (the one host that goes alone until the
 * first success), 3 the rest. The scheduler derives it from the hosts' marks
 * and the rollout's canary in every reconcile.
 */
export type Stage = 1 | 2 | 3;

/** fleet.RolloutHost */
export interface RolloutHost {
  host: string;
  state: HostState;
  stage: Stage;
  needId?: string;
  /**
   * The lease the member holds or was served by. A member of an aborted
   * rollout gets the outcome of its lease; one that held a lease when its
   * rollout closed otherwise stays leased with it, and the lease says how it
   * ended.
   */
  leaseId?: string;
  /** The person who skipped the member; absent on a member the scheduler skipped (its host was forgotten). */
  skippedBy?: string;
  /** The failure reason, or the reason given with a skip. */
  error?: string;
  /** noWorse marks a member done through a lease released as no worse than before it: no success of the rollout. */
  noWorse?: boolean;
  updatedAt: string;
}

/** fleet.RolloutTarget: one part of what a group rollout moves its containers to. */
export interface RolloutTarget {
  /** The tag reference, and the digest and revision it moves to. */
  image: string;
  digest: string;
  revision?: string;
  /** The key of a soft deploy (SpecNeed.key); absent for a tag that moved. */
  spec?: string;
  /** The container a soft deploy was first reported for. */
  container?: string;
  /** The soft deploy changes the command line. */
  flags?: boolean;
}

/** views.RolloutView: a fleet.Rollout with its policy. */
export interface Rollout {
  id: string;
  kind: RolloutKind;
  /** unit:<kind>[#<team>]@<hash of the targets> (unit:/<image>@<hash> for unlabelled containers), packages:<key>, reboot, manual:<id>, local:<host>:<hash>. */
  key: string;
  title: string;
  /** The kind and team a group rollout moves (no kind: unlabelled containers; no team: staff's, answering to policyImage), and what it moves them to, sorted. */
  containerKind?: ContainerKind;
  team?: string;
  targets?: List<RolloutTarget>;
  packages?: PackageChange[];
  /** action is the host action of an action rollout. */
  action?: string;
  /** pauseAfterCanary pauses the rollout at its first success, next to the settings of its kind. */
  pauseAfterCanary?: boolean;
  state: RolloutState;
  reason?: string;
  paused: boolean;
  /**
   * Why the rollout is paused, when there is more to say than that a person
   * paused it: the scheduler paused it at its first success ("paused after
   * the canary; resume to continue"), or a person aborted a rollout of an
   * observed cause, which stays open while hosts report the cause
   * ("aborted: <reason>; resume to continue"). Empty once it is resumed.
   */
  pauseReason?: string;
  /** approved: a person approved the rollout (manual policy, or an auto one at a closed gate). */
  approved: boolean;
  force: boolean;
  /** ignoreBudget: the rollout skips the budget and the network gate; host limit and order still apply. */
  ignoreBudget: boolean;
  /** ignoreGate: a person let the rollout pass a closed network gate; budget, host limit and order still apply. */
  ignoreGate: boolean;
  canary?: string;
  createdBy: string;
  createdAt: string;
  updatedAt: string;
  endedAt?: string;
  hosts: List<RolloutHost>;
  /** views.RolloutView: the policy the rollout answers to (fleet.Settings.RolloutPolicy); manual waits for approval. */
  policy: Policy;
  /** What decided the policy of an image rollout; absent for every other rollout. */
  policySource?: PolicySource;
}

/** fleet.PolicySource: policyClients, policyKinds, policyImage, or deploy: every target is an Ansible deploy (always auto). */
export type PolicySource = 'client' | 'kind' | 'default' | 'deploy';

/** fleet.LeaseState */
export type LeaseState = 'granted' | 'running' | 'verifying' | 'released' | 'failed' | 'expired' | 'cancelled';

/**
 * fleet.LeaseCancel: the mark of a lease that a person ended while it was
 * open (the end-lease action). The lease stays open and keeps its slot until
 * its agent reports, and then ends as cancelled (failed when the agent goes silent).
 */
interface LeaseCancel {
  at: string;
  /** The person, as recorded in the audit log. */
  by: string;
  /** The state the lease was in when it was cancelled. For people: no rule reads it. */
  state: LeaseState;
  reason?: string;
}

/**
 * fleet.LeaseApproval: the mark of a verifying lease a person approved (the
 * approve-lease action); the next reconcile releases it as ready.
 */
interface LeaseApproval {
  at: string;
  by: string;
  reason?: string;
}

/** fleet.PeerCheck: one client's peer count held against the fleet in the readiness check of a lease. */
export interface PeerCheck {
  /** "EL" or "CL". */
  layer: string;
  /** What the host's client reported. */
  peers: number;
  /** The median of the reference hosts. */
  median: number;
  /** The share of the median that the host must have. */
  needs: number;
}

/**
 * fleet.LeaseOutcome: how the readiness check of a lease whose host was not
 * ready before it ended. recovered: the host passed the full readiness check;
 * no-worse: released after baselineGrace without passing it, no signal worse
 * than in the baseline; worse: failed with a signal worse than in the baseline.
 */
export type LeaseOutcome = 'recovered' | 'no-worse' | 'worse';

/** fleet.BaselineContainer: one managed container in a LeaseBaseline. */
interface BaselineContainer {
  name: string;
  kind?: ContainerKind;
  state: string;
  restartCount: number;
}

/** fleet.LeaseBaseline: the host's state at grant time, from its last sync. */
export interface LeaseBaseline {
  /** The time of that sync. */
  at: string;
  node: NodeHealth;
  /** The host's validators with a duty and those that attested in the newest liveness epoch. */
  duty: number;
  live: number;
  liveKnown: boolean;
  /** The managed containers. */
  containers?: BaselineContainer[];
  /** Which client had fewer peers than the fleet allows. */
  belowFleet?: string;
  /** Why the host was not ready; empty when it was. */
  unready?: string;
  /** The component the unreadiness starts at: el, cl, a probed kind or vc; empty when none. */
  root?: ContainerKind;
}

/** fleet.Lease */
export interface Lease {
  id: string;
  host: string;
  state: LeaseState;
  needs: List<Need>;
  rollouts: Dict<string>;
  weight: number;
  /** The validators that attested when the lease was granted: what it cost the budget. For display. */
  charged: number;
  /** The class of the work: node, slot or independent. */
  impact?: Impact;
  force: boolean;
  grantedBy: string;
  grantedAt: string;
  expiresAt: string;
  agentDoneAt?: string;
  endedAt?: string;
  operation?: Operation;
  readiness?: string;
  /** When the node check passes if the host stays at head with peers; set only while the readiness note waits for that. */
  readyAt?: string;
  /** The comparison with the fleet that the last readiness check made, per client it applied to. */
  peerChecks?: PeerCheck[];
  error?: string;
  /**
   * Set once a person ended the lease. While the lease is open the agent is
   * told with every sync and stops after the step in progress.
   */
  cancelled?: LeaseCancel;
  /** Set once a person approved the verifying lease. */
  approved?: LeaseApproval;
  /**
   * recheck marks a lease that only repeats the readiness check of a failed
   * lease whose steps the agent did, after a retry; the agent is not offered it.
   */
  recheck?: boolean;
  /**
   * The host's state at grant time; absent when its last sync was too old. A
   * host that was not ready then is held to no worse than that once
   * baselineGrace has passed.
   */
  baseline?: LeaseBaseline;
  /** The run of syncs after the done report in which no signal was worse than in the baseline. */
  noWorseSince?: string;
  noWorseUntil?: string;
  /** Set when a lease whose host was not ready before it ends. */
  outcome?: LeaseOutcome;
}

/** fleet.AuditEntry */
export interface AuditEntry {
  id: number;
  time: string;
  actor: string;
  action: string;
  target?: string;
  detail?: Record<string, unknown>;
}

/** fleet.Policy */
export type Policy = 'auto' | 'manual';

/** fleet.Settings; durations are Go duration strings ("30m0s"). */
export interface Settings {
  maxUnavailablePercent: number;
  maxLeasedPercent: number;
  maxIndependentPercent: number;
  minParticipationPercent: number;
  maxFinalityLagEpochs: number;
  /** Distinct slots after its operation for which a host's validators must have attestations included; fewer for a host with fewer validators. */
  readyAttestations: number;
  /** How long the node of a host without validators must stay at head with peers (Go duration); "0s" is its first healthy sync. */
  readyStableFor: string;
  minPeers: number;
  leaseTtl: string;
  hostStaleAfter: string;
  readinessTimeout: string;
  /**
   * How long after its operation a host that was not ready before its lease
   * waits for the full readiness check before it is released as no worse than
   * before (Go duration). Between readyStableFor and readinessTimeout.
   */
  baselineGrace: string;
  /** The leases with image work that are granted or pulling at once; 0 is no bound. */
  maxConcurrentPulls: number;
  policyImage: Policy;
  policyPackages: Policy;
  policyReboot: Policy;
  /**
   * The image policy per client ("geth": "manual"); always an object. The
   * image rollouts of a client that is listed answer to its entry, the
   * others to policyKinds, then policyImage. It is changed with the
   * client-policy action.
   */
  policyClients: Record<string, Policy>;
  /** The image policy per container kind ("cl": "manual", "other" for unlabelled containers); always an object. A kind that is not listed follows policyImage. */
  policyKinds: Record<string, Policy>;
  /** The policy per host action; always an object. An action that is not listed is manual. */
  actionPolicies: Record<string, Policy>;
  /** Pause a rollout of the kind when its first host is ready, until a person resumes it. */
  pauseAfterCanaryImage: boolean;
  pauseAfterCanaryPackages: boolean;
  pauseAfterCanaryReboot: boolean;
  /** Share (0–1) of the fleet's median peer count a client must have before a slot is released; 0, the default, is off. */
  readyPeerShare: number;
  /** How often the controller looks a tag up at its registry (Go duration). */
  lookupInterval: string;
  paused: boolean;
}

// --------------------------------------------------------------- scheduler

/** scheduler.RollRequest */
export interface RollRequest {
  hosts: string[];
  all: boolean;
  containers: string[];
  staggered: boolean;
  force: boolean;
  ignoreBudget: boolean;
  /** ignoreGate passes a closed network gate; budget, host limit and order still apply. */
  ignoreGate?: boolean;
  reason: string;
}

/** scheduler.RebootRequest */
export interface RebootRequest {
  hosts: string[];
  all: boolean;
  staggered: boolean;
  force: boolean;
  ignoreBudget: boolean;
  ignoreGate?: boolean;
  reason: string;
}

/** fleet.OfflineMark: a person's mark that a host is known offline. */
export interface OfflineMark {
  reason: string;
  /** until is when the mark ends by itself; absent: until a person lifts it. */
  until?: string;
}

/** scheduler.OfflineRequest: the body of mark-offline and unmark-offline. */
export interface OfflineRequest {
  hosts: string[];
  reason: string;
  until?: string;
}

/** scheduler.HoldRequest: the body of hold, unhold and forget. */
export interface HoldRequest {
  hosts: string[];
  reason: string;
  /** holders names the holds an unhold lifts ("staff" or teams); absent: the holds the login sets. */
  holders?: string[];
}

/**
 * scheduler.EndLeaseRequest: the body of end-lease. The action is a request
 * to cancel an open lease (granted, running or verifying): the lease is
 * marked (Lease.cancelled) and stays open until the agent reports or the
 * lease runs out, and then ends as cancelled. No hold is set. Ending a lease
 * that is already cancelled changes nothing and succeeds; a lease that has
 * ended is answered 409.
 */
export interface EndLeaseRequest {
  lease: string;
  reason: string;
}

/** scheduler.ClientPolicyRequest: the body of client-policy. An empty policy returns the client to policyImage. */
export interface ClientPolicyRequest {
  client: string;
  policy: Policy | '';
  reason: string;
}

/**
 * scheduler.ImageOverrideRequest: the body of image-override. An empty image
 * clears the image override. until is when the override ends (RFC 3339, at
 * most 7 days from now; the controller's default is 24 hours); the entry has
 * one end for image and flags together.
 */
export interface ImageOverrideRequest {
  host: string;
  container: string;
  image: string;
  until?: string;
  reason: string;
}

/** scheduler.FlagOverrideRequest: the body of flag-override. Both lists empty clears the flag override. */
export interface FlagOverrideRequest {
  host: string;
  container: string;
  addFlags: string[];
  removeFlags: string[];
  until?: string;
  reason: string;
}

/**
 * scheduler.ClientOverrideRequest: the body of client-override. One part of an
 * override for the container of one kind of a client on each named host; an
 * empty image, or no flags, clears that part. Hosts the login may not act
 * on, or that cannot take it, come back in ActionResult.rejected.
 */
export interface ClientOverrideRequest {
  client: string;
  kind: ContainerKind;
  hosts: string[];
  part: 'image' | 'flags';
  image?: string;
  addFlags?: string[];
  removeFlags?: string[];
  until?: string;
  reason: string;
}

/** scheduler.ScopeRequest: the body of pause, resume, abort, ignore-gate and ignore-budget. */
export interface ScopeRequest {
  rollout: string;
  reason: string;
}

/** scheduler.ApproveLeaseRequest: the body of approve-lease, the verifying lease of one member of a rollout. */
export interface ApproveLeaseRequest {
  rollout: string;
  host: string;
  reason: string;
}

/** scheduler.RolloutHostsRequest: the body of skip and retry. */
export interface RolloutHostsRequest {
  rollout: string;
  hosts: string[];
  reason: string;
}

/**
 * scheduler.HostActionRequest: the body of host-action. Exactly one of hosts,
 * client and all selects the hosts; named hosts the login may not start the
 * action on come back in ActionResult.rejected.
 */
export interface HostActionRequest {
  action: string;
  hosts?: string[];
  client?: string;
  all?: boolean;
  pauseAfterCanary?: boolean;
  ignoreGate?: boolean;
  reason: string;
}

/** scheduler.ActionResult */
export interface ActionResult {
  message: string;
  rollout?: string;
  hosts?: string[];
  rejected?: Record<string, string>;
}

/** scheduler.Gate */
export interface Gate {
  ok: boolean;
  reasons: List<string>;
  network: NetworkHealth;
}

/** scheduler.Budget */
export interface Budget {
  totalWeight: number;
  allowedWeight: number;
  unavailableWeight: number;
  leasedHosts: number;
  maxLeasedHosts: number;
  /** Hosts whose lease is about independent containers alone, and their limit. */
  independentHosts?: number;
  maxIndependent?: number;
  /** Hosts marked known offline and their validators: left out of the budget and the gate. */
  offlineHosts?: number;
  offlineWeight?: number;
}

/**
 * scheduler.WaitKind: the class of a waiting reason. budget, host-limit,
 * pulls (maxConcurrentPulls leases with image work are granted or pulling),
 * order and pull (the host waits to pull an image again after a failed pull)
 * clear by themselves; working clears when the operation the host works on
 * without a lease is over; gate clears when the network recovers; the rest
 * needs somebody to act.
 */
export type WaitKind =
  | 'budget'
  | 'host-limit'
  | 'pulls'
  | 'order'
  | 'gate'
  | 'verb'
  | 'pull'
  | 'halted'
  | 'approval'
  | 'rollout-paused'
  | 'paused'
  | 'silent'
  | 'working'
  | 'held';

/** scheduler.QueueEntry */
export interface QueueEntry {
  host: string;
  position: number;
  reason: string;
  wait: WaitKind;
  /**
   * The host an order wait is for: a member whose host is broken, or the canary. It need not be in the queue: it may
   * hold a lease, or be expected to report.
   */
  after?: string;
  /**
   * The rollout the reason is about (the paused, halted or unapproved one, the
   * one whose order the host waits in, or whose node work waits at the gate or
   * for the budget); absent when the reason is about the host or the fleet.
   */
  rollout?: string;
  rollouts: List<string>;
}

/** scheduler.SlotState */
type SlotState = 'idle' | 'queued' | 'leased' | 'verifying' | 'failed' | 'held' | 'offline' | 'stale';

/** scheduler.HostPlan */
export interface HostPlan {
  slot: SlotState;
  weight: number;
  live: number;
  liveKnown: boolean;
  leaseId?: string;
  reason?: string;
  /** What a host without a lease is not ready for by its last sync ("EL is not reachable"). */
  notReady?: string;
  /** holdRequests are the hold and release-hold directives the host has not answered yet, oldest first. */
  holdRequests?: List<PendingHold>;
}

/** scheduler.PendingHold: a hold or release-hold directive that waits for the host. */
export interface PendingHold {
  holder: string;
  release?: boolean;
  by?: string;
  reason?: string;
  at: string;
}

/** scheduler.Plan */
export interface Plan {
  computedAt: string;
  paused: boolean;
  gate: Gate;
  budget: Budget;
  queue: List<QueueEntry>;
  hosts: Dict<HostPlan>;
}

// ------------------------------------------------------------------- store

/** store.PackageVersion */
interface PackageVersion {
  version: string;
  hosts: List<string>;
}

/** store.PackageMatch */
export interface PackageMatch {
  name: string;
  versions: List<PackageVersion>;
}

// --------------------------------------------------------------------- api

/** api.ErrorBody */
export interface ErrorBody {
  error: string;
}

/** api.Config */
export interface Config {
  network: string;
  version: string;
  authProviderUrl: string;
  /** authService is the controller's name in a token's "services" claim. */
  authService: string;
  /** uiScript is a script the UI includes (e.g. a site menu), with its integrity hash. */
  uiScript?: string;
  uiScriptIntegrity?: string;
  /** The minute of the hour at which the image builder's scheduled run starts; absent when it has no such schedule. */
  buildMinute?: number;
}

/** api.PublicImageRef */
export interface PublicImageRef {
  image: string;
  digest?: string;
  revision?: string;
}

/** api.PublicHost */
export interface PublicHost {
  name: string;
  el?: PublicImageRef;
  cl?: PublicImageRef;
  updatePending: boolean;
}

/** api.PublicHosts */
export interface PublicHosts {
  hosts: List<PublicHost>;
}

/**
 * upstream.Status: how the image of a tag stands against the branch it is
 * built from.
 */
export interface ImageSource {
  /** The GitHub repository (owner/name) and the branch or tag the image is built from. */
  repo: string;
  ref: string;
  /** The commit the image the tag points at was built from; absent when that is not known. */
  commit?: string;
  /** The commit the branch is at; absent before the first answer and when the repository has no such ref. */
  head?: string;
  /** The branch is ahead of the image: a build is due. */
  pending?: boolean;
  /** Why the newest check failed; the rest is of the last good one. */
  error?: string;
}

/** api.PublicImage */
export interface PublicImage {
  image: string;
  kind: ContainerKind;
  digest?: string;
  revision?: string;
  hostsOn: List<string>;
  hostsBehind: List<string>;
  /** Absent for an image whose source is not known. */
  source?: ImageSource;
  /** The commit the images at digest and at the digests the hosts run were built from, by digest; unknown ones are left out. */
  commits?: Dict<string>;
}

/** api.PublicImages */
export interface PublicImages {
  images: List<PublicImage>;
}

/**
 * api.SlotHistory: what the tracked validators did slot by slot over a window
 * that ends now. Columnar: point i of every list covers the bucketSlots slots
 * from firstSlot + i * bucketSlots on; the last point is the bucket the clock
 * is in.
 */
export interface SlotHistory {
  firstSlot: number;
  /** When firstSlot began. */
  firstTime: string;
  /** The slot duration in milliseconds; 0 while the chain's timing is unknown. */
  slotMs: number;
  slotsPerEpoch: number;
  /** Slots per point: 1 for a short window, a power of two for a long one. */
  bucketSlots: number;
  minParticipationPercent: number;
  epochSeconds: number;
  /** Stored slots per point; 0 means nothing is known about the point. */
  recorded: List<number>;
  /** Recorded slots without a block. */
  missed: List<number>;
  /** Tracked validators with an attestation duty, and those whose attestation a block carries. */
  assigned: List<number>;
  included: List<number>;
  /** The counts of the point's slot with the lowest share included; absent when a point is one slot. */
  worstAssigned?: List<number>;
  worstIncluded?: List<number>;
  /**
   * The spans of the window during which hosts were off; absent without a
   * login. Bounded: with many, spans of one kind that overlap stand as one
   * span for several hosts.
   */
  outages?: List<Outage>;
  /** How many different hosts were off in the window, whatever outages was merged to. */
  outageHosts?: number;
  /** In how many slots a host must have missed attestations, no further than an epoch apart, to count as off. */
  offSlots?: number;
}

/** api.Outage: a span during which a host was off, or several hosts when spans of a kind were merged. */
export interface Outage {
  /** How many hosts the span stands for, and up to eight of their names. */
  hosts: number;
  names: List<string>;
  from: string;
  to: string;
  /** The host was already off when the window began. */
  fromStart?: boolean;
  /** It is still off: `to` is the last moment known of. */
  open?: boolean;
  /** The lease that explains the span, when it stands for one host. */
  lease?: string;
  /** What the lease does; absent for a span no lease explains. */
  work?: 'image' | 'packages' | 'action' | 'reboot' | 'mixed' | (string & {});
  rollout?: string;
  /** The lease failed or expired. */
  failed?: boolean;
  /** The lease applies a deploy on the host (upgradoor deploy, e.g. from Ansible). */
  deployed?: boolean;
  /** Attestations missed, and due, in the slots of the span in which the hosts missed some. */
  missed: number;
  assigned: number;
  /** How many validators the hosts run. */
  validators: number;
}

/** fleet.SlotParticipation: what the tracked validators did in one slot. */
export interface SlotRow {
  slot: number;
  /** false when the slot has no canonical block. */
  proposed: boolean;
  assigned: number;
  included: number;
  /** The hosts that missed attestations in the slot, with their duties; absent when none did, and without a login. */
  hosts?: { host: string; assigned: number; included: number }[];
}

/** The slots an event stream carries: the current epoch and the two before it, oldest first. */
interface SlotRows {
  rows: SlotRow[];
}

/** api.ContainerListing: a container as the host list shows it. */
export type ContainerListing = Pick<
  Container,
  'name' | 'image' | 'digest' | 'revision' | 'state' | 'managed' | 'kind' | 'impact' | 'team'
>;

/**
 * api.HostListing: one row of the host list, what the overview, the host
 * table and the selection need. The rest of a host is in its details
 * (HostSummary, HostDetail).
 */
export interface HostListing {
  name: string;
  roles: List<string>;
  /** The number of validators counted for the host. */
  validators: number;
  lastSeen: string;
  stale: boolean;
  agentVersion: string;
  /** The protocol version of the agent's last sync; 0 before the first. */
  agentProtocol: number;
  /** Why the controller serves the host nothing: its agent speaks a protocol version it does not support. Absent when served. */
  incompatible?: string;
  containers: List<ContainerListing>;
  os: OSSummary;
  pending: Pending;
  uptimeSeconds: number;
  /** The resource use in brief; absent when unknown and for a login without the full view of the host. */
  usage?: HostUsage;
  /** When the host booted. Only a stream sets it (the uptime then follows from the clock); the REST view has uptimeSeconds. */
  bootTime?: string;
  hold: Hold;
  /** holders are the holders of hold.holds: "host" first, then "staff", then the teams. */
  holders: List<string>;
  /** offline is the known-offline mark while it is in force. */
  offline?: OfflineMark;
  verbs: List<Verb>;
  /** The host actions the agent reports; always an array. */
  actions: List<ActionInfo>;
  /** The kinds of the host's open needs, one entry per need. */
  needs: List<Pick<Need, 'kind'>>;
  plan: HostPlan;
}

/** api.HostSummary: a host in full, as its details carry it. */
export interface HostSummary extends HostListing {
  containers: List<Container>;
  node: NodeHealth;
  /**
   * broken says what a lease of the host failed on: per group of rollouts
   * ("image:<reference>", "packages") the ID of the lease that failed. The
   * mark is set when a lease fails, for every group it carried a step of, and
   * cleared for a group when a lease is released with that group's steps
   * done. Such a host goes first in a rollout of the group. Left out when the
   * host is in order.
   */
  broken?: Record<string, string>;
  needs: List<Need>;
  /** The resource use by the last sync; absent before the first and for a login without the full view. */
  resources?: Resources;
}

/** api.Hosts */
export interface Hosts {
  hosts: List<HostListing>;
}

/** api.HostDetail */
export interface HostDetail extends HostSummary {
  certHash: string;
  firstSeen: string;
  remoteAddr: string;
  /** The validators the validator ranges inventory assigns to the host, active or not. */
  assignedValidators: number;
  inventory?: Inventory;
  leases: List<Lease>;
  history: List<AuditEntry>;
  /**
   * reduced: the login lacks the feature details for this host. The system, the packages, the unmanaged
   * containers, the internals of other teams' containers, the identity and the host actions are left out.
   */
  reduced?: boolean;
}

/** api.ClientLayer: which row of the Clients tab a client belongs to. */
export type ClientLayer = 'consensus' | 'execution';

/** api.ClientHostState: the one state a host has on a client's page; where several apply, the first of this list. */
export type ClientHostState = 'failed' | 'rolling' | 'held' | 'overridden' | 'pending' | 'current';

/** CLIENT_HOST_STATES lists the states in their order of precedence. */
export const CLIENT_HOST_STATES: readonly ClientHostState[] = [
  'failed',
  'rolling',
  'held',
  'overridden',
  'pending',
  'current',
];

/** api.ClientImage: one image reference the containers of a client follow. */
export interface ClientImage {
  kind: ContainerKind;
  image: string;
  /** What the tag points at, by the freshest registry check; absent when no host has looked yet. */
  digest?: string;
  revision?: string;
  /** Hosts with a container on the reference, and how many of them run another digest. */
  hosts: number;
  behind: number;
  /** The command line most of these containers report without a flag override, and how many hosts report exactly it. */
  command?: List<string>;
  commandHosts: number;
  /** The arguments more than half of those hosts report; one that differs on every host is not in it. */
  commonArgs?: List<string>;
}

/** api.ClientView: one client, a row of GET /api/v1/clients and of the stream's client table. */
export interface ClientView {
  name: string;
  layer: ClientLayer;
  /** The client's own image policy; policySet is false and policy empty when it has none. */
  policy: Policy | '';
  policySet: boolean;
  /** What the image rollouts of each kind of the client answer to without a policy of its own: policyKinds, else policyImage. */
  defaultPolicies: Record<ContainerKind, Policy>;
  images: List<ClientImage>;
  /** The hosts that run the client; the counts below are those hosts by their state and add up to it. */
  hosts: number;
  current: number;
  pending: number;
  rolling: number;
  overridden: number;
  held: number;
  failed: number;
  /** Hosts on which a container of the client has an image override, a flag override, whatever their state. */
  imageOverrides: number;
  flagOverrides: number;
  /** version changes whenever anything in the client's host list changes. */
  version: string;
}

/** api.Clients */
export interface Clients {
  clients: List<ClientView>;
}

/** api.ClientHost: one host on a client's page. */
export interface ClientHost {
  host: string;
  state: ClientHostState;
  /** More about the state: why the host waits, what failed, why it is held. */
  reason?: string;
  /** The agent is silent; everything else is its last report. */
  stale?: boolean;
  slot: SlotState;
  /** The host's containers of this client, with client, command, override and overridable. */
  containers: List<Container>;
  /** The host's resource use, for a login with the full view of the host. */
  usage?: HostUsage;
}

/** api.ClientHosts: one page of GET /api/v1/clients/{client}/hosts, ordered by host name. */
export interface ClientHosts {
  client: string;
  /** ClientView.version of the list the page was cut from. */
  version: string;
  /** The hosts of the client, and how many of them the filter selects. */
  total: number;
  matched: number;
  offset: number;
  limit: number;
  hosts: List<ClientHost>;
  /** With names=true: the names of every matched host (at most 2000) instead of a page. */
  names?: List<string>;
}

/** api.Packages */
export interface Packages {
  query: string;
  matches: List<PackageMatch>;
  /** The number of package names the query selects; on the first page only. */
  total?: number;
  /** Continues after this page (the `after` parameter); absent on the last page. */
  next?: string;
}

/** views.ImageVersion: one digest of a reference; digest is empty for an image without a registry digest. */
interface ImageVersion {
  digest: string;
  revision?: string;
  hosts: List<string>;
}

/** views.ImageLookup: where a tag points by the controller's newest lookup; with an error, digest is the last good one. */
interface ImageLookup {
  digest?: string;
  checkedAt: string;
  error?: string;
}

/** views.ImageMatch: one image reference with the digests it runs at across the fleet. */
export interface ImageMatch {
  name: string;
  versions: List<ImageVersion>;
  /** Absent for a reference nobody follows (a container that is never rolled or pinned to a digest). */
  registry?: ImageLookup;
}

/**
 * api.ImagesRefresh: the number of tags that are looked up now, and of those
 * looked up too recently to be looked up again until next.
 */
export interface ImagesRefresh {
  tags: number;
  recent?: number;
  next?: string;
  /** How many images have their source branch read now. */
  sources?: number;
  message: string;
}

/**
 * beacon.EndpointState: ok follows the chain; behind is on it but lags its
 * head; syncing says so (or its EL is offline); forked has a head off the
 * followed chain; down did not answer.
 */
export type EndpointState = 'ok' | 'behind' | 'syncing' | 'forked' | 'down';

/** beacon.EndpointStatus: what the controller saw of one beacon endpoint; label carries no credentials. */
export interface BeaconEndpoint {
  label: string;
  state: EndpointState;
  /** headSlot, headRoot and finalizedEpoch are what the endpoint last reported; absent before. */
  headSlot?: number;
  headRoot?: string;
  finalizedEpoch?: number;
  successAt?: string;
  error?: string;
  errorAt?: string;
}

/** views.ValidatorRangeRow: the half-open range [start, end) of one host. */
export interface ValidatorRangeRow {
  start: number;
  end: number;
  host: string;
  count: number;
  /** unknown: upgradoor knows no host of that name. */
  unknown?: boolean;
}

/** views.ValidatorHostTotal: what the inventory gives one host. */
export interface ValidatorHostTotal {
  host: string;
  ranges: number;
  validators: number;
  unknown?: boolean;
}

/** views.ValidatorRangesSource: the validator ranges inventory; url is empty when none is configured. */
export interface ValidatorRangesSource {
  url?: string;
  attemptAt?: string;
  successAt?: string;
  changedAt?: string;
  etag?: string;
  lastModified?: string;
  error?: string;
  validators: number;
  unknownHosts: number;
  ranges: List<ValidatorRangeRow>;
  hosts: List<ValidatorHostTotal>;
}

/** views.ChainErrors: the error texts of the beacon service, which the other views carry as fixed texts. */
export interface ChainErrors {
  error?: string;
  timingError?: string;
  livenessError?: string;
}

/** views.DataSources: GET /api/v1/settings/sources (admin). */
export interface DataSources {
  beacon: List<BeaconEndpoint>;
  chain: ChainErrors;
  validatorRanges: ValidatorRangesSource;
}

/** views.ValidatorRangesRefresh: what a refresh of the validator ranges did, and the sources after it. */
export interface ValidatorRangesRefresh {
  fetched: boolean;
  changed?: boolean;
  error?: string;
  /** next: when a fetch may be asked for again, when one ran too recently. */
  next?: string;
  message: string;
  sources: DataSources;
}

/** api.Images: one page of the images the fleet's containers run; it pages like Packages. */
export interface Images {
  query: string;
  matches: List<ImageMatch>;
  total?: number;
  next?: string;
}

/** api.Rollouts */
export interface Rollouts {
  open: List<Rollout>;
  recent: List<Rollout>;
  leases: List<Lease>;
  recentLeases: List<Lease>;
}

/** api.SettingsBody */
export interface SettingsBody {
  settings: Settings;
  defaults: Settings;
  /** The kinds of the fleet's managed containers: el, cl, vc first, then by name. */
  kinds: List<ContainerKind>;
}

/** api.Audit */
export interface Audit {
  entries: List<AuditEntry>;
  nextBefore: number;
}

// ------------------------------------------------------------ event stream

/** api.AuditHead: the newest entry of the audit log, as the stream reports it. */
export interface AuditHead {
  latestId: number;
}

/**
 * The views the page reads from an event stream, by the name of the REST
 * route each mirrors. The stream carries them as tables (api/wire.ts); the
 * views are assembled from those.
 */
export interface LiveViews {
  'public-hosts': PublicHosts;
  'public-images': PublicImages;
  'public-network': Gate;
  /** The newest slots of the slot history, the ones that can still change (fleet.SlotParticipation). */
  slots: SlotRows;
  hosts: Hosts;
  /** One row per client; the hosts of a client are asked for page by page (GET /api/v1/clients/{client}/hosts). */
  clients: Clients;
  plan: Plan;
  rollouts: Rollouts;
  audit: AuditHead;
}

export type LiveView = keyof LiveViews;

/** PRIVATE_VIEWS are the views only the authenticated stream carries. */
export const PRIVATE_VIEWS: readonly LiveView[] = ['hosts', 'clients', 'plan', 'rollouts', 'audit'];
