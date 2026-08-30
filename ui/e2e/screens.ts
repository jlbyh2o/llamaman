/**
 * The screen inventory the sweep walks.
 *
 * One row per route DESIGN section 4 lists, with the heading that proves the screen actually
 * rendered rather than merely returned HTML. Detail routes that need an id created at runtime are
 * not here — they belong to the specs that create the row.
 */
export interface Screen {
  /** Route path, exactly as the router sees it. */
  path: string;
  /** File-name stem for the screenshot. */
  name: string;
  /** The `<h1>` this screen owns. */
  heading: RegExp;
}

export const SCREENS: Screen[] = [
  { path: '/', name: 'dashboard', heading: /^Dashboard$/ },
  { path: '/instances', name: 'instances', heading: /^Instances$/ },
  { path: '/instances/new', name: 'instance-new', heading: /instance/i },
  { path: '/models', name: 'models', heading: /^Model library$/ },
  { path: '/models/browse', name: 'models-browse', heading: /browse|hugging face/i },
  { path: '/downloads', name: 'downloads', heading: /^Downloads$/ },
  { path: '/llamacpp', name: 'llamacpp', heading: /^llama\.cpp$/ },
  { path: '/bench', name: 'bench', heading: /bench/i },
  { path: '/bench/new', name: 'bench-new', heading: /bench|run/i },
  { path: '/bench/compare', name: 'bench-compare', heading: /compare/i },
  { path: '/tokens', name: 'tokens', heading: /token/i },
  { path: '/settings', name: 'settings', heading: /^Settings$/ },
  { path: '/system', name: 'system', heading: /^System$/ },
  { path: '/events', name: 'events', heading: /^Events$/ },
];
