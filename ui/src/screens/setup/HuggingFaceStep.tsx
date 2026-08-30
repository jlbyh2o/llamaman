import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  CheckCircle2,
  FolderPlus,
  HardDrive,
  KeyRound,
  RefreshCw,
  Star,
  Trash2,
} from 'lucide-react';
import {
  Badge,
  Button,
  Field,
  FormField,
  Input,
  Meter,
  PanelHeader,
  Progress,
  StatusBadge,
  toast,
} from '../../components';
import { api } from '../../api/client';
import { ApiError } from '../../api/errors';
import { queryKeys } from '../../api/keys';
import type { CacheRoot, CacheScan } from '../../api/types';
import { formatBytes, formatCount, formatRelative } from '../../format';
import { WizardStep } from '../../setup/WizardStep';
import { fieldProps } from './fieldProps';

/**
 * Wizard step `hf` (DESIGN section 11.2): "optional token (validated via whoami), cache-root
 * detection and confirmation, then a scan".
 *
 * Two independent things share the step because they are the same subject to a user — "where do
 * models come from, and where do they land" — and both are optional, which is why section 11.2
 * marks it skippable.
 *
 * **The token is never a setting** (section 3.4): it has its own validating triple, and
 * `PUT /api/v1/hf/token` answers with `{present, hint, valid, user, scopes}` and never the token
 * itself. The whoami identity comes back in that response, so a saved token displays who it belongs
 * to without a second call and without the secret ever returning.
 *
 * **The scan is where "6 models already on disk" comes from** (the models step's first line). It is
 * a job, so it is started with a 202 and watched: there is no `cache` SSE topic in section 3.14, so
 * this is the one place in the wizard that reads a row on an interval — and only while a scan is
 * actually running.
 */
export function HuggingFaceStep() {
  const queryClient = useQueryClient();
  const [scanId, setScanId] = useState<string | null>(null);

  const roots = useQuery({
    queryKey: queryKeys.cache.roots(),
    queryFn: () => api.get('/api/v1/cache/roots'),
  });

  const scan = useMutation({
    mutationFn: (rootId: string) => api.post('/api/v1/cache/scan', { body: { root_id: rootId } }),
    onSuccess: (receipt) => {
      setScanId(receipt?.subject.id ?? null);
    },
    onError: (error) => toast.error(error),
  });

  const promote = useMutation({
    mutationFn: (id: string) => api.post('/api/v1/cache/roots/{id}/promote', { path: { id } }),
    onSuccess: async (result) => {
      setScanId(result?.scan?.subject.id ?? null);
      await queryClient.invalidateQueries({ queryKey: queryKeys.family('cache') });
      if (result?.restart_required) {
        toast.warn('The new cache location applies to the daemon after a restart.');
      }
    },
    onError: (error) => toast.error(error),
  });

  const rootRows: readonly CacheRoot[] = roots.data?.items ?? [];

  return (
    <WizardStep step="hf">
      <div className="space-y-6">
        <TokenSection />

        <section className="space-y-3">
          <PanelHeader
            title="Cache location"
            description="Detected from HF_HOME, XDG state and the usual default. Models are stored in the Hugging Face cache layout, so anything already downloaded is found rather than re-fetched."
          />

          {roots.isPending ? (
            <p className="text-sm text-[var(--lm-text-faint)]">Reading cache roots…</p>
          ) : rootRows.length === 0 ? (
            <p className="text-sm text-[var(--lm-text-muted)]">
              No cache root is registered. Add one below and it becomes scan-and-serve immediately;
              promoting it makes it the location new downloads are written to.
            </p>
          ) : (
            <ul className="space-y-3">
              {rootRows.map((root) => (
                <RootRow
                  key={root.id}
                  root={root}
                  scanning={scan.isPending || promote.isPending}
                  onScan={() => scan.mutate(root.id)}
                  onPromote={() => promote.mutate(root.id)}
                />
              ))}
            </ul>
          )}

          {scanId ? <ScanProgress id={scanId} /> : null}

          <AddRoot
            onAdded={(receipt) => {
              setScanId(receipt ?? null);
            }}
          />
        </section>
      </div>
    </WizardStep>
  );
}

/** The optional token, and the identity it validates as. */
function TokenSection() {
  const queryClient = useQueryClient();
  const [token, setToken] = useState('');

  const status = useQuery({
    queryKey: queryKeys.hf.token(),
    queryFn: () => api.get('/api/v1/hf/token'),
    retry: false,
  });

  const save = useMutation({
    mutationFn: () => api.put('/api/v1/hf/token', { body: { token: token.trim() } }),
    onSuccess: (result) => {
      setToken('');
      queryClient.setQueryData(queryKeys.hf.token(), result);
      toast.success(result?.user ? `Signed in to Hugging Face as ${result.user}` : 'Token saved');
    },
  });

  const remove = useMutation({
    mutationFn: () => api.delete('/api/v1/hf/token'),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: queryKeys.hf.token() });
    },
    onError: (error) => toast.error(error),
  });

  const present = status.data?.present === true;
  const invalidToken = save.error instanceof ApiError && save.error.code === 'hf_token_invalid';

  return (
    <section className="space-y-3">
      <PanelHeader
        title="Hugging Face token"
        description="Optional. Public repositories need none; gated and private ones do."
        actions={
          present ? (
            <Button
              variant="ghost"
              icon={<Trash2 />}
              loading={remove.isPending}
              onClick={() => remove.mutate()}
            >
              Remove
            </Button>
          ) : null
        }
      />

      {present ? (
        <div className="flex flex-wrap items-center gap-3 rounded-[var(--lm-radius)] bg-[var(--lm-surface-sunken)] px-3 py-2">
          <CheckCircle2 aria-hidden className="size-4 shrink-0 text-[var(--lm-ok)]" />
          <dl className="grid flex-1 gap-3 sm:grid-cols-3">
            <Field label="Account" mono>
              {status.data?.user || 'unknown'}
            </Field>
            <Field label="Token" mono>
              {status.data?.hint || '••••'}
            </Field>
            <Field label="Scopes" mono>
              {status.data?.scopes?.length ? status.data.scopes.join(', ') : '—'}
            </Field>
          </dl>
          {status.data?.valid === false ? (
            <Badge tone="danger">Rejected by Hugging Face</Badge>
          ) : (
            <Badge tone="ok">Validated</Badge>
          )}
        </div>
      ) : (
        <form
          className="flex flex-wrap items-end gap-3"
          onSubmit={(event) => {
            event.preventDefault();
            if (!token.trim() || save.isPending) return;
            save.mutate();
          }}
        >
          <FormField
            label="Access token"
            className="min-w-64 flex-1"
            error={invalidToken ? 'Hugging Face did not accept that token.' : undefined}
            hint="Created under Settings → Access Tokens on huggingface.co. It is sealed on this host and never returned by the API."
          >
            {(field) => (
              <Input
                {...field}
                mono
                type="password"
                autoComplete="off"
                spellCheck={false}
                placeholder="hf_…"
                value={token}
                onChange={(event) => setToken(event.target.value)}
              />
            )}
          </FormField>
          <Button
            type="submit"
            icon={<KeyRound />}
            loading={save.isPending}
            disabled={token.trim().length === 0}
          >
            Validate and save
          </Button>
        </form>
      )}
    </section>
  );
}

function RootRow({
  root,
  scanning,
  onScan,
  onPromote,
}: {
  root: CacheRoot;
  scanning: boolean;
  onScan: () => void;
  onPromote: () => void;
}) {
  const total = root.total_bytes ?? 0;
  const free = root.free_bytes ?? 0;

  return (
    <li className="rounded-[var(--lm-radius)] border border-[var(--lm-border)] p-3">
      <div className="flex flex-wrap items-center gap-2">
        <HardDrive aria-hidden className="size-4 shrink-0 text-[var(--lm-text-faint)]" />
        <span className="lm-numeric min-w-0 flex-1 truncate text-[13px] text-[var(--lm-text)]">
          {root.path}
        </span>
        {root.is_primary ? <Badge tone="accent">Primary</Badge> : null}
        {root.writable ? null : <Badge tone="warn">Read-only</Badge>}
        {root.symlinks_ok ? null : <Badge tone="warn">No symlinks</Badge>}
        <Button size="sm" icon={<RefreshCw />} loading={scanning} onClick={onScan}>
          Scan
        </Button>
        {root.is_primary ? null : (
          <Button size="sm" icon={<Star />} loading={scanning} onClick={onPromote}>
            Make primary
          </Button>
        )}
      </div>

      <dl className="mt-2 grid gap-3 sm:grid-cols-3">
        <Field label="Models" mono>
          {formatCount(root.models)}
        </Field>
        <Field label="Model bytes" mono>
          {formatBytes(root.bytes_on_disk)}
        </Field>
        <Field label="Last scan" mono>
          {root.last_scan_at ? formatRelative(root.last_scan_at) : 'never'}
        </Field>
      </dl>

      {total > 0 ? (
        <Meter
          className="mt-2"
          used={total - free}
          total={total}
          label="Filesystem"
          detail={`${formatBytes(free)} free of ${formatBytes(total)}`}
        />
      ) : null}
    </li>
  );
}

/**
 * A scan in flight.
 *
 * Section 3.14 lists eight SSE topics and `cache` is not one of them, so this reads the row on an
 * interval — and stops the moment the row reaches a terminal state, which is what keeps "no polling
 * loops where SSE topics exist" honest rather than convenient.
 */
function ScanProgress({ id }: { id: string }) {
  const scan = useQuery({
    queryKey: queryKeys.cache.scan(id),
    queryFn: () => api.get('/api/v1/cache/scans/{id}', { path: { id } }),
    refetchInterval: (query) => {
      const state = (query.state.data as CacheScan | undefined)?.state;
      return state === 'running' || state === 'queued' || state === undefined ? 1000 : false;
    },
  });

  const row = scan.data;
  if (!row) return null;
  const running = row.state === 'running' || row.state === 'queued';

  return (
    <div className="space-y-2 rounded-[var(--lm-radius)] bg-[var(--lm-surface-sunken)] p-3">
      <div className="flex flex-wrap items-center gap-2">
        <StatusBadge kind="job" state={row.state} />
        <span className="text-sm text-[var(--lm-text-muted)]">
          {running
            ? `${formatCount(row.files_seen)} files seen in ${formatCount(row.dirs_seen)} directories`
            : `${formatCount(row.models_found)} models found · ${formatCount(row.models_added)} new · ${formatBytes(row.bytes_total)}`}
        </span>
      </div>
      {running ? <Progress value={null} aria-label="Cache scan progress" /> : null}
      {row.strays_found > 0 ? (
        <p className="text-xs text-[var(--lm-text-faint)]">
          {formatCount(row.strays_found)} stray files noted. They are listed under Models later;
          nothing was moved or deleted.
        </p>
      ) : null}
      {row.error_message ? (
        <p className="text-xs text-[var(--lm-danger)]">{row.error_message}</p>
      ) : null}
    </div>
  );
}

/** The relocate affordance: register another directory, then promote it if it should be the one. */
function AddRoot({ onAdded }: { onAdded: (scanId: string | undefined) => void }) {
  const queryClient = useQueryClient();
  const [path, setPath] = useState('');
  const [open, setOpen] = useState(false);

  const add = useMutation({
    mutationFn: () => api.post('/api/v1/cache/roots', { body: { path: path.trim() } }),
    onSuccess: async (result) => {
      setPath('');
      setOpen(false);
      await queryClient.invalidateQueries({ queryKey: queryKeys.family('cache') });
      onAdded(result?.scan?.subject.id);
    },
  });

  const error = add.error instanceof ApiError ? add.error : null;

  if (!open) {
    return (
      <Button variant="ghost" icon={<FolderPlus />} onClick={() => setOpen(true)}>
        Store models somewhere else
      </Button>
    );
  }

  return (
    <form
      className="flex flex-wrap items-end gap-3"
      onSubmit={(event) => {
        event.preventDefault();
        if (!path.trim() || add.isPending) return;
        add.mutate();
      }}
    >
      <FormField
        label="Cache directory"
        className="min-w-72 flex-1"
        error={error ? error.message : undefined}
        hint="An absolute path the service identity can write. A new root is scan-and-serve only until it is promoted; nothing is moved or copied."
      >
        {(field) => (
          <Input
            {...fieldProps(field)}
            mono
            autoFocus
            spellCheck={false}
            placeholder="/srv/models"
            value={path}
            onChange={(event) => setPath(event.target.value)}
          />
        )}
      </FormField>
      <Button type="submit" loading={add.isPending} disabled={path.trim().length === 0}>
        Add location
      </Button>
      <Button variant="ghost" onClick={() => setOpen(false)}>
        Cancel
      </Button>
    </form>
  );
}
