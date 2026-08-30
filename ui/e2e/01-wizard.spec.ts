import type { Page } from '@playwright/test';
import { ADMIN_PASSWORD, STORAGE_STATE, expect, reset, settle, shot, test, visit } from './support';

/**
 * The first-run wizard, end to end, against a real daemon (DESIGN section 11.2).
 *
 * `POST /setup/password` claims the host and the claim is one-way, so this spec is written to be
 * re-runnable rather than once-only: it enters at `/setup`, signs in when the daemon answers that
 * the host is already claimed, and resumes from whichever step `wizard_steps` says is active. That
 * is the same resumability section 11.2 requires of a browser refresh, so exercising it is not a
 * concession — it is the documented behavior.
 *
 * Nothing here reaches the network beyond the daemon and, when explicitly asked for, GitHub: the
 * llama.cpp step is asserted for its rendering and its plan preview always, and only installs when
 * LLAMAMAN_E2E_INSTALL_LLAMACPP=1 is set, because that step is not skippable and an install is a
 * real download.
 */

test.describe.configure({ mode: 'serial' });

test('the wizard runs from a fresh host to the dashboard', async ({
  page,
  context,
  diagnostics,
}) => {
  // The llama.cpp step opts into a REAL download and build, and the assertion that waits for it
  // allows 15 minutes. The file's own 60 s budget cuts the whole test off long before that, so the
  // wait can never be reached and the opted-in run fails on the clock rather than on the app.
  if (process.env.LLAMAMAN_E2E_INSTALL_LLAMACPP === '1') test.setTimeout(1_200_000);

  /* -- entry: the wizard's own resume point -------------------------------- */

  await visit(page, '/setup');

  if (page.url().includes('/login')) {
    // Already claimed: sign in, then come back to the wizard where it left off.
    await page.getByRole('textbox').first().fill(ADMIN_PASSWORD);
    await page.getByRole('button', { name: /sign in/i }).click();
    await page.waitForURL((url) => !url.pathname.startsWith('/login'), { timeout: 20_000 });
    await visit(page, '/setup');
    // The bounce itself logs the 401s that caused it. They are the subject of
    // `03-signed-out.spec.ts`, not of this test.
    reset(diagnostics);
  }

  /* -- step 1: password ---------------------------------------------------- */

  if (page.url().endsWith('/setup/password')) {
    await expect(
      page.getByRole('heading', { name: 'Set the admin password', exact: true, level: 1 }),
    ).toBeVisible();
    await page.screenshot({ path: shot('wizard-1-password'), fullPage: true });

    // The daemon is on loopback, so D38's setup token is not required and the field is absent.
    await expect(page.getByRole('textbox', { name: 'Setup token' })).toHaveCount(0);

    const claim = page.getByRole('button', { name: 'Claim this host' });
    await expect(claim).toBeDisabled();
    await page.getByRole('textbox', { name: 'Admin password' }).fill(ADMIN_PASSWORD);
    await page.getByRole('textbox', { name: 'Confirm password' }).fill(ADMIN_PASSWORD);
    await expect(page.getByRole('progressbar')).toBeVisible();
    await expect(claim).toBeEnabled();
    await claim.click();
    await expect(page).toHaveURL(/\/setup\/toolchain$/, { timeout: 20_000 });
  }

  /* -- step 2: toolchain — this host's real probe --------------------------- */

  if (page.url().endsWith('/setup/toolchain')) {
    await settle(page);
    await expect(
      page.getByRole('heading', { name: 'Check the build toolchain', exact: true, level: 1 }),
    ).toBeVisible();
    // The probe is `GET /llamacpp/plan` asked once per backend; these chips are its verdict.
    await expect(page.getByText('CPU build').first()).toBeVisible({ timeout: 30_000 });
    await expect(page.getByText('CUDA build').first()).toBeVisible();
    // Real per-tool rows from this host, not a placeholder list.
    await expect(page.getByText('cmake').first()).toBeVisible();
    await expect(page.getByText('gcc').first()).toBeVisible();
    await page.screenshot({ path: shot('wizard-2-toolchain'), fullPage: true });

    await page.getByRole('button', { name: /^Continue (with CUDA|CPU-only)$/ }).click();
    await expect(page).toHaveURL(/\/setup\/llamacpp$/, { timeout: 20_000 });
  }

  /* -- step 3: llama.cpp ---------------------------------------------------- */

  if (page.url().endsWith('/setup/llamacpp')) {
    await settle(page);
    await expect(
      page.getByRole('heading', { name: 'Install llama.cpp', exact: true, level: 1 }),
    ).toBeVisible();

    // The CPU prebuilt channel, and the plan preview that says what will happen BEFORE anything
    // runs (section 6.3): acquisition, the resolved build tag, the asset, and the space check.
    await expect(page.getByRole('combobox', { name: 'Channel' })).toHaveText('Stable');
    await expect(page.getByRole('combobox', { name: 'Backend' })).toHaveText('CPU');
    await expect(page.getByText('Prebuilt download')).toBeVisible();
    // `.first()` because a version row already installed on this host repeats the same build tag
    // further down the page; the plan strip is the topmost occurrence.
    await expect(page.getByText(/^b\d+$/).first()).toBeVisible();
    await expect(page.getByText(/bin-ubuntu-x64\.tar\.gz$/).first()).toBeVisible();
    await page.screenshot({ path: shot('wizard-3-llamacpp'), fullPage: true });

    const install = page.getByRole('button', { name: 'Download this version' });
    await expect(install).toBeVisible();

    if (process.env.LLAMAMAN_E2E_INSTALL_LLAMACPP !== '1') {
      // Default: assert the step renders and validates, then stop. The step is not skippable and
      // the install is a real download, so the wizard cannot go further without opting in.
      await expect(page.getByRole('button', { name: 'Waiting for a build' })).toBeDisabled();
      test.skip(
        true,
        'set LLAMAMAN_E2E_INSTALL_LLAMACPP=1 to install llama.cpp and run the rest of the wizard',
      );
      return;
    }

    // `can_proceed` gates the button; a plan that refuses says why in the same strip.
    await expect(install).toBeEnabled({ timeout: 30_000 });
    await install.click();

    // A finished install leaves the version `ready` but NOT active, and the step offers an
    // "Activate <tag>" button for exactly that state. Continue reads `GET /llamacpp/active`, so
    // waiting on Continue alone would sit out the whole budget against a build that installed
    // perfectly. Either control appearing ends the wait: a resumed run whose build is already
    // active never renders the Activate button at all.
    const activate = page.getByRole('button', { name: /^Activate / });
    const proceed = page.getByRole('button', { name: 'Continue', exact: true });
    await expect
      .poll(async () => (await proceed.count()) > 0 || (await activate.count()) > 0, {
        timeout: 900_000,
      })
      .toBe(true);
    if ((await activate.count()) > 0) await activate.click();

    // Continue turns on only when a version is `ready` and active, read from `GET /llamacpp/active`
    // rather than from the click — which is what makes the step resumable mid-build.
    await expect(proceed).toBeEnabled({ timeout: 120_000 });
    await page.screenshot({ path: shot('wizard-3-llamacpp-installed'), fullPage: true });
    await proceed.click();
    await expect(page).toHaveURL(/\/setup\/hf$/, { timeout: 20_000 });
  }

  /* -- step 4: Hugging Face — token skipped, cache detected ----------------- */

  if (page.url().endsWith('/setup/hf')) {
    await settle(page);
    // Pinned to the step's own h1, as every step assertion in this spec is: an unqualified name is
    // a SUBSTRING match, and this step also renders a "Hugging Face token" panel heading, so the
    // bare locator resolved to two elements and failed on strict mode.
    await expect(
      page.getByRole('heading', { name: 'Hugging Face', exact: true, level: 1 }),
    ).toBeVisible();
    // The detection chain of section 7.2 resolved a hub directory from this host's environment.
    await expect(page.getByText(/hub$/).first()).toBeVisible({ timeout: 20_000 });
    await page.screenshot({ path: shot('wizard-4-hf'), fullPage: true });
    await advance(page, 'models');
  }

  /* -- step 5: models — what the cache scan found --------------------------- */

  if (page.url().endsWith('/setup/models')) {
    await settle(page);
    await expect(
      page.getByRole('heading', { name: 'Get a model', exact: true, level: 1 }),
    ).toBeVisible();
    await page.screenshot({ path: shot('wizard-5-models'), fullPage: true });
    await advance(page, 'instance');
  }

  /* -- step 6: the first instance ------------------------------------------ */

  if (page.url().endsWith('/setup/instance')) {
    await settle(page);
    await expect(
      page.getByRole('heading', { name: 'Create the first instance', exact: true, level: 1 }),
    ).toBeVisible();
    await page.screenshot({ path: shot('wizard-6-instance'), fullPage: true });
    await advance(page, 'done');
  }

  /* -- step 7: done --------------------------------------------------------- */

  await settle(page);
  await expect(page.getByRole('heading', { name: 'Ready', exact: true, level: 1 })).toBeVisible();
  await page.screenshot({ path: shot('wizard-7-done'), fullPage: true });
  await page
    .getByRole('button', { name: /finish|dashboard|go to/i })
    .first()
    .click();

  await page.waitForURL((url) => !url.pathname.startsWith('/setup'), { timeout: 30_000 });
  await settle(page);
  await expect(page.getByRole('navigation', { name: 'Main' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Dashboard' })).toBeVisible();
  await page.screenshot({ path: shot('wizard-8-dashboard'), fullPage: true });

  // Left for a later spec that wants the session without signing in again.
  await context.storageState({ path: STORAGE_STATE });
});

/** Leaves a step by its own control: Skip when the server offers it, Continue otherwise. */
async function advance(page: Page, next: string) {
  const skip = page.getByRole('button', { name: 'Skip this step' });
  if (await skip.isVisible().catch(() => false)) await skip.click();
  else
    await page
      .getByRole('button', { name: /^(Continue|Finish)/ })
      .first()
      .click();
  await expect(page).toHaveURL(new RegExp(`/setup/${next}$`), { timeout: 30_000 });
}
