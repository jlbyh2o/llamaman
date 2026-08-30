/**
 * The app-facing names for the generated schema.
 *
 * `schema.d.ts` is machine output and reads like it — `components["schemas"]["List[github.com/…
 * .InstanceDTO]"]`. Everything in the UI imports from here instead, so a rename on the Go side is
 * one edit in this file rather than a hundred across the screens.
 *
 * The state unions at the bottom are the one thing NOT taken from the schema: the Go DTOs carry
 * these columns as plain `string`, so the generator can only emit `string`. They are transcribed
 * here from the `CHECK (… IN (…))` constraints of DESIGN section 2 and are the enums the status
 * badge and every state-keyed color are keyed to. When the DTOs grow real enums, these aliases
 * collapse onto the generated types and nothing else moves.
 */

import type { components } from './schema';

type Schemas = components['schemas'];

/* -- envelopes ------------------------------------------------------------- */

export type ErrorEnvelope = Schemas['ErrorEnvelope'];
export type Meta = Schemas['Meta'];
export type Health = Schemas['Health'];

/** `{"items":[…],"total":N,"next_cursor":"01J…"|null}` — the list shape of DESIGN section 3. */
export interface ListPage<T> {
  items: T[];
  total: number;
  next_cursor?: string | null;
}

/* -- auth and setup -------------------------------------------------------- */

export type SessionState = Schemas['SessionStateDTO'];
export type Session = Schemas['SessionDTO'];
export type SetupState = Schemas['SetupStateDTO'];
export type WizardStep = Schemas['WizardStepDTO'];

/* -- jobs, events, notifications ------------------------------------------- */

export type Job = Schemas['JobDTO'];
export type JobReceipt = Schemas['JobReceiptDTO'];
export type Subject = Schemas['SubjectDTO'];
export type AuditEvent = Schemas['EventDTO'];

/* -- instances ------------------------------------------------------------- */

export type Instance = Schemas['InstanceDTO'];
export type InstanceDetail = Schemas['InstanceDetailDTO'];
export type InstanceStatus = Schemas['InstanceStatusDTO'];
export type InstanceStart = Schemas['InstanceStartDTO'];
export type InstanceRef = Schemas['InstanceRefDTO'];
export type FlagSet = Schemas['FlagSet'];
export type NGpuLayers = Schemas['NGpuLayers'];

/* -- models, cache, downloads ---------------------------------------------- */

export type Model = Schemas['ModelDTO'];
export type ModelDetail = Schemas['ModelDetailDTO'];
export type ModelFile = Schemas['ModelFileDTO'];
export type DeletePreview = Schemas['DeletePreviewDTO'];
export type CacheRoot = Schemas['CacheRootDTO'];
export type CacheScan = Schemas['CacheScanDTO'];
export type StrayFile = Schemas['StrayFileDTO'];
export type Download = Schemas['DownloadDTO'];
export type DownloadFile = Schemas['DownloadFileDTO'];

/* -- Hugging Face ---------------------------------------------------------- */

export type HFSearchResult = Schemas['HFSearchResultDTO'];
export type HFModel = Schemas['HFModelDTO'];
export type HFTree = Schemas['HFTreeDTO'];
export type HFTreeGroup = Schemas['HFTreeGroupDTO'];
export type HFTreeEntry = Schemas['HFTreeEntryDTO'];
export type HFCard = Schemas['HFCardDTO'];
export type HFPeek = Schemas['HFPeekDTO'];
export type TokenStatus = Schemas['TokenStatusDTO'];

/* -- fit ------------------------------------------------------------------- */

export type FitReport = Schemas['FitReportDTO'];
export type FitBatchReport = Schemas['FitBatchReportDTO'];
export type FitDevice = Schemas['FitDeviceDTO'];
export type FitInputs = Schemas['FitInputsDTO'];
export type FitRecommendation = Schemas['FitRecommendationDTO'];

/* -- llama.cpp and updates ------------------------------------------------- */

export type LlamacppVersion = Schemas['LlamacppVersionDTO'];
export type LlamacppVersionDetail = Schemas['LlamacppVersionDetailDTO'];
export type LlamacppPlan = Schemas['PlanDTO'];
export type Release = Schemas['ReleaseDTO'];
export type UpdateStatus = Schemas['UpdateStatusDTO'];
export type UpdateRelease = Schemas['UpdateReleaseDTO'];
export type SelfUpdate = Schemas['SelfUpdateDTO'];
export type LogPage = Schemas['LogPageDTO'];

/* -- tokens and the gateway ------------------------------------------------ */

export type ApiToken = Schemas['APITokenDTO'];
export type ApiTokenDetail = Schemas['TokenDetailDTO'];
export type TokenUsage = Schemas['TokenUsageDTO'];
export type GatewayDenial = Schemas['GatewayDenialDTO'];

/* -- benchmarks ------------------------------------------------------------ */

export type BenchRun = Schemas['BenchRunDTO'];
export type BenchRunDetail = Schemas['BenchRunDetailDTO'];
export type BenchPoint = Schemas['BenchPointDTO'];
export type BenchResultRow = Schemas['BenchResultRowDTO'];
export type BenchPreflight = Schemas['BenchPreflightDTO'];
export type BenchCompare = Schemas['BenchCompareDTO'];
export type BenchSeries = Schemas['BenchSeriesDTO'];

/* ========================================================================== */
/* State enums — DESIGN section 2's CHECK constraints, transcribed             */
/* ========================================================================== */

/** `instance_status.state` (section 2.8). Nine states; the badge colors all of them. */
export const INSTANCE_STATES = [
  'unknown',
  'stopped',
  'starting',
  'loading',
  'ready',
  'degraded',
  'stopping',
  'failed',
  'crash-looping',
] as const;
export type InstanceState = (typeof INSTANCE_STATES)[number];

/**
 * The four derived flags of section 2.8 — badges, never states, because an instance that is serving
 * traffic can carry one and still be `ready`.
 */
export const INSTANCE_FLAGS = [
  'restart_required',
  'stale_version',
  'inhibited',
  'draft_unverified',
] as const;
export type InstanceFlag = (typeof INSTANCE_FLAGS)[number];

/** `jobs.state` (section 2.3). */
export const JOB_STATES = [
  'queued',
  'leased',
  'running',
  'paused',
  'interrupted',
  'succeeded',
  'failed',
  'canceled',
] as const;
export type JobState = (typeof JOB_STATES)[number];

/** `downloads.state` (section 2.7). */
export const DOWNLOAD_STATES = [
  'queued',
  'resolving',
  'running',
  'paused',
  'verifying',
  'succeeded',
  'failed',
  'canceled',
] as const;
export type DownloadState = (typeof DOWNLOAD_STATES)[number];

/** `models.state` (section 2.6). */
export const MODEL_STATES = [
  'planned',
  'downloading',
  'incomplete',
  'verifying',
  'ready',
  'corrupt',
  'missing',
  'deleting',
  'deleted',
] as const;
export type ModelState = (typeof MODEL_STATES)[number];

/** `llamacpp_versions.state` (section 2.5). */
export const LLAMACPP_STATES = [
  'pending',
  'resolving',
  'fetching',
  'building',
  'verifying',
  'ready',
  'failed',
  'failed_verification',
  'canceled',
  'deleting',
  'deleted',
] as const;
export type LlamacppState = (typeof LLAMACPP_STATES)[number];

/** `bench_runs.state` (section 2.10). */
export const BENCH_STATES = [
  'draft',
  'queued',
  'preflight',
  'running',
  'succeeded',
  'partial',
  'failed',
  'canceled',
] as const;
export type BenchState = (typeof BENCH_STATES)[number];

/** `api_tokens.state` (section 2.9). `revoked` is terminal. */
export const TOKEN_STATES = ['active', 'disabled', 'revoked'] as const;
export type TokenState = (typeof TOKEN_STATES)[number];

/** `self_updates.state` (section 2.11). */
export const SELF_UPDATE_STATES = [
  'planned',
  'downloading',
  'verifying',
  'staged',
  'swapping',
  'succeeded',
  'failed',
  'canceled',
] as const;
export type SelfUpdateState = (typeof SELF_UPDATE_STATES)[number];

/** `wizard_steps.step` and `.state` (section 2.11 / section 11.2). */
export const WIZARD_STEPS = [
  'password',
  'toolchain',
  'llamacpp',
  'hf',
  'models',
  'instance',
  'done',
] as const;
export type WizardStepId = (typeof WIZARD_STEPS)[number];

export const WIZARD_STEP_STATES = ['pending', 'active', 'skipped', 'complete'] as const;
export type WizardStepState = (typeof WIZARD_STEP_STATES)[number];

/** `events.level` and `notifications.severity` (section 2.11). */
export const EVENT_LEVELS = ['debug', 'info', 'warn', 'error'] as const;
export type EventLevel = (typeof EVENT_LEVELS)[number];

export const NOTIFICATION_SEVERITIES = ['info', 'warn', 'error'] as const;
export type NotificationSeverity = (typeof NOTIFICATION_SEVERITIES)[number];

/** `events.category` (section 2.11). */
export const EVENT_CATEGORIES = [
  'llamacpp',
  'model',
  'download',
  'instance',
  'token',
  'bench',
  'auth',
  'update',
  'system',
  'gateway',
] as const;
export type EventCategory = (typeof EVENT_CATEGORIES)[number];

/** `events.actor` (section 2.11). */
export const EVENT_ACTORS = ['admin', 'system', 'systemd', 'wizard', 'cli'] as const;
export type EventActor = (typeof EVENT_ACTORS)[number];

/** The fit verdict of section 3.9. */
export const FIT_VERDICTS = ['fits', 'partial', 'wont_run'] as const;
export type FitVerdict = (typeof FIT_VERDICTS)[number];
