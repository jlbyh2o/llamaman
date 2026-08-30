/**
 * The typed fetch client.
 *
 * Every request the SPA makes goes through `request()`, which is typed off the generated
 * `paths` — the path, its method, its path params, its query params, its body and its success
 * response all come from api/openapi.json, so a route that changes shape is a compile error rather
 * than a runtime surprise (DESIGN section 4, D43).
 *
 * It also owns the three cross-cutting behaviors of DESIGN section 3:
 *
 *  - **Error envelope.** Any non-2xx becomes an `ApiError` with the closed `code`, so callers
 *    branch on `err.code` and never on a message.
 *  - **CSRF.** Double-submit: login sets a non-HttpOnly `lm_csrf` cookie, and every non-GET echoes
 *    it in `X-CSRF-Token`.
 *  - **The two structural codes.** `401 unauthorized` and `409 setup_required` are broadcast to the
 *    shell, which sends the browser to `/login` or `/setup`. They are broadcast rather than acted on
 *    here so that this module never imports the router and stays testable without a DOM history.
 */

import { ApiError, TransportError, SETUP_REQUIRED, UNAUTHORIZED } from './errors';
import type { ErrorBody } from './errors';
import type { paths } from './schema';

/* -------------------------------------------------------------------------- */
/* Type plumbing over the generated schema                                     */
/* -------------------------------------------------------------------------- */

export type HttpMethod = 'get' | 'put' | 'post' | 'delete' | 'patch';

/** Paths that actually declare `M`. Absent methods are generated as `M?: never`, i.e. `undefined`. */
export type PathsWithMethod<M extends HttpMethod> = {
  [P in keyof paths]: paths[P] extends Record<M, unknown>
    ? paths[P][M] extends undefined
      ? never
      : P
    : never;
}[keyof paths];

export type Operation<P extends keyof paths, M extends HttpMethod> = M extends keyof paths[P]
  ? paths[P][M]
  : never;

type PathParamsOf<Op> = Op extends { parameters: { path?: infer T } } ? T : undefined;
type QueryOf<Op> = Op extends { parameters: { query?: infer T } } ? T : undefined;
type BodyOf<Op> = Op extends { requestBody?: { content: infer C } }
  ? C extends { 'application/json': infer B }
    ? B
    : undefined
  : undefined;

type SuccessStatus = '200' | '201' | '202' | '204';
type ResponsesOf<Op> = Op extends { responses: infer R } ? R : never;
type SuccessOf<Op> = ResponsesOf<Op>[Extract<keyof ResponsesOf<Op>, SuccessStatus>];

/**
 * The body of a successful response. A `204` is generated as `{ content?: never }`, which fails the
 * `{ content: … }` check and lands on `undefined` — exactly what `request()` resolves with.
 */
export type ResponseData<Op> = SuccessOf<Op> extends { content: infer C } ? C[keyof C] : undefined;

/** Convenience alias: the success body of `GET /api/v1/instances` is `Data<'/api/v1/instances'>`. */
export type Data<P extends PathsWithMethod<'get'>> = ResponseData<Operation<P, 'get'>>;

interface CommonOptions {
  signal?: AbortSignal;
  /**
   * Job-creating POSTs accept `Idempotency-Key` (D39/D65): a repeat inside the ten-minute window
   * returns the original job with `200` instead of starting a second one.
   */
  idempotencyKey?: string;
  headers?: Record<string, string>;
}

/** Keys of `T` that a caller must supply. Drives which options are required, and whether any are. */
type RequiredKeysOf<T> = Exclude<
  { [K in keyof T]: T extends Record<K, T[K]> ? K : never }[keyof T],
  undefined
>;

/**
 * `path`, `query` and `body` are each required exactly when the operation cannot be called without
 * them: absent from the schema means `?: never`, present-but-entirely-optional (every query param
 * optional, which is most of them) means optional, and anything with a required member means
 * required. So `api.get('/api/v1/instances')` compiles and
 * `api.get('/api/v1/instances/{id}')` does not.
 */
type MaybeRequired<K extends string, T> = [T] extends [undefined]
  ? { [P in K]?: never }
  : undefined extends T
    ? { [P in K]?: T }
    : [RequiredKeysOf<T>] extends [never]
      ? { [P in K]?: T }
      : { [P in K]: T };

export type RequestOptions<Op> = CommonOptions &
  MaybeRequired<'path', PathParamsOf<Op>> &
  MaybeRequired<'query', QueryOf<Op>> &
  MaybeRequired<'body', BodyOf<Op>>;

type OptionsArg<Op> =
  RequiredKeysOf<RequestOptions<Op>> extends never
    ? [options?: RequestOptions<Op>]
    : [options: RequestOptions<Op>];

/* -------------------------------------------------------------------------- */
/* Cross-cutting behavior                                                       */
/* -------------------------------------------------------------------------- */

export type AuthEvent = 'unauthorized' | 'setup-required';
type AuthListener = (event: AuthEvent) => void;

const authListeners = new Set<AuthListener>();

/**
 * Subscribe to the two structural responses. The shell uses this to navigate; nothing else should.
 * Returns an unsubscribe function.
 */
export function onAuthEvent(listener: AuthListener): () => void {
  authListeners.add(listener);
  return () => authListeners.delete(listener);
}

function emitAuthEvent(event: AuthEvent): void {
  for (const listener of [...authListeners]) {
    try {
      listener(event);
    } catch {
      // A misbehaving listener must not turn a 401 into an unhandled rejection.
    }
  }
}

/** Read a cookie by name. `lm_csrf` is deliberately not HttpOnly so this can find it. */
export function readCookie(name: string): string | null {
  if (typeof document === 'undefined') return null;
  const prefix = `${name}=`;
  for (const part of document.cookie.split('; ')) {
    if (part.startsWith(prefix)) return decodeURIComponent(part.slice(prefix.length));
  }
  return null;
}

export const CSRF_COOKIE = 'lm_csrf';
export const CSRF_HEADER = 'X-CSRF-Token';

/** Where the API lives. Same origin always: the SPA is served by the daemon that answers it. */
export const API_BASE = '/api/v1';

/* -------------------------------------------------------------------------- */
/* URL building                                                                 */
/* -------------------------------------------------------------------------- */

/**
 * Substitute `{name}` placeholders.
 *
 * Repo ids contain a slash (`bartowski/Qwen3-8B-GGUF`), which is why section 3.6 puts the verb in
 * front of a `{repo...}` multi-segment wildcard. Those params must therefore keep their slashes:
 * each segment is encoded, the separators are not.
 */
export function buildPath(template: string, params: Record<string, unknown> | undefined): string {
  return template.replace(/\{([^}.]+)(\.\.\.)?\}/g, (_all, name: string, splat?: string) => {
    const value = params?.[name];
    if (value === undefined || value === null || value === '') {
      throw new TypeError(`missing path parameter "${name}" for ${template}`);
    }
    const raw = String(value);
    return splat ? raw.split('/').map(encodeURIComponent).join('/') : encodeURIComponent(raw);
  });
}

/** Query string from a param object. Arrays repeat the key; `undefined` and `null` are dropped. */
export function buildQuery(query: Record<string, unknown> | undefined): string {
  if (!query) return '';
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === null) continue;
    if (Array.isArray(value)) {
      for (const v of value) {
        if (v !== undefined && v !== null) search.append(key, String(v));
      }
    } else {
      search.append(key, String(value));
    }
  }
  const s = search.toString();
  return s ? `?${s}` : '';
}

/* -------------------------------------------------------------------------- */
/* The request                                                                  */
/* -------------------------------------------------------------------------- */

function parseErrorBody(status: number, text: string): ErrorBody {
  try {
    const parsed = JSON.parse(text) as { error?: Partial<ErrorBody> };
    const err = parsed.error;
    if (err && typeof err.code === 'string') {
      return {
        code: err.code,
        message: typeof err.message === 'string' ? err.message : err.code,
        ...(err.details && typeof err.details === 'object'
          ? { details: err.details as Record<string, unknown> }
          : {}),
      };
    }
  } catch {
    // Fall through: a proxy or a crash can produce a non-JSON body.
  }
  return { code: `http_${status}`, message: text.trim().slice(0, 300) || `HTTP ${status}` };
}

async function readBody(res: Response): Promise<unknown> {
  if (res.status === 204 || res.headers.get('Content-Length') === '0') return undefined;
  const type = res.headers.get('Content-Type') ?? '';
  if (type.includes('application/json')) {
    const text = await res.text();
    if (!text) return undefined;
    return JSON.parse(text) as unknown;
  }
  return res.text();
}

export async function request<M extends HttpMethod, P extends PathsWithMethod<M>>(
  method: M,
  path: P,
  ...args: OptionsArg<Operation<P, M>>
): Promise<ResponseData<Operation<P, M>>> {
  type Options = CommonOptions & {
    path?: Record<string, unknown>;
    query?: Record<string, unknown>;
    body?: unknown;
  };
  const options = (args[0] ?? {}) as Options;

  const url = `${buildPath(path as string, options.path)}${buildQuery(options.query)}`;
  const label = `${method.toUpperCase()} ${path as string}`;

  const headers: Record<string, string> = { Accept: 'application/json', ...options.headers };
  const hasBody = options.body !== undefined;
  if (hasBody) headers['Content-Type'] = 'application/json';

  // Double-submit CSRF (section 3): every non-GET echoes the lm_csrf cookie.
  if (method !== 'get') {
    const token = readCookie(CSRF_COOKIE);
    if (token) headers[CSRF_HEADER] = token;
  }
  if (options.idempotencyKey) headers['Idempotency-Key'] = options.idempotencyKey;

  let res: Response;
  try {
    res = await fetch(url, {
      method: method.toUpperCase(),
      headers,
      credentials: 'same-origin',
      ...(hasBody ? { body: JSON.stringify(options.body) } : {}),
      ...(options.signal ? { signal: options.signal } : {}),
    });
  } catch (cause) {
    if (cause instanceof DOMException && cause.name === 'AbortError') throw cause;
    throw new TransportError('the daemon is unreachable', label, { cause });
  }

  if (!res.ok) {
    const body = parseErrorBody(res.status, await res.text().catch(() => ''));
    if (res.status === 401 || body.code === UNAUTHORIZED) emitAuthEvent('unauthorized');
    else if (body.code === SETUP_REQUIRED) emitAuthEvent('setup-required');
    throw new ApiError(res.status, body, label);
  }

  try {
    return (await readBody(res)) as ResponseData<Operation<P, M>>;
  } catch (cause) {
    throw new TransportError('the response body was not valid JSON', label, { cause });
  }
}

/* Method-shaped conveniences. `api.get('/api/v1/instances')` reads better at a call site than
 * `request('get', '/api/v1/instances')`, and the types are identical. */
export const api = {
  get: <P extends PathsWithMethod<'get'>>(
    path: P,
    ...args: OptionsArg<Operation<P, 'get'>>
  ): Promise<ResponseData<Operation<P, 'get'>>> => request('get', path, ...args),

  post: <P extends PathsWithMethod<'post'>>(
    path: P,
    ...args: OptionsArg<Operation<P, 'post'>>
  ): Promise<ResponseData<Operation<P, 'post'>>> => request('post', path, ...args),

  put: <P extends PathsWithMethod<'put'>>(
    path: P,
    ...args: OptionsArg<Operation<P, 'put'>>
  ): Promise<ResponseData<Operation<P, 'put'>>> => request('put', path, ...args),

  patch: <P extends PathsWithMethod<'patch'>>(
    path: P,
    ...args: OptionsArg<Operation<P, 'patch'>>
  ): Promise<ResponseData<Operation<P, 'patch'>>> => request('patch', path, ...args),

  delete: <P extends PathsWithMethod<'delete'>>(
    path: P,
    ...args: OptionsArg<Operation<P, 'delete'>>
  ): Promise<ResponseData<Operation<P, 'delete'>>> => request('delete', path, ...args),
} as const;
