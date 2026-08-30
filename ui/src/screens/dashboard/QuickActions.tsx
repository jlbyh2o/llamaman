import { Link } from '@tanstack/react-router';
import { Activity, Boxes, Download, Gauge, Plus, Search } from 'lucide-react';
import type { ReactNode } from 'react';
import { Panel, PanelHeader } from '../../components';

/**
 * The quick actions of DESIGN section 4, screen 3.
 *
 * Deliberately links and not buttons. Everything here opens a screen that owns the work — the
 * instance form, the Hugging Face browser, the version manager — rather than firing a mutation from
 * a card that could not show what happened next; a landing page is where you decide what to do, not
 * where irreversible things are started. Being real links also means middle-click, ⌘-click and the
 * keyboard all behave the way a technical user expects.
 */
export function QuickActions({ hasInstances }: { hasInstances: boolean }) {
  return (
    <Panel>
      <PanelHeader title="Quick actions" />
      <ul className="mt-3 grid gap-1">
        <ActionLink
          to="/instances/new"
          icon={<Plus />}
          label={hasInstances ? 'Create another instance' : 'Create the first instance'}
          hint="Three panes, a live fit panel and the rendered argv"
        />
        <ActionLink
          to="/models/browse"
          icon={<Search />}
          label="Browse Hugging Face"
          hint="True file sizes and a fit verdict per quantization"
        />
        <ActionLink
          to="/models"
          icon={<Boxes />}
          label="Local models"
          hint="What is on disk, grouped by repository"
        />
        <ActionLink
          to="/downloads"
          icon={<Download />}
          label="Download queue"
          hint="Progress, speed, pause and reorder"
        />
        <ActionLink
          to="/bench/new"
          icon={<Gauge />}
          label="Run a benchmark"
          hint="A sweep with its point count and time estimated first"
        />
        <ActionLink
          to="/system"
          icon={<Activity />}
          label="System report"
          hint="Toolchain, GPUs, disk and the journal"
        />
      </ul>
    </Panel>
  );
}

function ActionLink({
  to,
  icon,
  label,
  hint,
}: {
  to: '/instances/new' | '/models/browse' | '/models' | '/downloads' | '/bench/new' | '/system';
  icon: ReactNode;
  label: string;
  hint: string;
}) {
  return (
    <li>
      <Link
        to={to}
        className="flex items-center gap-2.5 rounded-[var(--lm-radius)] px-2 py-1.5 hover:bg-[var(--lm-neutral-soft)]"
      >
        <span aria-hidden className="shrink-0 text-[var(--lm-text-faint)] [&>svg]:size-4">
          {icon}
        </span>
        <span className="min-w-0">
          <span className="block truncate text-sm text-[var(--lm-text)]">{label}</span>
          <span className="block truncate text-xs text-[var(--lm-text-faint)]">{hint}</span>
        </span>
      </Link>
    </li>
  );
}
