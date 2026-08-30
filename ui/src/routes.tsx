/**
 * The route tree.
 *
 * Code-based routes, not file-based: DESIGN section 4 chose TanStack Router for "type-safe params
 * *and* search params — filters, sort and comparison selections live in the URL", and declaring the
 * tree in one file is what makes that contract readable in one place. Every search schema lives in
 * `searchParams.ts`; every leaf is a component in its own file under `screens/<area>/`, so the
 * areas can be built in parallel without two agents editing the same file.
 *
 * Three shells, and the difference between them is which of them has a session:
 *
 *   /login    — no session, no stream.
 *   /setup/*  — the wizard: no session until its first step creates one, so no stream either.
 *   (app)     — a pathless layout route: AuthGate, then the SSE provider, then the shell.
 *
 * Seventeen screens of section 4, mapped:
 *   1 /login · 2 /setup/* · 3 / · 4 /instances · 5 /instances/new, /instances/$id/edit ·
 *   6 /instances/$id · 7 /models · 8 /models/browse · 9 /models/browse/$repo · 10 /models/$id ·
 *   11 /downloads · 12 /llamacpp · 13 /bench, /bench/new, /bench/$id, /bench/compare ·
 *   14 /tokens · 15 /settings · 16 /system · 17 /events
 */

import { createRootRoute, createRoute, createRouter } from '@tanstack/react-router';

import { AppLayout } from './layout/AppLayout';
import { NotFound } from './layout/NotFound';
import { RootLayout } from './layout/RootLayout';
import { RouteError } from './layout/RouteError';
import { SetupIndex } from './setup/SetupIndex';
import { WizardShell } from './setup/WizardShell';

import { LoginScreen } from './screens/auth/LoginScreen';
import { DashboardScreen } from './screens/dashboard/DashboardScreen';
import { EventsScreen } from './screens/events/EventsScreen';
import { DownloadsScreen } from './screens/downloads/DownloadsScreen';
import { LlamacppScreen } from './screens/llamacpp/LlamacppScreen';
import { SettingsScreen } from './screens/settings/SettingsScreen';
import { SystemScreen } from './screens/system/SystemScreen';
import { TokensScreen } from './screens/tokens/TokensScreen';

import { InstanceDetailScreen } from './screens/instances/InstanceDetailScreen';
import { InstanceEditScreen } from './screens/instances/InstanceEditScreen';
import { InstanceNewScreen } from './screens/instances/InstanceNewScreen';
import { InstancesScreen } from './screens/instances/InstancesScreen';

import { BrowseRepoScreen } from './screens/models/BrowseRepoScreen';
import { BrowseScreen } from './screens/models/BrowseScreen';
import { ModelDetailScreen } from './screens/models/ModelDetailScreen';
import { ModelsScreen } from './screens/models/ModelsScreen';

import { BenchCompareScreen } from './screens/bench/BenchCompareScreen';
import { BenchNewScreen } from './screens/bench/BenchNewScreen';
import { BenchRunScreen } from './screens/bench/BenchRunScreen';
import { BenchScreen } from './screens/bench/BenchScreen';

import { HuggingFaceStep } from './screens/setup/HuggingFaceStep';
import { InstanceStep } from './screens/setup/InstanceStep';
import { LlamacppStep } from './screens/setup/LlamacppStep';
import { ModelsStep } from './screens/setup/ModelsStep';
import { PasswordStep } from './screens/setup/PasswordStep';
import { ToolchainStep } from './screens/setup/ToolchainStep';
import { DoneStep } from './screens/setup/DoneStep';

import {
  benchCompareSearchSchema,
  benchRunSearchSchema,
  benchSearchSchema,
  browseSearchSchema,
  downloadsSearchSchema,
  eventsSearchSchema,
  instanceDetailSearchSchema,
  instanceFormSearchSchema,
  instancesSearchSchema,
  llamacppSearchSchema,
  loginSearchSchema,
  modelsSearchSchema,
  repoSearchSchema,
  settingsSearchSchema,
  systemSearchSchema,
  tokensSearchSchema,
} from './searchParams';

type Search = Record<string, unknown>;

/* -- root ------------------------------------------------------------------ */

const rootRoute = createRootRoute({
  component: RootLayout,
  notFoundComponent: NotFound,
});

/* -- /login ---------------------------------------------------------------- */

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/login',
  component: LoginScreen,
  validateSearch: (search: Search) => loginSearchSchema.parse(search),
});

/* -- /setup/* -------------------------------------------------------------- */

const setupRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/setup',
  component: WizardShell,
});

const setupIndexRoute = createRoute({
  getParentRoute: () => setupRoute,
  path: '/',
  component: SetupIndex,
});

const setupStepRoutes = [
  createRoute({ getParentRoute: () => setupRoute, path: 'password', component: PasswordStep }),
  createRoute({ getParentRoute: () => setupRoute, path: 'toolchain', component: ToolchainStep }),
  createRoute({ getParentRoute: () => setupRoute, path: 'llamacpp', component: LlamacppStep }),
  createRoute({ getParentRoute: () => setupRoute, path: 'hf', component: HuggingFaceStep }),
  createRoute({ getParentRoute: () => setupRoute, path: 'models', component: ModelsStep }),
  createRoute({ getParentRoute: () => setupRoute, path: 'instance', component: InstanceStep }),
  createRoute({ getParentRoute: () => setupRoute, path: 'done', component: DoneStep }),
];

/* -- the application ------------------------------------------------------- */

/** Pathless: it contributes the shell and the session gate, not a URL segment. */
const appRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: 'app',
  component: AppLayout,
});

const dashboardRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/',
  component: DashboardScreen,
});

const instancesRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/instances',
  component: InstancesScreen,
  validateSearch: (search: Search) => instancesSearchSchema.parse(search),
});

const instanceNewRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/instances/new',
  component: InstanceNewScreen,
  validateSearch: (search: Search) => instanceFormSearchSchema.parse(search),
});

const instanceDetailRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/instances/$id',
  component: InstanceDetailScreen,
  validateSearch: (search: Search) => instanceDetailSearchSchema.parse(search),
});

const instanceEditRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/instances/$id/edit',
  component: InstanceEditScreen,
  validateSearch: (search: Search) => instanceFormSearchSchema.parse(search),
});

const modelsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/models',
  component: ModelsScreen,
  validateSearch: (search: Search) => modelsSearchSchema.parse(search),
});

/**
 * `/models/browse` is a layout with two children rather than a leaf, because a repo id contains a
 * slash (`bartowski/Qwen3-8B-GGUF`) and has to be a splat. Section 3.6 solves the same problem
 * server-side by putting the verb in front of the `{repo...}` wildcard; this is its client half,
 * and it keeps the id readable in the URL instead of percent-encoding the separator.
 */
const modelsBrowseRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/models/browse',
});

const modelsBrowseIndexRoute = createRoute({
  getParentRoute: () => modelsBrowseRoute,
  path: '/',
  component: BrowseScreen,
  validateSearch: (search: Search) => browseSearchSchema.parse(search),
});

const modelsBrowseRepoRoute = createRoute({
  getParentRoute: () => modelsBrowseRoute,
  path: '$',
  component: BrowseRepoScreen,
  validateSearch: (search: Search) => repoSearchSchema.parse(search),
});

const modelDetailRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/models/$id',
  component: ModelDetailScreen,
});

const downloadsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/downloads',
  component: DownloadsScreen,
  validateSearch: (search: Search) => downloadsSearchSchema.parse(search),
});

const llamacppRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/llamacpp',
  component: LlamacppScreen,
  validateSearch: (search: Search) => llamacppSearchSchema.parse(search),
});

const benchRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/bench',
  component: BenchScreen,
  validateSearch: (search: Search) => benchSearchSchema.parse(search),
});

const benchNewRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/bench/new',
  component: BenchNewScreen,
});

const benchCompareRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/bench/compare',
  component: BenchCompareScreen,
  validateSearch: (search: Search) => benchCompareSearchSchema.parse(search),
});

const benchRunRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/bench/$id',
  component: BenchRunScreen,
  validateSearch: (search: Search) => benchRunSearchSchema.parse(search),
});

const tokensRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/tokens',
  component: TokensScreen,
  validateSearch: (search: Search) => tokensSearchSchema.parse(search),
});

const settingsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/settings',
  component: SettingsScreen,
  validateSearch: (search: Search) => settingsSearchSchema.parse(search),
});

const systemRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/system',
  component: SystemScreen,
  validateSearch: (search: Search) => systemSearchSchema.parse(search),
});

const eventsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/events',
  component: EventsScreen,
  validateSearch: (search: Search) => eventsSearchSchema.parse(search),
});

/* -- assembly -------------------------------------------------------------- */

export const routeTree = rootRoute.addChildren([
  loginRoute,
  setupRoute.addChildren([setupIndexRoute, ...setupStepRoutes]),
  appRoute.addChildren([
    dashboardRoute,
    instancesRoute,
    instanceNewRoute,
    instanceDetailRoute,
    instanceEditRoute,
    modelsRoute,
    modelsBrowseRoute.addChildren([modelsBrowseIndexRoute, modelsBrowseRepoRoute]),
    modelDetailRoute,
    downloadsRoute,
    llamacppRoute,
    benchRoute,
    benchNewRoute,
    benchCompareRoute,
    benchRunRoute,
    tokensRoute,
    settingsRoute,
    systemRoute,
    eventsRoute,
  ]),
]);

export function createAppRouter() {
  return createRouter({
    routeTree,
    defaultPreload: 'intent',
    // No route-level loaders (DESIGN section 4): every screen reads its data through TanStack Query,
    // which is the cache the SSE frames patch. A loader would be a second, unpatched copy.
    defaultPendingMs: 150,
    // The boundary section 4's screens do not have on their own. A query failure is handled by the
    // screen that made it (`QueryError`); this is for a render that threw, which would otherwise
    // leave a blank region inside a working shell.
    defaultErrorComponent: RouteError,
  });
}

export const router = createAppRouter();

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router;
  }
}
