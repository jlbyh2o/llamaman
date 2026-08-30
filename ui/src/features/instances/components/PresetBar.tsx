/**
 * Presets: apply one to this form, or save this form as one.
 *
 * A preset is `flags_json` plus `extra_flags` and nothing else (section 2.8's `flag_presets`), so
 * applying one deliberately leaves the name, the models and both ports alone — those are the
 * identity of *this* instance, and a preset that overwrote them would be a template pretending to
 * be a configuration. Section 3.11's `apply` endpoint says the same thing from the other side: it
 * takes instance ids and an explicit `overwrite` list, and answers with a per-instance diff.
 */

import { useState } from 'react';
import { BookmarkPlus, Layers } from 'lucide-react';
import {
  Button,
  Dialog,
  DialogContent,
  FormField,
  Input,
  Select,
  Textarea,
} from '../../../components';
import type { FlagPreset } from '../types';

export interface PresetBarProps {
  presets: readonly FlagPreset[];
  /** Absent while the daemon does not serve `/presets` yet; the bar then explains rather than lies. */
  unavailable?: boolean;
  onApply: (preset: FlagPreset) => void;
  onSave: (input: { name: string; description: string }) => void;
  saving?: boolean;
}

export function PresetBar({
  presets,
  unavailable = false,
  onApply,
  onSave,
  saving = false,
}: PresetBarProps) {
  const [selected, setSelected] = useState('');
  const [open, setOpen] = useState(false);
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');

  if (unavailable) {
    return (
      <p className="text-xs text-[var(--lm-text-faint)]">
        Presets are not served by this daemon yet.
      </p>
    );
  }

  return (
    <div className="flex flex-wrap items-center gap-2">
      <Select
        value={selected}
        onValueChange={(value) => {
          setSelected(value);
          const preset = presets.find((p) => p.id === value);
          if (preset) onApply(preset);
        }}
        options={presets.map((preset) => ({
          value: preset.id,
          label: preset.name,
          ...(preset.description ? { description: preset.description } : {}),
        }))}
        placeholder={presets.length === 0 ? 'No presets yet' : 'Apply a preset…'}
        disabled={presets.length === 0}
        className="w-56"
        aria-label="Apply a preset"
      />
      <span className="hidden items-center gap-1 text-xs text-[var(--lm-text-faint)] sm:flex">
        <Layers aria-hidden className="size-3.5" />
        flags only — models and ports are left alone
      </span>

      <div className="flex-1" />

      <Button size="sm" variant="ghost" icon={<BookmarkPlus />} onClick={() => setOpen(true)}>
        Save as preset
      </Button>

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent
          size="sm"
          title="Save these flags as a preset"
          description="The flag set and the extra flags are stored. Nothing else is."
          footer={
            <>
              <Button variant="ghost" onClick={() => setOpen(false)}>
                Cancel
              </Button>
              <Button
                variant="primary"
                loading={saving}
                disabled={name.trim() === ''}
                onClick={() => {
                  onSave({ name: name.trim(), description: description.trim() });
                  setOpen(false);
                  setName('');
                  setDescription('');
                }}
              >
                Save preset
              </Button>
            </>
          }
        >
          <div className="space-y-3">
            <FormField label="Name" required>
              {(field) => (
                <Input
                  {...field}
                  value={name}
                  onChange={(event) => setName(event.target.value)}
                  placeholder="24 GB, long context"
                  autoFocus
                />
              )}
            </FormField>
            <FormField label="Description">
              {(field) => (
                <Textarea
                  {...field}
                  rows={2}
                  value={description}
                  onChange={(event) => setDescription(event.target.value)}
                  placeholder="What this preset is for."
                />
              )}
            </FormField>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  );
}
