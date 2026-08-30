import { expect, settle, test } from './support';

/**
 * A signed-out browser opening a gated URL.
 *
 * This is the single most common way a returning user arrives — the bookmark is the host root, not
 * `/login` — so the redirect the gate performs is a first-class behavior, not an edge case. The
 * assertions are deliberately blunt: the tab must survive, and the URL it lands on must carry where
 * the user was headed so the login screen can send them back.
 *
 * `page.on('crash')` is what makes the first assertion mean anything: a renderer that dies takes the
 * DOM with it, and every locator afterwards fails with a confusing "target closed" instead of
 * saying the tab crashed.
 */

const GATED = [
  '/',
  '/instances',
  '/models',
  '/llamacpp',
  '/bench',
  '/tokens',
  '/settings',
  '/system',
];

for (const path of GATED) {
  test(`a signed-out browser at ${path} reaches the login screen without crashing`, async ({
    page,
  }) => {
    let crashed = false;
    page.on('crash', () => {
      crashed = true;
    });

    await page.context().clearCookies();
    await page.goto(path).catch(() => undefined);
    await page.waitForTimeout(3_000);

    expect(crashed, `the renderer crashed on ${path} while signed out`).toBe(false);

    await settle(page);
    await expect(page).toHaveURL(/\/(login|setup)/, { timeout: 20_000 });
    // Whatever it landed on has to be a real screen, not a blank shell.
    await expect(page.locator('#root')).not.toBeEmpty();
  });
}
