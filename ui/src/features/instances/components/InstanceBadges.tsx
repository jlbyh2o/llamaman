/**
 * The four derived flags of section 2.8, as badges beside the state — never as a state.
 *
 * "An instance that is serving traffic cannot also be in a state that excludes it from every
 * ready-gated behavior", so `restart_required`, `stale_version`, `inhibited` and `draft_unverified`
 * are computed on read and rendered *next to* `ready`, not instead of it. `draft_validation` gets
 * one more badge than the flag does: `mismatch` is an error state the launcher refuses to start on
 * (exit 65), which is a different thing from the deferred check the flag reports.
 */

import { FlagBadge } from '../../../components';
import type { Instance } from '../../../api/types';

export function InstanceBadges({
  instance,
  className,
}: {
  instance: Instance;
  className?: string;
}) {
  const badges: { flag: string; reason?: string }[] = [];
  if (instance.restart_required) badges.push({ flag: 'restart_required' });
  if (instance.stale_version) badges.push({ flag: 'stale_version' });
  if (instance.inhibited) {
    badges.push({
      flag: 'inhibited',
      ...(instance.inhibit_reason ? { reason: instance.inhibit_reason } : {}),
    });
  }
  if (instance.draft_validation === 'mismatch') badges.push({ flag: 'draft_mismatch' });
  else if (instance.draft_unverified) badges.push({ flag: 'draft_unverified' });

  if (badges.length === 0) return null;
  return (
    <span className={className}>
      {badges.map((badge) => (
        <FlagBadge
          key={badge.flag}
          flag={badge.flag}
          {...(badge.reason === undefined ? {} : { reason: badge.reason })}
          className="mr-1"
        />
      ))}
    </span>
  );
}
