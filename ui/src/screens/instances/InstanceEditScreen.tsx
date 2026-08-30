/**
 * `/instances/:id/edit` — screen 5 in edit mode.
 *
 * The same form, seeded from the row and submitted as a `PATCH` carrying `generation`. A mismatch is
 * `409 conflict_generation`, and it has exactly one honest reading: someone edited the configuration
 * underneath you. None of section 2.8's seven exceptional writers bumps `generation`, so it is never
 * housekeeping — which is why `<InstanceForm>` shows that as its own banner with a reload rather
 * than as a save that failed.
 */

import { useNavigate, useParams, useSearch } from '@tanstack/react-router';
import { Button, EmptyState, LoadingPanel } from '../../components';
import { useInstanceDetail, useInstanceRow } from '../../features/instances/api';
import type { FormPane } from '../../features/instances/components/InstanceForm';
import { InstanceFormPage } from './InstanceFormPage';

export function InstanceEditScreen() {
  const { id } = useParams({ from: '/app/instances/$id/edit' });
  const search = useSearch({ from: '/app/instances/$id/edit' });
  const navigate = useNavigate({ from: '/instances/$id/edit' });

  const row = useInstanceRow(id, true);
  const detail = useInstanceDetail(id);
  const instance = row ?? detail.data?.instance;

  if (!instance) {
    return detail.isLoading ? (
      <LoadingPanel>Loading configuration…</LoadingPanel>
    ) : (
      <EmptyState
        tone="error"
        title="No instance with that id"
        description="It may have been purged."
        action={
          <Button onClick={() => void navigate({ to: '/instances' })}>Back to instances</Button>
        }
      />
    );
  }

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-lg font-semibold tracking-tight text-[var(--lm-text)]">
          Edit {instance.name}
        </h1>
        <p className="mt-1 text-xs text-[var(--lm-text-muted)]">
          Saving changes the configuration, not what is running. A running instance keeps serving
          and is flagged “restart required” until it is restarted.
        </p>
      </div>

      <InstanceFormPage
        mode="edit"
        instance={instance}
        pane={search.pane ?? 'model'}
        onPaneChange={(pane: FormPane) =>
          void navigate({ search: (prev) => ({ ...prev, pane }), replace: true })
        }
      />
    </div>
  );
}
