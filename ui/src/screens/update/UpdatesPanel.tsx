/**
 * The Updates group's body (DESIGN section 4 screen 15, section 3.14, section 12).
 *
 * Three states, and the panel only ever shows one of them:
 *
 *  1. **Settled.** The release feed, with one-click apply per release.
 *  2. **In flight.** `GET /update/status.pending` names a swap this boot did not necessarily start —
 *     a page reload mid-restart lands here too — so the staged-progress card is driven by that field
 *     and by `in_flight.state`, not by local component state that a reload would lose.
 *  3. **A downgrade was chosen.** Section 12.4's warning, before the click and — with the server's
 *     own `procedure` array — after it.
 */

import { useState } from 'react';
import { AlertCircle, RefreshCw } from 'lucide-react';
import {
  Badge,
  Button,
  EmptyState,
  LoadingPanel,
  Mono,
  PanelHeader,
  Progress,
  toast,
} from '../../components';
import { formatRelative } from '../../format';
import { DowngradeNotice } from './DowngradeNotice';
import { ReleaseFeed } from './ReleaseFeed';
import {
  isUpdateInFlight,
  useApplyUpdate,
  useCheckUpdates,
  useUpdateReleases,
  useUpdateStatus,
} from './queries';

const STAGE_PERCENT: Record<string, number> = {
  planned: 10,
  downloading: 30,
  verifying: 50,
  staged: 65,
  swapping: 85,
  succeeded: 100,
  failed: 100,
  canceled: 100,
};

export function UpdatesPanel() {
  const status = useUpdateStatus();
  const releases = useUpdateReleases();
  const check = useCheckUpdates();
  const apply = useApplyUpdate();

  const [confirmTag, setConfirmTag] = useState<string | null>(null);
  const [applyingTag, setApplyingTag] = useState<string | null>(null);
  const [procedure, setProcedure] = useState<string[] | null>(null);

  if (status.isLoading) return <LoadingPanel>Reading update status…</LoadingPanel>;
  const data = status.data;
  const inFlight = isUpdateInFlight(data);

  const startApply = (tag: string) => {
    setApplyingTag(tag);
    apply.mutate(tag, {
      onSuccess: (res) => {
        if (res.schema_warning) setProcedure(res.procedure ?? []);
        toast.success('Update staged', {
          description: 'The daemon will restart shortly. This page keeps watching automatically.',
        });
      },
      onError: (err) => {
        setApplyingTag(null);
        toast.error(err);
      },
    });
  };

  const releaseFor = (tag: string) => releases.data?.items.find((r) => r.tag === tag);

  return (
    <div className="space-y-4">
      <PanelHeader
        title="Software updates"
        description={
          data
            ? `Running ${data.current_version}${data.last_checked_at ? ` · checked ${formatRelative(data.last_checked_at)}` : ''}`
            : undefined
        }
        actions={
          !inFlight ? (
            <Button
              size="sm"
              variant="secondary"
              icon={<RefreshCw />}
              loading={check.isPending}
              onClick={() => check.mutate(undefined, { onError: (err) => toast.error(err) })}
            >
              Check for updates
            </Button>
          ) : undefined
        }
      />

      {data?.update_available && !inFlight ? (
        <p className="flex items-center gap-1.5 text-sm text-[var(--lm-accent)]">
          <AlertCircle className="size-4" aria-hidden />A newer release, {data.latest_version}, is
          available.
        </p>
      ) : null}

      {inFlight ? (
        <div className="space-y-3 rounded-[var(--lm-radius)] border border-[var(--lm-border)] bg-[var(--lm-surface-sunken)] p-4">
          <div className="flex items-center justify-between gap-2">
            <p className="text-sm font-medium text-[var(--lm-text)]">
              Updating {data?.pending?.from_version ?? data?.current_version} →{' '}
              {data?.pending?.target_version ?? data?.in_flight?.to_version}
            </p>
            {data?.in_flight ? (
              <Badge tone="info" dot pulse>
                {data.in_flight.state}
              </Badge>
            ) : null}
          </div>
          <Progress
            value={STAGE_PERCENT[data?.in_flight?.state ?? 'planned'] ?? null}
            label={
              data?.pending && !data.in_flight?.state
                ? 'Waiting for the swap to be confirmed'
                : 'Staging the update'
            }
          />
          {status.isError ? (
            <p className="text-xs text-[var(--lm-text-muted)]">
              The daemon is restarting — the public gateway ports stay open throughout (D58), but
              this page will be briefly unreachable. Retrying automatically…
            </p>
          ) : null}
          {data?.in_flight?.state === 'failed' ? (
            <p className="text-xs text-[var(--lm-danger)]">
              {data.in_flight.error_message ??
                'The update failed. If the daemon reached a failed state, it has already been reverted automatically (D88).'}
            </p>
          ) : null}
        </div>
      ) : (
        <>
          {confirmTag ? (
            <div className="space-y-3">
              <DowngradeNotice />
              <div className="flex items-center justify-end gap-2">
                <Button variant="ghost" onClick={() => setConfirmTag(null)}>
                  Cancel
                </Button>
                <Button
                  variant="primary"
                  loading={apply.isPending}
                  onClick={() => {
                    const tag = confirmTag;
                    setConfirmTag(null);
                    startApply(tag);
                  }}
                >
                  Update anyway
                </Button>
              </div>
            </div>
          ) : null}

          {procedure ? (
            <div className="space-y-2">
              <p className="text-sm font-medium text-[var(--lm-text)]">
                Staged. To make the downgrade to <Mono>{applyingTag}</Mono> stick, run these five
                commands after it settles:
              </p>
              <DowngradeNotice procedure={procedure} />
            </div>
          ) : null}

          {releases.isLoading ? (
            <LoadingPanel>Reading the release feed…</LoadingPanel>
          ) : !releases.data || releases.data.items.length === 0 ? (
            <EmptyState
              title="No releases cached yet"
              description="Check for updates to refresh the feed."
            />
          ) : (
            <ReleaseFeed
              releases={releases.data.items}
              disabled={apply.isPending}
              applyingTag={applyingTag}
              onApply={(tag) => {
                const release = releaseFor(tag);
                if (release?.older) {
                  setConfirmTag(tag);
                  return;
                }
                startApply(tag);
              }}
            />
          )}
        </>
      )}
    </div>
  );
}
