import { defineConfig, devices } from '@playwright/test';

/**
 * End-to-end configuration for the suite in `e2e/`.
 *
 * The suite drives a REAL `llamaman serve` — there is no mock server and no dev server. Start one
 * against a scratch state directory and point the suite at it:
 *
 *   make ui && go build -o ./dist/llamaman ./cmd/llamaman
 *   env -u INVOCATION_ID XDG_STATE_HOME=/tmp/lm-e2e HF_HOME=/tmp/lm-e2e/hf \
 *       ./dist/llamaman serve --port 45551
 *   cd ui && npm run test:e2e
 *
 * `XDG_STATE_HOME` is the seam: DESIGN section 11.1 step 1's chain reads it whenever the daemon is
 * not running under a service manager, so a scratch run needs no flag and no config file, which
 * SPEC section 3.9 forbids anyway. `INVOCATION_ID` has to be unset because a desktop terminal
 * inherits one from its own session scope, which makes the chain think it is under systemd and fall
 * through to /var/lib/llamaman.
 *
 * Environment:
 *   LLAMAMAN_E2E_BASE_URL          where the daemon listens (default http://127.0.0.1:45551)
 *   LLAMAMAN_E2E_PASSWORD          the admin password to claim with / sign in with
 *   LLAMAMAN_E2E_SHOTS             screenshot directory (default ../../llamaman-e2e-shots)
 *   LLAMAMAN_E2E_INSTALL_LLAMACPP  set to 1 to let the wizard actually install llama.cpp
 *
 * It is serial on purpose. The wizard claims the host exactly once (`POST /setup/password` is
 * one-way), so `01-wizard.spec.ts` has to finish before anything that needs a session runs, and two
 * workers driving one daemon would race the same rows.
 */

const baseURL = process.env.LLAMAMAN_E2E_BASE_URL ?? 'http://127.0.0.1:45551';

export default defineConfig({
  testDir: './e2e',
  outputDir: './e2e/.output',
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 0,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : [['list']],
  timeout: 60_000,
  expect: { timeout: 15_000 },
  use: {
    baseURL,
    headless: true,
    viewport: { width: 1280, height: 900 },
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    // The daemon issues a session cookie over plain HTTP on loopback; nothing here needs TLS.
    ignoreHTTPSErrors: true,
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
