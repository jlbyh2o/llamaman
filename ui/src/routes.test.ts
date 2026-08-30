/**
 * The route tree is the contract with DESIGN section 4's screen list.
 *
 * Seventeen screens are named there; every one of them has to have a URL, and every URL has to have
 * a component. A missing route is a screen nobody can reach, and an extra one is a screen nobody
 * designed — so both directions are asserted, and the list below is transcribed from section 4
 * rather than derived from the code it checks.
 */

import { describe, expect, it } from 'vitest';
import { routeTree } from './routes';
import { NAV_GROUPS } from './layout/navigation';
import { WIZARD_STEP_METAS } from './setup/steps';

interface AnyRoute {
  fullPath?: string;
  id?: string;
  options?: { component?: unknown };
  children?: AnyRoute[];
}

function walk(route: AnyRoute, out: AnyRoute[] = []): AnyRoute[] {
  out.push(route);
  for (const child of route.children ?? []) walk(child, out);
  return out;
}

const routes = walk(routeTree as unknown as AnyRoute);

/** Leaf paths, normalized: no trailing slash except the root. */
const paths = new Set(
  routes
    .filter((route) => route.options?.component !== undefined)
    .map((route) => route.fullPath ?? '')
    .map((path) => (path.length > 1 ? path.replace(/\/$/, '') : path))
    .filter((path) => path !== ''),
);

const EXPECTED = [
  '/login',
  '/setup',
  '/setup/password',
  '/setup/toolchain',
  '/setup/llamacpp',
  '/setup/hf',
  '/setup/models',
  '/setup/instance',
  '/setup/done',
  '/',
  '/instances',
  '/instances/new',
  '/instances/$id',
  '/instances/$id/edit',
  '/models',
  '/models/browse',
  '/models/browse/$',
  '/models/$id',
  '/downloads',
  '/llamacpp',
  '/bench',
  '/bench/new',
  '/bench/$id',
  '/bench/compare',
  '/tokens',
  '/settings',
  '/system',
  '/events',
];

describe('route tree', () => {
  it('declares every screen DESIGN section 4 names', () => {
    for (const path of EXPECTED) {
      expect(paths.has(path), `${path} has no route`).toBe(true);
    }
  });

  it('declares nothing section 4 does not', () => {
    expect([...paths].sort()).toEqual([...EXPECTED].sort());
  });

  it('has a route for every wizard step row of section 2.11', () => {
    for (const step of WIZARD_STEP_METAS) {
      expect(paths.has(step.path), `${step.id} has no route`).toBe(true);
    }
  });

  it('points every navigation entry at a real route', () => {
    for (const group of NAV_GROUPS) {
      for (const item of group.items) {
        expect(paths.has(item.to), `nav entry ${item.to} points nowhere`).toBe(true);
      }
    }
  });

  it('reaches the repo screen through a splat, because a repo id contains a slash', () => {
    // Section 3.6's server-side constraint has a client-side twin: `bartowski/Qwen3-8B-GGUF` cannot
    // be a single path segment, so `/models/browse/$` carries it unescaped and readable.
    expect(paths.has('/models/browse/$')).toBe(true);
  });
});
