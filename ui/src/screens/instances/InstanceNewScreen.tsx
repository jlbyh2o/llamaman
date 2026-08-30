/**
 * `/instances/new` — DESIGN section 4, screen 5.
 *
 * The pane, the prefilled model and the preset all live in the URL (`searchParams.ts`), so the
 * browse screen's "serve this model" path and a link someone pastes into chat both land on the same
 * form in the same state.
 */

import { useNavigate, useSearch } from '@tanstack/react-router';
import { InstanceFormPage } from './InstanceFormPage';
import type { FormPane } from '../../features/instances/components/InstanceForm';

export function InstanceNewScreen() {
  const search = useSearch({ from: '/app/instances/new' });
  const navigate = useNavigate({ from: '/instances/new' });

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-lg font-semibold tracking-tight text-[var(--lm-text)]">New instance</h1>
        <p className="mt-1 text-xs text-[var(--lm-text-muted)]">
          One llama-server process: a model, a port, and the flags it runs with. Nothing starts
          until you say so.
        </p>
      </div>

      <InstanceFormPage
        mode="create"
        initialModelId={search.model_id}
        pane={search.pane ?? 'model'}
        onPaneChange={(pane: FormPane) =>
          void navigate({ search: (prev) => ({ ...prev, pane }), replace: true })
        }
      />
    </div>
  );
}
