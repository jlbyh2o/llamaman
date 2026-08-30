/**
 * Per-instance auth mode (DESIGN section 2.8's `auth_mode`, section 9.2's
 * `auth_mode=='none' ? allow : verify(token)`).
 *
 * This lives on the tokens screen rather than the instance form because it is the one setting that
 * makes every token on the page irrelevant for a given instance — the gateway's `/health` and
 * `/llamaman/info` are always public (section 3.15), but flipping to `none` opens everything else on
 * that instance's public port to anyone who can reach it, which is why the switch is gated behind a
 * typed confirmation rather than a plain click.
 */

import { useState } from 'react';
import { ShieldAlert } from 'lucide-react';
import { ConfirmDialog, DataTable, Panel, PanelHeader, Switch, toast } from '../../components';
import type { Column } from '../../components';
import type { Instance } from '../../api/types';
import { useSetInstanceAuthMode } from './hooks';

export function InstanceAuthPanel({ instances }: { instances: readonly Instance[] }) {
  const setAuthMode = useSetInstanceAuthMode();
  const [pending, setPending] = useState<Instance | null>(null);

  function requestChange(instance: Instance, next: 'token' | 'none') {
    if (next === 'none') {
      setPending(instance);
      return;
    }
    setAuthMode.mutate(
      { id: instance.id, generation: instance.generation, authMode: next },
      { onError: (err) => toast.error(err) },
    );
  }

  function confirmNone() {
    if (!pending) return;
    setAuthMode.mutate(
      { id: pending.id, generation: pending.generation, authMode: 'none' },
      {
        onSuccess: () => {
          toast.warn(`${pending.display_name || pending.name} no longer requires a token`);
          setPending(null);
        },
        onError: (err) => {
          toast.error(err);
          setPending(null);
        },
      },
    );
  }

  const columns: Column<Instance>[] = [
    {
      id: 'name',
      header: 'Instance',
      sortValue: (row) => row.name,
      cell: (row) => (
        <span className="font-medium text-[var(--lm-text)]">{row.display_name || row.name}</span>
      ),
    },
    {
      id: 'port',
      header: 'Public port',
      mono: true,
      align: 'right',
      secondary: true,
      cell: (row) => row.public_port,
    },
    {
      id: 'auth',
      header: 'Requires a token',
      align: 'right',
      cell: (row) => (
        <div className="flex items-center justify-end gap-2">
          {row.auth_mode === 'none' ? (
            <span className="flex items-center gap-1 text-xs text-[var(--lm-warn)]">
              <ShieldAlert aria-hidden className="size-3.5" />
              Open
            </span>
          ) : null}
          <Switch
            checked={row.auth_mode !== 'none'}
            pending={setAuthMode.isPending && setAuthMode.variables?.id === row.id}
            onCheckedChange={(checked) => requestChange(row, checked ? 'token' : 'none')}
            aria-label={`Require a token for ${row.display_name || row.name}`}
          />
        </div>
      ),
    },
  ];

  return (
    <Panel flush>
      <div className="border-b border-[var(--lm-border)] px-4 py-3">
        <PanelHeader
          title="Instance authentication"
          description="Every instance requires a valid token by default. Turning this off makes its public port open to anyone who can reach it."
        />
      </div>
      <DataTable columns={columns} rows={instances} rowKey={(row) => row.id} />

      <ConfirmDialog
        open={pending !== null}
        onOpenChange={(open) => !open && setPending(null)}
        title="Allow unauthenticated access?"
        tone="danger"
        confirmLabel="Turn off token requirement"
        busy={setAuthMode.isPending}
        consequences={
          <p>
            Anyone who can reach port <span className="lm-numeric">{pending?.public_port}</span> on
            this host will be able to use{' '}
            <span className="font-medium">{pending?.display_name || pending?.name}</span> without a
            token — every one of the tokens above will stop mattering for it. This is meant for a
            trusted network only.
          </p>
        }
        onConfirm={confirmNone}
      />
    </Panel>
  );
}
