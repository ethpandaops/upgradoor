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
type Verb = 'roll' | 'packages' | 'reboot';

/** protocol.Validators: the validator range a host reports with its sync. */
interface ValidatorReport {
  start: number;
  end: number;
  /** Where the agent read the range: the marker file of the validator keys, or its configuration. */
  source: 'marker' | 'config' | (string & {});
  /** known: start and end are the range. missing: the marker file is not there. invalid: the range cannot be used. */
  state: 'known' | 'missing' | 'invalid' | (string & {});
  error?: string;
}

/** protocol.Hold */
export interface Hold {
  local: boolean;
  localReason?: string;
  controller: boolean;
}

/** protocol.ContainerKind */
export type ContainerKind = 'el' | 'cl' | 'vc' | 'other';

/**
 * protocol.Impact: how the controller schedules work on a container. node: the
 * validator budget, the host limit and the gate; slot: the host limit and the
 * gate; independent: a limit of its own.
 */
export type Impact = 'node' | 'slot' | 'independent';

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
  kind?: ContainerKind;
  impact?: Impact;
  configuredImage?: string;
  registry?: RegistryCheck;
  /** owners are the teams that own a managed container; absent when nobody does. */
  owners?: string[];
  /**
   * client is the client software a managed container runs ("geth",
   * "lighthouse"); a validator client carries the client of its consensus
   * client. This and the three fields below are in a host's details and in
   * the rows of a client's host list, never in the rows of the host table.
   */
  client?: string;
  /** command is the command line of a managed container: what a flag override changes. */
  command?: string[];
  /** override is the entry of the host's overrides file for the container, or what is left to undo of one. */
  override?: ContainerOverride;
  /** overridable says what the container's group lets anybody override; absent when nothing (tooling). */
  overridable?: Overridable;
  /**
   * group is the roll group of a managed container: its
   * io.ethpandaops.upgradoor.group label, its entry in the name map of the
   * host's agent.yaml, or "default".
   */
  group?: string;
  /** notRolled says why the agent rolls this container to nothing (its updates are still reported). */
  notRolled?: string;
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

/** protocol.Overridable: what the container's group allows to override; absent for tooling. */
interface Overridable {
  image?: boolean;
  flags?: boolean;
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

/** protocol.NodeHealth */
interface NodeHealth {
  el?: ELHealth;
  cl?: CLHealth;
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
export type NeedKind = 'image' | 'override' | 'spec' | 'packages' | 'reboot';

/** protocol.ImageNeed */
interface ImageNeed {
  container: string;
  /** The roll group of the container: the needs of one group and client are one rollout. */
  group?: string;
  /** The client the container runs; the group rollouts of each client are their own. */
  client?: string;
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
  /** The roll group of the container: the needs of one group and client are one rollout. */
  group?: string;
  /** The client the container runs; the group rollouts of each client are their own. */
  client?: string;
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
}

/** protocol.Phase. `pulling`: the images are pulled first; nothing is stopped yet. */
export type Phase = 'pulling' | 'draining' | 'applying' | 'rebooting' | 'starting' | 'verifying' | 'done' | 'failed';

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

/** protocol.Step */
export interface Step {
  needId: string;
  status: StepStatus;
  error?: string;
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
  livenessKnown: boolean;
  livenessEpoch: number;
  /** Why the beacon service's last count of the per-slot data failed, if it did. */
  livenessError?: string;
  tracked: number;
  live: number;
}

/**
 * fleet.RolloutKind. `group` moves the containers of one roll group of one
 * client (a beacon node and its validator client, an execution client) onto
 * what the hosts' image and spec needs of it ask for. `local` is a rollout of one host for what was asked for
 * on the host itself (an override, a roll from its command line): it has one
 * member, answers to no policy and has no canary.
 */
export type RolloutKind = 'group' | 'packages' | 'reboot' | 'manual' | 'local';

/** fleet.RolloutState */
export type RolloutState = 'active' | 'halted' | 'completed' | 'superseded' | 'aborted';

/** fleet.HostState */
export type HostState = 'expected' | 'pending' | 'leased' | 'done' | 'failed' | 'skipped' | 'aborted';

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
  updatedAt: string;
}

/** fleet.RolloutTarget: one part of what a group rollout moves its group to. */
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

/** fleet.Rollout */
export interface Rollout {
  id: string;
  kind: RolloutKind;
  /** group:<group>#<client>@<hash of the targets> (group:default/<image>@<hash> for the default group), packages:<key>, reboot, manual:<id>, local:<host>:<hash>. */
  key: string;
  title: string;
  /** The roll group a group rollout moves, and what it moves the group's containers to, sorted. */
  group?: string;
  targets?: List<RolloutTarget>;
  /** The client whose image policy a group rollout answers to; absent when it answers to policyImage. */
  client?: string;
  packages?: PackageChange[];
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
  approved: boolean;
  force: boolean;
  ignoreBudget: boolean;
  canary?: string;
  createdBy: string;
  createdAt: string;
  updatedAt: string;
  endedAt?: string;
  hosts: List<RolloutHost>;
}

/** fleet.LeaseState */
export type LeaseState = 'granted' | 'running' | 'verifying' | 'released' | 'failed' | 'expired';

/**
 * fleet.LeaseCancel: the mark of a lease that a person ended while it was
 * open (the end-lease action). The lease stays open and keeps its slot until
 * its agent reports or goes silent, and then ends as failed.
 */
interface LeaseCancel {
  at: string;
  /** The person, as recorded in the audit log. */
  by: string;
  /** The state the lease was in when it was cancelled. For people: no rule reads it. */
  state: LeaseState;
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
  /** The comparison with the fleet that the last readiness check made, per client it applied to. */
  peerChecks?: PeerCheck[];
  error?: string;
  /**
   * Set once a person ended the lease. While the lease is open the agent is
   * told with every sync and stops after the step in progress.
   */
  cancelled?: LeaseCancel;
  /**
   * recheck marks a lease that only repeats the readiness check of a failed
   * lease whose steps the agent did, after a retry; the agent is not offered it.
   */
  recheck?: boolean;
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
  readyEpochs: number;
  minPeers: number;
  leaseTtl: string;
  hostStaleAfter: string;
  readinessTimeout: string;
  policyImage: Policy;
  policyPackages: Policy;
  policyReboot: Policy;
  /** The policy of the default group's rollouts: one per image, of the containers no label or name map puts in a group. */
  policyDefaultGroup: Policy;
  /**
   * The image policy per client ("geth": "manual"); always an object. The
   * image rollouts of a client that is listed answer to its entry, the
   * others to policyImage. It is changed with the client-policy action.
   */
  policyClients: Record<string, Policy>;
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
  reason: string;
}

/** scheduler.RebootRequest */
export interface RebootRequest {
  hosts: string[];
  all: boolean;
  staggered: boolean;
  force: boolean;
  ignoreBudget: boolean;
  reason: string;
}

/** fleet.OfflineMark: a person's mark that a host is known offline. */
export interface OfflineMark {
  reason: string;
  /** until is when the mark ends by itself; absent: until a person lifts it. */
  until?: string;
  /** by is "team" for a mark set through team_hold. */
  by?: string;
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
}

/**
 * scheduler.EndLeaseRequest: the body of end-lease. The action is a request
 * to cancel an open lease (granted, running or verifying): the lease is
 * marked (Lease.cancelled) and stays open until the agent reports or the
 * lease runs out, and then ends as failed. No hold is set. Ending a lease
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

/** scheduler.ScopeRequest: the body of pause, resume and abort. */
export interface ScopeRequest {
  rollout: string;
  reason: string;
}

/** scheduler.RolloutHostsRequest: the body of skip and retry. */
export interface RolloutHostsRequest {
  rollout: string;
  hosts: string[];
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
 * order and pull (the host waits to pull an image again after a failed pull) clear by themselves; working clears when the operation the host
 * works on without a lease is over; gate clears when the network recovers;
 * the rest needs somebody to act.
 */
export type WaitKind =
  | 'budget'
  | 'host-limit'
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
   * The rollout the reason is about (the paused, halted or unapproved one, or
   * the one whose order the host waits in); absent when the reason is about
   * the host or the fleet.
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

/** api.PublicImage */
export interface PublicImage {
  image: string;
  kind: ContainerKind;
  digest?: string;
  revision?: string;
  hostsOn: List<string>;
  hostsBehind: List<string>;
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
  work?: 'image' | 'packages' | 'reboot' | 'mixed' | (string & {});
  rollout?: string;
  /** The lease failed or expired. */
  failed?: boolean;
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
  'name' | 'image' | 'digest' | 'revision' | 'state' | 'managed' | 'kind' | 'impact' | 'owners' | 'group'
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
  containers: List<ContainerListing>;
  os: OSSummary;
  pending: Pending;
  uptimeSeconds: number;
  /** When the host booted. Only a stream sets it (the uptime then follows from the clock); the REST view has uptimeSeconds. */
  bootTime?: string;
  hold: Hold;
  holdWanted: boolean;
  /** holdReason is the reason of the controller hold (holdWanted); left out when there is none. */
  holdReason?: string;
  /**
   * holdBy says who set the controller hold: "team:<teams>" for a hold that
   * these teams set through a team feature, which a team feature of one of
   * them may lift again, "staff" for every other hold. Left out when the host
   * is not held.
   */
  holdBy?: string;
  /** offline is the known-offline mark while it is in force. */
  offline?: OfflineMark;
  verbs: List<Verb>;
  /** The kinds of the host's open needs, one entry per need. */
  needs: List<Pick<Need, 'kind'>>;
  plan: HostPlan;
}

/** api.HostSummary: a host in full, as its details carry it. */
export interface HostSummary extends HostListing {
  /** What the host's last sync said about its validator range; absent when it reports none. */
  validatorReport?: ValidatorReport;
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
  /** The [start, end) range counted for the host. It follows validatorReport and differs while the host does not know its range. */
  validatorStart?: number;
  validatorEnd?: number;
  /** The other hosts that report validators this host reports too. */
  validatorOverlaps?: string[];
  inventory?: Inventory;
  leases: List<Lease>;
  history: List<AuditEntry>;
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
}

/** api.ClientView: one client, a row of GET /api/v1/clients and of the stream's client table. */
export interface ClientView {
  name: string;
  layer: ClientLayer;
  /** The image policy the client's rollouts answer to; policySet is false when that is policyImage. */
  policy: Policy;
  policySet: boolean;
  /** owners own a container of the client on some host; allOwned own every one of them. */
  owners: List<string>;
  allOwned: List<string>;
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
  message: string;
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
  settings: SettingsBody;
  audit: AuditHead;
}

export type LiveView = keyof LiveViews;

/** PRIVATE_VIEWS are the views only the authenticated stream carries. */
export const PRIVATE_VIEWS: readonly LiveView[] = ['hosts', 'clients', 'plan', 'rollouts', 'settings', 'audit'];
