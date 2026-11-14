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
export type List<T> = T[] | null;
/** A Go map without omitempty. */
export type Dict<T> = Record<string, T> | null;

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
export type Verb = 'roll' | 'packages' | 'reboot';

/** protocol.Hold */
export interface Hold {
  local: boolean;
  localReason?: string;
  controller: boolean;
}

/** protocol.ContainerKind */
export type ContainerKind = 'el' | 'cl' | 'vc' | 'other';

/** protocol.RegistryCheck */
export interface RegistryCheck {
  digest?: string;
  revision?: string;
  checkedAt: string;
  error?: string;
  pulled: boolean;
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
  health?: string;
  restartCount: number;
  startedAt: string;
  managed: boolean;
  kind?: ContainerKind;
  configuredImage?: string;
  custom?: boolean;
  registry?: RegistryCheck;
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
export interface NodeHealth {
  el?: ELHealth;
  cl?: CLHealth;
}

/** protocol.OSSummary */
export interface OSSummary {
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
export type NeedKind = 'image' | 'packages' | 'reboot';

/** protocol.ImageNeed */
export interface ImageNeed {
  container: string;
  image: string;
  fromDigest?: string;
  toDigest: string;
  toRevision?: string;
}

/** protocol.PackageChange */
export interface PackageChange {
  name: string;
  from: string;
  to: string;
}

/** protocol.PackagesNeed */
export interface PackagesNeed {
  packages: List<PackageChange>;
}

/** protocol.RebootNeed */
export interface RebootNeed {
  reasons: List<string>;
}

/** protocol.Need */
export interface Need {
  id: string;
  kind: NeedKind;
  directiveId?: string;
  image?: ImageNeed;
  packages?: PackagesNeed;
  reboot?: RebootNeed;
}

/** protocol.Phase */
export type Phase = 'draining' | 'applying' | 'rebooting' | 'starting' | 'verifying' | 'done' | 'failed';

/** protocol.StepStatus */
export type StepStatus = 'pending' | 'running' | 'done' | 'failed' | 'superseded' | 'refused';

/** protocol.Step */
export interface Step {
  needId: string;
  status: StepStatus;
  error?: string;
}

/** protocol.Operation */
export interface Operation {
  leaseId: string;
  phase: Phase;
  startedAt: string;
  updatedAt: string;
  error?: string;
  steps: List<Step>;
  disrupted: boolean;
}

/** protocol.OSInfo */
export interface OSInfo {
  id: string;
  versionId: string;
  codename: string;
  prettyName: string;
  arch: string;
}

/** protocol.KernelInfo */
export interface KernelInfo {
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

/** fleet.Liveness */
export interface Liveness {
  host: string;
  epoch: number;
  live: number;
  total: number;
}

/** fleet.NetworkHealth */
export interface NetworkHealth {
  updatedAt: string;
  error?: string;
  genesisTime: string;
  secondsPerSlot: number;
  slotsPerEpoch: number;
  headSlot: number;
  currentEpoch: number;
  finalizedEpoch: number;
  finalityLagEpochs: number;
  livenessKnown: boolean;
  livenessEpoch: number;
  /**
   * True while the beacon node is known not to serve the liveness endpoint.
   * The gate then judges finality only; readiness and the budget use the
   * agents' probes.
   */
  livenessUnsupported: boolean;
  tracked: number;
  live: number;
}

/** fleet.RolloutKind */
export type RolloutKind = 'image' | 'packages' | 'reboot' | 'manual';

/** fleet.RolloutState */
export type RolloutState = 'active' | 'halted' | 'completed' | 'superseded' | 'aborted';

/** fleet.HostState */
export type HostState = 'expected' | 'pending' | 'leased' | 'done' | 'failed' | 'skipped';

/** fleet.Stage: 0 no validators, 1 canary, 2 batches, 3 bootnodes. */
export type Stage = 0 | 1 | 2 | 3;

/** fleet.RolloutHost */
export interface RolloutHost {
  host: string;
  state: HostState;
  stage: Stage;
  needId?: string;
  leaseId?: string;
  error?: string;
  updatedAt: string;
}

/** fleet.Rollout */
export interface Rollout {
  id: string;
  kind: RolloutKind;
  key: string;
  title: string;
  image?: string;
  digest?: string;
  revision?: string;
  packages?: PackageChange[];
  state: RolloutState;
  reason?: string;
  paused: boolean;
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

/** fleet.Lease */
export interface Lease {
  id: string;
  host: string;
  state: LeaseState;
  needs: List<Need>;
  rollouts: Dict<string>;
  weight: number;
  charged: number;
  force: boolean;
  grantedBy: string;
  grantedAt: string;
  expiresAt: string;
  agentDoneAt?: string;
  endedAt?: string;
  operation?: Operation;
  readiness?: string;
  error?: string;
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
export type Policy = 'auto' | 'manual' | 'paused';

/** fleet.Settings; durations are Go duration strings ("30m0s"). */
export interface Settings {
  maxUnavailablePercent: number;
  minParticipationPercent: number;
  maxFinalityLagEpochs: number;
  soakEpochs: number;
  leaseTtl: string;
  hostStaleAfter: string;
  readinessTimeout: string;
  policyImage: Policy;
  policyPackages: Policy;
  policyReboot: Policy;
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

/** scheduler.HoldRequest: the body of hold, unhold and forget. */
export interface HoldRequest {
  hosts: string[];
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
}

/** scheduler.QueueEntry */
export interface QueueEntry {
  host: string;
  position: number;
  reason: string;
  rollouts: List<string>;
}

/** scheduler.SlotState */
export type SlotState = 'idle' | 'queued' | 'leased' | 'verifying' | 'failed' | 'held' | 'stale';

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
export interface PackageVersion {
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
}

/** api.PublicImageRef */
export interface PublicImageRef {
  image: string;
  digest?: string;
  revision?: string;
  custom?: boolean;
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

/** api.HostSummary */
export interface HostSummary {
  name: string;
  roles: List<string>;
  validators: number;
  lastSeen: string;
  stale: boolean;
  agentVersion: string;
  containers: List<Container>;
  node: NodeHealth;
  os: OSSummary;
  pending: Pending;
  uptimeSeconds: number;
  hold: Hold;
  holdWanted: boolean;
  verbs: List<Verb>;
  needs: List<Need>;
  plan: HostPlan;
}

/** api.Hosts */
export interface Hosts {
  hosts: List<HostSummary>;
}

/** api.HostDetail */
export interface HostDetail extends HostSummary {
  certHash: string;
  generation: number;
  firstSeen: string;
  remoteAddr: string;
  validatorStart?: number;
  validatorEnd?: number;
  inventory?: Inventory;
  liveness: List<Liveness>;
  leases: List<Lease>;
  history: List<AuditEntry>;
}

/** api.Packages */
export interface Packages {
  query: string;
  matches: List<PackageMatch>;
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
