/**
 * The daemon's refusals, landed on the field that caused them.
 *
 * Every code here is one the create/patch routes declare (section 3.10's error tables), and each
 * has a field the user can actually act on. A refusal shown only as a toast makes someone hunt
 * through sixty fields for the one that is wrong; a refusal on the field is the same information at
 * the place where it can be fixed.
 *
 * `conflict_generation` is the exception and is deliberately not a field error: nothing the user
 * typed is wrong. Someone edited the configuration underneath them, which is a different sentence
 * and a different remedy — reload, look at what changed, decide again (section 3's PATCH contract).
 */

import { ApiError } from '../../api/errors';
import type { InstanceFormValues } from './values';

export interface FieldRefusal {
  field: keyof InstanceFormValues;
  message: string;
}

export interface RefusalReport {
  /** Per-field messages to hand to `setError`. */
  fields: FieldRefusal[];
  /** A message for the banner: nothing to point at, or a stale-generation conflict. */
  banner?: string;
  /** True for `409 conflict_generation` — the form offers a reload rather than a retry. */
  stale?: boolean;
}

export function refusalReport(
  error: unknown,
  /** The internal pool, which is what tells the two port fields apart in a `port_unavailable`. */
  pool: { min: number; max: number } = { min: 21000, max: 21999 },
): RefusalReport {
  if (!(error instanceof ApiError)) {
    return error === undefined || error === null
      ? { fields: [] }
      : {
          fields: [],
          banner: 'The daemon did not answer. Check that llamaman.service is running.',
        };
  }

  switch (error.code) {
    case 'instance_name_taken':
      return {
        fields: [
          {
            field: 'name',
            message: 'Another live instance already has this name.',
          },
        ],
      };

    case 'instance_name_invalid':
      return { fields: [{ field: 'name', message: error.message }] };

    case 'port_unavailable': {
      // `details` carries `{"port":N,"reason":…}`; the port says which of the two fields it was.
      const port = error.details['port'];
      const field =
        typeof port === 'number' && port >= pool.min && port <= pool.max
          ? 'internal_port'
          : 'public_port';
      return { fields: [{ field, message: error.message }] };
    }

    case 'draft_vocab_mismatch':
      return { fields: [{ field: 'draft_model_id', message: error.message }] };

    case 'ngl_auto_conflict':
      return { fields: [{ field: 'tensor_split', message: error.message }] };

    case 'extra_flag_forbidden':
      return { fields: [{ field: 'extra_flags', message: error.message }] };

    case 'model_missing':
      return { fields: [{ field: 'model_id', message: error.message }] };

    case 'bad_flags':
      return { fields: [], banner: error.message };

    case 'conflict_generation':
      return {
        fields: [],
        stale: true,
        banner:
          'Someone else changed this instance while you were editing it. Reload to see their ' +
          'version before saving yours.',
      };

    default:
      return { fields: [], banner: error.message };
  }
}
