/**
 * The strength meter of DESIGN section 11.2's `password` row.
 *
 * The daemon's rules are deliberately thin — `internal/auth`'s `ValidatePassword` refuses only what
 * is unusable, "an empty or trivially short password, and one long enough to be a denial of service
 * against the hasher", because "SPEC §4 asks for argon2id and a lockout, not for a composition
 * policy, and §11.2 puts a strength METER in front of the user rather than a rejection".
 *
 * So this module *advises* and never blocks: the only thing it can say `no` to is the same
 * eight-character floor the server enforces, and everything else it says is a suggestion the user
 * is free to ignore. It is pure so the advice can be tested without a browser.
 */

import type { Tone } from '../../components';

/** `internal/auth.MinPasswordLen`. Kept in step with the server, whose 400 carries `min_length`. */
export const MIN_PASSWORD_LENGTH = 8;

/** `internal/auth.MaxPasswordLen` — the bound on work an unauthenticated caller can ask for. */
export const MAX_PASSWORD_LENGTH = 1024;

/** The length at which the meter stops asking for more characters. */
const COMFORTABLE_LENGTH = 16;

export interface PasswordStrength {
  /** 0 (unusable) to 4 (strong). The meter's fill is `score / 4`. */
  score: 0 | 1 | 2 | 3 | 4;
  label: string;
  tone: Tone;
  /** What would make it stronger, most useful first. Empty at the top of the scale. */
  suggestions: string[];
  /** Whether the daemon would accept it at all. The submit button reads this, not `score`. */
  acceptable: boolean;
}

/**
 * Substrings that make a password guessable no matter how long it is. Short on purpose: a real
 * dictionary is megabytes and belongs nowhere near a management UI's bundle, while this catches the
 * handful of strings someone actually types into a box labeled "admin password".
 */
const PREDICTABLE = [
  'password',
  'llamaman',
  'llama',
  'admin',
  'root',
  'changeme',
  'letmein',
  'welcome',
  'qwerty',
  'secret',
  '123456',
];

/** Four or more of the same character in a row: `aaaa`, `!!!!`. */
const RUN_OF_ONE = /(.)\1{3,}/;

/** Four or more consecutive code points ascending or descending: `abcd`, `4321`. */
function hasSequentialRun(value: string, length = 4): boolean {
  let ascending = 1;
  let descending = 1;
  for (let i = 1; i < value.length; i += 1) {
    const delta = value.charCodeAt(i) - value.charCodeAt(i - 1);
    ascending = delta === 1 ? ascending + 1 : 1;
    descending = delta === -1 ? descending + 1 : 1;
    if (ascending >= length || descending >= length) return true;
  }
  return false;
}

/** How many of the four character classes appear. */
export function characterClasses(password: string): number {
  let count = 0;
  if (/[a-z]/.test(password)) count += 1;
  if (/[A-Z]/.test(password)) count += 1;
  if (/[0-9]/.test(password)) count += 1;
  if (/[^A-Za-z0-9]/.test(password)) count += 1;
  return count;
}

const LABELS: Record<PasswordStrength['score'], { label: string; tone: Tone }> = {
  0: { label: 'Too short', tone: 'danger' },
  1: { label: 'Weak', tone: 'danger' },
  2: { label: 'Fair', tone: 'warn' },
  3: { label: 'Good', tone: 'accent' },
  4: { label: 'Strong', tone: 'ok' },
};

function clamp(score: number): PasswordStrength['score'] {
  return Math.max(0, Math.min(4, score)) as PasswordStrength['score'];
}

/**
 * Score a candidate password.
 *
 * Length carries most of the weight, because for a hash this expensive it genuinely is what matters:
 * a sixteen-character passphrase beats an eight-character one with a symbol bolted on. Variety adds
 * one point. Anything predictable is capped at "weak" however long it is, so a very long
 * `passwordpasswordpassword` cannot climb the scale.
 */
export function scorePassword(password: string): PasswordStrength {
  const acceptable =
    password.length >= MIN_PASSWORD_LENGTH && password.length <= MAX_PASSWORD_LENGTH;
  const suggestions: string[] = [];

  if (password.length === 0) {
    return {
      score: 0,
      ...LABELS[0],
      label: 'Enter a password',
      suggestions: [],
      acceptable: false,
    };
  }
  if (!acceptable) {
    const label =
      password.length > MAX_PASSWORD_LENGTH
        ? `Longer than the ${MAX_PASSWORD_LENGTH}-character limit`
        : `At least ${MIN_PASSWORD_LENGTH} characters`;
    return { score: 0, tone: 'danger', label, suggestions: [], acceptable: false };
  }

  let score = 1;
  if (password.length >= 12) score += 1;
  if (password.length >= COMFORTABLE_LENGTH) score += 1;

  const classes = characterClasses(password);
  if (classes >= 3) score += 1;

  const lower = password.toLowerCase();
  const predictable =
    PREDICTABLE.some((word) => lower.includes(word)) ||
    RUN_OF_ONE.test(password) ||
    hasSequentialRun(password);
  if (predictable) score = Math.min(score, 1);

  if (password.length < COMFORTABLE_LENGTH) {
    suggestions.push(
      `Length helps most — ${COMFORTABLE_LENGTH} characters or more is comfortable.`,
    );
  }
  if (classes < 3) {
    suggestions.push('Mixing upper case, digits or punctuation adds another class of guesses.');
  }
  if (predictable) {
    suggestions.push('This contains a run or a word an attacker would try first.');
  }

  const scored = clamp(score);
  return { score: scored, ...LABELS[scored], suggestions, acceptable };
}

/** The meter's fill, 0–100. */
export function strengthPercent(strength: PasswordStrength): number {
  return (strength.score / 4) * 100;
}
