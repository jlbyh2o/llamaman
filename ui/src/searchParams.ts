/**
 * URL search-param schemas.
 *
 * "Type-safe params *and* search params: filters, sort and comparison selections live in the URL,
 * which is what a technical tool needs (shareable links, working back button)" — DESIGN section 4.
 * Every filter a screen offers is declared here and validated by the router, so a hand-edited URL
 * degrades to the default rather than crashing the route.
 *
 * `.catch()` on every field is what makes that true: zod returns the fallback instead of throwing,
 * which is the right behavior for a query string someone pasted into chat.
 */

import { z } from 'zod';

const optionalString = z.string().min(1).optional().catch(undefined);
const optionalBool = z.boolean().optional().catch(undefined);

/** `/login` — where to go once the password is accepted. */
export const loginSearchSchema = z.object({
  redirect: optionalString,
});
export type LoginSearch = z.infer<typeof loginSearchSchema>;

/** `/instances` — the table's filter and sort. */
export const instancesSearchSchema = z.object({
  q: optionalString,
  state: optionalString,
  sort: optionalString,
  desc: optionalBool,
  include_deleted: optionalBool,
});
export type InstancesSearch = z.infer<typeof instancesSearchSchema>;

/** `/models` — the local library. */
export const modelsSearchSchema = z.object({
  q: optionalString,
  state: optionalString,
  kind: optionalString,
  sort: optionalString,
  desc: optionalBool,
});
export type ModelsSearch = z.infer<typeof modelsSearchSchema>;

/** `/models/browse` — the Hugging Face search, whose whole state is shareable. */
export const browseSearchSchema = z.object({
  q: optionalString,
  author: optionalString,
  sort: optionalString,
  gated: optionalBool,
});
export type BrowseSearch = z.infer<typeof browseSearchSchema>;

/** `/models/browse/$` — which quant's fit report is open, and against which GPUs. */
export const repoSearchSchema = z.object({
  file: optionalString,
  gpu: z.array(z.string()).optional().catch(undefined),
});
export type RepoSearch = z.infer<typeof repoSearchSchema>;

/** `/downloads` — active queue or the whole ledger. */
export const downloadsSearchSchema = z.object({
  state: z.enum(['active', 'all']).optional().catch(undefined),
});
export type DownloadsSearch = z.infer<typeof downloadsSearchSchema>;

/** `/llamacpp` — which channel the install dialog is looking at, and which version is expanded. */
export const llamacppSearchSchema = z.object({
  channel: z.enum(['stable', 'nightly', 'custom']).optional().catch(undefined),
  version: optionalString,
  tab: optionalString,
});
export type LlamacppSearch = z.infer<typeof llamacppSearchSchema>;

/** `/bench` — history filters. */
export const benchSearchSchema = z.object({
  q: optionalString,
  model_id: optionalString,
  state: optionalString,
  sort: optionalString,
  desc: optionalBool,
});
export type BenchSearch = z.infer<typeof benchSearchSchema>;

/**
 * `/bench/compare` — the axis pickers of section 3.13's compare endpoint. This is the screen the
 * URL matters most for: a comparison is a thing people send each other.
 */
export const benchCompareSearchSchema = z.object({
  runs: z.array(z.string()).optional().catch(undefined),
  x: optionalString,
  y: optionalString,
  series: optionalString,
});
export type BenchCompareSearch = z.infer<typeof benchCompareSearchSchema>;

/** `/bench/$id` — which point is selected in the results table. */
export const benchRunSearchSchema = z.object({
  point: optionalString,
});
export type BenchRunSearch = z.infer<typeof benchRunSearchSchema>;

/** `/tokens` */
export const tokensSearchSchema = z.object({
  q: optionalString,
  state: optionalString,
});
export type TokensSearch = z.infer<typeof tokensSearchSchema>;

/** `/events` — the audit log's filters, mirroring `GET /events/log`. */
export const eventsSearchSchema = z.object({
  category: optionalString,
  level: optionalString,
  subject_id: optionalString,
  before: optionalString,
  q: optionalString,
});
export type EventsSearch = z.infer<typeof eventsSearchSchema>;

/** `/settings` — the nine groups of section 3.4, as a tab in the URL. */
export const SETTINGS_GROUPS = [
  'general',
  'network',
  'huggingface',
  'storage',
  'builds',
  'benchmarks',
  'security',
  'updates',
  'danger',
] as const;
export type SettingsGroup = (typeof SETTINGS_GROUPS)[number];

export const settingsSearchSchema = z.object({
  group: z.enum(SETTINGS_GROUPS).optional().catch(undefined),
});
export type SettingsSearch = z.infer<typeof settingsSearchSchema>;

/** `/system` — which report card is open. */
export const systemSearchSchema = z.object({
  tab: z.enum(['toolchain', 'gpus', 'disk', 'units', 'journal']).optional().catch(undefined),
});
export type SystemSearch = z.infer<typeof systemSearchSchema>;

/** `/instances/$id` — which pane of the detail screen. */
export const instanceDetailSearchSchema = z.object({
  tab: z.enum(['overview', 'logs', 'slots', 'tokens', 'starts']).optional().catch(undefined),
});
export type InstanceDetailSearch = z.infer<typeof instanceDetailSearchSchema>;

/** `/instances/new` and `/instances/$id/edit` — the three panes of section 4 screen 5. */
export const instanceFormSearchSchema = z.object({
  pane: z.enum(['model', 'performance', 'advanced']).optional().catch(undefined),
  /** Prefill from a model chosen on the browse screen. */
  model_id: optionalString,
  preset_id: optionalString,
});
export type InstanceFormSearch = z.infer<typeof instanceFormSearchSchema>;
