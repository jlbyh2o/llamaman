/**
 * Theme preference.
 *
 * Three values, and `system` is the default: the palette then follows `prefers-color-scheme`, which
 * is what theme.css's media query implements. `dark` and `light` write `data-theme` onto <html>,
 * which the two override blocks in theme.css match.
 *
 * This is one of the three things Zustand holds (DESIGN section 4: "the sliver of client state").
 * The preference is a per-browser convenience, so it lives in localStorage rather than in the
 * database — nothing about it belongs to the host being managed.
 */

import { create } from 'zustand';

export type ThemePreference = 'system' | 'dark' | 'light';

const STORAGE_KEY = 'llamaman.theme';

function readStored(): ThemePreference {
  try {
    const value = localStorage.getItem(STORAGE_KEY);
    if (value === 'dark' || value === 'light' || value === 'system') return value;
  } catch {
    // Private windows and blocked site data both throw here; the default is correct either way.
  }
  return 'system';
}

/** Reflect the preference onto <html>, which is what the CSS selectors read. */
export function applyTheme(preference: ThemePreference): void {
  if (typeof document === 'undefined') return;
  const root = document.documentElement;
  if (preference === 'system') root.removeAttribute('data-theme');
  else root.setAttribute('data-theme', preference);
}

interface ThemeStore {
  preference: ThemePreference;
  setPreference: (preference: ThemePreference) => void;
  /** Cycle system -> dark -> light -> system, which is what the header button does. */
  cycle: () => void;
}

export const useThemeStore = create<ThemeStore>((set, get) => ({
  preference: readStored(),
  setPreference: (preference) => {
    applyTheme(preference);
    try {
      localStorage.setItem(STORAGE_KEY, preference);
    } catch {
      // A preference that cannot be persisted still applies for this session.
    }
    set({ preference });
  },
  cycle: () => {
    const order: ThemePreference[] = ['system', 'dark', 'light'];
    const next = order[(order.indexOf(get().preference) + 1) % order.length];
    get().setPreference(next ?? 'system');
  },
}));

/** Call once at startup, before the first paint, so the stored choice is never a flash. */
export function initTheme(): void {
  applyTheme(readStored());
}

/**
 * The theme actually in effect, resolving `system` against the media query. Components that must
 * branch on the palette (a canvas chart, an SVG gradient) read this; CSS never needs it.
 */
export function resolveTheme(preference: ThemePreference): 'dark' | 'light' {
  if (preference !== 'system') return preference;
  if (typeof window === 'undefined' || !window.matchMedia) return 'dark';
  return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
}
