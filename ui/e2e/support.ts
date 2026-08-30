import { expect, test as base } from '@playwright/test';
import type { ConsoleMessage, Page, TestInfo } from '@playwright/test';

/**
 * Shared support for the live end-to-end suite.
 *
 * Two things every spec here needs and neither of which Playwright gives for free:
 *
 *  - **A console/page-error budget of zero.** A screen that renders but logs a React key warning, a
 *    failed fetch or an unhandled rejection is not a screen that works, so the fixture collects
 *    them and the assertion helpers fail on any.
 *  - **Screenshots outside the repository.** `LLAMAMAN_E2E_SHOTS` names the directory; it defaults
 *    to a sibling of the checkout so a run never leaves images in git's way.
 */

/**
 * Where screenshots land. The default is a sibling of the checkout, resolved from `ui/` — outside
 * the repository on purpose, so a run never leaves images in git's way.
 */
export const SHOTS_DIR = process.env.LLAMAMAN_E2E_SHOTS ?? '../../llamaman-e2e-shots';

/** The password the wizard claims the host with, and the one the session specs sign in with. */
export const ADMIN_PASSWORD = process.env.LLAMAMAN_E2E_PASSWORD ?? 'correct-horse-battery-staple';

/** Where `01-wizard.spec.ts` leaves the session for the specs that follow. */
export const STORAGE_STATE = 'e2e/.auth/session.json';

/** The path a screenshot goes to. Playwright creates the directory. */
export function shot(name: string): string {
  return `${SHOTS_DIR}/${name}.png`;
}

/**
 * Console lines that are noise from the harness rather than from the app.
 *
 * Kept deliberately short: anything added here is a defect that stops being reported.
 */
const IGNORED = [
  // Chromium logs this for any favicon/manifest the page does not ship; it is not app behavior.
  /Failed to load resource: the server responded with a status of 404 \(Not Found\).*favicon/i,
];

export interface Diagnostics {
  /** console.error / console.warn lines, in order. */
  console: string[];
  /** Uncaught exceptions and unhandled rejections. */
  pageErrors: string[];
  /** Requests that never produced a response (DNS, connection reset, aborted). */
  failedRequests: string[];
  /** Responses with a 5xx status — a 4xx can be a documented, handled refusal. */
  serverErrors: string[];
  /**
   * The 4xx statuses this page actually received, which is what makes the console filter below
   * possible: Chromium's own "Failed to load resource" line carries the status but not the URL,
   * so it cannot be judged on its own.
   */
  clientErrorStatuses: number[];
}

/**
 * Chromium's generic resource-load complaint, which it logs for every non-2xx response the page
 * fetches. The status is the only thing it tells us — there is no URL in the text.
 */
const RESOURCE_STATUS = /Failed to load resource: the server responded with a status of (\d{3})/;

export function watch(page: Page): Diagnostics {
  const d: Diagnostics = {
    console: [],
    pageErrors: [],
    failedRequests: [],
    serverErrors: [],
    clientErrorStatuses: [],
  };

  page.on('console', (msg: ConsoleMessage) => {
    if (msg.type() !== 'error' && msg.type() !== 'warning') return;
    const text = `${msg.type()}: ${msg.text()}`;
    if (IGNORED.some((re) => re.test(text))) return;
    d.console.push(text);
  });
  page.on('pageerror', (err) => d.pageErrors.push(String(err)));
  page.on('requestfailed', (req) => {
    const failure = req.failure()?.errorText ?? 'unknown';
    // Navigations the test itself aborted are not defects.
    if (failure === 'net::ERR_ABORTED') return;
    d.failedRequests.push(`${req.method()} ${req.url()} — ${failure}`);
  });
  page.on('response', (res) => {
    const status = res.status();
    if (status >= 500) d.serverErrors.push(`${status} ${res.request().method()} ${res.url()}`);
    else if (status >= 400 && !d.clientErrorStatuses.includes(status))
      d.clientErrorStatuses.push(status);
  });

  return d;
}

/**
 * Fails the test if anything was collected, naming everything at once.
 *
 * One class of console line is dropped here rather than in `watch`, because judging it needs the
 * responses this page received: Chromium logs "Failed to load resource: the server responded with
 * a status of 404 (Not Found)" for a 4xx the app asked for and handled, and that text names no URL
 * to tell it apart from a real one. The response listener above has already applied this harness's
 * rule — 5xx is a defect, "a 4xx can be a documented, handled refusal" — so a resource line whose
 * status matches a 4xx this page actually received is the same event reported twice, and failing
 * on it would contradict the rule two lines up. A 4xx that IS a defect still fails the test the way
 * every other one does: through the assertion the spec makes about what the screen shows.
 *
 * `GET /api/v1/llamacpp/active` on a fresh host is the case that forced this: internal/api
 * documents its 404 as "the ordinary state on a fresh install, not an error condition".
 */
export function expectClean(d: Diagnostics, where: string) {
  const handled = (line: string) => {
    const m = RESOURCE_STATUS.exec(line);
    return m !== null && d.clientErrorStatuses.includes(Number(m[1]));
  };
  const problems = [
    ...d.pageErrors.map((m) => `page error: ${m}`),
    ...d.console.filter((m) => !handled(m)).map((m) => `console ${m}`),
    ...d.failedRequests.map((m) => `request failed: ${m}`),
    ...d.serverErrors.map((m) => `server error: ${m}`),
  ];
  expect(problems, `${where} produced browser diagnostics:\n  ${problems.join('\n  ')}`).toEqual(
    [],
  );
}

/** The fixture: every test gets a watched page and a clean-console assertion at the end. */
export const test = base.extend<{ diagnostics: Diagnostics }>({
  diagnostics: async ({ page }, use, testInfo: TestInfo) => {
    const d = watch(page);
    await use(d);
    if (testInfo.status === testInfo.expectedStatus) expectClean(d, testInfo.title);
  },
});

export { expect } from '@playwright/test';

/**
 * Waits for the SPA to have settled: the router has rendered a route and TanStack Query has no
 * request in flight. `networkidle` alone is not enough, because the event stream is a long-lived
 * response that never idles.
 */
export async function settle(page: Page) {
  await page.waitForLoadState('domcontentloaded');
  await expect(page.locator('#root')).not.toBeEmpty();
  await page.waitForFunction(() => document.readyState === 'complete', null, { timeout: 20_000 });
  // A short settle beat: enough for a query that resolved to have painted.
  await page.waitForTimeout(500);
}

/**
 * Navigates and waits for the screen to be painted.
 *
 * `waitUntil: 'domcontentloaded'` rather than the default `load` on purpose: every screen inside the
 * shell holds an open `GET /api/v1/events` stream, and Chromium keeps the departing document's
 * EventSource socket around long enough that the *next* document's `load` event can sit behind it
 * for the connection's whole keepalive. That is a browser bookkeeping artifact, not the app being
 * slow — `document.readyState` reaches `complete` in about two seconds either way, which is what
 * `settle` waits for.
 */
export async function visit(page: Page, path: string) {
  await page.goto(path, { waitUntil: 'domcontentloaded' });
  await settle(page);
}

/** Forgets everything collected so far — for a step whose noise the test has already accounted for. */
export function reset(d: Diagnostics) {
  d.console.length = 0;
  d.pageErrors.length = 0;
  d.failedRequests.length = 0;
  d.serverErrors.length = 0;
  d.clientErrorStatuses.length = 0;
}

/**
 * Signs in through the login screen.
 *
 * It goes to `/login` DIRECTLY rather than letting a gated route bounce it there, because the
 * bounce is itself under test in `03-signed-out.spec.ts` and a helper that depended on it would
 * hide the failure it is meant to expose.
 */
export async function signIn(page: Page, password: string) {
  await page.goto('/login', { waitUntil: 'domcontentloaded' });
  await settle(page);
  await page.getByRole('textbox').first().fill(password);
  await page.getByRole('button', { name: /sign in/i }).click();
  await page.waitForURL((url) => !url.pathname.startsWith('/login'), { timeout: 30_000 });
  await settle(page);
}
