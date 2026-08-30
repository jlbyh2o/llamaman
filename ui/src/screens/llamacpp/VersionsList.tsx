/**
 * The installed-version table (DESIGN section 4 screen 12, section 3.5, D25, D71).
 *
 * "Rebuild recommended" is not a version column — section 6.4 raises it as a `notifications` row
 * naming the version (a host CPU flag or GPU set mismatch, D21) — so it is read here by matching
 * `GET /system/notifications` against each row's id rather than invented as a derived field the
 * backend does not carry yet.
 */

import { useState } from 'react';
import { AlertTriangle, Ban, Hammer, RotateCcw, Trash2 } from 'lucide-react';
import {
  Badge,
  Button,
  ConfirmDialog,
  DataTable,
  Mono,
  StatusBadge,
  Tooltip,
  toast,
} from '../../components';
import type { Column } from '../../components';
import { formatBytes, formatRelative, formatTimestamp } from '../../format';
import { useNotifications } from '../../features/system/queries';
import type { LlamacppVersion } from '../../api/types';
import { ActivateDialog } from './ActivateDialog';
import {
  useCancelLlamacpp,
  useDeleteLlamacpp,
  useInstallLlamacpp,
  useRetryLlamacpp,
} from './queries';

const LIVE_STATES = new Set(['pending', 'resolving', 'fetching', 'building', 'verifying']);
const RETRYABLE_STATES = new Set(['failed', 'failed_verification', 'canceled']);

export interface VersionsListProps {
  versions: readonly LlamacppVersion[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}

export function VersionsList({ versions, selectedId, onSelect }: VersionsListProps) {
  const [activating, setActivating] = useState<LlamacppVersion | null>(null);
  const [deleting, setDeleting] = useState<LlamacppVersion | null>(null);

  const notifications = useNotifications();
  const cancel = useCancelLlamacpp();
  const retry = useRetryLlamacpp();
  const del = useDeleteLlamacpp();
  const rebuild = useInstallLlamacpp();

  const rebuildRecommended = new Map(
    (notifications.data ?? [])
      .filter((n) => n.subject?.type === 'llamacpp_version' && !n.dismissed_at)
      .map((n) => [n.subject!.id, n.message]),
  );

  const deleteBlockReason = (row: LlamacppVersion): string | null => {
    if (row.is_active) return 'The active build cannot be deleted.';
    if (row.previous_active)
      return 'This is the rollback target — activating a different build frees it.';
    if (row.in_use_by.length > 0)
      return `In use by a running process (${row.in_use_by.join(', ')}).`;
    return null;
  };

  const columns: Column<LlamacppVersion>[] = [
    {
      id: 'tag',
      header: 'Version',
      sortValue: (row) => row.created_at,
      cell: (row) => (
        <div className="flex items-center gap-2">
          <Mono>{row.tag}</Mono>
          {row.is_active ? <Badge tone="accent">Active</Badge> : null}
          {row.previous_active ? <Badge tone="neutral">Rollback target</Badge> : null}
          {rebuildRecommended.has(row.id) ? (
            <Tooltip content={rebuildRecommended.get(row.id)}>
              <span>
                <Badge tone="warn" icon={<AlertTriangle />}>
                  Rebuild recommended
                </Badge>
              </span>
            </Tooltip>
          ) : null}
        </div>
      ),
    },
    {
      id: 'channel',
      header: 'Channel',
      cell: (row) => <span className="capitalize">{row.channel}</span>,
    },
    {
      id: 'backend',
      header: 'Backend',
      cell: (row) => <Mono>{row.backend}</Mono>,
      secondary: true,
    },
    {
      id: 'state',
      header: 'State',
      cell: (row) => (
        <StatusBadge
          kind="llamacpp"
          state={row.state}
          {...(row.failing_step ? { label: `Failed — ${row.failing_step}` } : {})}
        />
      ),
    },
    {
      id: 'size',
      header: 'Size',
      align: 'right',
      cell: (row) => <Mono>{formatBytes(row.size_bytes)}</Mono>,
      sortValue: (row) => row.size_bytes ?? 0,
      secondary: true,
    },
    {
      id: 'created_at',
      header: 'Created',
      cell: (row) => (
        <span title={formatTimestamp(row.created_at)}>{formatRelative(row.created_at)}</span>
      ),
      sortValue: (row) => row.created_at,
      secondary: true,
    },
    {
      id: 'actions',
      header: '',
      align: 'right',
      width: '1%',
      cell: (row) => (
        <div className="flex items-center justify-end gap-1.5">
          {LIVE_STATES.has(row.state) ? (
            <Button
              size="sm"
              variant="ghost"
              icon={<Ban />}
              loading={cancel.isPending}
              onClick={(e) => {
                e.stopPropagation();
                cancel.mutate(row.id, { onError: (err) => toast.error(err) });
              }}
            >
              Cancel
            </Button>
          ) : RETRYABLE_STATES.has(row.state) ? (
            <Button
              size="sm"
              variant="ghost"
              icon={<RotateCcw />}
              loading={retry.isPending}
              onClick={(e) => {
                e.stopPropagation();
                retry.mutate(row.id, { onError: (err) => toast.error(err) });
              }}
            >
              Retry
            </Button>
          ) : row.state === 'ready' && !row.is_active ? (
            <Button
              size="sm"
              variant="secondary"
              onClick={(e) => {
                e.stopPropagation();
                setActivating(row);
              }}
            >
              Activate
            </Button>
          ) : null}
          {rebuildRecommended.has(row.id) ? (
            <Button
              size="sm"
              variant="ghost"
              icon={<Hammer />}
              loading={rebuild.isPending}
              onClick={(e) => {
                e.stopPropagation();
                rebuild.mutate(
                  {
                    channel: row.channel as 'stable' | 'nightly' | 'custom',
                    tag: row.tag,
                    backend: row.backend as 'cpu' | 'cuda',
                    force_rebuild: true,
                  },
                  {
                    onSuccess: () => toast.success('Rebuild queued'),
                    onError: (err) => toast.error(err),
                  },
                );
              }}
            >
              Rebuild
            </Button>
          ) : null}
          <Tooltip content={deleteBlockReason(row) ?? 'Remove this build’s directory.'}>
            <span>
              <Button
                size="sm"
                variant="ghost"
                icon={<Trash2 />}
                disabled={deleteBlockReason(row) !== null}
                onClick={(e) => {
                  e.stopPropagation();
                  setDeleting(row);
                }}
                className="text-[var(--lm-danger)]"
              >
                Delete
              </Button>
            </span>
          </Tooltip>
        </div>
      ),
    },
  ];

  return (
    <>
      <DataTable
        columns={columns}
        rows={versions}
        rowKey={(row) => row.id}
        onRowClick={(row) => onSelect(row.id)}
        isRowActive={(row) => row.id === selectedId}
        caption="Installed llama.cpp builds"
      />

      {activating ? (
        <ActivateDialog
          open
          onOpenChange={(open) => !open && setActivating(null)}
          mode={{ kind: 'activate', id: activating.id, tag: activating.tag }}
        />
      ) : null}

      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(open) => !open && setDeleting(null)}
        title={`Delete ${deleting?.tag ?? ''}?`}
        description="This removes the build's directory from disk. It cannot be undone."
        confirmLabel="Delete"
        busy={del.isPending}
        onConfirm={() => {
          if (!deleting) return;
          del.mutate(deleting.id, {
            onSuccess: () => {
              toast.success('Build removed');
              setDeleting(null);
            },
            onError: (err) => toast.error(err),
          });
        }}
      />
    </>
  );
}
