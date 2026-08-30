/**
 * `extra_flags`, client side.
 *
 * SPEC section 3.3's escape hatch, and the exact rules of `internal/instances/extraflags.go`
 * transcribed so the form can refuse a bad value *before* a round trip and explain why. The server
 * is still the authority — it runs the same split against the stored row at save time and again in
 * the launcher — so nothing here relaxes a rule; it only says the same "no" sooner.
 *
 * Two halves, both from DESIGN section 5.7:
 *
 *  1. **POSIX word rules, and nothing else a shell would do.** Quotes group, a backslash escapes,
 *     an unterminated quote is an error rather than an implicit close. Globbing, `$`, backticks,
 *     `$(…)`, `~`, redirection and `;`/`&&`/`|` are ordinary text — which is what makes the field
 *     safe to hand to `syscall.Exec` without a shell, and what makes it safe to preview here.
 *  2. **Five forbidden overrides.** `--host`/`--port` are the listener identity the gateway proxies
 *     to and the supervisor may reassign after an exit 78; `-m`/`--model` is the resolved path the
 *     models service maintains inside `config_hash`; `--api-key` would put a credential into argv,
 *     where `GET /instances/{id}/command` shows it and the journal records it.
 */

/** The five overrides `ParseExtraFlags` rejects with `422 extra_flag_forbidden`. */
export const FORBIDDEN_EXTRA_FLAGS = ['--host', '--port', '-m', '--model', '--api-key'] as const;

export interface SplitOk {
  ok: true;
  words: string[];
}

export interface SplitFailure {
  ok: false;
  /** The message the field shows. Worded like the Go error, because it is the same rule. */
  error: string;
}

export type SplitResult = SplitOk | SplitFailure;

/**
 * Split a string into words the way a POSIX shell would, and do nothing else a shell would do.
 *
 * Ported line for line from `instances.SplitWords`: unquoted whitespace separates; single quotes
 * are literal; double quotes are literal except a backslash before `"`, `\` or a newline; a bare
 * backslash escapes the next character; an unterminated quote or a trailing backslash is an error.
 */
export function splitWords(input: string): SplitResult {
  const runes = [...input];
  const words: string[] = [];
  let current = '';
  let started = false;

  const flush = () => {
    if (started) {
      words.push(current);
      current = '';
      started = false;
    }
  };

  for (let i = 0; i < runes.length; i += 1) {
    const c = runes[i] as string;

    if (c === ' ' || c === '\t' || c === '\n' || c === '\r') {
      flush();
      continue;
    }

    if (c === "'") {
      started = true;
      let j = i + 1;
      while (j < runes.length && runes[j] !== "'") {
        current += runes[j];
        j += 1;
      }
      if (j >= runes.length) return { ok: false, error: 'unterminated single quote' };
      i = j;
      continue;
    }

    if (c === '"') {
      started = true;
      let j = i + 1;
      while (j < runes.length && runes[j] !== '"') {
        if (runes[j] === '\\' && j + 1 < runes.length) {
          const next = runes[j + 1] as string;
          if (next === '"' || next === '\\' || next === '\n') {
            current += next;
            j += 2;
            continue;
          }
        }
        current += runes[j];
        j += 1;
      }
      if (j >= runes.length) return { ok: false, error: 'unterminated double quote' };
      i = j;
      continue;
    }

    if (c === '\\') {
      if (i + 1 >= runes.length) return { ok: false, error: 'trailing backslash escapes nothing' };
      started = true;
      i += 1;
      current += runes[i];
      continue;
    }

    started = true;
    current += c;
  }

  flush();
  return { ok: true, words };
}

/**
 * The flag name of one argv word, if it is a flag: `--jinja` is `--jinja`, `--port=8080` is
 * `--port`, and neither `8080` nor `-0.5` is one. Mirrors `instances.flagName`, including the
 * "first character after the dashes must be a letter" rule that keeps a negative number from
 * being read as a flag.
 */
export function flagName(word: string): string | null {
  if (word.length < 2 || !word.startsWith('-')) return null;
  const rest = word.replace(/^-+/, '');
  const first = rest.charAt(0);
  if (!/[A-Za-z]/.test(first)) return null;
  const eq = word.indexOf('=');
  return eq === -1 ? word : word.slice(0, eq);
}

export interface ExtraFlagsResult {
  words: string[];
  /** A refusal: an unparseable string, or one of the five overrides. Empty when the value is fine. */
  error?: string;
  /** The forbidden flag, when that is what went wrong — the field highlights it. */
  forbidden?: string;
}

/**
 * The save-time check, whole: split, then refuse the five overrides. The message is the one the
 * daemon sends back with `422 extra_flag_forbidden`, so the field and the toast agree.
 */
export function parseExtraFlags(input: string): ExtraFlagsResult {
  const split = splitWords(input);
  if (!split.ok) return { words: [], error: split.error };

  for (const word of split.words) {
    const name = flagName(word);
    if (name === null) continue;
    for (const bad of FORBIDDEN_EXTRA_FLAGS) {
      if (name !== bad) continue;
      return {
        words: [],
        forbidden: bad,
        error:
          `extra_flags may not override ${bad}: it is rendered by Llama Man and changing it ` +
          'would break the gateway, the model resolution or the start ledger',
      };
    }
  }
  return { words: split.words };
}

/** Flags this design already models as fields, spelled as they appear in argv (section 5.7). */
const MODELED_FLAGS = new Set([
  '-c',
  '-ngl',
  '-b',
  '-ub',
  '-np',
  '-t',
  '-tb',
  '-fa',
  '-ctk',
  '-ctv',
  '-sm',
  '-ts',
  '-mg',
  '--device',
  '--alias',
  '--jinja',
  '--chat-template',
  '--chat-template-file',
  '--embedding',
  '--pooling',
  '--reranking',
  '--mlock',
  '--no-mmap',
  '-cb',
  '-nocb',
  '--rope-scaling',
  '--rope-freq-base',
  '--rope-freq-scale',
  '--yarn-ext-factor',
  '--yarn-attn-factor',
  '--keep',
  '--predict',
  '--defrag-thold',
  '--cache-reuse',
  '--numa',
  '-C',
  '--prio',
  '--slot-save-path',
  '--verbosity',
  '--draft-max',
  '--draft-min',
  '--draft-p-min',
  '-cd',
  '-ngld',
  '--props',
  '--slots',
  '--metrics',
  '--mmproj',
  '-md',
  '--no-webui',
]);

/**
 * Flags in `extra_flags` that this form already has a field for.
 *
 * Not an error — the renderer appends `extra_flags` last, so it wins — but a warning worth having:
 * a value set in two places is a configuration whose meaning depends on argument order, which is
 * exactly the kind of surprise the escape hatch should not create quietly.
 */
export function duplicatedFlags(input: string): string[] {
  const split = splitWords(input);
  if (!split.ok) return [];
  const seen: string[] = [];
  for (const word of split.words) {
    const name = flagName(word);
    if (name && MODELED_FLAGS.has(name) && !seen.includes(name)) seen.push(name);
  }
  return seen;
}
