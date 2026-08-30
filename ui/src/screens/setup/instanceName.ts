/**
 * Turning a model into an instance name.
 *
 * D11's grammar is `^[a-z0-9][a-z0-9-]{0,31}$`, enforced in three places on the server "and that is
 * not redundancy: this string becomes a systemd unit instance id". The wizard prefills a name
 * rather than asking for one, so the suggestion has to satisfy that grammar by construction — and
 * the same predicate validates what the user types over the top of it, so the form says no before
 * the server has to.
 */

/** D11's pattern, transcribed. The server is still the authority; this only avoids a round trip. */
export const INSTANCE_NAME_PATTERN = /^[a-z0-9][a-z0-9-]{0,31}$/;

export const INSTANCE_NAME_HELP =
  'One to thirty-two characters of lowercase letters, digits and hyphens, starting with a letter or digit — it becomes a systemd unit name.';

export function isValidInstanceName(name: string): boolean {
  return INSTANCE_NAME_PATTERN.test(name);
}

/**
 * Suggest a name from a repository id and a quantization label.
 *
 * `bartowski/Qwen3-8B-GGUF` + `Q4_K_M` becomes `qwen3-8b-q4-k-m`: the owner is dropped because it
 * says nothing about the instance, the `-gguf` suffix is dropped because every model here is one,
 * and everything else is folded into the grammar. An empty result falls back to a name that is at
 * least valid, because a form that prefills nothing is worse than one that prefills a placeholder.
 */
export function suggestInstanceName(repoId: string, quantLabel?: string | null): string {
  const repoTail = repoId.split('/').pop() ?? repoId;
  const base = repoTail.replace(/[-_.]?gguf$/i, '');
  const joined = quantLabel ? `${base}-${quantLabel}` : base;

  const slug = joined
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+/, '')
    .replace(/-+$/, '')
    .slice(0, 32)
    .replace(/-+$/, '');

  return isValidInstanceName(slug) ? slug : 'llama-1';
}

/** `name`, `name-2`, `name-3` … — the first that is not already taken. */
export function uniqueInstanceName(suggested: string, taken: readonly string[]): string {
  const used = new Set(taken);
  if (!used.has(suggested)) return suggested;
  for (let n = 2; n < 100; n += 1) {
    const suffix = `-${n}`;
    const candidate = `${suggested.slice(0, 32 - suffix.length).replace(/-+$/, '')}${suffix}`;
    if (!used.has(candidate) && isValidInstanceName(candidate)) return candidate;
  }
  return suggested;
}
