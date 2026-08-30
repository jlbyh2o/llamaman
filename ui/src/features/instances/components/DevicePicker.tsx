/**
 * Which GPUs this instance may use.
 *
 * `--device` is the only device selector (D66): the launcher sets no `CUDA_VISIBLE_DEVICES`,
 * because setting both *renumbers* the devices llama.cpp sees and turns a wrong-GPU bug into
 * something silent. That leaves one mapping true everywhere —
 * `nvidia-smi index == gpus.gpu_index == llama.cpp's CUDA<n>` — so the label under each card is the
 * card's own index, and the resolved `--device` string is shown verbatim rather than described.
 *
 * The UUIDs are what get stored beside it (`device_uuids`), never rendered into argv: the supervisor
 * compares them against the live map on every ready transition and raises F22 when the ordering
 * changed under the instance.
 */

import { Cpu } from 'lucide-react';
import { Badge, FormField, Mono } from '../../../components';
import { cn } from '../../../components/cn';
import { formatBytes } from '../../../format';

export interface PickableDevice {
  uuid: string;
  index: number;
  name: string;
  freeBytes?: number | null;
  totalBytes?: number | null;
}

export interface DevicePickerProps {
  devices: readonly PickableDevice[];
  selected: readonly string[];
  onChange: (uuids: string[]) => void;
  /** The `--device` value these UUIDs resolve to right now. */
  resolved: string;
  disabled?: boolean;
}

export function DevicePicker({
  devices,
  selected,
  onChange,
  resolved,
  disabled = false,
}: DevicePickerProps) {
  const toggle = (uuid: string) => {
    onChange(selected.includes(uuid) ? selected.filter((u) => u !== uuid) : [...selected, uuid]);
  };

  const hint =
    devices.length === 0
      ? 'The device list comes from the fit estimate. Pick a model to populate it.'
      : selected.length === 0
        ? 'Nothing selected: llama.cpp sees every device this build can enumerate.'
        : null;

  return (
    <FormField label="Devices" flag="--device" {...(hint === null ? {} : { hint })}>
      {() => (
        <div className="space-y-2">
          {devices.length > 0 ? (
            <ul className="grid gap-2 sm:grid-cols-2">
              {devices.map((device) => {
                const checked = selected.includes(device.uuid);
                return (
                  <li key={device.uuid}>
                    <label
                      className={cn(
                        'flex cursor-pointer items-start gap-2 rounded-[var(--lm-radius)] border p-2.5',
                        checked
                          ? 'border-[var(--lm-accent)] bg-[var(--lm-accent-soft)]'
                          : 'border-[var(--lm-border)] bg-[var(--lm-surface-sunken)] hover:border-[var(--lm-border-strong)]',
                        disabled && 'cursor-not-allowed opacity-50',
                      )}
                    >
                      <input
                        type="checkbox"
                        className="mt-0.5 size-3.5 accent-[var(--lm-accent)]"
                        checked={checked}
                        disabled={disabled}
                        onChange={() => toggle(device.uuid)}
                      />
                      <span className="min-w-0">
                        <span className="flex items-center gap-1.5 text-sm text-[var(--lm-text)]">
                          <Cpu aria-hidden className="size-3.5 text-[var(--lm-text-faint)]" />
                          <Mono>CUDA{device.index}</Mono>
                          <span className="truncate">{device.name}</span>
                        </span>
                        <span className="lm-numeric mt-0.5 block text-[12px] text-[var(--lm-text-muted)]">
                          {device.freeBytes === null || device.freeBytes === undefined
                            ? 'free memory unknown'
                            : `${formatBytes(device.freeBytes)} free`}
                          {device.totalBytes ? ` of ${formatBytes(device.totalBytes)}` : ''}
                        </span>
                      </span>
                    </label>
                  </li>
                );
              })}
            </ul>
          ) : null}

          {selected.length > 0 ? (
            <p className="flex items-center gap-2 text-xs text-[var(--lm-text-muted)]">
              Renders as
              <Badge tone="neutral">
                <Mono>--device {resolved || '—'}</Mono>
              </Badge>
            </p>
          ) : null}
        </div>
      )}
    </FormField>
  );
}
