/**
 * `-ngl`, the four-mode offload control (D51, DESIGN section 5.7).
 *
 * The modes are not four spellings of one number:
 *
 *   auto  — **no `-ngl` at all**, which is precisely what leaves llama.cpp's own `--fit` enabled to
 *           decide at load time, when the free VRAM is actually known. Never resolved by us: doing
 *           so would make `config_hash` move because free memory moved.
 *   all   — `-ngl 999`
 *   none  — `-ngl 0`, the CPU-only case Safe start also uses
 *   count — `-ngl N`, what "Pin" writes when a user accepts the calculator's advisory
 *
 * So the advisory line beside `auto` is the whole point of the control: llama.cpp chooses, we say
 * what we would have chosen, and one click turns our number into a saved configuration — "never
 * something the launcher does behind the user's back".
 */

import { Pin } from 'lucide-react';
import { Controller } from 'react-hook-form';
import { Badge, Button, FormField, Input, Select } from '../../../components';
import type { SelectOption } from '../../../components';
import type { FitReport } from '../../../api/types';
import { NGL_MODES } from '../values';
import type { NglMode } from '../values';
import { aria } from './fields';
import type { Form } from './fields';

const MODE_OPTIONS: SelectOption<NglMode>[] = [
  {
    value: 'auto',
    label: 'Auto — llama.cpp decides',
    description: 'No -ngl is passed, so llama.cpp’s own --fit picks the offload at load time.',
  },
  { value: 'all', label: 'All layers', description: '-ngl 999' },
  { value: 'none', label: 'CPU only', description: '-ngl 0' },
  { value: 'count', label: 'A fixed number of layers', description: '-ngl N' },
];

export interface NglFieldProps {
  form: Form;
  report?: FitReport | undefined;
  /** `llamacpp_versions.supports_fit`. False makes `auto` render as `-ngl 999` (section 5.7). */
  supportsFit?: boolean | undefined;
  /** Writes the advisory count into the form as `{mode:'count', count:N}`. */
  onPin?: (count: number) => void;
  pinning?: boolean;
}

export function NglField({ form, report, supportsFit, onPin, pinning = false }: NglFieldProps) {
  const mode = form.watch('ngl_mode');
  const error = form.formState.errors.ngl_mode?.message ?? form.formState.errors.ngl_count?.message;

  const advisory =
    report && mode === 'auto'
      ? `We estimate ${report.max_n_gpu_layers} of ${report.inputs.n_layer} layers fit.`
      : null;

  return (
    <div className="space-y-2">
      <FormField
        label="GPU offload"
        flag="-ngl"
        error={typeof error === 'string' ? error : undefined}
        hint={
          supportsFit === false
            ? 'This build predates --fit, so `auto` behaves as `all` (-ngl 999).'
            : 'Auto passes no -ngl at all, which is what keeps llama.cpp’s own --fit in charge.'
        }
      >
        {(field) => (
          <div className="flex items-start gap-2">
            <Controller
              control={form.control}
              name="ngl_mode"
              render={({ field: controlled }) => (
                <Select
                  {...aria(field)}
                  value={controlled.value}
                  onValueChange={(next) => {
                    controlled.onChange(next);
                    if (next !== 'count') form.setValue('ngl_count', '');
                  }}
                  options={MODE_OPTIONS}
                  className="flex-1"
                />
              )}
            />
            {mode === 'count' ? (
              <Input
                {...form.register('ngl_count')}
                mono
                inputMode="numeric"
                aria-label="Layers to offload"
                placeholder="37"
                className="w-24"
              />
            ) : null}
          </div>
        )}
      </FormField>

      {advisory ? (
        <div className="flex flex-wrap items-center gap-2">
          <Badge tone="accent">{advisory}</Badge>
          {onPin && report ? (
            <Button
              size="sm"
              variant="ghost"
              icon={<Pin />}
              loading={pinning}
              onClick={() => onPin(report.max_n_gpu_layers)}
            >
              Pin {report.max_n_gpu_layers}
            </Button>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

/** The four modes, for anything that needs to label one outside the control. */
export const NGL_MODE_LABELS: Record<(typeof NGL_MODES)[number], string> = {
  auto: 'auto',
  all: 'all layers',
  none: 'CPU only',
  count: 'fixed count',
};
