/**
 * "What went wrong, and what to do about it."
 *
 * DESIGN section 4, screen 6 ends with "the remediation card for the last exit code (section 17)",
 * and section 17's own opening line is the standard this table is held to: "Every row is a designed
 * behavior with a UI remediation card, and every row has a test." The launcher's exit codes
 * (section 5.6) and the inhibit reasons (section 2.8) are the two vocabularies a stopped instance
 * speaks in, so both are answered here.
 *
 * A row that offers an action names it; the screen decides whether that action is reachable on this
 * host — under F10 every control answers `409 systemd_unavailable`, and a card that offered a button
 * which cannot work would be worse than one that explains.
 */

export type RemediationAction =
  'start' | 'safe-start' | 'reset-failed' | 'edit' | 'models' | 'llamacpp';

export interface Remediation {
  title: string;
  detail: string;
  /** The one next step, when there is one. */
  action?: { label: string; kind: RemediationAction };
  tone: 'warn' | 'danger';
}

/** `instance_starts.error_code` (section 5.6's table) → the card. */
const BY_ERROR_CODE: Record<string, Remediation> = {
  instance_missing: {
    title: 'The instance row was gone when the unit started',
    detail:
      'The launcher loaded the row, found it deleted and exited 64 without launching anything. ' +
      'That is the safety net for a unit that is still enabled for an instance that no longer exists.',
    tone: 'warn',
  },
  bad_flags: {
    title: 'The saved flags are not ones this build accepts',
    detail:
      'The launcher could not parse the stored configuration (exit 65). Open the configuration and ' +
      'fix the flag it named; the same check runs at save time, so a flag that was legal when it ' +
      'was saved may have been dropped by a newer llama.cpp.',
    action: { label: 'Edit configuration', kind: 'edit' },
    tone: 'danger',
  },
  draft_vocab_mismatch: {
    title: 'The draft model does not share the primary model’s vocabulary',
    detail:
      'Speculative decoding would produce garbage output, so the launcher refuses to start (exit 65). ' +
      'Detach the draft model, or pick one built from the same tokenizer.',
    action: { label: 'Edit configuration', kind: 'edit' },
    tone: 'danger',
  },
  runtime_missing: {
    title: 'The active llama.cpp build is missing or unusable',
    detail:
      'Exit 69: the binary the symlink points at could not be executed. Rolling back to the ' +
      'retained previous build is one click, and a rebuild is the other.',
    action: { label: 'Open llama.cpp', kind: 'llamacpp' },
    tone: 'danger',
  },
  runtime_rebuilding: {
    title: 'The active build is being rebuilt in place',
    detail:
      'Exit 69 while the version row is not ready. Nothing to do: the supervisor starts this ' +
      'instance on its own once the rebuild finishes.',
    tone: 'warn',
  },
  launcher_db_unavailable: {
    title: 'The launcher could not reach the database',
    detail:
      'Exit 70. The daemon writes this ledger row itself when the launcher cannot. Collect a ' +
      'diagnostics bundle from the System screen before restarting.',
    tone: 'danger',
  },
  model_missing: {
    title: 'The model file is gone',
    detail:
      'Exit 72: the resolved GGUF path no longer exists — it was deleted, or its cache root was ' +
      'detached. Re-download it from the recorded repository and revision, or point the instance at ' +
      'another copy.',
    action: { label: 'Open models', kind: 'models' },
    tone: 'danger',
  },
  schema_mismatch: {
    title: 'The daemon has not finished upgrading the database',
    detail:
      'Exit 75: the launcher saw a schema it does not understand. This clears itself — the ' +
      'supervisor starts the instance once the daemon is up on the new schema.',
    action: { label: 'Start', kind: 'start' },
    tone: 'warn',
  },
  schema_ahead: {
    title: 'The database is newer than this launcher',
    detail:
      'Exit 75 with the schema ahead of the binary: something downgraded the daemon under a ' +
      'migrated database. Restore the binary, or restore the database snapshot from before the ' +
      'upgrade — llamaman restore-db is the deliberate path.',
    tone: 'danger',
  },
  port_conflict: {
    title: 'The internal port was already bound',
    detail:
      'Exit 78. The supervisor reassigns an internal port from the 21000–21999 pool before the next ' +
      'start, so this usually resolves itself; the start history shows both ports on the failed ' +
      'attempt and the new one on the next row.',
    tone: 'warn',
  },
};

/** The exit codes of section 5.6, for a row whose `error_code` did not survive. */
const BY_EXIT_CODE: Record<number, string> = {
  64: 'instance_missing',
  65: 'bad_flags',
  69: 'runtime_missing',
  70: 'launcher_db_unavailable',
  72: 'model_missing',
  75: 'schema_mismatch',
  78: 'port_conflict',
};

/** The three refusals `inhibited` carries (section 2.8). Not failures — decisions. */
const BY_INHIBIT_REASON: Record<string, Remediation> = {
  policy_never: {
    title: 'Restarts are turned off for this instance',
    detail:
      'The restart policy is "never", so nothing will bring it back on its own. Start it by hand, ' +
      'or change the policy.',
    action: { label: 'Start', kind: 'start' },
    tone: 'warn',
  },
  crash_loop: {
    title: 'Crash looping — the supervisor has stopped restarting it',
    detail:
      'More failed starts than the restart budget allows inside the restart window. Safe start runs ' +
      'once with -ngl 0 and a 2048-token context to separate a GPU problem from a model problem, and ' +
      'is never persisted. Reset failed clears the latch and starts the window over.',
    action: { label: 'Safe start', kind: 'safe-start' },
    tone: 'danger',
  },
  clean_exit: {
    title: 'llama-server exited cleanly, so it was not restarted',
    detail:
      'The policy is "on failure" and the last run ended without an error. That is the promise the ' +
      'policy makes; starting it again is a decision only you can make.',
    action: { label: 'Start', kind: 'start' },
    tone: 'warn',
  },
};

export interface RemediationInput {
  errorCode?: string | null;
  exitCode?: number | null;
  inhibited?: boolean;
  inhibitReason?: string | null;
  lastError?: string | null;
}

/** The card for a stopped, failed or inhibited instance, or null when there is nothing to say. */
export function remediationFor(input: RemediationInput): Remediation | null {
  if (input.inhibited && input.inhibitReason) {
    const inhibit = BY_INHIBIT_REASON[input.inhibitReason];
    if (inhibit) return inhibit;
  }

  const code =
    (input.errorCode && BY_ERROR_CODE[input.errorCode] ? input.errorCode : null) ??
    (input.exitCode !== null && input.exitCode !== undefined
      ? (BY_EXIT_CODE[input.exitCode] ?? null)
      : null);
  if (code) {
    const known = BY_ERROR_CODE[code];
    if (known) return known;
  }

  if (input.exitCode !== null && input.exitCode !== undefined && input.exitCode !== 0) {
    return {
      title: `llama-server exited ${input.exitCode}`,
      detail:
        input.lastError?.trim() ||
        'The journal for this unit has the detail. Open the logs pane below; the last lines before ' +
          'the exit are usually the whole story.',
      tone: 'danger',
    };
  }
  return null;
}

/**
 * The F1 hint: an out-of-memory load is the one failure whose remedy the fit calculator can compute.
 * Matched on the journal text the launcher leaves behind, exactly as section 17's detection column
 * describes.
 */
export function looksLikeVramOom(lines: readonly string[]): boolean {
  return lines.some((line) =>
    /cudaMalloc failed|out of memory|CUDA error: out of memory/i.test(line),
  );
}
