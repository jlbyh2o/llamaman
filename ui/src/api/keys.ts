/**
 * One query-key vocabulary for the whole app.
 *
 * Two rules make the SSE layer possible (DESIGN section 4: frames *patch* the cache instead of
 * triggering a refetch):
 *
 *  1. Every key starts with a **family** — `'instances'`, `'downloads'`, … — that matches an SSE
 *     topic, so the patcher can find every cached query a frame could touch with one prefix match.
 *  2. Within a family, `['…', 'list', filters?]` and `['…', 'detail', id]` are the only two shapes.
 *     A patch for entity `id` updates the detail query and rewrites that id inside every cached
 *     list, whatever filters produced it.
 *
 * Screens add filter objects as the third element; they never invent a new shape.
 */

import type { QueryKey } from '@tanstack/react-query';

/** The families, one per SSE topic plus the ones nothing streams. */
export const FAMILIES = [
  'meta',
  'auth',
  'setup',
  'system',
  'settings',
  'instances',
  'presets',
  'models',
  'cache',
  'hf',
  'downloads',
  'llamacpp',
  'fit',
  'tokens',
  'gateway',
  'bench',
  'jobs',
  'events',
  'notifications',
  'update',
] as const;
export type Family = (typeof FAMILIES)[number];

type Filters = Record<string, unknown> | undefined;

const list = (family: Family, filters?: Filters): QueryKey =>
  filters === undefined ? [family, 'list'] : [family, 'list', filters];
const detail = (family: Family, id: string): QueryKey => [family, 'detail', id];

export const queryKeys = {
  /** The prefix that matches every query in a family — what `invalidateQueries` takes. */
  family: (family: Family): QueryKey => [family],
  list,
  detail,

  meta: (): QueryKey => ['meta', 'detail', 'self'],

  auth: {
    session: (): QueryKey => ['auth', 'detail', 'session'],
    sessions: (): QueryKey => list('auth', { kind: 'sessions' }),
  },

  setup: {
    state: (): QueryKey => ['setup', 'detail', 'state'],
    toolchain: (): QueryKey => ['setup', 'detail', 'toolchain'],
  },

  system: {
    info: (): QueryKey => ['system', 'detail', 'info'],
    capabilities: (): QueryKey => ['system', 'detail', 'capabilities'],
    toolchain: (): QueryKey => ['system', 'detail', 'toolchain'],
    gpus: (): QueryKey => ['system', 'detail', 'gpus'],
    disk: (): QueryKey => ['system', 'detail', 'disk'],
    units: (): QueryKey => ['system', 'detail', 'units'],
  },

  settings: {
    all: (): QueryKey => ['settings', 'detail', 'all'],
  },

  instances: {
    list: (filters?: Filters): QueryKey => list('instances', filters),
    detail: (id: string): QueryKey => detail('instances', id),
    status: (id: string): QueryKey => ['instances', 'detail', id, 'status'],
    starts: (id: string): QueryKey => ['instances', 'detail', id, 'starts'],
    command: (id: string): QueryKey => ['instances', 'detail', id, 'command'],
    usage: (id: string, range?: Filters): QueryKey => ['instances', 'detail', id, 'usage', range],
  },

  models: {
    list: (filters?: Filters): QueryKey => list('models', filters),
    detail: (id: string): QueryKey => detail('models', id),
    metadata: (id: string): QueryKey => ['models', 'detail', id, 'metadata'],
    deletePreview: (id: string): QueryKey => ['models', 'detail', id, 'delete-preview'],
  },

  cache: {
    roots: (): QueryKey => list('cache', { kind: 'roots' }),
    strays: (): QueryKey => list('cache', { kind: 'strays' }),
    scan: (id: string): QueryKey => detail('cache', id),
  },

  hf: {
    search: (filters?: Filters): QueryKey => list('hf', filters),
    model: (repo: string): QueryKey => detail('hf', repo),
    tree: (repo: string): QueryKey => ['hf', 'detail', repo, 'tree'],
    card: (repo: string): QueryKey => ['hf', 'detail', repo, 'card'],
    token: (): QueryKey => ['hf', 'detail', 'token'],
    githubToken: (): QueryKey => ['hf', 'detail', 'github-token'],
  },

  downloads: {
    list: (filters?: Filters): QueryKey => list('downloads', filters),
    detail: (id: string): QueryKey => detail('downloads', id),
  },

  llamacpp: {
    active: (): QueryKey => ['llamacpp', 'detail', 'active'],
    versions: (filters?: Filters): QueryKey => list('llamacpp', filters),
    detail: (id: string): QueryKey => detail('llamacpp', id),
    log: (id: string): QueryKey => ['llamacpp', 'detail', id, 'log'],
    releases: (channel: string): QueryKey => list('llamacpp', { kind: 'releases', channel }),
    plan: (filters: Filters): QueryKey => list('llamacpp', { kind: 'plan', ...filters }),
  },

  tokens: {
    list: (filters?: Filters): QueryKey => list('tokens', filters),
    detail: (id: string): QueryKey => detail('tokens', id),
    usage: (id: string, range?: Filters): QueryKey => ['tokens', 'detail', id, 'usage', range],
  },

  gateway: {
    denials: (): QueryKey => list('gateway', { kind: 'denials' }),
  },

  bench: {
    runs: (filters?: Filters): QueryKey => list('bench', filters),
    detail: (id: string): QueryKey => detail('bench', id),
    results: (id: string): QueryKey => ['bench', 'detail', id, 'results'],
    preflight: (filters: Filters): QueryKey => list('bench', { kind: 'preflight', ...filters }),
    series: (filters: Filters): QueryKey => list('bench', { kind: 'series', ...filters }),
  },

  jobs: {
    list: (filters?: Filters): QueryKey => list('jobs', filters),
    detail: (id: string): QueryKey => detail('jobs', id),
  },

  events: {
    log: (filters?: Filters): QueryKey => list('events', filters),
  },

  notifications: {
    list: (): QueryKey => list('notifications'),
  },

  update: {
    status: (): QueryKey => ['update', 'detail', 'status'],
    releases: (): QueryKey => list('update', { kind: 'releases' }),
  },
} as const;
