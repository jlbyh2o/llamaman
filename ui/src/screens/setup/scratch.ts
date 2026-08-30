/**
 * The wizard's scratch state.
 *
 * DESIGN section 4 gives Zustand exactly one job — "the sliver of client state: wizard scratch
 * state, theme, unsaved instance-form drafts" — and this is that sliver.
 *
 * Nothing here is a source of truth. Where the wizard *stands* is `wizard_steps`, read from
 * `GET /setup/state`; what *exists* is versions, models, roots and instances, read from their own
 * endpoints. What this store carries is the one thing no table records: a choice an earlier step
 * made that a later step should default to — the backend the toolchain probe steered towards, and
 * the model picked out of a Hugging Face search whose results are not persisted anywhere.
 *
 * Every consumer falls back to server state when the store is empty, which is what keeps section
 * 11.2's promise intact: a browser refresh loses this and loses nothing.
 */

import { create } from 'zustand';

/** The two backends `GET /llamacpp/plan` accepts (section 3.5). */
export type Backend = 'cpu' | 'cuda';

interface WizardScratchState {
  /** Chosen on the toolchain step; the llama.cpp step's default. Null means "decide from the host". */
  backend: Backend | null;
  /** The local model the instance step should be prefilled from. */
  modelId: string | null;
  /** The repository the models step was last looking at, so returning to it reopens the tree. */
  repoId: string | null;
  setBackend: (backend: Backend | null) => void;
  setModelId: (modelId: string | null) => void;
  setRepoId: (repoId: string | null) => void;
  reset: () => void;
}

const EMPTY = { backend: null, modelId: null, repoId: null } as const;

export const useWizardScratch = create<WizardScratchState>((set) => ({
  ...EMPTY,
  setBackend: (backend) => set({ backend }),
  setModelId: (modelId) => set({ modelId }),
  setRepoId: (repoId) => set({ repoId }),
  reset: () => set({ ...EMPTY }),
}));
