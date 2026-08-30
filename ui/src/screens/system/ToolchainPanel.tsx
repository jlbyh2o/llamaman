/** Toolchain preflight (DESIGN section 3.3 `GET /system/toolchain`, section 11.2). */

import { useEffect, useRef } from 'react';
import { ExternalLink, RefreshCw } from 'lucide-react';
import { Badge, Button, DataTable, LoadingPanel, Mono, PanelHeader, toast } from '../../components';
import type { Column } from '../../components';
import { isTerminalJobState, useJob } from '../../features/system/jobs';
import { useProbeToolchain, useToolchain } from '../../features/system/queries';
import type { ToolchainCheck } from '../../features/system/types';
import { useQueryClient } from '@tanstack/react-query';
import { queryKeys } from '../../api/keys';

export function ToolchainPanel() {
  const toolchain = useToolchain();
  const probe = useProbeToolchain();
  const client = useQueryClient();
  const jobIdRef = useRef<string | null>(null);
  const job = useJob(probe.data?.job_id ?? null);

  useEffect(() => {
    if (!job.data || !jobIdRef.current) return;
    if (!isTerminalJobState(job.data.state)) return;
    jobIdRef.current = null;
    void client.invalidateQueries({ queryKey: queryKeys.system.toolchain() });
    if (job.data.state === 'succeeded') toast.success('Toolchain re-checked');
    else toast.error('The toolchain re-check failed');
  }, [job.data, client]);

  const onRecheck = () => {
    probe.mutate(undefined, {
      onSuccess: (receipt) => {
        jobIdRef.current = receipt.job_id ?? null;
      },
      onError: (err) => toast.error(err),
    });
  };

  const running = probe.isPending || (job.data ? !isTerminalJobState(job.data.state) : false);

  const columns: Column<ToolchainCheck>[] = [
    {
      id: 'name',
      header: 'Tool',
      cell: (row) => <Mono>{row.name}</Mono>,
      sortValue: (row) => row.name,
    },
    {
      id: 'ok',
      header: 'Status',
      cell: (row) => (
        <Badge tone={row.ok ? 'ok' : row.found ? 'warn' : 'danger'} dot>
          {row.ok ? 'OK' : row.found ? 'Below minimum' : 'Not found'}
        </Badge>
      ),
      sortValue: (row) => (row.ok ? 1 : 0),
    },
    {
      id: 'version',
      header: 'Version',
      cell: (row) => <Mono>{row.version ?? '—'}</Mono>,
      secondary: true,
    },
    {
      id: 'min_version',
      header: 'Minimum',
      cell: (row) => <Mono>{row.min_version ?? '—'}</Mono>,
      secondary: true,
    },
    {
      id: 'path',
      header: 'Path',
      cell: (row) => <Mono className="text-[var(--lm-text-faint)]">{row.path ?? '—'}</Mono>,
      secondary: true,
    },
    {
      id: 'note',
      header: 'Guidance',
      cell: (row) =>
        row.note || row.docs_url ? (
          <div className="flex items-center gap-2 text-xs text-[var(--lm-text-muted)]">
            <span>{row.note}</span>
            {row.docs_url ? (
              <a
                href={row.docs_url}
                target="_blank"
                rel="noreferrer"
                className="inline-flex items-center gap-1 text-[var(--lm-accent)] hover:underline"
              >
                Docs <ExternalLink className="size-3" aria-hidden />
              </a>
            ) : null}
          </div>
        ) : (
          <span className="text-[var(--lm-text-faint)]">—</span>
        ),
    },
  ];

  return (
    <div className="space-y-3">
      <PanelHeader
        title="Toolchain"
        description="Build prerequisites for a source build: compilers, cmake, ninja, git, and the CUDA toolkit when a GPU is present."
        actions={
          <Button
            size="sm"
            variant="secondary"
            icon={<RefreshCw />}
            loading={running}
            onClick={onRecheck}
          >
            Re-check
          </Button>
        }
      />
      {toolchain.isLoading ? (
        <LoadingPanel>Reading the toolchain report…</LoadingPanel>
      ) : (
        <DataTable
          columns={columns}
          rows={toolchain.data ?? []}
          rowKey={(row) => row.name}
          caption="Toolchain preflight results"
        />
      )}
    </div>
  );
}
