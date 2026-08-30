/**
 * `/llamacpp` — DESIGN section 4, screen 12.
 *
 * "Active version panel, version list with rollback, install dialog (channel/tag/custom git) with
 * the plan preview, and a virtualized build-log viewer." The active panel and the table are both
 * projections of the same `llamacpp` SSE topic (section 3.14), so a build finishing, a canary
 * rolling through the fleet, or a rollback landing all show up here without a refresh.
 */

import { useState } from 'react';
import { useNavigate, useSearch } from '@tanstack/react-router';
import { AlertTriangle, Download, Undo2 } from 'lucide-react';
import {
  Badge,
  Button,
  EmptyState,
  Field,
  LoadingPanel,
  Mono,
  Panel,
  PanelHeader,
  QueryError,
} from '../../components';
import { formatBytes, formatTimestamp } from '../../format';
import type { LlamacppState } from '../../api/types';
import { ActivateDialog } from './ActivateDialog';
import { BuildLogPanel } from './BuildLogPanel';
import { InstallDialog } from './InstallDialog';
import { useActiveLlamacpp, useLlamacppVersions } from './queries';
import { VersionsList } from './VersionsList';

export function LlamacppScreen() {
  const navigate = useNavigate({ from: '/llamacpp' });
  const search = useSearch({ from: '/app/llamacpp' });

  const active = useActiveLlamacpp();
  const versions = useLlamacppVersions();

  const [installOpen, setInstallOpen] = useState(false);
  const [rollbackOpen, setRollbackOpen] = useState(false);

  const selectedId = search.tab ?? null;
  const select = (id: string) =>
    void navigate({ search: (prev) => ({ ...prev, tab: prev.tab === id ? undefined : id }) });

  const items = versions.data?.items ?? [];
  const selected = items.find((v) => v.id === selectedId) ?? null;
  const hasRollbackTarget = items.some((v) => v.previous_active);

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-lg font-semibold tracking-tight text-[var(--lm-text)]">llama.cpp</h1>
        <div className="flex items-center gap-2">
          <Button
            variant="secondary"
            icon={<Undo2 />}
            disabled={!hasRollbackTarget}
            onClick={() => setRollbackOpen(true)}
          >
            Roll back
          </Button>
          <Button variant="primary" icon={<Download />} onClick={() => setInstallOpen(true)}>
            Install a build
          </Button>
        </div>
      </div>

      {active.isLoading ? (
        <LoadingPanel>Reading the active build…</LoadingPanel>
      ) : active.data ? (
        <Panel>
          <PanelHeader
            title={
              <span className="flex items-center gap-2">
                Active: <Mono>{active.data.version.tag}</Mono>
                <Badge tone="ok">{active.data.version.backend}</Badge>
              </span>
            }
            description={
              active.data.version.activated_at
                ? `Activated ${formatTimestamp(active.data.version.activated_at)}`
                : undefined
            }
          />
          <dl className="mt-3 grid grid-cols-2 gap-x-4 gap-y-3 sm:grid-cols-4">
            <Field label="Acquisition">{active.data.version.acquisition}</Field>
            <Field label="Resolved commit" mono>
              {active.data.version.resolved_commit ?? '—'}
            </Field>
            <Field label="Fit support">
              {active.data.version.supports_fit ? 'Supported' : 'Not supported'}
            </Field>
            <Field label="CUDA architectures" mono>
              {active.data.cuda_arch_list ?? '—'}
            </Field>
          </dl>
          {active.data.devices_output ? (
            <details className="mt-3">
              <summary className="cursor-pointer text-xs font-medium text-[var(--lm-text-muted)]">
                Devices reported by this build
              </summary>
              <pre className="lm-numeric mt-2 max-h-40 overflow-auto rounded-[var(--lm-radius)] bg-[var(--lm-surface-sunken)] p-2 text-[11px] whitespace-pre-wrap text-[var(--lm-text-muted)]">
                {active.data.devices_output}
              </pre>
            </details>
          ) : null}
        </Panel>
      ) : active.isError ? (
        // "No active build" is a claim about the host. A failed read is not a basis for it: this
        // host may have one installed and serving, and telling the user to install another is how
        // a second build gets made for nothing.
        <QueryError
          title="The active build could not be read"
          error={active.error}
          onRetry={() => void active.refetch()}
        />
      ) : (
        <EmptyState
          icon={<AlertTriangle />}
          title="No active build"
          description="Install a llama.cpp build to start creating instances."
          action={
            <Button variant="primary" onClick={() => setInstallOpen(true)}>
              Install a build
            </Button>
          }
        />
      )}

      <div className="space-y-3">
        <PanelHeader
          title="Versions"
          description="Only the active build and one rollback target are kept on disk (D25)."
        />
        {versions.isPending ? (
          <LoadingPanel>Reading installed versions…</LoadingPanel>
        ) : versions.isError ? (
          <QueryError
            title="The installed versions could not be read"
            error={versions.error}
            onRetry={() => void versions.refetch()}
          />
        ) : items.length === 0 ? (
          <EmptyState title="No builds installed yet" />
        ) : (
          <VersionsList versions={items} selectedId={selectedId} onSelect={select} />
        )}
      </div>

      {selected ? (
        <Panel flush>
          <div className="flex items-center justify-between gap-2 border-b border-[var(--lm-border)] px-3 py-2">
            <p className="flex items-center gap-2 text-sm font-medium text-[var(--lm-text)]">
              Build log — <Mono>{selected.tag}</Mono>
            </p>
            {selected.size_bytes != null ? (
              <span className="text-xs text-[var(--lm-text-faint)]">
                {formatBytes(selected.size_bytes)} on disk
              </span>
            ) : null}
          </div>
          <BuildLogPanel id={selected.id} state={selected.state as LlamacppState} />
        </Panel>
      ) : null}

      <InstallDialog
        open={installOpen}
        onOpenChange={setInstallOpen}
        defaultChannel={search.channel ?? 'stable'}
      />
      {rollbackOpen ? (
        <ActivateDialog open onOpenChange={setRollbackOpen} mode={{ kind: 'rollback' }} />
      ) : null}
    </div>
  );
}
