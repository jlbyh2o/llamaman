/**
 * The error envelope, as one thrown value.
 *
 * DESIGN section 3: every non-2xx carries `{"error":{"code","message","details"}}` with a matching
 * status, and the codes are a closed enum in internal/model. The client turns that into an
 * `ApiError`, so a screen branches on `err.code` — a stable contract — rather than on a status or a
 * message string.
 */

export interface ErrorBody {
  code: string;
  message: string;
  details?: Record<string, unknown>;
}

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly details: Record<string, unknown>;
  /** The request that produced it, for log correlation: `GET /api/v1/instances`. */
  readonly request: string;

  constructor(status: number, body: ErrorBody, request: string) {
    super(body.message || `${status}`);
    this.name = 'ApiError';
    this.status = status;
    this.code = body.code;
    this.details = body.details ?? {};
    this.request = request;
  }

  /**
   * The retry hint two endpoints carry, normalized to milliseconds: `retry_after_sec` on
   * `429 locked_out` (section 3.1) and `retry_after_ms` on `429 restart_rate_limited` (D93).
   * `null` when the response named neither.
   */
  get retryAfterMs(): number | null {
    const ms = this.details['retry_after_ms'];
    if (typeof ms === 'number' && Number.isFinite(ms)) return ms;
    const sec = this.details['retry_after_sec'];
    if (typeof sec === 'number' && Number.isFinite(sec)) return sec * 1000;
    return null;
  }

  /**
   * The remediation commands a degraded-mode 409 carries — `hints` on the instance-delete response,
   * and the `sudo systemctl …` / `sudo llamaman install-units …` lines section 3.3 attaches to
   * `systemd_denied`, `restart_unavailable`, `autostart_unavailable` and friends.
   */
  get hints(): string[] {
    const raw = this.details['hints'];
    if (Array.isArray(raw)) return raw.filter((h): h is string => typeof h === 'string');
    const one = this.details['hint'];
    return typeof one === 'string' ? [one] : [];
  }

  toString(): string {
    return `${this.name}: ${this.code} (${this.status}) — ${this.message}`;
  }
}

export function isApiError(err: unknown): err is ApiError {
  return err instanceof ApiError;
}

/** True when `err` is an ApiError carrying any of `codes`. */
export function hasCode(err: unknown, ...codes: string[]): boolean {
  return isApiError(err) && codes.includes(err.code);
}

/**
 * The two codes the shell reacts to structurally rather than by rendering a message.
 *
 * `setup_required` is section 3's setup gate: "the SPA routes to the wizard on that code alone, so
 * there is no separate 'is it configured' flag to keep in sync". `unauthorized` is a session that
 * expired underneath an open tab.
 */
export const SETUP_REQUIRED = 'setup_required';
export const UNAUTHORIZED = 'unauthorized';

/** A network failure or an unparseable body — never a response the daemon meant to send. */
export class TransportError extends Error {
  readonly request: string;
  constructor(message: string, request: string, options?: { cause?: unknown }) {
    super(message, options);
    this.name = 'TransportError';
    this.request = request;
  }
}
