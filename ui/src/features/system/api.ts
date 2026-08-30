/**
 * Fetch wrappers for `/api/v1/system/*` and `/api/v1/settings` (DESIGN section 3.3, section 3.4).
 *
 * These go through the GENERATED, typed client now. They used to go through a `raw<T>()` helper that
 * cast the path and method to `never` — one deliberate hole punched through `request()`'s
 * compile-time checking, because `api/openapi.json` did not know these paths existed. It does now
 * (`internal/api/system.go` and `settings.go` register them, and the document is a projection of the
 * route registry), so the hole is closed and section 4's "the types can never lie about the API"
 * holds for this area like every other.
 */

import { api } from '../../api/client';
import type { ListPage } from '../../api/types';
import type {
  Capabilities,
  DiskUsageEntry,
  Gpu,
  JournalLine,
  PatchSettingsResponse,
  ResetSettingsResponse,
  RestartResponse,
  SettingsResponse,
  SystemInfo,
  SystemNotification,
  ToolchainCheck,
  UnitStatus,
} from './types';
import type { JobReceipt } from '../../api/types';

/* -- list envelopes ---------------------------------------------------------- */
/* The Go generator emits its list envelope as `List[github.com/…]`, and openapi-typescript splits
 * that schema name on its slashes — so every `{items,total,next_cursor}` response degrades to `any`
 * in `schema.d.ts` however well typed it is on the wire. `asPage` is what restores the element type,
 * and `features/models/api.ts` has the same helper for the same reason. It is a narrowing of an
 * `unknown`, not a cast: a response that is not a page comes back as an empty one. */
function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

export function asPage<T>(value: unknown): ListPage<T> {
  if (Array.isArray(value)) return { items: value as T[], total: value.length, next_cursor: null };
  if (!isRecord(value) || !Array.isArray(value['items'])) return { items: [], total: 0 };
  const items = value['items'] as T[];
  const total = typeof value['total'] === 'number' ? value['total'] : items.length;
  const cursor = value['next_cursor'];
  return { items, total, next_cursor: typeof cursor === 'string' ? cursor : null };
}

/**
 * Drop `undefined` values, which `exactOptionalPropertyTypes` forbids writing into an optional key.
 * Constrained to `object` rather than `Record<string, unknown>`: a plain interface — most of the
 * params types in this app — has no index signature and is not itself assignable to `Record`, even
 * though every one of its properties is.
 */
export function compact<T extends object>(input: T): { [K in keyof T]?: Exclude<T[K], undefined> } {
  const out: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(input)) {
    if (value !== undefined) out[key] = value;
  }
  return out as { [K in keyof T]?: Exclude<T[K], undefined> };
}

/** `{id, aria-describedby?, aria-invalid?}` from a `FormField` render prop, for a `Select` — whose
 * own props declare those two without `| undefined`, unlike the DOM attributes React ships with. */
export function selectFieldProps(field: {
  id: string;
  'aria-describedby': string | undefined;
  'aria-invalid': boolean | undefined;
}): { id: string; 'aria-describedby'?: string; 'aria-invalid'?: boolean } {
  return {
    id: field.id,
    ...(field['aria-describedby'] !== undefined
      ? { 'aria-describedby': field['aria-describedby'] }
      : {}),
    ...(field['aria-invalid'] !== undefined ? { 'aria-invalid': field['aria-invalid'] } : {}),
  };
}

/* -- system ------------------------------------------------------------------ */

export const systemApi = {
  info: (signal?: AbortSignal): Promise<SystemInfo> =>
    api.get('/api/v1/system/info', { ...(signal ? { signal } : {}) }),

  capabilities: (signal?: AbortSignal): Promise<Capabilities> =>
    api.get('/api/v1/system/capabilities', { ...(signal ? { signal } : {}) }),

  toolchain: async (signal?: AbortSignal): Promise<ToolchainCheck[]> =>
    asPage<ToolchainCheck>(
      await api.get('/api/v1/system/toolchain', { ...(signal ? { signal } : {}) }),
    ).items,

  probeToolchain: (): Promise<JobReceipt> =>
    api.post('/api/v1/system/toolchain/probe') as Promise<JobReceipt>,

  gpus: async (signal?: AbortSignal): Promise<Gpu[]> =>
    asPage<Gpu>(await api.get('/api/v1/system/gpus', { ...(signal ? { signal } : {}) })).items,

  disk: async (signal?: AbortSignal): Promise<DiskUsageEntry[]> =>
    asPage<DiskUsageEntry>(await api.get('/api/v1/system/disk', { ...(signal ? { signal } : {}) }))
      .items,

  units: async (signal?: AbortSignal): Promise<UnitStatus[]> =>
    asPage<UnitStatus>(await api.get('/api/v1/system/units', { ...(signal ? { signal } : {}) }))
      .items,

  journal: async (
    params: { unit?: string; lines?: number },
    signal?: AbortSignal,
  ): Promise<JournalLine[]> =>
    asPage<JournalLine>(
      await api.get('/api/v1/system/journal', {
        query: compact({
          ...(params.unit !== undefined ? { unit: params.unit } : {}),
          ...(params.lines !== undefined ? { lines: params.lines } : {}),
        }),
        ...(signal ? { signal } : {}),
      }),
    ).items,

  notifications: async (signal?: AbortSignal): Promise<SystemNotification[]> =>
    asPage<SystemNotification>(
      await api.get('/api/v1/system/notifications', { ...(signal ? { signal } : {}) }),
    ).items,

  dismissNotification: (id: string): Promise<void> =>
    api.post('/api/v1/system/notifications/{id}/dismiss', { path: { id } }),

  restart: (): Promise<RestartResponse> => api.post('/api/v1/system/restart'),

  /**
   * The D50 diagnostics bundle (section 11.3, section 4 screen 16).
   *
   * It is a plain browser navigation rather than a fetch, and that is the point: the response is an
   * archive with a `Content-Disposition`, so letting the browser handle it gets the user a file with
   * the daemon's own filename and no blob URL to revoke. The session cookie rides along exactly as
   * it does for every other request.
   */
  diagnosticsUrl: (): string => '/api/v1/system/diagnostics',
};

/* -- settings ------------------------------------------------------------------ */

export const settingsApi = {
  get: (signal?: AbortSignal): Promise<SettingsResponse> =>
    api.get('/api/v1/settings', { ...(signal ? { signal } : {}) }),

  patch: (values: Record<string, unknown>): Promise<PatchSettingsResponse> =>
    api.patch('/api/v1/settings', { body: values as never }),

  reset: (keys: string[]): Promise<ResetSettingsResponse> =>
    api.post('/api/v1/settings/reset', { body: { keys } }),
};
