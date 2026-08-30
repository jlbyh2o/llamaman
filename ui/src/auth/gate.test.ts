import { describe, expect, it } from 'vitest';
import { gateDecision } from './gate';

/**
 * The regression these tests exist for.
 *
 * Opening any gated route on a claimed host with no session crashed the renderer in under a hundred
 * milliseconds: the guard compared `location.href` against `'/login'`, and TanStack updates
 * `location` as soon as a navigation commits — so the effect re-fired with `href` already equal to
 * `/login?redirect=%2F`, no longer matched its own guard, and redirected again with the previous URL
 * nested one level deeper into `redirect`. The URL doubled in length on every pass.
 *
 * The first block below is that loop, stated as an invariant: whatever `/login` looks like — bare,
 * with a redirect, with a redirect that already nests one — the gate must decide to render, never to
 * navigate again.
 */

const CLAIMED_NO_SESSION = { claimed: true, setupComplete: true, authenticated: false };

describe('the gate never redirects away from a redirect destination', () => {
  const destinations = ['/login', '/setup', '/setup/password', '/setup/llamacpp', '/setup/done'];

  for (const pathname of destinations) {
    it(`renders at ${pathname} rather than navigating again`, () => {
      expect(gateDecision({ pathname, ...CLAIMED_NO_SESSION })).toEqual({ kind: 'render' });
      expect(
        gateDecision({ pathname, claimed: false, setupComplete: false, authenticated: false }),
      ).toEqual({ kind: 'render' });
    });
  }

  it('reads a path, so a search string can never make the guard stop matching', () => {
    // `pathname` never carries `?redirect=…`; this asserts the shape of the input the gate takes,
    // which is the whole fix. A caller passing an href would be the bug returning.
    expect(gateDecision({ pathname: '/login', ...CLAIMED_NO_SESSION }).kind).toBe('render');
  });
});

describe('the four branches of DESIGN section 3.1', () => {
  const gated = [
    '/',
    '/instances',
    '/models',
    '/llamacpp',
    '/bench',
    '/tokens',
    '/settings',
    '/system',
  ];

  it('sends an unclaimed host to the wizard', () => {
    for (const pathname of gated) {
      expect(
        gateDecision({ pathname, claimed: false, setupComplete: false, authenticated: false }),
      ).toEqual({ kind: 'setup' });
    }
  });

  it('sends a claimed host with no session to the login screen', () => {
    for (const pathname of gated) {
      expect(gateDecision({ pathname, ...CLAIMED_NO_SESSION })).toEqual({ kind: 'login' });
    }
  });

  /**
   * Section 11.2's resumability. `claimed` and `setup_complete` are different questions, and this is
   * the state that separates them: the password step has run, so an admin account exists, but the
   * wizard has not reached `done`. Answering "render" here is what silently abandoned an unfinished
   * wizard from every entry point except a `/setup` URL.
   */
  it('sends a signed-in browser back to an unfinished wizard', () => {
    expect(
      gateDecision({ pathname: '/', claimed: true, setupComplete: false, authenticated: true }),
    ).toEqual({ kind: 'setup' });
  });

  it('logs in before resuming the wizard, because every step after the password needs a session', () => {
    expect(
      gateDecision({ pathname: '/', claimed: true, setupComplete: false, authenticated: false }),
    ).toEqual({ kind: 'login' });
  });

  it('renders for a signed-in browser on a finished host', () => {
    for (const pathname of gated) {
      expect(
        gateDecision({ pathname, claimed: true, setupComplete: true, authenticated: true }),
      ).toEqual({ kind: 'render' });
    }
  });
});
