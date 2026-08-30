import { useState } from 'react';
import { useNavigate } from '@tanstack/react-router';
import { useQuery } from '@tanstack/react-query';
import { AlertTriangle, Check, Cpu, ExternalLink, HelpCircle, RefreshCw, X } from 'lucide-react';
import { Badge, Button, PanelHeader, Select, toast } from '../../components';
import { api } from '../../api/client';
import { queryKeys } from '../../api/keys';
import type { LlamacppPlan } from '../../api/types';
import { formatBytes } from '../../format';
import { WizardStep } from '../../setup/WizardStep';
import { nextStepPath, useSkipStep } from '../../setup/useWizard';
import { useWizardScratch } from './scratch';
import type { Backend } from './scratch';
import { DISTRO_FAMILIES, TOOLCHAIN_TOOLS, packageHint, toolVerdict } from './toolchainGuidance';
import type { DistroFamily, ToolGuidance, ToolVerdict } from './toolchainGuidance';

/**
 * Wizard step `toolchain` (DESIGN section 11.2): "probe with per-tool found/version/needed and
 * distro-doc links; Re-check; Continue CPU-only".
 *
 * **Where the probe comes from.** `GET /api/v1/llamacpp/plan` is the endpoint that already answers
 * "what would happen, and whether it can" (section 3.5), and its `missing_tools` is by construction
 * the same list section 6.5's `preflight` aborts on — the plan and the wizard read one probe, so
 * they can never disagree. It is asked twice, once per backend, with `force_source=1`: a *prebuilt*
 * plan reports no missing tools ("a prebuilt needs no compiler, so its missing-tools list is empty
 * by construction"), and the question this step is asking is what this host could *build*.
 *
 * The difference between the two answers is the whole content of the step: a tool missing from the
 * CPU plan blocks every source build, one missing only from the CUDA plan costs GPU offload and
 * nothing else. That second case is the "Continue CPU-only" branch, and it is the primary button
 * here rather than a footnote, because on a host with no CUDA toolkit it is the right answer.
 *
 * Nothing is installed for the user and no command is suggested to run as root (section 6.5). Each
 * tool names the package that carries it and links its upstream documentation; the distribution
 * picker is a display choice, since the daemon's own detection is not on this endpoint.
 */
export function ToolchainStep() {
  const navigate = useNavigate();
  const skip = useSkipStep();
  const setBackend = useWizardScratch((state) => state.setBackend);
  const [family, setFamily] = useState<DistroFamily | 'all'>('all');

  const cpu = usePlanProbe('cpu');
  const cuda = usePlanProbe('cuda');

  const probing = cpu.isFetching || cuda.isFetching;
  const cpuPlan = cpu.data;
  const cudaPlan = cuda.data;

  const cudaOK = cudaPlan?.can_proceed === true;
  const cpuOK = cpuPlan?.can_proceed === true;
  const answered = cpuPlan !== undefined || cudaPlan !== undefined;

  // The backend this step steers the next one towards. CUDA when the host can build it, CPU when it
  // cannot — which is exactly what "Continue CPU-only" means once it is a decision rather than a
  // button.
  const chosen: Backend = cudaOK ? 'cuda' : 'cpu';

  const proceed = () => {
    setBackend(chosen);
    if (chosen === 'cpu') {
      // Section 11.2's skippable column: this step is skippable "to CPU-only", and the skip is a
      // row state rather than a client-side jump, so it is recorded before moving on.
      skip.mutate('toolchain', {
        onSuccess: () => void navigate({ to: nextStepPath('toolchain') }),
        onError: (error) => toast.error(error),
      });
      return;
    }
    void navigate({ to: nextStepPath('toolchain') });
  };

  return (
    <WizardStep
      step="toolchain"
      banner={
        answered ? (
          <div className="flex flex-wrap items-center gap-2">
            <BackendVerdict label="CPU build" ok={cpuOK} plan={cpuPlan} />
            <BackendVerdict label="CUDA build" ok={cudaOK} plan={cudaPlan} />
            {cudaPlan?.cuda_arch?.length ? (
              <Badge tone="neutral">Compute capability {cudaPlan.cuda_arch.join(', ')}</Badge>
            ) : null}
          </div>
        ) : null
      }
      primaryAction={
        <Button
          variant="primary"
          loading={skip.isPending}
          disabled={probing && !answered}
          onClick={proceed}
        >
          {cudaOK ? 'Continue with CUDA' : 'Continue CPU-only'}
        </Button>
      }
    >
      <div className="space-y-4">
        <PanelHeader
          title="Build tools"
          description={
            probing
              ? 'Probing this host…'
              : cpu.isError || cuda.isError
                ? 'The probe could not complete. Re-check once the daemon can reach its release feed.'
                : 'Read from the same probe the build preflight aborts on, so nothing here can disagree with a build.'
          }
          actions={
            <>
              <Select<DistroFamily | 'all'>
                value={family}
                onValueChange={setFamily}
                aria-label="Show package names for"
                className="w-56"
                options={[
                  { value: 'all', label: 'Every distribution' },
                  ...DISTRO_FAMILIES.map((entry) => ({ value: entry.id, label: entry.label })),
                ]}
              />
              <Button
                icon={<RefreshCw />}
                loading={probing}
                onClick={() => {
                  void cpu.refetch();
                  void cuda.refetch();
                }}
              >
                Re-check
              </Button>
            </>
          }
        />

        <ul className="divide-y divide-[var(--lm-border)]">
          {TOOLCHAIN_TOOLS.map((tool) => (
            <ToolRow
              key={tool.name}
              tool={tool}
              verdict={toolVerdict(tool, cpuPlan?.missing_tools, cudaPlan?.missing_tools)}
              family={family}
            />
          ))}
        </ul>

        <SpaceLine plan={cpuPlan ?? cudaPlan} />

        {!cudaOK && cpuOK ? (
          <p className="text-sm text-[var(--lm-text-muted)]">
            A CPU-only host is a supported host: llama.cpp runs models on the CPU, and everything
            after this step — downloads, instances, benchmarks — works the same way. Installing the
            CUDA toolkit later and rebuilding from Settings is all it takes to change this answer.
          </p>
        ) : null}
      </div>
    </WizardStep>
  );
}

/**
 * One backend's plan, forced to the source branch so `missing_tools` is populated.
 *
 * `staleTime: 0` because "Re-check" must mean re-check, and `retry: false` because a plan that
 * failed to resolve a release tag will fail the same way three times and the error is the answer.
 */
function usePlanProbe(backend: Backend) {
  return useQuery({
    queryKey: queryKeys.llamacpp.plan({ backend, probe: 'toolchain' }),
    queryFn: () =>
      api.get('/api/v1/llamacpp/plan', {
        query: { channel: 'stable', backend, force_source: true },
      }),
    staleTime: 0,
    retry: false,
  });
}

function BackendVerdict({
  label,
  ok,
  plan,
}: {
  label: string;
  ok: boolean;
  plan: LlamacppPlan | undefined;
}) {
  if (!plan) return <Badge tone="neutral">{label}: unknown</Badge>;
  const missing = plan.missing_tools.length;
  return (
    <Badge tone={ok ? 'ok' : 'warn'} dot>
      {label}:{' '}
      {ok
        ? 'ready to build'
        : missing > 0
          ? `${missing} tool${missing === 1 ? '' : 's'} missing`
          : 'not enough free space'}
    </Badge>
  );
}

const VERDICT_STYLE: Record<
  ToolVerdict,
  { tone: 'ok' | 'warn' | 'danger' | 'neutral'; label: string; Icon: typeof Check }
> = {
  present: { tone: 'ok', label: 'Found', Icon: Check },
  blocking: { tone: 'danger', label: 'Missing', Icon: X },
  'cuda-only': { tone: 'warn', label: 'Missing — CUDA only', Icon: AlertTriangle },
  unreported: { tone: 'neutral', label: 'Optional', Icon: HelpCircle },
};

function ToolRow({
  tool,
  verdict,
  family,
}: {
  tool: ToolGuidance;
  verdict: ToolVerdict;
  family: DistroFamily | 'all';
}) {
  const style = VERDICT_STYLE[verdict];
  const Icon = style.Icon;
  const hint = verdict === 'present' ? null : packageHint(tool, family);

  return (
    <li className="flex items-start gap-3 py-2.5 first:pt-0 last:pb-0">
      <span
        aria-hidden
        className="mt-0.5 flex size-5 shrink-0 items-center justify-center rounded-full bg-[var(--lm-neutral-soft)] text-[var(--lm-text-faint)]"
      >
        <Icon className="size-3" />
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline gap-2">
          <span className="lm-numeric text-[13px] font-medium text-[var(--lm-text)]">
            {tool.label}
          </span>
          <Badge tone={style.tone}>{style.label}</Badge>
        </div>
        <p className="mt-0.5 text-xs text-[var(--lm-text-muted)]">{tool.purpose}</p>
        {hint ? <p className="mt-0.5 text-xs text-[var(--lm-text-faint)]">{hint}</p> : null}
      </div>
      <a
        href={tool.docsUrl}
        target="_blank"
        rel="noreferrer noopener"
        className="mt-0.5 inline-flex shrink-0 items-center gap-1 text-xs text-[var(--lm-accent)] underline-offset-4 hover:underline"
      >
        Docs
        <ExternalLink aria-hidden className="size-3" />
      </a>
    </li>
  );
}

/** Free space is the other half of `can_proceed`, and it is the half a tool list cannot show. */
function SpaceLine({ plan }: { plan: LlamacppPlan | undefined }) {
  if (!plan) return null;
  return (
    <div className="flex items-center gap-2 rounded-[var(--lm-radius)] bg-[var(--lm-surface-sunken)] px-3 py-2">
      <Cpu aria-hidden className="size-4 shrink-0 text-[var(--lm-text-faint)]" />
      <p className="text-xs text-[var(--lm-text-muted)]">
        A source build needs{' '}
        <span className="lm-numeric text-[var(--lm-text)]">{formatBytes(plan.required_bytes)}</span>{' '}
        of free space for objects and binaries;{' '}
        <span className="lm-numeric text-[var(--lm-text)]">{formatBytes(plan.free_bytes)}</span> is
        available.
      </p>
      {plan.free_space_ok ? null : <Badge tone="danger">Not enough</Badge>}
    </div>
  );
}
