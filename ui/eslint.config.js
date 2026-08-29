import js from '@eslint/js';

// TODO(DESIGN section 14): typescript-eslint is a declared dependency and is
// installed, but its current stable release refuses to load against TypeScript
// 7 ("typescript-eslint does not support TS 7.0"). Since the latest-stable
// directive (D45) governs the TypeScript version, the TS rule set waits for
// upstream rather than the compiler waiting for the linter. When
// typescript-eslint ships TS 7 support: drop the `src` ignore below, restore
//
//   ...tseslint.configs.recommended
//
// and delete ui/.npmrc's legacy-peer-deps line.
//
// Type errors are not going unchecked in the meantime: `npm run typecheck`
// runs `tsc -b` in strict mode and is a CI job of its own.
export default [
  { ignores: ['dist', 'node_modules', 'src'] },
  js.configs.recommended,
  {
    files: ['**/*.js'],
    languageOptions: { ecmaVersion: 2022, sourceType: 'module' },
  },
];
