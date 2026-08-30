/**
 * The Hugging Face and GitHub token editors (DESIGN section 3.4, section 3.6, section 6.2).
 *
 * "Secrets are not settings" — each has its own validating triple
 * (`GET`/`PUT`/`DELETE /api/v1/{hf,github}/token`) and never appears in `GET /api/v1/settings`,
 * because a settings value is returned in the clear and these must not be. The UI renders them
 * inside the groups they belong to anyway — HF under Hugging Face, GitHub under Builds, the latter
 * beside its api.github.com rate-limit headroom (section 6.2) — so this is the one component that
 * bridges the two.
 */

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Check, KeyRound, Trash2 } from 'lucide-react';
import { Badge, Button, ConfirmDialog, FormField, Input, Spinner, toast } from '../../components';
import { api } from '../../api/client';
import { queryKeys } from '../../api/keys';
import { formatCount, formatTimestamp } from '../../format';

type Kind = 'hf' | 'github';

const ENDPOINT = {
  hf: {
    get: () => api.get('/api/v1/hf/token'),
    put: (token: string) => api.put('/api/v1/hf/token', { body: { token } }),
    del: () => api.delete('/api/v1/hf/token'),
    key: queryKeys.hf.token(),
    placeholder: 'hf_XXXX',
  },
  github: {
    get: () => api.get('/api/v1/github/token'),
    put: (token: string) => api.put('/api/v1/github/token', { body: { token } }),
    del: () => api.delete('/api/v1/github/token'),
    key: queryKeys.hf.githubToken(),
    placeholder: 'ghp_XXXX',
  },
} as const;

export function SecretField({ kind, label }: { kind: Kind; label: string }) {
  const endpoint = ENDPOINT[kind];
  const client = useQueryClient();
  const [draft, setDraft] = useState('');
  const [editing, setEditing] = useState(false);
  const [removing, setRemoving] = useState(false);

  const status = useQuery({ queryKey: endpoint.key, queryFn: endpoint.get });

  const save = useMutation({
    mutationFn: (token: string) => endpoint.put(token),
    onSuccess: async () => {
      setDraft('');
      setEditing(false);
      await client.invalidateQueries({ queryKey: endpoint.key });
      toast.success(`${label} saved`);
    },
    onError: (err) => toast.error(err),
  });

  const remove = useMutation({
    mutationFn: () => endpoint.del(),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: endpoint.key });
      toast.success(`${label} removed`);
    },
    onError: (err) => toast.error(err),
  });

  const data = status.data;

  return (
    <FormField
      label={label}
      hint={
        data?.present && data.valid && data.user
          ? `Authenticated as ${data.user}${data.scopes.length ? ` · scopes: ${data.scopes.join(', ')}` : ''}`
          : kind === 'github'
            ? 'Optional. Without it, release checks use the anonymous api.github.com rate limit.'
            : 'Optional. Needed only for gated repositories.'
      }
    >
      {(field) =>
        status.isLoading ? (
          <Spinner label={`Reading ${label}`} />
        ) : editing || !data?.present ? (
          <div className="flex items-center gap-2">
            <Input
              {...field}
              mono
              type="password"
              placeholder={endpoint.placeholder}
              value={draft}
              autoComplete="off"
              spellCheck={false}
              onChange={(event) => setDraft(event.target.value)}
            />
            <Button
              size="sm"
              variant="primary"
              icon={<Check />}
              loading={save.isPending}
              disabled={draft.trim() === ''}
              onClick={() => save.mutate(draft.trim())}
            >
              Save
            </Button>
            {data?.present ? (
              <Button size="sm" variant="ghost" onClick={() => setEditing(false)}>
                Cancel
              </Button>
            ) : null}
          </div>
        ) : (
          <div className="flex items-center gap-2">
            <Badge tone={data.valid ? 'ok' : 'warn'} icon={<KeyRound />}>
              {data.hint}
            </Badge>
            {data.valid === false ? <Badge tone="danger">No longer valid</Badge> : null}
            {kind === 'github' && data.rate_limit ? (
              <span className="text-xs text-[var(--lm-text-faint)]">
                {formatCount(data.rate_limit.remaining)}/{formatCount(data.rate_limit.limit)} until{' '}
                {formatTimestamp(data.rate_limit.reset_at)}
              </span>
            ) : null}
            <div className="flex-1" />
            <Button size="sm" variant="secondary" onClick={() => setEditing(true)}>
              Replace
            </Button>
            <Button
              size="sm"
              variant="ghost"
              icon={<Trash2 />}
              loading={remove.isPending}
              onClick={() => setRemoving(true)}
              className="text-[var(--lm-danger)]"
            >
              Remove
            </Button>

            {/*
              Every other destructive action in this app is gated — instance delete and purge,
              model delete, token revoke, version delete, bench run delete, settings reset — and
              this one was not, while sitting in the same button row as "Replace". A mis-click
              cost the user a trip back to the provider: Hugging Face shows an access token
              exactly once, at creation, so the stored value is unrecoverable, and gated-repo
              browsing and every queued download against a gated repo stop working until a new one
              is minted.
            */}
            <ConfirmDialog
              open={removing}
              onOpenChange={setRemoving}
              title={`Remove the ${label.toLowerCase()}?`}
              description={
                kind === 'hf'
                  ? 'Hugging Face shows an access token once, when it is created, so this one cannot be recovered — you would mint a new one at huggingface.co.'
                  : 'GitHub shows a token once, when it is created, so this one cannot be recovered — you would mint a new one at github.com.'
              }
              consequences={
                kind === 'hf'
                  ? 'Browsing gated repositories stops working, and any queued download from one will fail until a new token is saved.'
                  : 'Release listings fall back to anonymous api.github.com rate limits — 60 requests an hour per IP instead of 5000 — so the nightly list may go stale.'
              }
              confirmLabel="Remove"
              busy={remove.isPending}
              onConfirm={() =>
                remove.mutate(undefined, {
                  onSettled: () => setRemoving(false),
                })
              }
            />
          </div>
        )
      }
    </FormField>
  );
}
