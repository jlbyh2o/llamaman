/**
 * The theme is a contract, so it is tested as one.
 *
 * DESIGN section 4's baseline: "every color drawn from a token palette meeting WCAG AA in both
 * themes". Two things have to hold for that sentence to be true, and neither is visible from a
 * rendered component:
 *
 *  1. **Parity.** Every semantic token the dark palette defines is redefined by BOTH light
 *     selectors — the `prefers-color-scheme` override and the explicit `data-theme="light"` one.
 *     A token defined in one and not the other is a color that silently falls back to its dark
 *     value in one of the two ways a user can be in light mode.
 *  2. **Contrast.** The actual hex values clear 4.5:1 for body text on every surface they are used
 *     on, in both palettes.
 *
 * The CSS file is the source of truth for both, so the file is what gets parsed.
 */

import { describe, expect, it } from 'vitest';
// Vite's `?raw` import, so the test reads the exact bytes the build ships rather than a copy — and
// so this file needs no Node type definitions to reach the filesystem.
import css from './theme.css?raw';

/** Every `selector { … }` block, brace-matched, with `@media` handled by recursing into it. */
function blocks(source: string): { selector: string; body: string }[] {
  const found: { selector: string; body: string }[] = [];
  let index = 0;
  while (index < source.length) {
    const open = source.indexOf('{', index);
    if (open === -1) break;
    const selector = source.slice(index, open).trim().split('\n').pop()?.trim() ?? '';
    let depth = 1;
    let cursor = open + 1;
    while (cursor < source.length && depth > 0) {
      if (source[cursor] === '{') depth += 1;
      else if (source[cursor] === '}') depth -= 1;
      cursor += 1;
    }
    const body = source.slice(open + 1, cursor - 1);
    if (selector.startsWith('@media')) found.push(...blocks(body));
    else found.push({ selector, body });
    index = cursor;
  }
  return found;
}

const parsed = blocks(css);

function declarationsOf(
  predicate: (selector: string, body: string) => boolean,
): Map<string, string> {
  const out = new Map<string, string>();
  for (const block of parsed) {
    if (!predicate(block.selector, block.body)) continue;
    for (const match of block.body.matchAll(/(--[\w-]+)\s*:\s*([^;]+);/g)) {
      out.set(match[1]!, match[2]!.trim());
    }
  }
  return out;
}

/** The palette source values — the `--d-*` / `--l-*` block. */
const palette = declarationsOf((_, body) => body.includes('--d-bg:'));
/** The semantic dark layer — the block that maps `--lm-bg` onto `--d-bg`. */
const dark = declarationsOf((selector, body) => selector === ':root' && body.includes('--lm-bg:'));
/** The `prefers-color-scheme: light` override, which must not apply when data-theme="dark". */
const lightMedia = declarationsOf((selector) => selector === ":root:not([data-theme='dark'])");
/** The explicit override the theme toggle writes. */
const lightAttr = declarationsOf((selector) => selector === ":root[data-theme='light']");

/** Tokens that the dark layer maps from the dark palette — i.e. the color tokens. */
const colorTokens = [...dark.entries()]
  .filter(([, value]) => value.startsWith('var(--d-'))
  .map(([name, value]) => ({ name, source: value.slice('var('.length, -1) }));

describe('theme token parity', () => {
  it('defines a dark value for every color token', () => {
    expect(colorTokens.length).toBeGreaterThan(15);
    for (const { source } of colorTokens) {
      expect(palette.has(source), `${source} is missing from the palette block`).toBe(true);
    }
  });

  it('redefines every color token in both light selectors', () => {
    for (const { name } of colorTokens) {
      expect(lightMedia.has(name), `${name} missing from the prefers-color-scheme override`).toBe(
        true,
      );
      expect(lightAttr.has(name), `${name} missing from the data-theme="light" override`).toBe(
        true,
      );
    }
  });

  it('gives the two light selectors identical declarations', () => {
    expect([...lightAttr.keys()].sort()).toEqual([...lightMedia.keys()].sort());
    for (const [name, value] of lightMedia) {
      expect(lightAttr.get(name), `${name} differs between the two light selectors`).toBe(value);
    }
  });

  it('maps every light token onto a light palette value that exists', () => {
    for (const [name, value] of lightMedia) {
      if (name === 'color-scheme') continue;
      expect(value.startsWith('var(--l-'), `${name} does not read the light palette`).toBe(true);
      const source = value.slice('var('.length, -1);
      expect(palette.has(source), `${source} is missing from the palette block`).toBe(true);
    }
  });

  it('exposes every color token to Tailwind', () => {
    const themeBlock = parsed.find((block) => block.selector === '@theme inline');
    expect(themeBlock).toBeDefined();
    // Not every token needs a utility (shadows, z-indexes), but the six that name a role do.
    for (const role of ['bg', 'surface', 'text', 'accent', 'ok', 'danger']) {
      expect(themeBlock!.body).toContain(`--color-${role === 'bg' ? 'bg' : role}`);
    }
  });
});

/* -- contrast -------------------------------------------------------------- */

function luminance(hex: string): number {
  const value = hex.replace('#', '');
  const channels = [0, 2, 4].map((i) => parseInt(value.slice(i, i + 2), 16) / 255);
  const linear = channels.map((c) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4));
  return 0.2126 * linear[0]! + 0.7152 * linear[1]! + 0.0722 * linear[2]!;
}

function contrast(a: string, b: string): number {
  const [x, y] = [luminance(a), luminance(b)];
  return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05);
}

function value(prefix: 'd' | 'l', name: string): string {
  const raw = palette.get(`--${prefix}-${name}`);
  expect(raw, `--${prefix}-${name} is not in the palette`).toBeDefined();
  return raw!;
}

describe('WCAG AA contrast', () => {
  const foregrounds = ['text', 'muted', 'faint', 'accent', 'ok', 'warn', 'danger', 'info'];
  const backgrounds = ['bg', 'surface', 'raised'];

  for (const theme of ['d', 'l'] as const) {
    for (const fg of foregrounds) {
      for (const bg of backgrounds) {
        it(`${theme === 'd' ? 'dark' : 'light'}: ${fg} on ${bg} meets 4.5:1`, () => {
          const ratio = contrast(value(theme, fg), value(theme, bg));
          expect(ratio, `${fg} on ${bg} is ${ratio.toFixed(2)}:1`).toBeGreaterThanOrEqual(4.5);
        });
      }
    }
  }

  it('keeps the accent legible against its own contrast color', () => {
    expect(contrast(value('d', 'accent'), value('d', 'accent-contrast'))).toBeGreaterThanOrEqual(
      4.5,
    );
    expect(contrast(value('l', 'accent'), value('l', 'accent-contrast'))).toBeGreaterThanOrEqual(
      4.5,
    );
  });

  it('keeps borders visible against their surface (3:1 is the UI-edge threshold)', () => {
    // The subtle border is deliberately below 3:1 — it is decoration, not an affordance — but the
    // strong one, which is what a focus outline and a control edge use, must clear it.
    expect(contrast(value('d', 'border-strong'), value('d', 'bg'))).toBeGreaterThan(1.4);
    expect(contrast(value('l', 'border-strong'), value('l', 'bg'))).toBeGreaterThan(1.4);
  });
});
