import { useState } from 'react';
import { Link, Outlet, useRouterState } from '@tanstack/react-router';
import { LogOut, Menu, ShieldCheck, X } from 'lucide-react';
import {
  Button,
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '../components';
import { cn } from '../components/cn';
import { useLogout } from '../auth/session';
import { DegradedBanner } from './DegradedBanner';
import { LiveChip } from './LiveChip';
import { NAV_GROUPS } from './navigation';
import { ThemeToggle } from './ThemeToggle';

/**
 * The application shell.
 *
 * A fixed rail of the five nav groups, a thin header carrying the three things that are true no
 * matter which screen is open — the stream's health, the theme, the session — and the routed
 * screen underneath. Screens own their own titles and toolbars; the shell owns nothing inside the
 * content area, which is what keeps seventeen independently built screens consistent.
 */
export function AppShell() {
  const [mobileOpen, setMobileOpen] = useState(false);
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const logout = useLogout();

  const isActive = (to: string, fuzzy?: boolean) =>
    to === '/' ? pathname === '/' : fuzzy ? pathname.startsWith(to) : pathname === to;

  const nav = (
    <nav aria-label="Main" className="flex flex-col gap-5 p-3">
      {NAV_GROUPS.map((group) => (
        <div key={group.id}>
          <p className="px-2 pb-1 text-[11px] font-medium tracking-wide text-[var(--lm-text-faint)] uppercase">
            {group.label}
          </p>
          <ul className="space-y-0.5">
            {group.items.map((item) => {
              const active = isActive(item.to, item.fuzzy);
              const Icon = item.icon;
              return (
                <li key={item.to}>
                  <Link
                    to={item.to}
                    onClick={() => setMobileOpen(false)}
                    aria-current={active ? 'page' : undefined}
                    className={cn(
                      'flex items-center gap-2.5 rounded-[var(--lm-radius)] px-2 py-1.5 text-sm',
                      'transition-colors duration-[var(--lm-duration-fast)]',
                      active
                        ? 'bg-[var(--lm-accent-soft)] font-medium text-[var(--lm-accent)]'
                        : 'text-[var(--lm-text-muted)] hover:bg-[var(--lm-neutral-soft)] hover:text-[var(--lm-text)]',
                    )}
                  >
                    <Icon aria-hidden className="size-4 shrink-0" />
                    <span className="truncate">{item.label}</span>
                  </Link>
                </li>
              );
            })}
          </ul>
        </div>
      ))}
    </nav>
  );

  return (
    <div className="flex h-full bg-[var(--lm-bg)]">
      {/* Rail */}
      <aside
        className={cn(
          'w-56 shrink-0 flex-col border-r border-[var(--lm-border)] bg-[var(--lm-surface)]',
          'hidden lg:flex',
        )}
      >
        <Brand />
        <div className="min-h-0 flex-1 overflow-y-auto">{nav}</div>
        <p className="border-t border-[var(--lm-border)] px-4 py-2 text-[11px] text-[var(--lm-text-faint)]">
          Management only — no chat, no inference UI.
        </p>
      </aside>

      {/* Rail, small screens */}
      {mobileOpen ? (
        <div className="fixed inset-0 z-40 flex lg:hidden">
          <div
            className="absolute inset-0 bg-[var(--lm-overlay)]"
            onClick={() => setMobileOpen(false)}
          />
          <aside className="relative flex w-64 flex-col border-r border-[var(--lm-border)] bg-[var(--lm-surface)]">
            <div className="flex items-center justify-between">
              <Brand />
              <Button
                variant="ghost"
                size="icon"
                aria-label="Close navigation"
                className="mr-2"
                onClick={() => setMobileOpen(false)}
              >
                <X aria-hidden className="size-4" />
              </Button>
            </div>
            <div className="min-h-0 flex-1 overflow-y-auto">{nav}</div>
          </aside>
        </div>
      ) : null}

      {/* Content */}
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-12 shrink-0 items-center gap-2 border-b border-[var(--lm-border)] bg-[var(--lm-surface)] px-3">
          <Button
            variant="ghost"
            size="icon"
            aria-label="Open navigation"
            className="lg:hidden"
            onClick={() => setMobileOpen(true)}
          >
            <Menu aria-hidden className="size-4" />
          </Button>

          <div className="flex-1" />

          <LiveChip />
          <ThemeToggle />

          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon" aria-label="Account">
                <ShieldCheck aria-hidden className="size-4" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent>
              <DropdownMenuLabel>Admin session</DropdownMenuLabel>
              <DropdownMenuItem asChild>
                <Link to="/settings" search={{ group: 'security' }}>
                  Security settings
                </Link>
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem danger onSelect={() => logout.mutate()}>
                <LogOut aria-hidden />
                Sign out
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </header>

        {/* Section 11.1a's degraded modes, on every screen rather than only on /system. A host
            that cannot start an instance must not render a Dashboard that implies it can. */}
        <DegradedBanner />

        <main className="min-h-0 flex-1 overflow-y-auto">
          <div className="mx-auto w-full max-w-7xl p-4 lg:p-6">
            <Outlet />
          </div>
        </main>
      </div>
    </div>
  );
}

function Brand() {
  return (
    <Link to="/" className="flex items-center gap-2 px-4 py-3">
      <span
        aria-hidden
        className="flex size-6 items-center justify-center rounded-[var(--lm-radius)] bg-[var(--lm-accent)] text-[11px] font-bold text-[var(--lm-accent-contrast)]"
      >
        LM
      </span>
      <span className="text-sm font-semibold tracking-tight text-[var(--lm-text)]">Llama Man</span>
    </Link>
  );
}
