/**
 * Port hints for the form.
 *
 * `GET /api/v1/ports/suggest` is the daemon's own answer and the form asks for it first; this is
 * what the field shows while that request is in flight, and what it falls back to on a daemon that
 * does not serve the route yet. The walk is the same one `instances.SuggestPort` performs — the
 * rules of `checkPort`, applied from a base until one passes — minus the bind probe, which no
 * browser can do. A suggestion the save would refuse is worse than no suggestion, so every
 * candidate goes through the same predicate the resolver uses.
 */

import { checkPort } from './schema';
import type { FormContext, PortKind, PortVerdict } from './schema';
import type { Instance } from '../../api/types';

/** `instances.DefaultPublicPortBase`: the first suggestion is 8080, the next 8081, and so on. */
export const PUBLIC_PORT_BASE = 8080;

/** `instances.PortScanLimit`, for the same reason: a walk must end. */
export const PORT_SCAN_LIMIT = 2048;

/**
 * Every live instance's claim on both ports.
 *
 * Soft-deleted rows hold nothing — all three unique indexes are partial over `deleted_at` (D68) —
 * so a list fetched with `?include_deleted=true` must not contribute its deleted rows here.
 */
export function portClaims(instances: readonly Instance[]) {
  return instances
    .filter((instance) => !instance.deleted_at)
    .map((instance) => ({
      instance_id: instance.id,
      name: instance.name,
      public_port: instance.public_port,
      internal_port: instance.internal_port,
    }));
}

/** The first port of this kind that passes every rule this side can check, or null. */
export function suggestPort(kind: PortKind, ctx: FormContext): number | null {
  const first = kind === 'public' ? PUBLIC_PORT_BASE : ctx.internalPortMin;
  const last = kind === 'public' ? 65535 : ctx.internalPortMax;
  for (
    let port = first, tried = 0;
    port <= last && tried < PORT_SCAN_LIMIT;
    port += 1, tried += 1
  ) {
    if (checkPort(kind, port, ctx).ok) return port;
  }
  return null;
}

export interface PortHint {
  tone: 'ok' | 'warn' | 'danger';
  message: string;
}

/**
 * The line under a port field.
 *
 * "Free" is deliberately hedged: another process can take the port between this hint and the
 * listen, which is why F6 exists as a runtime fallback and why the daemon probes again at save
 * time. The hint reports what is *known* — the rules over rows — and says the rest is checked when
 * you save.
 */
export function portHint(kind: PortKind, raw: string, ctx: FormContext): PortHint | null {
  const trimmed = raw.trim();
  if (trimmed === '') {
    return {
      tone: 'ok',
      message: 'Left blank, the daemon allocates one for you.',
    };
  }
  const port = Number(trimmed);
  if (!Number.isInteger(port)) return null;

  const verdict: PortVerdict = checkPort(kind, port, ctx);
  if (verdict.ok) {
    return {
      tone: 'ok',
      message:
        kind === 'public'
          ? 'No other instance holds this port. The daemon binds it when the instance starts.'
          : 'Inside the internal pool and unclaimed.',
    };
  }
  return {
    tone: verdict.reason === 'in_use_by_instance' ? 'danger' : 'warn',
    message: verdict.message ?? 'This port cannot be used.',
  };
}
