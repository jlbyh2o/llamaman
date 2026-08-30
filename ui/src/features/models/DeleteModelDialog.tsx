/**
 * Deleting a model, with the preview that makes the number honest.
 *
 * D28 is the reason this dialog exists at all rather than being a confirm box: the space a delete
 * frees is *not* the model's size. The Hugging Face cache stores one blob per object and links
 * every snapshot that uses it, so deleting one quantization of a repository that holds three may
 * free almost nothing — and `GET /models/{id}/delete-preview` is the endpoint that says which case
 * this is, with blobs refcounted across every snapshot in the repository.
 *
 * The guard is the daemon's, not ours: `DELETE` answers `409 model_in_use` naming the instances,
 * and a soft-deleted instance is never one of them. The dialog reads `in_use_by` from the preview
 * so it can say so before the click instead of after it.
 */

import { Trash2 } from 'lucide-react';
import { Link } from '@tanstack/react-router';

import { Button, Dialog, DialogContent, Spinner, toast } from '../../components';
import type { Model } from '../../api/types';
import { formatBytes, formatCount } from '../../format';
import { useDeleteModel, useDeletePreview } from './queries';

export interface DeleteModelDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  model: Model | null;
  /** Called after the delete is accepted — the model detail screen navigates away. */
  onDeleted?: () => void;
}

export function DeleteModelDialog({
  open,
  onOpenChange,
  model,
  onDeleted,
}: DeleteModelDialogProps) {
  const id = model?.id ?? '';
  const preview = useDeletePreview(id, open);
  const remove = useDeleteModel();

  const inUse = preview.data?.in_use_by ?? [];
  const blocked = inUse.length > 0;

  const confirm = () => {
    if (!model) return;
    remove.mutate(model.id, {
      onSuccess: () => {
        toast.success(`Deleting ${model.quant_label ?? model.primary_file}`, {
          description: `${formatBytes(preview.data?.bytes ?? 0)} will be freed from ${model.repo_id}`,
        });
        onOpenChange(false);
        onDeleted?.();
      },
      onError: (error) => toast.error(error),
    });
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        size="md"
        title="Delete this model from disk"
        description={model ? `${model.repo_id} · ${model.primary_file}` : undefined}
        footer={
          <>
            <Button variant="ghost" onClick={() => onOpenChange(false)} disabled={remove.isPending}>
              Cancel
            </Button>
            <Button
              variant="danger"
              icon={<Trash2 />}
              onClick={confirm}
              disabled={blocked || preview.isLoading}
              loading={remove.isPending}
            >
              Delete files
            </Button>
          </>
        }
      >
        {preview.isLoading ? (
          <div className="flex items-center gap-2 py-6 text-sm text-[var(--lm-text-muted)]">
            <Spinner />
            Refcounting blobs across this repository…
          </div>
        ) : preview.error ? (
          <p className="text-sm text-[var(--lm-danger)]">{(preview.error as Error).message}</p>
        ) : preview.data ? (
          <div className="space-y-4">
            <dl className="grid grid-cols-2 gap-3">
              <div>
                <dt className="text-[11px] tracking-wide text-[var(--lm-text-faint)] uppercase">
                  Files removed
                </dt>
                <dd className="lm-numeric mt-0.5 text-[13px] text-[var(--lm-text)]">
                  {formatCount(preview.data.files)}
                </dd>
              </div>
              <div>
                <dt className="text-[11px] tracking-wide text-[var(--lm-text-faint)] uppercase">
                  Space freed
                </dt>
                <dd className="lm-numeric mt-0.5 text-[13px] text-[var(--lm-text)]">
                  {formatBytes(preview.data.bytes)}
                </dd>
              </div>
              <div>
                <dt className="text-[11px] tracking-wide text-[var(--lm-text-faint)] uppercase">
                  Blobs kept
                </dt>
                <dd className="lm-numeric mt-0.5 text-[13px] text-[var(--lm-text)]">
                  {formatCount(preview.data.blobs_shared_kept)}
                </dd>
              </div>
              <div>
                <dt className="text-[11px] tracking-wide text-[var(--lm-text-faint)] uppercase">
                  Kept because shared
                </dt>
                <dd className="lm-numeric mt-0.5 text-[13px] text-[var(--lm-text)]">
                  {formatBytes(preview.data.shared_bytes)}
                </dd>
              </div>
            </dl>

            {preview.data.blobs_shared_kept > 0 ? (
              <p className="text-xs text-[var(--lm-text-muted)]">
                {formatCount(preview.data.blobs_shared_kept)} blobs stay on disk because another
                snapshot of this repository still links them. That is why the space freed is less
                than this model&rsquo;s size.
              </p>
            ) : null}

            {preview.data.removes_repo_dir ? (
              <p className="text-xs text-[var(--lm-text-muted)]">
                This is the last model in its repository directory, so the directory goes too.
              </p>
            ) : null}

            {blocked ? (
              <div
                role="alert"
                className="rounded-[var(--lm-radius)] border border-[var(--lm-danger)]/35 bg-[var(--lm-danger-soft)] px-3 py-2 text-xs text-[var(--lm-text)]"
              >
                <p className="font-medium">
                  {inUse.length === 1 ? 'An instance uses' : `${inUse.length} instances use`} this
                  model.
                </p>
                <ul className="mt-1 space-y-0.5">
                  {inUse.map((ref) => (
                    <li key={ref.id}>
                      <Link
                        to="/instances/$id"
                        params={{ id: ref.id }}
                        onClick={() => onOpenChange(false)}
                        className="text-[var(--lm-accent)] underline-offset-4 hover:underline"
                      >
                        {ref.name}
                      </Link>
                      <span className="text-[var(--lm-text-muted)]"> — {ref.role}</span>
                    </li>
                  ))}
                </ul>
                <p className="mt-1 text-[var(--lm-text-muted)]">
                  Point them at another model, or delete them, before removing these files.
                </p>
              </div>
            ) : null}
          </div>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}
