/**
 * One Hugging Face repository — DESIGN section 4, screen 9.
 *
 * "Rendered card, quant table with **true sizes** and a fit verdict per quant per GPU, shard groups
 * collapsed, mmproj auto-paired, download button."
 *
 * The screen is three queries and one estimate. `GET /hf/model` gives the repository's facts,
 * `GET /hf/tree` gives the quantizations already grouped into downloadable units with true
 * `lfs.size` totals, `GET /hf/card` gives the README as sanitized HTML, and
 * `POST /fit/estimate-batch` measures every quantization against this host's live VRAM — reading
 * each GGUF header over an HTTP range request, so the verdict is available *before* twenty
 * gigabytes are downloaded rather than after.
 *
 * Which quantization is expanded and which GPUs participate live in the URL (`?file=`, `?gpu=`),
 * because "here is the quant that fits your card" is a link worth sending. The context length and
 * cache types are screen state: they are a question being asked, not a filter over a list, and the
 * `repoSearchSchema` of searchParams.ts deliberately declares only the two selections.
 */

import { useMemo, useState } from 'react';
import { Link, useNavigate, useParams, useSearch } from '@tanstack/react-router';
import { ExternalLink, Lock, Puzzle, ThumbsUp } from 'lucide-react';

import { Badge, Button, EmptyState, Panel, PanelHeader } from '../../components';
import type { FitReport, HFTreeGroup } from '../../api/types';
import { formatBytes, formatCount, formatRelative } from '../../format';
import { DEFAULT_FIT_SETTINGS } from '../../features/models/api';
import type { FitSettings } from '../../features/models/api';
import { DownloadDialog } from '../../features/models/DownloadDialog';
import { FitControls } from '../../features/models/FitControls';
import { devicesOf, useFitBatch } from '../../features/models/fit';
import { gatedFrom, hubUrl, useHFCard, useHFModel, useHFTree } from '../../features/models/hf';
import { ExternalLinkButton, linkButtonClass } from '../../features/models/LinkButton';
import { ModelCard } from '../../features/models/ModelCard';
import { primaryFileOf, QuantTable } from '../../features/models/QuantTable';

export function BrowseRepoScreen() {
  const params = useParams({ from: '/app/models/browse/$' });
  const search = useSearch({ from: '/app/models/browse/$' });
  const navigate = useNavigate();

  const repoId = params._splat ?? '';

  const model = useHFModel(repoId);
  const tree = useHFTree(repoId);
  const card = useHFCard(repoId);

  const [tuning, setTuning] = useState<Omit<FitSettings, 'gpus'>>(DEFAULT_FIT_SETTINGS);
  const settings: FitSettings = { ...tuning, gpus: search.gpu ?? [] };

  const [pending, setPending] = useState<HFTreeGroup | null>(null);

  const groups = useMemo(() => tree.data?.groups ?? [], [tree.data]);
  const projectors = useMemo(() => tree.data?.mmproj ?? [], [tree.data]);
  const files = useMemo(() => groups.map(primaryFileOf).filter(Boolean), [groups]);

  const fit = useFitBatch({
    repoId,
    ...(tree.data?.revision ? { revision: tree.data.revision } : {}),
    files,
    settings,
  });

  const reports = useMemo(() => {
    const byFile = new Map<string, FitReport>();
    for (const report of fit.data?.reports ?? []) {
      if (report.file) byFile.set(report.file, report);
    }
    return byFile;
  }, [fit.data]);

  const devices = useMemo(() => devicesOf(fit.data?.reports ?? []), [fit.data]);

  const setSearch = (next: { file?: string | undefined; gpu?: string[] | undefined }) => {
    void navigate({
      to: '/models/browse/$',
      params: { _splat: repoId },
      search: (prev) => ({ ...prev, ...next }),
      replace: true,
    });
  };

  const gated = gatedFrom(tree.error) ?? gatedFrom(model.error);

  if (gated) {
    return (
      <div className="space-y-4">
        <RepoHeading repoId={repoId} />
        <EmptyState
          tone="error"
          icon={<Lock />}
          title="This repository is gated"
          description="Its files are listed only to accounts that have been granted access, and grants are made on the Hub itself, in a browser."
          action={
            <ExternalLinkButton
              href={gated.requestUrl || hubUrl(repoId)}
              variant="primary"
              icon={<ExternalLink />}
            >
              Request access on Hugging Face
            </ExternalLinkButton>
          }
          secondaryAction={
            <Link
              to="/settings"
              search={{ group: 'huggingface' }}
              className={linkButtonClass('ghost')}
            >
              Check the stored token
            </Link>
          }
        />
      </div>
    );
  }

  return (
    <div className="space-y-4">
      <RepoHeading repoId={repoId} />

      {model.data ? (
        <Panel>
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div className="flex flex-wrap items-center gap-2">
              {model.data.gated ? (
                <Badge tone="warn" icon={<Lock />}>
                  Gated
                </Badge>
              ) : null}
              {model.data.private ? <Badge tone="info">Private</Badge> : null}
              {model.data.gguf ? (
                <Badge tone="neutral">
                  {model.data.gguf.architecture} · {formatCount(model.data.gguf.context_length)} ctx
                </Badge>
              ) : null}
              {Object.keys(model.data.local_model_ids).length > 0 ? (
                <Badge tone="ok">
                  {formatCount(Object.keys(model.data.local_model_ids).length)} on this host
                </Badge>
              ) : null}
            </div>

            <div className="lm-numeric flex flex-wrap items-center gap-3 text-xs text-[var(--lm-text-muted)]">
              <span>↓ {formatCount(model.data.downloads)}</span>
              <span className="flex items-center gap-1">
                <ThumbsUp aria-hidden className="size-3" />
                {formatCount(model.data.likes)}
              </span>
              {model.data.last_modified ? (
                <span title={model.data.last_modified}>
                  updated {formatRelative(model.data.last_modified)}
                </span>
              ) : null}
              <span title="The commit this page is describing">
                {model.data.sha.slice(0, 12) || '—'}
              </span>
            </div>
          </div>
        </Panel>
      ) : null}

      <FitControls
        settings={settings}
        onChange={(next) => {
          const { gpus, ...rest } = next;
          setTuning(rest);
          setSearch({ gpu: gpus.length > 0 ? gpus : undefined });
        }}
        devices={devices}
        trainedCtx={model.data?.gguf?.context_length ?? null}
        busy={fit.isFetching}
      />

      <Panel flush>
        <div className="border-b border-[var(--lm-border)] px-4 py-3">
          <PanelHeader
            title="Quantizations"
            description="Sizes are the true LFS object sizes, not the pointer files. A shard set is one row."
          />
        </div>

        {tree.isPending ? (
          <p className="px-4 py-8 text-center text-sm text-[var(--lm-text-muted)]">
            Reading the file tree…
          </p>
        ) : tree.error ? (
          <div className="p-4">
            <EmptyState
              tone="error"
              title="The file tree could not be read"
              description={(tree.error as Error).message}
              action={<Button onClick={() => void tree.refetch()}>Try again</Button>}
            />
          </div>
        ) : (
          <QuantTable
            groups={groups}
            reports={reports}
            recommendedFile={fit.data?.recommended_file ?? ''}
            openFile={search.file}
            onOpenChange={(file) => setSearch({ file })}
            onDownload={setPending}
            fitLoading={fit.isPending || fit.isFetching}
            {...(fit.error ? { fitError: (fit.error as Error).message } : {})}
          />
        )}
      </Panel>

      {projectors.length > 0 ? (
        <Panel className="space-y-3">
          <PanelHeader
            title="Multimodal projectors"
            description="Paired automatically when a quantization is downloaded, and stored as their own model so every quantization here can share one."
          />
          <ul className="space-y-1">
            {projectors.map((projector) => (
              <li
                key={projector.key}
                className="flex items-center justify-between gap-3 rounded-[var(--lm-radius)] bg-[var(--lm-surface-sunken)] px-3 py-2"
              >
                <span className="lm-numeric flex min-w-0 items-center gap-2 text-[13px] text-[var(--lm-text)]">
                  <Puzzle aria-hidden className="size-3.5 shrink-0 text-[var(--lm-text-faint)]" />
                  <span className="truncate">{primaryFileOf(projector)}</span>
                </span>
                <span className="lm-numeric shrink-0 text-xs text-[var(--lm-text-muted)]">
                  {projector.local_model_id ? 'on disk · ' : ''}
                  {formatBytes(projector.total_bytes)}
                </span>
              </li>
            ))}
          </ul>
        </Panel>
      ) : null}

      <ModelCard
        card={card.data}
        repoId={repoId}
        loading={card.isPending}
        {...(card.error ? { unavailable: (card.error as Error).message } : {})}
      />

      <DownloadDialog
        open={pending !== null}
        onOpenChange={(open) => {
          if (!open) setPending(null);
        }}
        repoId={repoId}
        {...(tree.data?.revision ? { revision: tree.data.revision } : {})}
        group={pending}
        mmprojGroups={projectors}
      />
    </div>
  );
}

function RepoHeading({ repoId }: { repoId: string }) {
  return (
    <div className="flex flex-wrap items-end justify-between gap-3">
      <div className="min-w-0">
        <Link
          to="/models/browse"
          className="text-xs text-[var(--lm-text-muted)] underline-offset-4 hover:underline"
        >
          ← Browse Hugging Face
        </Link>
        <h1 className="lm-numeric mt-0.5 truncate text-lg font-semibold tracking-tight text-[var(--lm-text)]">
          {repoId}
        </h1>
      </div>
      <a
        href={hubUrl(repoId)}
        target="_blank"
        rel="noreferrer noopener"
        className="inline-flex items-center gap-1.5 text-sm text-[var(--lm-accent)] underline-offset-4 hover:underline"
      >
        View on Hugging Face
        <ExternalLink aria-hidden className="size-3.5" />
      </a>
    </div>
  );
}
