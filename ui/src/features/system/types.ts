/**
 * System and Settings DTOs (DESIGN section 3.3 / 3.4 / 3.14).
 *
 * Every alias here now comes from the GENERATED schema, exactly like `api/types.ts` does for every
 * other area of the app. It did not always: `internal/api` had no `system.go` or `settings.go`, so
 * `api/openapi.json` — which is generated from the route registry — carried none of these paths, and
 * this file held hand-transcribed interfaces instead. The consequence was the one section 4's "the
 * types can never lie about the API" exists to prevent: a shape change on `/system`, `/settings`,
 * instance control or presets failed at runtime rather than at `tsc`, and CI's drift gate passed on
 * a document that was simply silent about them.
 *
 * Those handlers exist now, the document describes them, and this file is aliases again. Adding a
 * field on the Go side changes `schema.d.ts` on the next `npm run gen:api`, and every screen reading
 * it either keeps compiling or stops — which is the whole point.
 */

import type { components } from '../../api/schema';

type Schemas = components['schemas'];

/* -- capabilities (section 3.3, section 11.1a) ----------------------------- */

export type Capabilities = Schemas['CapabilitiesDTO'];
export type SystemdControl = Capabilities['systemd_control'];
export type SystemdScope = Capabilities['systemd_scope'];
export type JournalRead = Capabilities['journal_read'];
export type ListenerContinuity = Capabilities['listener_continuity'];

/** One entry of section 11.1a's enumeration: what is unavailable, why, and how to fix it. */
export type DegradedMode = Schemas['DegradedModeDTO'];

/* -- the rest of section 3.3 ------------------------------------------------- */

export type SystemInfo = Schemas['SystemInfoDTO'];
export type ToolchainCheck = Schemas['ToolchainCheckDTO'];
export type Gpu = Schemas['GPUDTO'];
export type DiskUsageEntry = Schemas['DiskUsageDTO'];
export type UnitStatus = Schemas['UnitStatusDTO'];
export type UnitDrift = UnitStatus['drift'];
export type JournalLine = Schemas['JournalLineDTO'];
export type SystemNotification = Schemas['NotificationDTO'];
export type RestartResponse = Schemas['RestartDTO'];

/* -- GET/PATCH/POST /settings (section 3.4) ------------------------------------ */

export type SettingDef = Schemas['SettingDefDTO'];
export type SettingKind = SettingDef['type'];
export type SettingsResponse = Schemas['SettingsDTO'];
export type PatchSettingsResponse = Schemas['PatchSettingsDTO'];
/** The reset answers with the whole settings document again, schema included. */
export type ResetSettingsResponse = SettingsResponse;

/* -- error codes this area's UI branches on (section 3.3/3.4, D93) ------------- */

/**
 * These are `internal/model.ErrorCode` members now, and the generated schema carries them as the
 * documented `error.code` enum of each route. The object stays because a screen branching on a
 * string literal it typed by hand is a screen that will eventually typo one — reading the code from
 * here means a rename on the Go side is a compile error at the call site rather than a branch that
 * silently stops matching.
 */
export const SYSTEM_ERROR_CODES = {
  systemdDenied: 'systemd_denied',
  systemdUnavailable: 'systemd_unavailable',
  restartUnavailable: 'restart_unavailable',
  restartRateLimited: 'restart_rate_limited',
  journalUnavailable: 'journal_unavailable',
  autostartUnavailable: 'autostart_unavailable',
  jobInFlight: 'job_in_flight',
  settingInvalid: 'setting_invalid',
} as const;
