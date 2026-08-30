import { useEffect, useRef } from 'react';
import { useNavigate, useRouterState } from '@tanstack/react-router';
import { PlugZap } from 'lucide-react';
import type { ReactNode } from 'react';
import { Button, EmptyState, LoadingPanel } from '../components';
import { gateDecision } from './gate';
import { useSession } from './session';

/**
 * The first decision the app makes.
 *
 * One query answers all four branches (DESIGN section 3.1): an unclaimed host goes to the wizard, a
 * claimed host whose wizard has not finished goes back to the wizard *after* it has a session, a
 * claimed one without a session goes to the login screen carrying where it was headed, and
 * everything else renders. A daemon that cannot be reached at all is its own state — never a login
 * prompt, which would be a lie about what is wrong.
 *
 * ## Two things here are load-bearing and neither is obvious
 *
 * **The guard reads `location.pathname`, never `location.href`.** TanStack updates `location` as
 * soon as a navigation is *committed*, before the new route has rendered — and `routes.tsx` sets
 * `defaultPendingMs: 150`, so this component stays mounted across that window. An effect that
 * compared `href` against `'/login'` therefore re-fired with `href` already equal to
 * `/login?redirect=%2F`, no longer matched its own guard, and redirected again with the previous URL
 * nested one level deeper into `redirect`. The URL doubled in length on every pass until the
 * renderer was OOM-killed — every gated route, in under a hundred milliseconds. `pathname` carries
 * no search string, so it matches on the second pass exactly as it did on the first.
 *
 * **`redirected` latches.** The pathname guard alone fixes the observed crash; the ref makes the
 * whole class unreachable. This effect's job is to fire one navigation per answer from the session
 * query, and a latch says that in the code rather than relying on every future edit to keep the
 * guard and the destination in agreement. It is cleared when the session answer itself changes, so a
 * logout or a completed wizard still redirects.
 */
export function AuthGate({ children }: { children: ReactNode }) {
  const navigate = useNavigate();
  const session = useSession();

  // The PATH only. See this component's docstring: comparing a full href against a bare path is
  // what turned this effect into an infinite redirect loop.
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const href = useRouterState({ select: (state) => state.location.href });

  const claimed = session.data?.claimed;
  const setupComplete = session.data?.setup_complete;
  const authenticated = session.data?.authenticated;

  // The answer this component last acted on. A new answer clears the latch.
  const decision = `${String(claimed)}|${String(setupComplete)}|${String(authenticated)}`;
  const redirected = useRef('');

  useEffect(() => {
    if (session.isPending || session.isError) return;
    if (redirected.current === decision) return;

    const next = gateDecision({
      pathname,
      claimed: claimed === true,
      setupComplete: setupComplete === true,
      authenticated: authenticated === true,
    });
    if (next.kind === 'render') return;

    redirected.current = decision;
    if (next.kind === 'setup') {
      void navigate({ to: '/setup', replace: true });
    } else {
      void navigate({ to: '/login', search: { redirect: href }, replace: true });
    }
  }, [
    session.isPending,
    session.isError,
    claimed,
    setupComplete,
    authenticated,
    decision,
    navigate,
    pathname,
    href,
  ]);

  if (session.isPending) return <LoadingPanel>Contacting the daemon…</LoadingPanel>;

  if (session.isError) {
    return (
      <div className="p-6">
        <EmptyState
          tone="error"
          icon={<PlugZap />}
          title="The daemon is not answering"
          description="The management API on this host did not respond. If llamaman.service was just restarted, this clears on its own; otherwise check `systemctl status llamaman.service` and the journal."
          action={
            <Button variant="primary" onClick={() => void session.refetch()}>
              Try again
            </Button>
          }
        />
      </div>
    );
  }

  if (!claimed || !authenticated || !setupComplete) {
    return <LoadingPanel>Redirecting…</LoadingPanel>;
  }

  return <>{children}</>;
}
