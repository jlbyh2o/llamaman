/**
 * The navigation structure.
 *
 * Five groups over the seventeen screens of DESIGN section 4, arranged by the object being managed
 * rather than by the API that serves it: what is running, what it runs on, what it runs, and the
 * host underneath. `/instances/new`, `/instances/:id`, `/models/browse/:repo`, `/bench/:id` and the
 * rest are reached from their list screens, so they are not nav entries.
 *
 * Data, not JSX, so the shell renders it and the tests can assert on it.
 */

import {
  Activity,
  Boxes,
  Cpu,
  Download,
  Gauge,
  KeyRound,
  LayoutDashboard,
  ListTree,
  Package,
  Search,
  Server,
  Settings,
  Wrench,
} from 'lucide-react';
import type { LucideIcon } from 'lucide-react';

export interface NavItem {
  /** The route path, exactly as the router declares it. */
  to: string;
  label: string;
  icon: LucideIcon;
  /** Match child routes too — `/instances` stays selected on `/instances/new`. */
  fuzzy?: boolean;
}

export interface NavGroup {
  id: string;
  label: string;
  items: NavItem[];
}

export const NAV_GROUPS: NavGroup[] = [
  {
    id: 'overview',
    label: 'Overview',
    items: [
      { to: '/', label: 'Dashboard', icon: LayoutDashboard },
      { to: '/events', label: 'Events', icon: ListTree },
    ],
  },
  {
    id: 'serving',
    label: 'Serving',
    items: [
      { to: '/instances', label: 'Instances', icon: Server, fuzzy: true },
      { to: '/tokens', label: 'API tokens', icon: KeyRound },
    ],
  },
  {
    id: 'models',
    label: 'Models',
    items: [
      { to: '/models', label: 'Library', icon: Boxes },
      { to: '/models/browse', label: 'Browse Hugging Face', icon: Search, fuzzy: true },
      { to: '/downloads', label: 'Downloads', icon: Download },
    ],
  },
  {
    id: 'runtime',
    label: 'Runtime',
    items: [
      { to: '/llamacpp', label: 'llama.cpp', icon: Package },
      { to: '/bench', label: 'Benchmarks', icon: Gauge, fuzzy: true },
    ],
  },
  {
    id: 'host',
    label: 'Host',
    items: [
      { to: '/system', label: 'System', icon: Cpu },
      { to: '/settings', label: 'Settings', icon: Settings },
    ],
  },
];

/** Icons the shell uses outside the nav list. */
export const SHELL_ICONS = { activity: Activity, wrench: Wrench } as const;
