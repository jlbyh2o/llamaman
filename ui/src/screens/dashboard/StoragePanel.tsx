import { Link } from '@tanstack/react-router';
import { Badge, Meter, Panel, PanelHeader } from '../../components';
import type { CacheRoot, LlamacppVersion } from '../../api/types';
import { formatBytes, formatCount } from '../../format';

/**
 * The disk-usage summary of DESIGN section 4, screen 3.
 *
 * It is assembled from the two places bytes actually accumulate rather than from a single "how full
 * is this host" number, because those two are what a person can act on: the Hugging Face cache
 * roots (`GET /cache/roots`, which carry `bytes_on_disk`, `free_bytes` and `total_bytes` per root)
 * and the llama.cpp version directories (`GET /llamacpp/versions`, `size_bytes` each). A filesystem
 * meter without that split tells you a disk is full and nothing about what to delete.
 */
export function StoragePanel({
  roots,
  versions,
}: {
  roots: readonly CacheRoot[];
  versions: readonly LlamacppVersion[];
}) {
  const versionBytes = versions.reduce((sum, row) => sum + (row.size_bytes ?? 0), 0);
  const modelBytes = roots.reduce((sum, root) => sum + root.bytes_on_disk, 0);

  return (
    <Panel>
      <PanelHeader
        title="Storage"
        description={`${formatBytes(modelBytes)} of models, ${formatBytes(versionBytes)} of llama.cpp builds`}
        actions={
          <Link
            to="/system"
            className="text-xs text-[var(--lm-accent)] underline-offset-4 hover:underline"
          >
            Details
          </Link>
        }
      />

      <ul className="mt-3 space-y-3">
        {roots.map((root) => {
          const total = root.total_bytes ?? 0;
          const free = root.free_bytes ?? 0;
          return (
            <li key={root.id}>
              <div className="flex items-center gap-2">
                <span className="lm-numeric min-w-0 flex-1 truncate text-xs text-[var(--lm-text-muted)]">
                  {root.path}
                </span>
                {root.is_primary ? <Badge tone="accent">Primary</Badge> : null}
              </div>
              {total > 0 ? (
                <Meter
                  className="mt-1"
                  used={total - free}
                  total={total}
                  label={`${formatCount(root.models)} models · ${formatBytes(root.bytes_on_disk)}`}
                  detail={`${formatBytes(free)} free`}
                />
              ) : (
                <p className="mt-1 text-xs text-[var(--lm-text-faint)]">
                  {formatCount(root.models)} models · {formatBytes(root.bytes_on_disk)} — the
                  filesystem could not be measured
                </p>
              )}
            </li>
          );
        })}
        {roots.length === 0 ? (
          <li className="text-xs text-[var(--lm-text-faint)]">No cache root is registered.</li>
        ) : null}
      </ul>
    </Panel>
  );
}
