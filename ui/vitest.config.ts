import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

/**
 * The unit suite.
 *
 * **Why `environment: 'node'` and not jsdom.** A DOM environment is a package — `jsdom` or
 * `happy-dom`, both optional peers of vitest — and DESIGN section 14's tables are the whole
 * dependency surface this project is allowed. It is also weaker evidence than it looks: jsdom does
 * not load Tailwind's stylesheet or resolve custom properties, so a jsdom test could assert that a
 * `data-theme` attribute changed and nothing more. It could not tell a correct palette from a
 * broken one.
 *
 * So the theme is proved where the theme actually lives:
 *
 *  - Components render through `react-dom/server`, in both themes, and are asserted to reference
 *    only `var(--lm-…)` tokens — no literal color reaches the markup.
 *  - `theme.css` itself is parsed, and every semantic token is checked to exist in the dark palette
 *    and both light selectors, with the WCAG AA contrast ratios computed from the real values.
 *
 * The rest of the suite is pure logic — formatters, the SSE reducer, the connection state machine,
 * URL building — which needs no environment at all. Playwright (DESIGN section 15) is where a real
 * browser enters the picture.
 */
export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'node',
    include: ['src/**/*.test.{ts,tsx}'],
    restoreMocks: true,
    // Vitest stubs CSS modules to an empty string by default, and that stub also swallows the
    // `?raw` import theme.test.ts uses to read the token file. Turning it off is what lets the
    // theme be tested against its actual bytes.
    css: true,
  },
});
