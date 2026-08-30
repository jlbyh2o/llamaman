import { Link, Outlet } from '@tanstack/react-router';
import { Check, CircleDashed, MinusCircle } from 'lucide-react';
import { LoadingPanel } from '../components';
import { cn } from '../components/cn';
import { ThemeToggle } from '../layout/ThemeToggle';
import { useWizard } from './useWizard';
import type { WizardStepView } from './useWizard';

/**
 * The wizard frame: a progress rail on the left, the step on the right.
 *
 * The rail is read from `wizard_steps` on every render, so it is the same picture after a refresh,
 * on a second browser, or after the daemon restarted mid-build (DESIGN section 11.2). Completed and
 * skipped steps are links, because every step is re-enterable; steps past the current one are not,
 * because their inputs do not exist yet.
 */
export function WizardShell() {
  const wizard = useWizard();

  return (
    <div className="flex min-h-full flex-col bg-[var(--lm-bg)]">
      <header className="flex h-12 shrink-0 items-center gap-2 border-b border-[var(--lm-border)] bg-[var(--lm-surface)] px-4">
        <span
          aria-hidden
          className="flex size-6 items-center justify-center rounded-[var(--lm-radius)] bg-[var(--lm-accent)] text-[11px] font-bold text-[var(--lm-accent-contrast)]"
        >
          LM
        </span>
        <span className="text-sm font-semibold tracking-tight text-[var(--lm-text)]">
          Llama Man
        </span>
        <span className="text-sm text-[var(--lm-text-faint)]">· first-run setup</span>
        <div className="flex-1" />
        <ThemeToggle />
      </header>

      <div className="mx-auto flex w-full max-w-5xl flex-1 flex-col gap-6 p-4 lg:flex-row lg:p-8">
        <aside className="lg:w-56 lg:shrink-0">
          <ol className="flex gap-2 overflow-x-auto lg:flex-col lg:gap-0.5 lg:overflow-visible">
            {wizard.steps
              .filter((step) => step.id !== 'done')
              .map((step, index) => (
                <RailItem
                  key={step.id}
                  step={step}
                  index={index}
                  active={wizard.activeId === step.id}
                />
              ))}
          </ol>
        </aside>

        <main className="min-w-0 flex-1">
          {wizard.isPending ? <LoadingPanel>Reading setup state…</LoadingPanel> : <Outlet />}
        </main>
      </div>
    </div>
  );
}

function RailItem({
  step,
  index,
  active,
}: {
  step: WizardStepView;
  index: number;
  active: boolean;
}) {
  const Icon =
    step.state === 'complete' ? Check : step.state === 'skipped' ? MinusCircle : CircleDashed;

  const body = (
    <span
      className={cn(
        'flex items-center gap-2.5 rounded-[var(--lm-radius)] px-2 py-1.5 text-sm whitespace-nowrap',
        active
          ? 'bg-[var(--lm-accent-soft)] font-medium text-[var(--lm-accent)]'
          : step.state === 'complete' || step.state === 'skipped'
            ? 'text-[var(--lm-text-muted)] hover:bg-[var(--lm-neutral-soft)] hover:text-[var(--lm-text)]'
            : 'text-[var(--lm-text-faint)]',
      )}
    >
      <span
        aria-hidden
        className={cn(
          'flex size-5 shrink-0 items-center justify-center rounded-full border text-[10px]',
          step.state === 'complete'
            ? 'border-[var(--lm-ok)] bg-[var(--lm-ok-soft)] text-[var(--lm-ok)]'
            : active
              ? 'border-[var(--lm-accent)] text-[var(--lm-accent)]'
              : 'border-[var(--lm-border-strong)] text-[var(--lm-text-faint)]',
        )}
      >
        {step.state === 'complete' || step.state === 'skipped' ? (
          <Icon className="size-3" />
        ) : (
          index + 1
        )}
      </span>
      {step.railLabel}
    </span>
  );

  return (
    <li aria-current={active ? 'step' : undefined}>
      {step.reachable && !active ? (
        <Link to={step.path} className="block">
          {body}
        </Link>
      ) : (
        body
      )}
    </li>
  );
}
