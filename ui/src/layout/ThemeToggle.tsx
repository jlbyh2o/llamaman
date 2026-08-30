import { Monitor, Moon, Sun } from 'lucide-react';
import { Button, Tooltip } from '../components';
import { useThemeStore } from '../theme/useTheme';
import type { ThemePreference } from '../theme/useTheme';

const NEXT_LABEL: Record<ThemePreference, string> = {
  system: 'Following the system theme — switch to dark',
  dark: 'Dark theme — switch to light',
  light: 'Light theme — follow the system',
};

const ICON = { system: Monitor, dark: Moon, light: Sun } as const;

/** Cycles system → dark → light. Dark is the default the app is designed around (SPEC section 5.4). */
export function ThemeToggle() {
  const preference = useThemeStore((state) => state.preference);
  const cycle = useThemeStore((state) => state.cycle);
  const Icon = ICON[preference];

  return (
    <Tooltip content={NEXT_LABEL[preference]}>
      <Button variant="ghost" size="icon" onClick={cycle} aria-label={NEXT_LABEL[preference]}>
        <Icon aria-hidden className="size-4" />
      </Button>
    </Tooltip>
  );
}
