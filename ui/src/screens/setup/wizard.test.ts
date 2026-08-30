/**
 * The wizard's pure logic.
 *
 * Everything the setup steps decide on their own — how strong a password is, what an instance
 * should be called, what a plan's `missing_tools` means for one tool — lives in a module with no
 * React and no fetch in it, so it can be proved here rather than only in a browser. The steps
 * themselves are thin over these three functions plus the server's answers.
 */

import { describe, expect, it } from 'vitest';

import {
  MAX_PASSWORD_LENGTH,
  MIN_PASSWORD_LENGTH,
  characterClasses,
  scorePassword,
  strengthPercent,
} from './passwordStrength';
import {
  INSTANCE_NAME_PATTERN,
  isValidInstanceName,
  suggestInstanceName,
  uniqueInstanceName,
} from './instanceName';
import { TOOLCHAIN_TOOLS, packageHint, toolVerdict } from './toolchainGuidance';

describe('scorePassword', () => {
  it('refuses only what the daemon refuses', () => {
    // internal/auth.ValidatePassword: shorter than 8, or longer than 1024.
    expect(scorePassword('').acceptable).toBe(false);
    expect(scorePassword('short12').acceptable).toBe(false);
    expect(scorePassword('a'.repeat(MIN_PASSWORD_LENGTH)).acceptable).toBe(true);
    expect(scorePassword('x'.repeat(MAX_PASSWORD_LENGTH + 1)).acceptable).toBe(false);
  });

  it('names the floor rather than scoring below it', () => {
    const tooShort = scorePassword('abc');
    expect(tooShort.score).toBe(0);
    expect(tooShort.label).toContain(String(MIN_PASSWORD_LENGTH));
  });

  it('rewards length over punctuation', () => {
    const short = scorePassword('Aa1!xyzq');
    const long = scorePassword('correcthorsebatterystaple');
    expect(long.score).toBeGreaterThan(short.score);
  });

  it('caps anything predictable at weak, however long', () => {
    expect(scorePassword('passwordpasswordpassword').score).toBe(1);
    expect(scorePassword('llamaman-is-the-best-app').score).toBe(1);
    expect(scorePassword('aaaaaaaaaaaaaaaaaaaa').score).toBe(1);
    expect(scorePassword('abcdefghijklmnop').score).toBe(1);
  });

  it('reaches the top of the scale for a long, varied, unpredictable secret', () => {
    const strength = scorePassword('Tr0ubador&Weft-Quill9');
    expect(strength.score).toBe(4);
    expect(strength.suggestions).toHaveLength(0);
    expect(strengthPercent(strength)).toBe(100);
  });

  it('always suggests something when it is not at the top', () => {
    const strength = scorePassword('lowercaseonly');
    expect(strength.score).toBeLessThan(4);
    expect(strength.suggestions.length).toBeGreaterThan(0);
  });

  it('counts the four character classes', () => {
    expect(characterClasses('abc')).toBe(1);
    expect(characterClasses('abcABC')).toBe(2);
    expect(characterClasses('abcABC123')).toBe(3);
    expect(characterClasses('abcABC123!')).toBe(4);
  });
});

describe('suggestInstanceName', () => {
  it('produces a name that satisfies D11 by construction', () => {
    const cases: [string, string | null][] = [
      ['bartowski/Qwen3-8B-GGUF', 'Q4_K_M'],
      ['TheBloke/Mixtral-8x7B-Instruct-v0.1-GGUF', 'Q5_K_S'],
      ['unsloth/gemma-3-27b-it-GGUF', null],
      ['ggml-org/models', 'F16'],
      ['UPPERCASE.ONLY', 'IQ2_XXS'],
    ];
    for (const [repo, quant] of cases) {
      const name = suggestInstanceName(repo, quant);
      expect(INSTANCE_NAME_PATTERN.test(name), `${repo} -> ${name}`).toBe(true);
    }
  });

  it('drops the owner and the redundant GGUF suffix', () => {
    expect(suggestInstanceName('bartowski/Qwen3-8B-GGUF', 'Q4_K_M')).toBe('qwen3-8b-q4-k-m');
  });

  it('never exceeds the thirty-two character unit-name bound', () => {
    const name = suggestInstanceName(
      'someone/An-Extremely-Long-Model-Repository-Name-GGUF',
      'Q4_K_M',
    );
    expect(name.length).toBeLessThanOrEqual(32);
    expect(isValidInstanceName(name)).toBe(true);
  });

  it('falls back to a valid name when nothing usable survives', () => {
    expect(isValidInstanceName(suggestInstanceName('---', null))).toBe(true);
  });

  it('rejects what the server would reject', () => {
    expect(isValidInstanceName('-leading-hyphen')).toBe(false);
    expect(isValidInstanceName('Capitals')).toBe(false);
    expect(isValidInstanceName('under_score')).toBe(false);
    expect(isValidInstanceName('')).toBe(false);
    expect(isValidInstanceName('a'.repeat(33))).toBe(false);
    expect(isValidInstanceName('a'.repeat(32))).toBe(true);
  });
});

describe('uniqueInstanceName', () => {
  it('leaves a free name alone', () => {
    expect(uniqueInstanceName('qwen3-8b', ['other'])).toBe('qwen3-8b');
  });

  it('walks past the names already taken, staying inside the grammar', () => {
    const taken = ['qwen3-8b', 'qwen3-8b-2'];
    const name = uniqueInstanceName('qwen3-8b', taken);
    expect(name).toBe('qwen3-8b-3');
    expect(isValidInstanceName(name)).toBe(true);
  });

  it('keeps a maximum-length name valid when it has to add a suffix', () => {
    const long = 'a'.repeat(32);
    const name = uniqueInstanceName(long, [long]);
    expect(name).not.toBe(long);
    expect(isValidInstanceName(name)).toBe(true);
  });
});

describe('toolVerdict', () => {
  const gcc = TOOLCHAIN_TOOLS.find((tool) => tool.name === 'gcc');
  const nvcc = TOOLCHAIN_TOOLS.find((tool) => tool.name === 'nvcc');
  const ninja = TOOLCHAIN_TOOLS.find((tool) => tool.name === 'ninja');

  it('covers the closed vocabulary internal/toolchain publishes', () => {
    const names = TOOLCHAIN_TOOLS.map((tool) => tool.name);
    for (const name of [
      'gcc',
      'g++',
      'cmake',
      'ninja',
      'make',
      'git',
      'ccache',
      'nvcc',
      'driver',
    ]) {
      expect(names).toContain(name);
    }
  });

  it('reads a tool missing from the CPU plan as blocking every source build', () => {
    expect(toolVerdict(gcc!, ['gcc'], ['gcc', 'nvcc'])).toBe('blocking');
  });

  it('reads a tool missing only from the CUDA plan as a CUDA-only gap', () => {
    expect(toolVerdict(nvcc!, [], ['nvcc'])).toBe('cuda-only');
  });

  it('reads a tool in neither list as present, once both plans have answered', () => {
    expect(toolVerdict(gcc!, [], [])).toBe('present');
    expect(toolVerdict(nvcc!, [], [])).toBe('present');
  });

  it('never claims anything about an optional tool, which never appears in either list', () => {
    expect(toolVerdict(ninja!, [], [])).toBe('unreported');
    expect(toolVerdict(ninja!, ['gcc'], ['gcc'])).toBe('unreported');
  });

  it('says nothing before a plan has answered', () => {
    expect(toolVerdict(gcc!, undefined, undefined)).toBe('unreported');
    expect(toolVerdict(nvcc!, [], undefined)).toBe('unreported');
  });
});

describe('packageHint', () => {
  it('names one package for a known family', () => {
    const cmake = TOOLCHAIN_TOOLS.find((tool) => tool.name === 'cmake')!;
    expect(packageHint(cmake, 'gentoo')).toBe('Package: dev-build/cmake');
  });

  it('lists every family when none is chosen', () => {
    const gxx = TOOLCHAIN_TOOLS.find((tool) => tool.name === 'g++')!;
    const hint = packageHint(gxx, 'all') ?? '';
    expect(hint).toContain('debian: build-essential');
    expect(hint).toContain('fedora: gcc-c++');
    expect(hint).toContain('alpine: build-base');
  });

  it('is honest when a family has no single package for a tool', () => {
    const nvcc = TOOLCHAIN_TOOLS.find((tool) => tool.name === 'nvcc')!;
    expect(packageHint(nvcc, 'alpine')).toContain('No single package');
  });

  it('has nothing to say for a tool no package carries', () => {
    const driver = TOOLCHAIN_TOOLS.find((tool) => tool.name === 'driver')!;
    expect(packageHint(driver, 'debian')).toBeNull();
  });

  it('never suggests a command to run as root (section 6.5)', () => {
    for (const tool of TOOLCHAIN_TOOLS) {
      const text = [packageHint(tool, 'all'), tool.purpose].filter(Boolean).join(' ');
      expect(text).not.toMatch(/\b(sudo|apt|dnf|pacman|zypper|apk|emerge)\b/);
    }
  });
});
