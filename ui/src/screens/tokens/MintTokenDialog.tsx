/**
 * The mint flow (DESIGN section 4, screen 14).
 *
 * Two panes in one dialog, and the transition between them is one-way: `POST /tokens` is `201` with
 * `{secret}` — "the only response in this API that ever contains it" (section 3.12) — so once that
 * response lands, the form is gone and cannot be reopened to see the secret differently. Closing
 * before "I've saved it" is checked is not blocked outright — `dismissible` stays true so a user isn't
 * trapped — but a plain Escape or backdrop click also drops the one copy of the secret this host will
 * ever show, so the copy button and the checkbox are the two things on that pane.
 */

import { useState } from 'react';
import { Check, Copy, KeyRound } from 'lucide-react';
import {
  Button,
  Dialog,
  DialogContent,
  FieldGroup,
  FormField,
  Input,
  Select,
  Switch,
  describeError,
  toast,
} from '../../components';
import type { FieldRenderProps } from '../../components';
import type { Instance } from '../../api/types';
import type { CreateTokenInput } from './hooks';
import { useCreateToken } from './hooks';

/** See `BenchNewScreen.selectFieldProps` — the same `Select` aria-prop gap, fixed the same way. */
function selectFieldProps(field: FieldRenderProps): {
  id: string;
  'aria-describedby'?: string;
  'aria-invalid'?: boolean;
} {
  return {
    id: field.id,
    ...(field['aria-describedby'] !== undefined
      ? { 'aria-describedby': field['aria-describedby'] }
      : {}),
    ...(field['aria-invalid'] !== undefined ? { 'aria-invalid': field['aria-invalid'] } : {}),
  };
}

export interface MintTokenDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  instances: readonly Instance[];
}

export function MintTokenDialog({ open, onOpenChange, instances }: MintTokenDialogProps) {
  const create = useCreateToken();

  const [name, setName] = useState('');
  const [scope, setScope] = useState<'global' | 'instances'>('global');
  const [instanceIds, setInstanceIds] = useState<Set<string>>(new Set());
  const [rateLimit, setRateLimit] = useState('');
  const [hasExpiry, setHasExpiry] = useState(false);
  const [expiresAt, setExpiresAt] = useState('');

  const [secret, setSecret] = useState<{ value: string; hint: string } | null>(null);
  const [copied, setCopied] = useState(false);
  const [saved, setSaved] = useState(false);

  function reset() {
    setName('');
    setScope('global');
    setInstanceIds(new Set());
    setRateLimit('');
    setHasExpiry(false);
    setExpiresAt('');
    setSecret(null);
    setCopied(false);
    setSaved(false);
  }

  function close(next: boolean) {
    if (!next) reset();
    onOpenChange(next);
  }

  async function submit() {
    if (!name.trim()) return;
    const input: CreateTokenInput = {
      name: name.trim(),
      scope,
      ...(scope === 'instances' ? { instanceIds: [...instanceIds] } : {}),
      ...(rateLimit ? { rateLimitRpm: Number(rateLimit) } : {}),
      ...(hasExpiry && expiresAt ? { expiresAt: new Date(expiresAt).toISOString() } : {}),
    };
    try {
      const result = await create.mutateAsync(input);
      setSecret({ value: result.secret, hint: result.hint });
    } catch (err) {
      const { title, description } = describeError(err);
      toast.error(title, { description });
    }
  }

  async function copy() {
    try {
      await navigator.clipboard.writeText(secret?.value ?? '');
      setCopied(true);
    } catch {
      toast.error('Could not copy — select and copy the secret manually.');
    }
  }

  function toggleInstance(id: string) {
    setInstanceIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  return (
    <Dialog open={open} onOpenChange={close}>
      {secret ? (
        <DialogContent
          title="Save this token's secret now"
          description="It is shown only this once — the daemon keeps a hash, never the value itself."
          dismissible={false}
          footer={
            <Button
              variant="primary"
              disabled={!saved}
              onClick={() => {
                close(false);
                toast.success('Token created', { description: name });
              }}
            >
              Done
            </Button>
          }
        >
          <div className="space-y-4">
            <div className="flex items-center gap-2 rounded-[var(--lm-radius)] border border-[var(--lm-border)] bg-[var(--lm-surface-sunken)] p-3">
              <code className="lm-numeric flex-1 truncate text-sm text-[var(--lm-text)]">
                {secret.value}
              </code>
              <Button
                variant={copied ? 'secondary' : 'primary'}
                size="sm"
                icon={copied ? <Check /> : <Copy />}
                onClick={() => void copy()}
              >
                {copied ? 'Copied' : 'Copy'}
              </Button>
            </div>
            <p className="text-xs text-[var(--lm-text-muted)]">
              Shown as <span className="lm-numeric">{secret.hint}</span> everywhere else it appears.
            </p>
            <label className="flex items-start gap-2 text-sm text-[var(--lm-text)]">
              <input
                type="checkbox"
                className="mt-0.5"
                checked={saved}
                onChange={(event) => setSaved(event.target.checked)}
              />
              I've saved this secret somewhere safe
            </label>
          </div>
        </DialogContent>
      ) : (
        <DialogContent
          title="New API token"
          size="lg"
          footer={
            <>
              <Button variant="ghost" onClick={() => close(false)} disabled={create.isPending}>
                Cancel
              </Button>
              <Button
                variant="primary"
                icon={<KeyRound />}
                loading={create.isPending}
                disabled={!name.trim() || (scope === 'instances' && instanceIds.size === 0)}
                onClick={() => void submit()}
              >
                Create token
              </Button>
            </>
          }
        >
          <div className="space-y-4">
            <FieldGroup title="Identity">
              <FormField label="Name" required hint="How you'll recognize this token in the list.">
                {(field) => (
                  <Input
                    {...field}
                    value={name}
                    onChange={(event) => setName(event.target.value)}
                    placeholder="claude-code, my-laptop, CI…"
                    autoFocus
                  />
                )}
              </FormField>
            </FieldGroup>

            <FieldGroup title="Scope" description="Which instances this token can reach.">
              <FormField label="Scope">
                {(field) => (
                  <Select
                    {...selectFieldProps(field)}
                    value={scope}
                    onValueChange={setScope}
                    options={[
                      {
                        value: 'global',
                        label: 'Every instance',
                        description: 'Current and future.',
                      },
                      {
                        value: 'instances',
                        label: 'Specific instances',
                        description: 'Pick which ones below.',
                      },
                    ]}
                  />
                )}
              </FormField>

              {scope === 'instances' ? (
                <div className="max-h-48 space-y-1 overflow-y-auto rounded-[var(--lm-radius)] border border-[var(--lm-border)] p-2">
                  {instances.length === 0 ? (
                    <p className="p-2 text-xs text-[var(--lm-text-muted)]">No instances yet.</p>
                  ) : (
                    instances.map((instance) => (
                      <label
                        key={instance.id}
                        className="flex items-center gap-2 rounded-[var(--lm-radius-sm)] px-2 py-1.5 text-sm text-[var(--lm-text)] hover:bg-[var(--lm-neutral-soft)]"
                      >
                        <input
                          type="checkbox"
                          checked={instanceIds.has(instance.id)}
                          onChange={() => toggleInstance(instance.id)}
                        />
                        {instance.display_name || instance.name}
                      </label>
                    ))
                  )}
                </div>
              ) : null}
            </FieldGroup>

            <FieldGroup title="Limits" description="Both optional.">
              <FormField label="Rate limit" hint="Requests per minute. Leave blank for none.">
                {(field) => (
                  <Input
                    {...field}
                    type="number"
                    min={1}
                    mono
                    value={rateLimit}
                    onChange={(event) => setRateLimit(event.target.value)}
                    suffix="req/min"
                  />
                )}
              </FormField>
              <FormField label="Expiration">
                {() => (
                  <div className="flex items-center gap-3">
                    <Switch checked={hasExpiry} onCheckedChange={setHasExpiry} />
                    {hasExpiry ? (
                      <input
                        type="datetime-local"
                        value={expiresAt}
                        onChange={(event) => setExpiresAt(event.target.value)}
                        className="h-8 rounded-[var(--lm-radius)] border border-[var(--lm-border)] bg-[var(--lm-surface-sunken)] px-2 text-sm text-[var(--lm-text)]"
                      />
                    ) : (
                      <span className="text-xs text-[var(--lm-text-muted)]">Never expires</span>
                    )}
                  </div>
                )}
              </FormField>
            </FieldGroup>
          </div>
        </DialogContent>
      )}
    </Dialog>
  );
}
