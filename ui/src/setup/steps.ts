/**
 * The wizard's six steps, plus the terminal one.
 *
 * Rows in `wizard_steps` (DESIGN section 2.11), one route each (section 4 screen 2), and the copy
 * comes from section 11.2's table. The server owns the *state* of each step — which is active,
 * which is skippable right now, which is blocked — so this file carries only what does not change:
 * the id, the route, and what the step is for.
 */

import type { WizardStepId } from '../api/types';

export interface WizardStepMeta {
  id: WizardStepId;
  path: string;
  title: string;
  /** One line under the title, in the wizard's own voice. */
  description: string;
  /** What the rail shows. Short enough for a 12rem column. */
  railLabel: string;
}

export const WIZARD_STEP_METAS: WizardStepMeta[] = [
  {
    id: 'password',
    path: '/setup/password',
    title: 'Set the admin password',
    description:
      'This claims the host. The password is hashed with argon2id and is the only credential for the management UI.',
    railLabel: 'Password',
  },
  {
    id: 'toolchain',
    path: '/setup/toolchain',
    title: 'Check the build toolchain',
    description:
      'A CUDA build needs gcc, cmake and the CUDA toolkit. Nothing here is installed for you — this reports what is present and what is missing.',
    railLabel: 'Toolchain',
  },
  {
    id: 'llamacpp',
    path: '/setup/llamacpp',
    title: 'Install llama.cpp',
    description:
      'Pick a channel and a version. The plan preview says whether this will be a download or a source build before anything starts, and you may leave this page while it runs.',
    railLabel: 'llama.cpp',
  },
  {
    id: 'hf',
    path: '/setup/hf',
    title: 'Hugging Face',
    description:
      'An optional token unlocks gated and private repositories. The cache location is detected here, and scanned so models already on disk show up as ready to use.',
    railLabel: 'Hugging Face',
  },
  {
    id: 'models',
    path: '/setup/models',
    title: 'Get a model',
    description:
      'Whatever the scan already found is listed first. Otherwise search Hugging Face with the fit calculator live; the download continues in the background.',
    railLabel: 'Models',
  },
  {
    id: 'instance',
    path: '/setup/instance',
    title: 'Create the first instance',
    description:
      'Prefilled from the model you chose, with flags the fit calculator recommends and a free port. Autostart is optional.',
    railLabel: 'First instance',
  },
  {
    id: 'done',
    path: '/setup/done',
    title: 'Ready',
    description:
      'Everything the wizard sets up is editable later in Settings. Nothing here is one-shot.',
    railLabel: 'Done',
  },
];

/** The six configurable steps; `done` is the landing, not a step with work in it. */
export const WIZARD_STEPS_IN_RAIL = WIZARD_STEP_METAS.filter((step) => step.id !== 'done');

export function stepMeta(id: string): WizardStepMeta | undefined {
  return WIZARD_STEP_METAS.find((step) => step.id === id);
}

export function stepIndex(id: string): number {
  return WIZARD_STEP_METAS.findIndex((step) => step.id === id);
}
