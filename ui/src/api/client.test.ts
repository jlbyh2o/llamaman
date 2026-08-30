/**
 * The client's three cross-cutting behaviors (DESIGN section 3): the error envelope, double-submit
 * CSRF, and the two codes the shell reacts to structurally.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, buildPath, buildQuery, CSRF_HEADER, onAuthEvent, request } from './client';
import type { AuthEvent } from './client';
import { ApiError, TransportError } from './errors';

type FetchCall = { url: string; init: RequestInit };

let calls: FetchCall[] = [];

function respond(
  status: number,
  body: unknown,
  headers: Record<string, string> = { 'Content-Type': 'application/json' },
): Response {
  const text = typeof body === 'string' ? body : JSON.stringify(body);
  return new Response(status === 204 ? null : text, { status, headers });
}

function mockFetch(handler: (call: FetchCall) => Response | Promise<Response>) {
  vi.stubGlobal('fetch', (url: string, init: RequestInit) => {
    calls.push({ url, init });
    return Promise.resolve(handler({ url, init }));
  });
}

/** Assert the call rejected with an ApiError, and hand it back narrowed. */
async function expectApiError(promise: Promise<unknown>): Promise<ApiError> {
  const outcome = await promise.then(
    () => null,
    (error: unknown) => error,
  );
  expect(outcome).toBeInstanceOf(ApiError);
  return outcome as ApiError;
}

beforeEach(() => {
  calls = [];
  // The client reads `lm_csrf` from document.cookie; the node environment has no document, so this
  // is the smallest stand-in that exercises the real code path.
  vi.stubGlobal('document', { cookie: 'other=1; lm_csrf=csrf-token-value' });
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('URL building', () => {
  it('substitutes and encodes a simple path parameter', () => {
    expect(buildPath('/api/v1/instances/{id}', { id: 'inst 1' })).toBe(
      '/api/v1/instances/inst%201',
    );
  });

  it('keeps the slashes inside a multi-segment repo wildcard', () => {
    // Section 3.6 puts the verb in front of `{repo...}` so the id stays readable; the separators
    // must survive encoding or the route no longer matches.
    expect(buildPath('/api/v1/hf/tree/{repo...}', { repo: 'bartowski/Qwen3-8B-GGUF' })).toBe(
      '/api/v1/hf/tree/bartowski/Qwen3-8B-GGUF',
    );
  });

  it('refuses to send a URL with an unfilled placeholder', () => {
    expect(() => buildPath('/api/v1/instances/{id}', {})).toThrow(/missing path parameter/);
  });

  it('drops absent query values and repeats arrays', () => {
    expect(buildQuery({ state: 'active', q: undefined, gpu: ['a', 'b'] })).toBe(
      '?state=active&gpu=a&gpu=b',
    );
    expect(buildQuery(undefined)).toBe('');
    expect(buildQuery({})).toBe('');
  });
});

describe('requests', () => {
  it('sends no CSRF header on a GET', async () => {
    mockFetch(() => respond(200, { version: '1.0.0' }));
    await api.get('/api/v1/meta');
    const headers = calls[0]!.init.headers as Record<string, string>;
    expect(headers[CSRF_HEADER]).toBeUndefined();
    expect(calls[0]!.init.credentials).toBe('same-origin');
  });

  it('echoes the lm_csrf cookie on every non-GET', async () => {
    mockFetch(() => respond(204, null));
    await api.post('/api/v1/auth/login', { body: { password: 'hunter2' } });
    const headers = calls[0]!.init.headers as Record<string, string>;
    expect(headers[CSRF_HEADER]).toBe('csrf-token-value');
    expect(headers['Content-Type']).toBe('application/json');
    expect(calls[0]!.init.body).toBe('{"password":"hunter2"}');
  });

  it('passes an Idempotency-Key through when one is given', async () => {
    mockFetch(() => respond(202, { job_id: 'j1', subject: { type: 'download', id: 'd1' } }));
    await request('post', '/api/v1/downloads', {
      body: { repo_id: 'a/b', files: ['x.gguf'] },
      idempotencyKey: 'key-1',
    });
    const headers = calls[0]!.init.headers as Record<string, string>;
    expect(headers['Idempotency-Key']).toBe('key-1');
  });

  it('resolves a 204 as undefined rather than crashing on an empty body', async () => {
    mockFetch(() => respond(204, null));
    await expect(api.post('/api/v1/auth/logout')).resolves.toBeUndefined();
  });

  it('returns text for a non-JSON success, which is what the log endpoints send', async () => {
    mockFetch(() => respond(200, 'line one\nline two', { 'Content-Type': 'text/plain' }));
    const body = await api.get('/api/v1/llamacpp/versions/{id}/log', { path: { id: 'v1' } });
    expect(body).toBe('line one\nline two');
  });
});

describe('errors', () => {
  it('turns the envelope into an ApiError carrying the closed code', async () => {
    mockFetch(() =>
      respond(409, {
        error: {
          code: 'model_in_use',
          message: 'qwen is serving traffic',
          details: { instances: ['inst-1'] },
        },
      }),
    );
    const error = await expectApiError(api.delete('/api/v1/models/{id}', { path: { id: 'm1' } }));
    expect(error.code).toBe('model_in_use');
    expect(error.status).toBe(409);
    expect(error.details['instances']).toEqual(['inst-1']);
    expect(error.request).toBe('DELETE /api/v1/models/{id}');
  });

  it('normalizes both retry hints to milliseconds', async () => {
    mockFetch(() =>
      respond(429, {
        error: { code: 'locked_out', message: 'too many', details: { retry_after_sec: 30 } },
      }),
    );
    const locked = await expectApiError(
      api.post('/api/v1/auth/login', { body: { password: 'x' } }),
    );
    expect(locked.retryAfterMs).toBe(30_000);

    calls = [];
    mockFetch(() =>
      respond(429, {
        error: { code: 'restart_rate_limited', message: 'wait', details: { retry_after_ms: 4500 } },
      }),
    );
    const limited = await expectApiError(api.post('/api/v1/update/check'));
    expect(limited.retryAfterMs).toBe(4500);
  });

  it('surfaces the remediation commands a degraded-mode 409 carries', async () => {
    mockFetch(() =>
      respond(409, {
        error: {
          code: 'systemd_denied',
          message: 'the manage-units grant was refused',
          details: {
            hints: ['sudo systemctl restart llamaman.service', 'sudo llamaman install-units'],
          },
        },
      }),
    );
    const error = await expectApiError(api.post('/api/v1/update/check'));
    expect(error.hints).toHaveLength(2);
  });

  it('does not pretend a non-JSON failure is an envelope', async () => {
    mockFetch(() => respond(502, '<html>bad gateway</html>', { 'Content-Type': 'text/html' }));
    const error = await expectApiError(api.get('/api/v1/meta'));
    expect(error.code).toBe('http_502');
  });

  it('reports a network failure as a transport error, not as a rejection from the daemon', async () => {
    vi.stubGlobal('fetch', () => Promise.reject(new Error('ECONNREFUSED')));
    const error = await api.get('/api/v1/meta').then(
      () => null,
      (e: unknown) => e,
    );
    expect(error).toBeInstanceOf(TransportError);
  });
});

describe('the two structural codes', () => {
  it('broadcasts unauthorized on a 401 so the shell can route to /login', async () => {
    const seen: AuthEvent[] = [];
    const unsubscribe = onAuthEvent((event) => seen.push(event));
    mockFetch(() => respond(401, { error: { code: 'unauthorized', message: 'no session' } }));
    await api.get('/api/v1/instances').catch(() => undefined);
    unsubscribe();
    expect(seen).toEqual(['unauthorized']);
  });

  it('broadcasts setup-required on the 409 the SPA routes to the wizard on', async () => {
    const seen: AuthEvent[] = [];
    const unsubscribe = onAuthEvent((event) => seen.push(event));
    mockFetch(() => respond(409, { error: { code: 'setup_required', message: 'not claimed' } }));
    await api.get('/api/v1/instances').catch(() => undefined);
    unsubscribe();
    expect(seen).toEqual(['setup-required']);
  });

  it('stops broadcasting once unsubscribed', async () => {
    const seen: AuthEvent[] = [];
    onAuthEvent((event) => seen.push(event))();
    mockFetch(() => respond(401, { error: { code: 'unauthorized', message: 'no session' } }));
    await api.get('/api/v1/instances').catch(() => undefined);
    expect(seen).toEqual([]);
  });
});
