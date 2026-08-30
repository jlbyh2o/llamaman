/**
 * The one Node global this suite reads.
 *
 * `@types/node` is not a dependency of this project and adding one for four `process.env` lookups
 * would be a dependency added for a type. The shim is deliberately minimal — the environment, and
 * nothing else — so a future `@types/node` would supersede it rather than fight it.
 */
declare const process: {
  readonly env: Readonly<Record<string, string | undefined>>;
};
