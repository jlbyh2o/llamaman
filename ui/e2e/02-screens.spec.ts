import type { Response } from '@playwright/test';
import { ADMIN_PASSWORD, expect, reset, shot, signIn, test, visit } from './support';
import { SCREENS } from './screens';

/**
 * Every area screen, in both themes and at both widths, against real API data.
 *
 * One test per (theme, width) rather than one per screen: signing in is the expensive part, the
 * screens are read-only, and a single failure that names every screen which broke is more useful
 * than fourteen that each name one.
 *
 * The bar is the one the design sets: the screen renders its own title, the browser logs nothing,
 * and no API call the screen makes fails. A 404 from a route the daemon does not serve yet fails
 * here on purpose — the feature api modules degrade such a call into an empty pane rather than a
 * red error, which is good behavior and a bad reason to call the screen finished.
 */

const THEMES = ['dark', 'light'] as const;
const WIDTHS = [1280, 900] as const;

for (const theme of THEMES) {
  for (const width of WIDTHS) {
    test(`every screen renders — ${theme} theme at ${width}px`, async ({ page, diagnostics }) => {
      test.setTimeout(300_000);
      await page.setViewportSize({ width, height: 900 });
      await page.addInitScript((t) => {
        try {
          localStorage.setItem('llamaman.theme', t as string);
        } catch {
          /* a private window still renders the default palette */
        }
      }, theme);

      await signIn(page, ADMIN_PASSWORD);
      await expect(page.locator('html')).toHaveAttribute('data-theme', theme);

      const broken: string[] = [];
      for (const screen of SCREENS) {
        const mark = {
          console: diagnostics.console.length,
          pageErrors: diagnostics.pageErrors.length,
          failed: diagnostics.failedRequests.length,
          server: diagnostics.serverErrors.length,
        };
        const bad: string[] = [];
        const onResponse = (r: Response) => {
          if (r.status() >= 400) {
            bad.push(`${r.status()} ${r.request().method()} ${new URL(r.url()).pathname}`);
          }
        };
        page.on('response', onResponse);

        await visit(page, screen.path);

        // The title a user sees. Its LEVEL is asserted separately, below.
        const title = page.getByRole('heading', { name: screen.heading }).first();
        if (!(await title.isVisible().catch(() => false))) {
          broken.push(`${screen.path}: no heading matching ${screen.heading}`);
        }

        await page.screenshot({ path: shot(`${screen.name}-${theme}-${width}`), fullPage: true });

        page.off('response', onResponse);
        const unique = [...new Set(bad)];
        if (unique.length) broken.push(`${screen.path}: ${unique.join(', ')}`);
        for (const line of [
          ...diagnostics.pageErrors.slice(mark.pageErrors),
          ...diagnostics.serverErrors.slice(mark.server),
          ...diagnostics.failedRequests.slice(mark.failed),
          ...diagnostics.console.slice(mark.console),
        ]) {
          broken.push(`${screen.path}: ${line}`);
        }
      }

      // Everything above has been attributed to a screen; the fixture's own end-of-test check
      // would repeat it all without saying where any of it happened.
      reset(diagnostics);

      expect(broken, `screens that did not render clean:\n  ${broken.join('\n  ')}`).toEqual([]);
    });
  }
}

test('every screen owns exactly one level-1 heading', async ({ page }) => {
  test.setTimeout(240_000);
  await signIn(page, ADMIN_PASSWORD);

  const wrong: string[] = [];
  for (const screen of SCREENS) {
    await visit(page, screen.path);
    const h1 = await page.locator('h1').allTextContents();
    if (h1.length !== 1)
      wrong.push(`${screen.path}: ${h1.length} <h1> elements (${JSON.stringify(h1)})`);
  }
  expect(
    wrong,
    `screens whose document outline does not start at h1:\n  ${wrong.join('\n  ')}`,
  ).toEqual([]);
});

test('the theme toggle honors light mode and dark mode', async ({ page }) => {
  await signIn(page, ADMIN_PASSWORD);
  await visit(page, '/');

  // The toggle cycles system -> dark -> light, and its label states where it will go next.
  const toggle = () => page.getByRole('button', { name: /theme/i });
  await toggle().click();
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
  await toggle().click();
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
  await toggle().click();
  await expect(page.locator('html')).not.toHaveAttribute('data-theme', /.*/);
});

test('every navigation link reaches a screen that renders', async ({ page }) => {
  test.setTimeout(240_000);
  await signIn(page, ADMIN_PASSWORD);
  await visit(page, '/');

  const nav = page.getByRole('navigation', { name: 'Main' });
  const hrefs = (await nav.getByRole('link').all()).map((l) => l.getAttribute('href'));
  const targets = (await Promise.all(hrefs)).filter((h): h is string => !!h);
  expect(targets.length).toBeGreaterThan(5);

  const dead: string[] = [];
  for (const href of targets) {
    await visit(page, href);
    if (page.url().includes('/login')) {
      dead.push(`${href} bounced to the login screen`);
      continue;
    }
    if ((await page.getByText(/page not found|no such route/i).count()) > 0) {
      dead.push(`${href} rendered the not-found screen`);
    }
    if ((await page.getByRole('heading').count()) === 0) dead.push(`${href} rendered no heading`);
  }
  expect(dead, `dead navigation links:\n  ${dead.join('\n  ')}`).toEqual([]);
});
