/**
 * One settings group, rendered from the registry (DESIGN section 3.4).
 *
 * Edits are staged locally and sent as a single `PATCH` per group — never per keystroke — because
 * `PATCH /settings` is "partial" and answers per key, and a screen that fired one request per field
 * would turn one save into N independent race conditions. `restart_required` on the *response*
 * (section 3.4: "the running daemon still holds the old value") is what the parent shows the
 * restart banner for; this component only marks which *fields* the registry says need one.
 */

import { useEffect, useState } from 'react';
import { Save, Undo2 } from 'lucide-react';
import { Badge, Button, FormField, Input, Select, Switch, toast } from '../../components';
import { selectFieldProps } from '../../features/system/api';
import type { SettingDef } from '../../features/system/types';
import { usePatchSettings } from '../../features/system/queries';
import { metaFor } from './settingsMeta';
import type { SettingsGroup } from '../../searchParams';

export interface SettingsGroupFormProps {
  group: SettingsGroup;
  schema: readonly SettingDef[];
  values: Record<string, unknown>;
  onSaved?: (restartRequired: boolean) => void;
}

function fieldsFor(group: SettingsGroup, schema: readonly SettingDef[]): SettingDef[] {
  return schema.filter((def) => metaFor(def.key).group === group);
}

export function SettingsGroupForm({ group, schema, values, onSaved }: SettingsGroupFormProps) {
  const fields = fieldsFor(group, schema);
  const [draft, setDraft] = useState<Record<string, unknown>>({});
  const patch = usePatchSettings();

  // A save (or an SSE-less refetch elsewhere) replaces `values` with the settled server state — drop
  // any staged edit that matches it, so the dirty set always means "differs from what is saved".
  useEffect(() => {
    setDraft((prev) => {
      const next: Record<string, unknown> = {};
      for (const [key, value] of Object.entries(prev)) {
        if (values[key] !== value) next[key] = value;
      }
      return next;
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [values]);

  const valueOf = (def: SettingDef) =>
    def.key in draft ? draft[def.key] : (values[def.key] ?? def.default);
  const set = (key: string, value: unknown) => setDraft((prev) => ({ ...prev, [key]: value }));
  const dirtyKeys = Object.keys(draft);

  const save = () => {
    if (dirtyKeys.length === 0) return;
    patch.mutate(draft, {
      onSuccess: (res) => {
        setDraft({});
        toast.success('Settings saved');
        onSaved?.(res.restart_required);
      },
      onError: (err) => toast.error(err),
    });
  };

  if (fields.length === 0) return null;

  return (
    <div className="space-y-4">
      {fields.map((def) => {
        const meta = metaFor(def.key);
        const value = valueOf(def);
        const label = (
          <span className="flex items-center gap-1.5">
            {meta.label}
            {def.restart_required ? (
              <Badge tone="warn" className="text-[10px]">
                Restart to apply
              </Badge>
            ) : null}
          </span>
        );

        return (
          <FormField key={def.key} label={label} hint={meta.hint} inline>
            {(field) => {
              if (def.type === 'bool') {
                return (
                  <Switch
                    {...field}
                    checked={Boolean(value)}
                    onCheckedChange={(checked) => set(def.key, checked)}
                  />
                );
              }
              if (def.type === 'enum') {
                return (
                  <Select
                    {...selectFieldProps(field)}
                    value={String(value)}
                    onValueChange={(next) => set(def.key, next)}
                    options={(def.enum ?? []).map((option) => ({ value: option, label: option }))}
                  />
                );
              }
              if (def.type === 'int') {
                return (
                  <Input
                    {...field}
                    type="number"
                    mono
                    value={String(value ?? '')}
                    min={def.min ?? undefined}
                    max={def.max ?? undefined}
                    suffix={meta.suffix}
                    onChange={(event) => {
                      const n = Number(event.target.value);
                      if (Number.isFinite(n)) set(def.key, n);
                    }}
                  />
                );
              }
              return (
                <Input
                  {...field}
                  mono={
                    def.key.includes('dir') || def.key.includes('flags') || def.key.includes('list')
                  }
                  value={String(value ?? '')}
                  onChange={(event) => set(def.key, event.target.value)}
                />
              );
            }}
          </FormField>
        );
      })}

      {dirtyKeys.length > 0 ? (
        <div className="flex items-center gap-2 border-t border-[var(--lm-border)] pt-3">
          <Button
            size="sm"
            variant="primary"
            icon={<Save />}
            loading={patch.isPending}
            onClick={save}
          >
            Save changes
          </Button>
          <Button size="sm" variant="ghost" icon={<Undo2 />} onClick={() => setDraft({})}>
            Discard
          </Button>
          <span className="text-xs text-[var(--lm-text-faint)]">
            {dirtyKeys.length} unsaved change{dirtyKeys.length === 1 ? '' : 's'}
          </span>
        </div>
      ) : null}
    </div>
  );
}
