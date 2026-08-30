/**
 * `/tokens` — API tokens (DESIGN section 4, screen 14).
 *
 * Three things live here, and each maps to one part of section 3.12: the list with the mint dialog
 * (never shows a secret except the instant one is minted), the per-token scope editor, and the
 * per-instance `auth_mode` panel — grouped on this screen rather than the instance form because it
 * is the one setting that makes every token above it moot for that instance.
 */

import { useMemo, useState } from 'react';
import { useNavigate, useSearch } from '@tanstack/react-router';
import { Ban, KeyRound, MoreHorizontal, Pencil, Plus, Power } from 'lucide-react';
import type { Column } from '../../components';
import {
  Badge,
  Button,
  ConfirmDialog,
  DataTable,
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
  EmptyState,
  Input,
  LoadingPanel,
  Panel,
  PanelHeader,
  Select,
  StatusBadge,
  toast,
} from '../../components';
import { formatCount, formatRelative, formatTimestamp } from '../../format';
import type { ApiToken } from '../../api/types';
import { InstanceAuthPanel } from './InstanceAuthPanel';
import { MintTokenDialog } from './MintTokenDialog';
import { ScopeEditorDialog } from './ScopeEditorDialog';
import { useInstancesForTokens, usePatchToken, useRevokeToken, useTokens } from './hooks';

function TokenActions({ token, onEditScope }: { token: ApiToken; onEditScope: () => void }) {
  const patch = usePatchToken(token.id);
  const revoke = useRevokeToken();
  const [revokeOpen, setRevokeOpen] = useState(false);
  const revoked = token.state === 'revoked';

  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="icon" aria-label={`Actions for ${token.name}`}>
            <MoreHorizontal />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent>
          <DropdownMenuItem onSelect={onEditScope} disabled={revoked}>
            <Pencil /> Edit scope
          </DropdownMenuItem>
          <DropdownMenuItem
            disabled={revoked || patch.isPending}
            onSelect={() =>
              patch.mutate(
                { state: token.state === 'active' ? 'disabled' : 'active' },
                { onError: (err) => toast.error(err) },
              )
            }
          >
            <Power /> {token.state === 'active' ? 'Disable' : 'Enable'}
          </DropdownMenuItem>
          <DropdownMenuItem danger disabled={revoked} onSelect={() => setRevokeOpen(true)}>
            <Ban /> Revoke
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>

      <ConfirmDialog
        open={revokeOpen}
        onOpenChange={setRevokeOpen}
        title={`Revoke "${token.name}"?`}
        description="This is permanent — the secret can never become valid again, even if reissued."
        confirmLabel="Revoke"
        busy={revoke.isPending}
        onConfirm={() =>
          revoke.mutate(token.id, {
            onSuccess: () => setRevokeOpen(false),
            onError: (err) => toast.error(err),
          })
        }
      />
    </>
  );
}

export function TokensScreen() {
  const search = useSearch({ from: '/app/tokens' });
  const navigate = useNavigate({ from: '/tokens' });

  const tokens = useTokens();
  const instances = useInstancesForTokens();

  const [mintOpen, setMintOpen] = useState(false);
  const [editingScope, setEditingScope] = useState<ApiToken | null>(null);

  const rows = useMemo(() => {
    const items = tokens.data?.items ?? [];
    const q = search.q?.toLowerCase();
    return items
      .filter((t) => !search.state || t.state === search.state)
      .filter((t) => !q || t.name.toLowerCase().includes(q) || t.prefix.toLowerCase().includes(q));
  }, [tokens.data, search.q, search.state]);

  const columns: Column<ApiToken>[] = [
    {
      id: 'name',
      header: 'Name',
      sortValue: (row) => row.name,
      cell: (row) => (
        <div>
          <div className="font-medium text-[var(--lm-text)]">{row.name}</div>
          <div className="lm-numeric text-xs text-[var(--lm-text-faint)]">{row.hint}</div>
        </div>
      ),
    },
    {
      id: 'scope',
      header: 'Scope',
      cell: (row) => (
        <Badge tone={row.scope === 'global' ? 'accent' : 'neutral'}>
          {row.scope === 'global' ? 'Global' : `${row.instance_ids.length} instance(s)`}
        </Badge>
      ),
    },
    {
      id: 'state',
      header: 'State',
      sortValue: (row) => row.state,
      cell: (row) => <StatusBadge kind="token" state={row.state} />,
    },
    {
      id: 'last_used',
      header: 'Last used',
      secondary: true,
      cell: (row) =>
        row.last_used_at ? (
          <span title={formatTimestamp(row.last_used_at)}>{formatRelative(row.last_used_at)}</span>
        ) : (
          <span className="text-[var(--lm-text-faint)]">Never</span>
        ),
    },
    {
      id: 'requests',
      header: 'Requests',
      align: 'right',
      mono: true,
      sortValue: (row) => row.request_count,
      cell: (row) => formatCount(row.request_count),
    },
    {
      id: 'rate_limit',
      header: 'Limit',
      align: 'right',
      mono: true,
      secondary: true,
      cell: (row) => (row.rate_limit_rpm ? `${row.rate_limit_rpm}/min` : '—'),
    },
    {
      id: 'actions',
      header: '',
      width: '2.5rem',
      cell: (row) => <TokenActions token={row} onEditScope={() => setEditingScope(row)} />,
    },
  ];

  return (
    <div className="space-y-6 p-6">
      <PanelHeader
        level={1}
        title="API tokens"
        description="Credentials the gateway accepts as Authorization: Bearer lm_…, X-API-Key, or ?api_key=."
        actions={
          <Button variant="primary" icon={<KeyRound />} onClick={() => setMintOpen(true)}>
            New token
          </Button>
        }
      />

      <Panel className="flex flex-wrap items-center gap-3">
        <Input
          className="min-w-48 flex-1"
          placeholder="Search by name or prefix…"
          value={search.q ?? ''}
          onChange={(event) =>
            void navigate({ search: (prev) => ({ ...prev, q: event.target.value || undefined }) })
          }
        />
        <Select
          value={search.state}
          onValueChange={(value) =>
            void navigate({ search: (prev) => ({ ...prev, state: value || undefined }) })
          }
          placeholder="All states"
          className="w-40"
          options={[
            { value: 'active', label: 'Active' },
            { value: 'disabled', label: 'Disabled' },
            { value: 'revoked', label: 'Revoked' },
          ]}
        />
        {search.q || search.state ? (
          <Button variant="ghost" size="sm" onClick={() => void navigate({ search: {} })}>
            Clear filters
          </Button>
        ) : null}
      </Panel>

      <Panel flush>
        <DataTable
          columns={columns}
          rows={rows}
          rowKey={(row) => row.id}
          loading={tokens.isLoading}
          empty={
            <EmptyState
              title="No API tokens yet"
              description="Mint one to let an external client call an instance's gateway."
              action={
                <Button variant="primary" icon={<Plus />} onClick={() => setMintOpen(true)}>
                  New token
                </Button>
              }
            />
          }
        />
      </Panel>

      {instances.isLoading ? (
        <LoadingPanel />
      ) : (
        <InstanceAuthPanel instances={instances.data?.items ?? []} />
      )}

      <MintTokenDialog
        open={mintOpen}
        onOpenChange={setMintOpen}
        instances={instances.data?.items ?? []}
      />
      <ScopeEditorDialog
        token={editingScope}
        onOpenChange={(open) => !open && setEditingScope(null)}
        instances={instances.data?.items ?? []}
      />
    </div>
  );
}
