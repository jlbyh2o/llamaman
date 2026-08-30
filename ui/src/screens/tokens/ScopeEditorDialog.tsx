/**
 * Rescoping an existing token — the other half of `PATCH /tokens/{id}` (DESIGN section 3.12), split
 * out from the mint flow because editing scope has none of the "shown once" ceremony minting does.
 *
 * `scope` itself is not one of `PatchAPITokenRequest`'s fields — only `instance_ids` is — so a
 * `global` token's breadth cannot be narrowed here, and an `instances` token's breadth cannot be
 * widened into `global`. Both are true to the wire contract: this editor only ever changes *which*
 * instances an already-`instances`-scoped token names.
 */

import { useEffect, useState } from 'react';
import { Button, Dialog, DialogContent, describeError, toast } from '../../components';
import type { ApiToken, Instance } from '../../api/types';
import { usePatchToken } from './hooks';

export interface ScopeEditorDialogProps {
  token: ApiToken | null;
  onOpenChange: (open: boolean) => void;
  instances: readonly Instance[];
}

export function ScopeEditorDialog({ token, onOpenChange, instances }: ScopeEditorDialogProps) {
  const patch = usePatchToken(token?.id ?? '');
  const [instanceIds, setInstanceIds] = useState<Set<string>>(new Set());

  useEffect(() => {
    if (!token) return;
    setInstanceIds(new Set(token.instance_ids));
  }, [token]);

  function toggle(id: string) {
    setInstanceIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  async function save() {
    if (!token) return;
    try {
      await patch.mutateAsync({ instanceIds: [...instanceIds] });
      toast.success('Scope updated');
      onOpenChange(false);
    } catch (err) {
      const { title, description } = describeError(err);
      toast.error(title, { description });
    }
  }

  if (token && token.scope === 'global') {
    return (
      <Dialog open onOpenChange={onOpenChange}>
        <DialogContent
          title={`Scope — ${token.name}`}
          description="This token was minted with global scope."
          footer={
            <Button variant="secondary" onClick={() => onOpenChange(false)}>
              Close
            </Button>
          }
        >
          <p className="text-sm text-[var(--lm-text-muted)]">
            A global token reaches every instance, current and future, and that breadth is fixed at
            creation — mint a new token scoped to specific instances if you want a narrower one.
          </p>
        </DialogContent>
      </Dialog>
    );
  }

  return (
    <Dialog open={token !== null} onOpenChange={onOpenChange}>
      <DialogContent
        title={token ? `Scope — ${token.name}` : 'Scope'}
        description="Which instances this token's credential is accepted on."
        footer={
          <>
            <Button variant="ghost" onClick={() => onOpenChange(false)} disabled={patch.isPending}>
              Cancel
            </Button>
            <Button
              variant="primary"
              loading={patch.isPending}
              disabled={instanceIds.size === 0}
              onClick={() => void save()}
            >
              Save
            </Button>
          </>
        }
      >
        <div className="max-h-56 space-y-1 overflow-y-auto rounded-[var(--lm-radius)] border border-[var(--lm-border)] p-2">
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
                  onChange={() => toggle(instance.id)}
                />
                {instance.display_name || instance.name}
              </label>
            ))
          )}
        </div>
      </DialogContent>
    </Dialog>
  );
}
