import { useEffect, useMemo, useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { ArrowLeft, Check, Database, Download, ExternalLink, Heart, Search } from 'lucide-react';
import {
  Badge,
  Button,
  EmptyState,
  FormField,
  Input,
  PanelHeader,
  Progress,
  Select,
  Spinner,
  StatusBadge,
  toast,
} from '../../components';
import { api } from '../../api/client';
import { ApiError } from '../../api/errors';
import { queryKeys } from '../../api/keys';
import type {
  Download as DownloadRow,
  FitReport,
  HFSearchResult,
  HFTreeGroup,
  Model,
} from '../../api/types';
import { EventStreamProvider, useLiveQueryOptions } from '../../events/EventStreamProvider';
import {
  formatBytes,
  formatByteProgress,
  formatBytesPerSecond,
  formatCount,
  formatSeconds,
} from '../../format';
import { WizardStep } from '../../setup/WizardStep';
import { fieldProps } from './fieldProps';
import { useWizardScratch } from './scratch';

/**
 * Wizard step `models` (DESIGN section 11.2): "shows scan results first ('6 models already on
 * disk'); otherwise HF search with the fit calculator live; the download continues in the
 * background".
 *
 * The order is the whole design of this step. A host that already has GGUFs in its Hugging Face
 * cache — the common case for anyone who has run llama.cpp by hand — should not be asked to
 * download one, so the scan's results come first and the step is skippable from there. Search is
 * the second offer, not the first.
 *
 * "The download continues in the background" is literal: `POST /downloads` answers 202 with a job
 * receipt (section 3.8), and this step neither blocks on it nor owns it. Leaving the page, or
 * finishing the wizard while a download runs, is expected — the queue is a screen of its own.
 */
export function ModelsStep() {
  return (
    <EventStreamProvider topics={['downloads']}>
      <ModelsStepBody />
    </EventStreamProvider>
  );
}

function ModelsStepBody() {
  const live = useLiveQueryOptions();
  const queryClient = useQueryClient();
  const scratch = useWizardScratch();
  const [searching, setSearching] = useState(false);

  const models = useQuery({
    queryKey: queryKeys.models.list({ state: 'ready' }),
    queryFn: () => api.get('/api/v1/models', { query: { state: 'ready', kind: 'text' } }),
    ...live,
  });

  const downloads = useQuery({
    queryKey: queryKeys.downloads.list({ state: 'active' }),
    queryFn: () => api.get('/api/v1/downloads', { query: { state: 'active' } }),
    ...live,
  });

  const onDisk: readonly Model[] = models.data?.items ?? [];
  const active: readonly DownloadRow[] = downloads.data?.items ?? [];

  // The step's own gate: a model to build an instance from. Anything on disk will do, and the
  // selection is scratch state because it is a preference, not a row.
  const selected = scratch.modelId ?? onDisk[0]?.id ?? null;
  const hasModel = onDisk.length > 0;

  // Nothing on disk and nothing downloading is the case search exists for; open it unasked.
  useEffect(() => {
    if (!models.isPending && onDisk.length === 0 && active.length === 0) setSearching(true);
  }, [models.isPending, onDisk.length, active.length]);

  // `models` is not an SSE topic (section 3.14 lists eight and this is not one of them), so the
  // library is re-read at the one moment it can have changed: when the active queue empties.
  const activeCount = active.length;
  const previousActive = useRef(activeCount);
  useEffect(() => {
    if (previousActive.current > 0 && activeCount === 0) {
      void queryClient.invalidateQueries({ queryKey: queryKeys.family('models') });
    }
    previousActive.current = activeCount;
  }, [activeCount, queryClient]);

  return (
    <WizardStep
      step="models"
      canContinue={hasModel}
      continueLabel={hasModel ? 'Continue' : 'Waiting for a model'}
      banner={
        active.length ? (
          <div className="space-y-2 rounded-[var(--lm-radius-lg)] border border-[var(--lm-border)] bg-[var(--lm-surface)] p-3">
            {active.map((row) => (
              <DownloadLine key={row.id} row={row} />
            ))}
          </div>
        ) : null
      }
    >
      {searching ? (
        <BrowseHuggingFace
          onBack={hasModel || active.length > 0 ? () => setSearching(false) : null}
        />
      ) : (
        <div className="space-y-3">
          <PanelHeader
            title={
              models.isPending
                ? 'Reading the cache…'
                : `${formatCount(onDisk.length)} model${onDisk.length === 1 ? '' : 's'} already on disk`
            }
            description="Found by the cache scan. Pick the one the first instance should serve."
            actions={
              <Button icon={<Search />} onClick={() => setSearching(true)}>
                Search Hugging Face
              </Button>
            }
          />

          {onDisk.length === 0 ? (
            <EmptyState
              dense
              icon={<Database />}
              title="No GGUF models in the cache yet"
              description="Search Hugging Face for one — the fit calculator will say which quantization this host can actually run."
              action={
                <Button variant="primary" icon={<Search />} onClick={() => setSearching(true)}>
                  Search Hugging Face
                </Button>
              }
            />
          ) : (
            <ul className="divide-y divide-[var(--lm-border)]">
              {onDisk.map((model) => (
                <li key={model.id}>
                  <label className="flex cursor-pointer items-center gap-3 py-2.5">
                    <input
                      type="radio"
                      name="wizard-model"
                      className="size-4 accent-[var(--lm-accent)]"
                      checked={selected === model.id}
                      onChange={() => scratch.setModelId(model.id)}
                    />
                    <span className="min-w-0 flex-1">
                      <span className="block truncate text-sm text-[var(--lm-text)]">
                        {model.repo_id}
                      </span>
                      <span className="lm-numeric block truncate text-xs text-[var(--lm-text-faint)]">
                        {model.primary_file}
                      </span>
                    </span>
                    {model.quant_label ? <Badge tone="neutral">{model.quant_label}</Badge> : null}
                    {model.has_vision ? <Badge tone="info">Vision</Badge> : null}
                    <span className="lm-numeric shrink-0 text-xs text-[var(--lm-text-muted)]">
                      {formatBytes(model.total_bytes)}
                    </span>
                  </label>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </WizardStep>
  );
}

/** One row of the live queue. `downloads` is an SSE topic, so these numbers arrive, not polled. */
function DownloadLine({ row }: { row: DownloadRow }) {
  const percent = row.bytes_total > 0 ? (row.bytes_done / row.bytes_total) * 100 : null;
  return (
    <Progress
      value={percent}
      tone={row.state === 'failed' ? 'danger' : 'accent'}
      label={
        <span className="flex items-center gap-2">
          <StatusBadge kind="download" state={row.state} />
          <span className="truncate">{row.repo_id}</span>
        </span>
      }
      detail={
        <>
          {formatByteProgress(row.bytes_done, row.bytes_total)}
          {row.speed_bps > 0 ? ` · ${formatBytesPerSecond(row.speed_bps)}` : ''}
          {row.eta_sec ? ` · ${formatSeconds(row.eta_sec)} left` : ''}
        </>
      }
    />
  );
}

/** Search, then the repository's quant table with a fit verdict per quantization. */
function BrowseHuggingFace({ onBack }: { onBack: (() => void) | null }) {
  const scratch = useWizardScratch();
  const [term, setTerm] = useState('');
  const [query, setQuery] = useState('');
  const [sort, setSort] = useState<'downloads' | 'likes' | 'trendingScore' | 'lastModified'>(
    'downloads',
  );

  const results = useQuery({
    queryKey: queryKeys.hf.search({ q: query, sort }),
    queryFn: () => api.get('/api/v1/hf/search', { query: { q: query, sort, limit: 20 } }),
    enabled: query.trim().length > 0,
    retry: false,
  });

  const items: readonly HFSearchResult[] = results.data?.items ?? [];
  const repo = scratch.repoId;

  if (repo) {
    return <RepoFiles repo={repo} onBack={() => scratch.setRepoId(null)} />;
  }

  return (
    <div className="space-y-3">
      <PanelHeader
        title="Search Hugging Face"
        description="GGUF repositories only. The fit calculator runs on the quantizations of whichever one you open."
        actions={
          onBack ? (
            <Button variant="ghost" icon={<ArrowLeft />} onClick={onBack}>
              Back to the cache
            </Button>
          ) : null
        }
      />

      <form
        className="flex flex-wrap items-end gap-3"
        onSubmit={(event) => {
          event.preventDefault();
          setQuery(term);
        }}
      >
        <FormField label="Search" className="min-w-64 flex-1">
          {(field) => (
            <Input
              {...field}
              autoFocus
              type="search"
              placeholder="qwen3 gguf"
              value={term}
              onChange={(event) => setTerm(event.target.value)}
            />
          )}
        </FormField>
        <FormField label="Sort by" className="w-44">
          {(field) => (
            <Select<'downloads' | 'likes' | 'trendingScore' | 'lastModified'>
              {...fieldProps(field)}
              value={sort}
              onValueChange={setSort}
              options={[
                { value: 'downloads', label: 'Downloads' },
                { value: 'likes', label: 'Likes' },
                { value: 'trendingScore', label: 'Trending' },
                { value: 'lastModified', label: 'Recently updated' },
              ]}
            />
          )}
        </FormField>
        <Button type="submit" icon={<Search />} loading={results.isFetching}>
          Search
        </Button>
      </form>

      {results.isError ? (
        <p className="text-sm text-[var(--lm-danger)]">
          {results.error instanceof ApiError
            ? results.error.message
            : 'Hugging Face could not be reached.'}
        </p>
      ) : null}

      {query && !results.isFetching && items.length === 0 && !results.isError ? (
        <EmptyState
          dense
          title="Nothing matched"
          description="Try a shorter term, or the repository owner's name."
        />
      ) : null}

      <ul className="divide-y divide-[var(--lm-border)]">
        {items.map((item) => (
          <li key={item.id}>
            <button
              type="button"
              onClick={() => scratch.setRepoId(item.id)}
              className="flex w-full items-center gap-3 py-2.5 text-left hover:bg-[var(--lm-neutral-soft)]"
            >
              <span className="min-w-0 flex-1">
                <span className="block truncate text-sm text-[var(--lm-text)]">{item.id}</span>
                <span className="block truncate text-xs text-[var(--lm-text-faint)]">
                  {item.gguf
                    ? `${item.gguf.architecture} · ${formatCount(item.gguf.total)} GGUF files · ${formatCount(item.gguf.context_length)} ctx`
                    : item.tags.slice(0, 4).join(' · ')}
                </span>
              </span>
              {item.gated ? <Badge tone="warn">Gated</Badge> : null}
              {item.private ? <Badge tone="warn">Private</Badge> : null}
              <span className="lm-numeric hidden shrink-0 items-center gap-1 text-xs text-[var(--lm-text-muted)] sm:flex">
                <Heart aria-hidden className="size-3" />
                {formatCount(item.likes)}
              </span>
              <span className="lm-numeric shrink-0 text-xs text-[var(--lm-text-muted)]">
                {formatCount(item.downloads)} ↓
              </span>
            </button>
          </li>
        ))}
      </ul>
    </div>
  );
}

/**
 * One repository's quantizations, with the fit report section 3.9 produces for each.
 *
 * `POST /fit/estimate-batch` reads the GGUF header of every candidate over HTTP Range before
 * anything is downloaded, so it is slow and it is allowed to fail: the quant table renders without
 * verdicts rather than not at all. The recommendation it returns is the one preselected.
 */
function RepoFiles({ repo, onBack }: { repo: string; onBack: () => void }) {
  const queryClient = useQueryClient();
  const scratch = useWizardScratch();

  const tree = useQuery({
    queryKey: queryKeys.hf.tree(repo),
    queryFn: () => api.get('/api/v1/hf/tree/{repo}', { path: { repo } }),
    retry: false,
  });

  const groups: readonly HFTreeGroup[] = useMemo(
    () => (tree.data?.groups ?? []).filter((group: HFTreeGroup) => !group.mmproj),
    [tree.data],
  );

  const candidates = useMemo(
    () => groups.flatMap((group) => (group.files[0] ? [group.files[0].path] : [])),
    [groups],
  );

  // A POST, but a computation rather than a change: it reads GGUF headers over HTTP range and
  // writes nothing, so it is a query keyed by its inputs and cached like one.
  const fit = useQuery({
    queryKey: queryKeys.list('fit', { repo, files: candidates }),
    queryFn: () =>
      api.post('/api/v1/fit/estimate-batch', {
        body: { repo_id: repo, files: candidates, flags: {} },
      }),
    enabled: candidates.length > 0,
    retry: false,
  });

  const reports = new Map<string, FitReport>(
    (fit.data?.reports ?? []).flatMap((report: FitReport): [string, FitReport][] =>
      report.file ? [[report.file, report]] : [],
    ),
  );
  const recommended = fit.data?.recommended_file ?? null;

  const download = useMutation({
    mutationFn: (group: HFTreeGroup) =>
      api.post('/api/v1/downloads', {
        body: {
          repo_id: repo,
          files: group.files.map((file) => file.path),
          include_mmproj: true,
        },
        idempotencyKey: `wizard-${repo}-${group.key}`,
      }),
    onSuccess: async (result) => {
      if (result?.model_id) scratch.setModelId(result.model_id);
      scratch.setRepoId(null);
      await queryClient.invalidateQueries({ queryKey: queryKeys.family('downloads') });
      await queryClient.invalidateQueries({ queryKey: queryKeys.family('models') });
      toast.success('Download queued', { description: 'It continues in the background.' });
    },
    onError: (error) => {
      if (error instanceof ApiError && error.code === 'hf_gated') {
        toast.error('This repository is gated', {
          description: 'Accept its terms on huggingface.co, then add a token on this step.',
        });
        return;
      }
      toast.error(error);
    },
  });

  return (
    <div className="space-y-3">
      <PanelHeader
        title={<span className="lm-numeric">{repo}</span>}
        description={
          fit.isFetching
            ? 'Reading each quantization’s GGUF header over HTTP range…'
            : 'True file sizes, shard groups collapsed, and what this host can hold.'
        }
        actions={
          <>
            <a
              href={`https://huggingface.co/${repo}`}
              target="_blank"
              rel="noreferrer noopener"
              className="inline-flex items-center gap-1 text-xs text-[var(--lm-accent)] underline-offset-4 hover:underline"
            >
              On huggingface.co
              <ExternalLink aria-hidden className="size-3" />
            </a>
            <Button variant="ghost" icon={<ArrowLeft />} onClick={onBack}>
              Back to results
            </Button>
          </>
        }
      />

      {tree.isPending ? <Spinner label="Reading the repository" /> : null}
      {tree.isError ? (
        <p className="text-sm text-[var(--lm-danger)]">
          {tree.error instanceof ApiError ? tree.error.message : 'The file list could not be read.'}
        </p>
      ) : null}

      <ul className="divide-y divide-[var(--lm-border)]">
        {groups.map((group) => {
          const report = group.files[0] ? reports.get(group.files[0].path) : undefined;
          const isRecommended = group.files[0]?.path === recommended;
          return (
            <li key={group.key} className="flex flex-wrap items-center gap-3 py-2.5">
              <span className="min-w-0 flex-1">
                <span className="flex items-center gap-2">
                  <span className="lm-numeric text-[13px] text-[var(--lm-text)]">
                    {group.quant_label}
                  </span>
                  {isRecommended ? (
                    <Badge tone="accent" icon={<Check />}>
                      Recommended
                    </Badge>
                  ) : null}
                  {group.local_model_id ? <Badge tone="ok">Already downloaded</Badge> : null}
                  {group.shard_total > 1 ? (
                    <Badge tone="neutral">{group.shard_total} shards</Badge>
                  ) : null}
                  {group.complete ? null : <Badge tone="warn">Incomplete shard set</Badge>}
                </span>
                {report ? (
                  <span className="mt-0.5 block text-xs text-[var(--lm-text-faint)]">
                    {report.notes?.[0] ??
                      `Needs ${formatBytes(report.required_vram_bytes)} of VRAM at ${formatCount(report.inputs.n_ctx)} context`}
                  </span>
                ) : null}
              </span>

              {report ? <StatusBadge kind="fit" state={report.verdict} /> : null}

              <span className="lm-numeric shrink-0 text-xs text-[var(--lm-text-muted)]">
                {formatBytes(group.total_bytes)}
              </span>

              <Button
                size="sm"
                variant={isRecommended ? 'primary' : 'secondary'}
                icon={<Download />}
                loading={download.isPending && download.variables?.key === group.key}
                disabled={!group.complete || Boolean(group.local_model_id)}
                onClick={() => download.mutate(group)}
              >
                {group.local_model_id ? 'On disk' : 'Download'}
              </Button>
            </li>
          );
        })}
      </ul>
    </div>
  );
}
