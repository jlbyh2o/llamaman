/**
 * One local model — DESIGN section 4, screen 10.
 *
 * "Files, blobs, metadata table, paired mmproj, verify, delete."
 *
 * A `models` row is one *logical* model: a repository, a revision, a quantization, possibly spanning
 * shards (section 2.6). So the files table is the interesting part of this screen — it is where the
 * Hugging Face cache stops being an implementation detail and becomes something to inspect: a
 * snapshot link, the blob it points at, whether the checksum was actually verified, and whether the
 * bytes are still there at all.
 *
 * The GGUF metadata is a tab rather than a section because the daemon re-reads it from the file on
 * request (section 3.7) rather than retaining a tokenizer table it will be asked about once. Opening
 * the tab is what asks.
 */

import { useState } from 'react';
import { Link, useNavigate, useParams } from '@tanstack/react-router';
import { BadgeCheck, Puzzle, ShieldCheck, Trash2 } from 'lucide-react';

import {
  Badge,
  Button,
  EmptyState,
  Field,
  LoadingPanel,
  Panel,
  PanelHeader,
  Select,
  StatusBadge,
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
  toast,
} from '../../components';
import type { ModelFile } from '../../api/types';
import { formatBytes, formatCount, formatRelative } from '../../format';
import { FILE_STATE_LABELS, KIND_LABELS } from '../../features/models/api';
import { DeleteModelDialog } from '../../features/models/DeleteModelDialog';
import { linkButtonClass } from '../../features/models/LinkButton';
import {
  useModel,
  useModelMetadata,
  usePairMmproj,
  useVerifyModel,
} from '../../features/models/queries';

export function ModelDetailScreen() {
  const { id } = useParams({ from: '/app/models/$id' });
  const navigate = useNavigate();

  const detail = useModel(id);
  const verify = useVerifyModel();
  const pair = usePairMmproj();

  const [tab, setTab] = useState('files');
  const [doomed, setDoomed] = useState(false);
  const metadata = useModelMetadata(id, tab === 'metadata');

  if (detail.isPending) return <LoadingPanel>Reading the model…</LoadingPanel>;
  if (detail.error || !detail.data) {
    return (
      <EmptyState
        tone="error"
        title="That model is not here"
        description={(detail.error as Error | null)?.message ?? 'No model has this id.'}
        action={
          <Link to="/models" className={linkButtonClass()}>
            Back to the library
          </Link>
        }
      />
    );
  }

  const { model, files, mmproj, mmproj_candidates: candidates } = detail.data;
  const inUse = model.in_use_by.filter((ref) => !ref.deleted);

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div className="min-w-0">
          <Link
            to="/models"
            className="text-xs text-[var(--lm-text-muted)] underline-offset-4 hover:underline"
          >
            ← Model library
          </Link>
          <h1 className="lm-numeric mt-0.5 truncate text-lg font-semibold tracking-tight text-[var(--lm-text)]">
            {model.quant_label ?? model.primary_file}
          </h1>
          <Link
            to="/models/browse/$"
            params={{ _splat: model.repo_id }}
            className="lm-numeric text-xs text-[var(--lm-text-muted)] underline-offset-4 hover:underline"
          >
            {model.repo_id}
          </Link>
        </div>

        <div className="flex flex-wrap items-center gap-2">
          <Button
            icon={<ShieldCheck />}
            loading={verify.isPending}
            onClick={() =>
              verify.mutate(model.id, {
                onSuccess: () =>
                  toast.info('Verifying', {
                    description: 'Every file is re-stated, and re-hashed when checksums are on.',
                  }),
                onError: (error) => toast.error(error),
              })
            }
          >
            Verify
          </Button>
          <Button variant="danger" icon={<Trash2 />} onClick={() => setDoomed(true)}>
            Delete
          </Button>
          <Link
            to="/instances/new"
            search={{ model_id: model.id }}
            className={linkButtonClass('primary')}
          >
            Create an instance
          </Link>
        </div>
      </div>

      <Panel className="space-y-3">
        <div className="flex flex-wrap items-center gap-2">
          <StatusBadge kind="model" state={model.state} />
          <Badge tone="neutral">{KIND_LABELS[model.kind] ?? model.kind}</Badge>
          {model.has_vision ? <Badge tone="info">vision</Badge> : null}
          {model.origin === 'scanned' ? <Badge tone="neutral">scanned</Badge> : null}
          {inUse.map((ref) => (
            <Link key={ref.id} to="/instances/$id" params={{ id: ref.id }}>
              <Badge tone="accent" title={`Used as the ${ref.role} model`}>
                {ref.name}
              </Badge>
            </Link>
          ))}
        </div>

        <dl className="grid grid-cols-2 gap-3 sm:grid-cols-4">
          <Field label="On disk" mono>
            {formatBytes(model.bytes_on_disk)}
          </Field>
          <Field label="Declared size" mono>
            {formatBytes(model.total_bytes)}
          </Field>
          <Field label="Architecture" mono>
            {model.arch ?? '—'}
          </Field>
          <Field label="File type" mono>
            {model.file_type ?? '—'}
          </Field>
          <Field label="Layers" mono>
            {model.n_layer === null || model.n_layer === undefined
              ? '—'
              : formatCount(model.n_layer)}
          </Field>
          <Field label="Trained context" mono>
            {model.n_ctx_train === null || model.n_ctx_train === undefined
              ? '—'
              : formatCount(model.n_ctx_train)}
          </Field>
          <Field label="Vocabulary" mono>
            {model.n_vocab === null || model.n_vocab === undefined
              ? '—'
              : formatCount(model.n_vocab)}
          </Field>
          <Field label="Experts" mono>
            {model.n_expert
              ? `${formatCount(model.n_expert_used ?? 0)} of ${formatCount(model.n_expert)}`
              : 'dense'}
          </Field>
          <Field label="Revision" mono>
            {model.ref_name ? `${model.ref_name} · ` : ''}
            {model.revision.slice(0, 12)}
          </Field>
          <Field label="Last verified" mono>
            {model.last_verified_at ? formatRelative(model.last_verified_at) : 'never'}
          </Field>
          <Field label="Cache root" mono className="col-span-2">
            {model.root_path}
          </Field>
        </dl>

        <div>
          <p className="text-[11px] tracking-wide text-[var(--lm-text-faint)] uppercase">
            Snapshot
          </p>
          <p className="lm-numeric mt-0.5 text-[12px] break-all text-[var(--lm-text-muted)]">
            {model.snapshot_dir}
          </p>
        </div>
      </Panel>

      <Tabs value={tab} onValueChange={setTab}>
        <TabsList>
          <TabsTrigger value="files">Files</TabsTrigger>
          <TabsTrigger value="projector">Projector</TabsTrigger>
          <TabsTrigger value="metadata">GGUF metadata</TabsTrigger>
        </TabsList>

        <TabsContent value="files">
          <Panel flush>
            <div className="w-full overflow-x-auto">
              <table className="w-full border-collapse text-sm">
                <caption className="sr-only">Files of this model, with their cache blobs</caption>
                <thead>
                  <tr className="border-b border-[var(--lm-border)]">
                    <Th>File</Th>
                    <Th>State</Th>
                    <Th align="right">Size</Th>
                    <Th align="right">On disk</Th>
                    <Th>Blob</Th>
                  </tr>
                </thead>
                <tbody>
                  {files.map((file) => (
                    <FileRow key={file.id} file={file} />
                  ))}
                </tbody>
              </table>
            </div>
          </Panel>
        </TabsContent>

        <TabsContent value="projector">
          <Panel className="space-y-3">
            <PanelHeader
              title="Multimodal projector"
              description="A projector is its own model row, shared across every quantization of a repository. Choosing one here pins the pairing, so no later cache scan overrules it."
            />

            {mmproj ? (
              <div className="flex flex-wrap items-center justify-between gap-2 rounded-[var(--lm-radius)] bg-[var(--lm-surface-sunken)] px-3 py-2">
                <Link
                  to="/models/$id"
                  params={{ id: mmproj.id }}
                  className="lm-numeric flex min-w-0 items-center gap-2 text-[13px] text-[var(--lm-text)] underline-offset-4 hover:underline"
                >
                  <Puzzle aria-hidden className="size-3.5 shrink-0" />
                  <span className="truncate">{mmproj.primary_file}</span>
                </Link>
                <span className="flex items-center gap-2">
                  {model.mmproj_auto ? (
                    <Badge tone="neutral" title="Chosen by the scanner, not pinned by hand.">
                      auto-paired
                    </Badge>
                  ) : (
                    <Badge tone="accent">pinned</Badge>
                  )}
                  <span className="lm-numeric text-xs text-[var(--lm-text-muted)]">
                    {formatBytes(mmproj.bytes_on_disk)}
                  </span>
                </span>
              </div>
            ) : (
              <p className="text-sm text-[var(--lm-text-muted)]">No projector is paired.</p>
            )}

            {candidates.length > 0 ? (
              <label className="block max-w-md">
                <span className="text-xs font-medium text-[var(--lm-text)]">
                  Pair a different projector
                </span>
                <Select
                  className="mt-1"
                  mono
                  value={mmproj?.id}
                  placeholder="Choose a projector…"
                  aria-label="Projector"
                  disabled={pair.isPending}
                  onValueChange={(value) =>
                    pair.mutate(
                      { id: model.id, mmproj_model_id: value },
                      {
                        onSuccess: () => toast.success('Projector paired'),
                        onError: (error) => toast.error(error),
                      },
                    )
                  }
                  options={candidates.map((candidate) => ({
                    value: candidate.id,
                    label: candidate.primary_file,
                    description: `${candidate.repo_id} · ${formatBytes(candidate.bytes_on_disk)}`,
                  }))}
                />
              </label>
            ) : (
              <p className="text-xs text-[var(--lm-text-faint)]">
                No projector on this host matches this model.
              </p>
            )}
          </Panel>
        </TabsContent>

        <TabsContent value="metadata">
          <Panel flush>
            {metadata.isPending ? (
              <LoadingPanel>Reading the GGUF header…</LoadingPanel>
            ) : metadata.error ? (
              <div className="p-4">
                <EmptyState
                  tone="error"
                  dense
                  title="The header could not be read"
                  description={(metadata.error as Error).message}
                />
              </div>
            ) : (
              <div className="max-h-[36rem] w-full overflow-auto">
                <table className="w-full border-collapse text-sm">
                  <caption className="sr-only">Every GGUF key and value</caption>
                  <tbody>
                    {Object.entries(metadata.data?.kv ?? {}).map(([key, value]) => (
                      <tr key={key} className="border-b border-[var(--lm-border)] last:border-b-0">
                        <th
                          scope="row"
                          className="lm-numeric w-1/3 px-3 py-1.5 text-left align-top text-[12px] font-normal text-[var(--lm-text-muted)]"
                        >
                          {key}
                        </th>
                        <td className="lm-numeric px-3 py-1.5 text-[12px] break-all text-[var(--lm-text)]">
                          {renderValue(value)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Panel>
        </TabsContent>
      </Tabs>

      <DeleteModelDialog
        open={doomed}
        onOpenChange={setDoomed}
        model={model}
        onDeleted={() => void navigate({ to: '/models' })}
      />
    </div>
  );
}

function Th({ children, align = 'left' }: { children: string; align?: 'left' | 'right' }) {
  return (
    <th
      scope="col"
      className={`px-3 py-2 text-[11px] font-medium tracking-wide text-[var(--lm-text-faint)] uppercase ${
        align === 'right' ? 'text-right' : 'text-left'
      }`}
    >
      {children}
    </th>
  );
}

function FileRow({ file }: { file: ModelFile }) {
  const missing = file.state === 'missing' || file.state === 'corrupt';
  return (
    <tr className="border-b border-[var(--lm-border)] last:border-b-0">
      <td className="lm-numeric px-3 py-2 text-[12px] text-[var(--lm-text)]">
        {file.filename}
        {file.shard_total > 1 ? (
          <span className="ml-2 text-[var(--lm-text-faint)]">
            shard {file.shard_index}/{file.shard_total}
          </span>
        ) : null}
      </td>
      <td className="px-3 py-2">
        <span className="flex items-center gap-1.5">
          <Badge tone={missing ? 'danger' : file.state === 'present' ? 'ok' : 'neutral'}>
            {FILE_STATE_LABELS[file.state] ?? file.state}
          </Badge>
          {file.checksum_verified ? (
            <BadgeCheck aria-label="Checksum verified" className="size-3.5 text-[var(--lm-ok)]" />
          ) : null}
        </span>
      </td>
      <td className="lm-numeric px-3 py-2 text-right text-[12px] text-[var(--lm-text-muted)]">
        {formatBytes(file.size_bytes)}
      </td>
      <td className="lm-numeric px-3 py-2 text-right text-[12px] text-[var(--lm-text-muted)]">
        {formatBytes(file.bytes_on_disk)}
      </td>
      <td className="lm-numeric px-3 py-2 text-[11px] break-all text-[var(--lm-text-faint)]">
        {file.blob_path ?? '—'}
      </td>
    </tr>
  );
}

/** GGUF values are scalars, arrays and nested objects; none of them are React nodes. */
function renderValue(value: unknown): string {
  if (value === null || value === undefined) return '—';
  if (typeof value === 'string') return value;
  if (typeof value === 'number' || typeof value === 'boolean') return String(value);
  if (Array.isArray(value)) {
    const head = value.slice(0, 8).map((item) => renderValue(item));
    return value.length > 8
      ? `[${head.join(', ')}, … ${formatCount(value.length)} values]`
      : `[${head.join(', ')}]`;
  }
  return JSON.stringify(value);
}
