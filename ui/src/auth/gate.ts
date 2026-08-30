/**
 * The shell's first decision, as a pure function.
 *
 * `AuthGate` renders it; this module decides it. Splitting them is not ceremony — the bug this
 * function exists to make untestable-by-construction was a redirect loop that crashed the renderer
 * on every gated route, and it was in the guard rather than in the rendering: the effect compared
 * `location.href` against `'/login'`, TanStack updates `location` as soon as a navigation commits
 * (before the new route renders, and `defaultPendingMs: 150` keeps the gate mounted through that
 * window), so the second pass saw `/login?redirect=%2F`, no longer matched, and redirected again
 * with the previous URL nested one level deeper. The URL doubled in length per pass until the tab
 * was OOM-killed.
 *
 * A decision that takes the path and the three session facts and returns one of four answers cannot
 * have that shape, and the tests beside this file pin it.
 */

export interface GateFacts {
  /** `location.pathname` — the PATH, never the href. A search string here is what looped. */
  pathname: string;
  /** `admin_account` exists (DESIGN section 3.1). */
  claimed: boolean;
  /** The wizard reached its `done` step. A different question from `claimed`. */
  setupComplete: boolean;
  /** This request carries a live session. */
  authenticated: boolean;
}

export type GateDecision =
  | { kind: 'render' }
  /** Go to the wizard — either unclaimed, or claimed with the wizard unfinished (section 11.2). */
  | { kind: 'setup' }
  /** Go to the login screen, carrying where the user was headed. */
  | { kind: 'login' };

/** The two shells that are redirect DESTINATIONS. Reaching the gate from one means a navigation is
 * already in flight, and a second one is exactly the loop this module exists to prevent. */
function isDestination(pathname: string): boolean {
  return pathname === '/login' || pathname === '/setup' || pathname.startsWith('/setup/');
}

/**
 * What the shell should do, given where the browser is and what the session says.
 *
 * The order is the contract:
 *
 *  1. An unclaimed host goes to the wizard. Nothing else is knowable yet.
 *  2. A claimed host with no session goes to the login screen. It goes there BEFORE the wizard
 *     check, because every wizard step after the password one is a session endpoint.
 *  3. A claimed, signed-in browser whose wizard has not finished goes back to the wizard. Section
 *     11.2 requires the wizard be resumable "from every entry point", and the bookmark a returning
 *     user has is the host root, not a `/setup` URL.
 *  4. Otherwise, render.
 */
export function gateDecision(facts: GateFacts): GateDecision {
  if (isDestination(facts.pathname)) return { kind: 'render' };
  if (!facts.claimed) return { kind: 'setup' };
  if (!facts.authenticated) return { kind: 'login' };
  if (!facts.setupComplete) return { kind: 'setup' };
  return { kind: 'render' };
}
