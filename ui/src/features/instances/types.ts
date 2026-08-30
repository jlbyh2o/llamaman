/**
 * The instance and preset shapes of DESIGN section 3.10, 3.10b and 3.11.
 *
 * Every alias here comes from the GENERATED schema now. It did not always:
 * `internal/api/instances.go` registered five routes and said why the rest were absent rather than
 * stubbed — "each needs the supervisor, the gateway or the fit calculator, and a route registered
 * here appears in api/openapi.json, where a 501 behind it would be a promise in the contract the
 * daemon cannot keep. They join the registry with the subsystems they call." They have joined, so
 * this file is aliases like `api/types.ts` and the fetch wrappers beside it are typed calls rather
 * than casts through `never`.
 */

import type { components } from '../../api/schema';

type Schemas = components['schemas'];

/** `POST /instances/{id}/{start|stop|restart|safe-start|reset-failed}` — a 202 receipt. */
export type ControlResponse = Schemas['InstanceControlDTO'];

/** `PUT /instances/{id}/autostart`. */
export type AutostartResponse = Schemas['AutostartDTO'];

/** `GET /instances/{id}/command` — copyable and auditable (section 5.7). */
export type InstanceCommand = Schemas['InstanceCommandDTO'];

/** One row of `instance_usage_daily` (section 2.9). */
export type InstanceUsageDay = Schemas['InstanceUsageDayDTO'];

/** `GET /instances/{id}/usage` — the daily rollups plus llama-server's own counter. */
export type InstanceUsage = Schemas['InstanceUsageDTO'];

/** `GET /instances/{id}/status` — the stored row plus a live unit read. */
export type InstanceLiveStatus = Schemas['InstanceLiveStatusDTO'];

/** `POST /instances/validate` — the dry run behind the argv preview and the draft check. */
export type ValidateResponse = Schemas['ValidateInstanceDTO'];

/**
 * Section 3.10a's three-valued draft verdict.
 *
 * The generated schema types `draft_validation` as a bare `string`: the Go generator reflects a
 * struct field's type, and the enum lives in a `CHECK` constraint rather than in the field. So the
 * union is stated here and `asDraftValidation` is the one place a response crosses into it — which
 * means a fourth value added upstream becomes `undefined` at one narrow point rather than a
 * `switch` somewhere that silently stops matching.
 */
export type DraftValidation = 'ok' | 'deferred' | 'mismatch';

export function asDraftValidation(value: string | undefined): DraftValidation | undefined {
  return value === 'ok' || value === 'deferred' || value === 'mismatch' ? value : undefined;
}

/** `POST /instances/{id}/pin-ngl` (D51). */
export type PinNglResponse = Schemas['PinNGLDTO'];

/** `GET /ports/suggest?kind=public|internal`. */
export type PortSuggestion = Schemas['PortSuggestionDTO'];

/** One row of `flag_presets` (section 2.8) over `GET/POST /presets`. */
export type FlagPreset = Schemas['PresetDTO'];

/** The body of the preset writes. */
export type PresetInput = Schemas['PresetInput'];

/** One journal entry, for the instance log pane. */
export type InstanceLogLine = Schemas['JournalLineDTO'];

/**
 * What one of the three llama-server pass-throughs answered.
 *
 * `json` is `unknown` on purpose and the daemon documents it that way: llama.cpp ships nightlies,
 * `/props` and `/slots` change with them, and a generated client typed against today's fields would
 * break on a build the user installed themselves. The client narrows it; see `ServerSlot`.
 */
export type UpstreamBody = Schemas['UpstreamBodyDTO'];

/**
 * `GET /instances/{id}/slots`, as this app reads it.
 *
 * This one shape stays hand-written, and it is the one place where that is correct rather than a
 * gap: it describes UPSTREAM's body, which this project does not own and cannot generate from its
 * own route registry. Every field is optional for the same reason.
 */
export interface ServerSlot {
  id: number;
  state?: number;
  is_processing?: boolean;
  prompt?: string;
  next_token?: { n_decoded?: number; n_remain?: number };
  n_ctx?: number;
}
