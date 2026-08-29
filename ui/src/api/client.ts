/**
 * Stub API client. The real one is generated against api/openapi.json by
 * `make openapi` (openapi-typescript in CI, never at runtime — DESIGN section
 * 4), so the hand-written shapes below are placeholders that the generated
 * schema will replace.
 */

/** Response of GET /api/v1/meta — what install.sh polls (DESIGN section 3.1). */
export interface Meta {
  version: string;
  commit: string;
  setup_complete: boolean;
  claimed: boolean;
  ui_port: number;
}

/** The error envelope every non-2xx response carries (DESIGN section 3). */
export interface ApiError {
  error: { code: string; message: string; details?: Record<string, unknown> };
}

const BASE = '/api/v1';

async function get<T>(path: string): Promise<T> {
  const res = await fetch(`${BASE}${path}`, {
    headers: { Accept: 'application/json' },
    credentials: 'same-origin',
  });
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as ApiError | null;
    throw new Error(body?.error.message ?? `${res.status} ${res.statusText}`);
  }
  return (await res.json()) as T;
}

export function getMeta(): Promise<Meta> {
  return get<Meta>('/meta');
}
