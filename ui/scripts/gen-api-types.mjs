#!/usr/bin/env node
/**
 * Emit ui/src/api/schema.d.ts from api/openapi.json.
 *
 * WHY THIS IS NOT `openapi-typescript`
 * ------------------------------------
 * DESIGN section 4 and section 14 name `openapi-typescript` as the generator, and it IS a declared
 * devDependency of this package. It cannot run in this tree today: it drives the TypeScript
 * *compiler API* (`ts.factory`) and declares `typescript: ^5.x` as a peer, while the latest-stable
 * directive (D45) puts TypeScript 7 in node_modules — whose JS entry point exports `version` and
 * `versionMajorMinor` and nothing else, because the TS 7 compiler is a native binary. Running it
 * fails with `Cannot read properties of undefined (reading 'createKeywordTypeNode')`.
 *
 * This is the same upstream gap ui/eslint.config.js and ui/.npmrc already record for
 * typescript-eslint, and it gets the same treatment: the dependency stays declared, the block is
 * written down, and the job it does is done by ~250 lines of stdlib Node in the meantime rather
 * than by pinning a second TypeScript.
 *
 * The emitted shape is deliberately openapi-typescript's own — `paths`, `components`, `operations`,
 * with `components["schemas"][…]` references and per-status `responses` — so the day the peer range
 * admits TypeScript 7 this file is deleted, `npm run gen:api` becomes the real tool, and no import
 * in ui/src changes.
 *
 * Usage:
 *   node scripts/gen-api-types.mjs           # write ui/src/api/schema.d.ts
 *   node scripts/gen-api-types.mjs --check   # exit 1 if the committed file is stale (CI drift gate)
 */

import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const SPEC = resolve(here, '../../api/openapi.json');
const OUT = resolve(here, '../src/api/schema.d.ts');

const HTTP_METHODS = ['get', 'put', 'post', 'delete', 'options', 'head', 'patch', 'trace'];

/** A TS object key needs quoting unless it is a plain identifier. */
function key(name) {
  return /^[A-Za-z_$][A-Za-z0-9_$]*$/.test(name) ? name : JSON.stringify(name);
}

function indent(depth) {
  return '    '.repeat(depth);
}

/** Wrap a description as a JSDoc comment, or return '' when there is nothing to say. */
function docComment(schema, depth) {
  const text = schema && typeof schema === 'object' ? schema.description : undefined;
  if (!text) return '';
  const pad = indent(depth);
  const lines = String(text).split('\n');
  if (lines.length === 1) return `${pad}/** ${lines[0].replace(/\*\//g, '*\\/')} */\n`;
  return (
    `${pad}/**\n` +
    lines.map((l) => `${pad} *${l ? ` ${l.replace(/\*\//g, '*\\/')}` : ''}`).join('\n') +
    `\n${pad} */\n`
  );
}

/** `#/components/schemas/Foo` -> `components["schemas"]["Foo"]`. */
function refToType(ref) {
  const parts = ref.replace(/^#\//, '').split('/');
  if (parts.length < 2) throw new Error(`unsupported $ref: ${ref}`);
  const [root, ...rest] = parts;
  return `${root}[${rest.map((p) => JSON.stringify(decodeURIComponent(p.replace(/~1/g, '/').replace(/~0/g, '~')))).join('][')}]`;
}

function scalarType(t, schema) {
  switch (t) {
    case 'string':
      // Enumerated strings become a literal union — the closed error-code and state enums of
      // DESIGN section 2.8 are worth having in the type system.
      if (Array.isArray(schema.enum) && schema.enum.length > 0) {
        return schema.enum.map((v) => JSON.stringify(v)).join(' | ');
      }
      return 'string';
    case 'integer':
    case 'number':
      if (Array.isArray(schema.enum) && schema.enum.length > 0) {
        return schema.enum.map((v) => JSON.stringify(v)).join(' | ');
      }
      return 'number';
    case 'boolean':
      return 'boolean';
    case 'null':
      return 'null';
    case 'array':
      return null; // handled by the caller
    case 'object':
      return null; // handled by the caller
    default:
      return 'unknown';
  }
}

/**
 * Render one JSON Schema node as a TypeScript type.
 *
 * The vocabulary handled is exactly what internal/api/openapi emits: $ref, type (string or array
 * of strings, including the `["string","null"]` nullable form Go's encoder produces for pointers),
 * properties, required, items, additionalProperties, anyOf, enum and format. Anything else is a
 * generator bug rather than something to guess at, so it throws.
 */
function renderSchema(schema, depth) {
  if (schema === true || schema === undefined) return 'unknown';
  if (schema === false) return 'never';
  if (typeof schema !== 'object') throw new Error(`unexpected schema node: ${String(schema)}`);

  if (schema.$ref) return refToType(schema.$ref);

  if (Array.isArray(schema.anyOf)) {
    return schema.anyOf.map((s) => renderSchema(s, depth)).join(' | ');
  }
  if (Array.isArray(schema.oneOf)) {
    return schema.oneOf.map((s) => renderSchema(s, depth)).join(' | ');
  }
  if (Array.isArray(schema.allOf)) {
    return schema.allOf.map((s) => renderSchema(s, depth)).join(' & ');
  }

  const types = Array.isArray(schema.type) ? schema.type : schema.type ? [schema.type] : [];

  if (types.length === 0) {
    if (schema.properties) return renderObject(schema, depth);
    if (schema.enum) return schema.enum.map((v) => JSON.stringify(v)).join(' | ');
    return 'unknown';
  }

  const parts = types.map((t) => {
    if (t === 'array') {
      const inner = renderSchema(schema.items ?? true, depth);
      return /[ |&]/.test(inner) ? `(${inner})[]` : `${inner}[]`;
    }
    if (t === 'object') return renderObject(schema, depth);
    return scalarType(t, schema);
  });

  return parts.join(' | ');
}

function renderObject(schema, depth) {
  const props = schema.properties ?? {};
  const names = Object.keys(props).sort();
  const required = new Set(schema.required ?? []);

  if (names.length === 0) {
    // A bare `object` with no properties is a free-form JSON bag — job progress, event details,
    // GGUF metadata. `Record<string, unknown>` keeps it honest without pretending to know more.
    if (schema.additionalProperties && typeof schema.additionalProperties === 'object') {
      return `Record<string, ${renderSchema(schema.additionalProperties, depth)}>`;
    }
    return 'Record<string, unknown>';
  }

  const pad = indent(depth + 1);
  const body = names
    .map((name) => {
      const child = props[name];
      const opt = required.has(name) ? '' : '?';
      return `${docComment(child, depth + 1)}${pad}${key(name)}${opt}: ${renderSchema(child, depth + 1)};`;
    })
    .join('\n');
  return `{\n${body}\n${indent(depth)}}`;
}

/** Group an operation's parameters by `in`, as openapi-typescript does. */
function renderParameters(op, depth) {
  const groups = { query: [], header: [], path: [], cookie: [] };
  for (const p of op.parameters ?? []) {
    if (!groups[p.in]) throw new Error(`unsupported parameter location: ${p.in}`);
    groups[p.in].push(p);
  }
  const pad = indent(depth + 1);
  const lines = [];
  for (const loc of ['query', 'header', 'path', 'cookie']) {
    const list = groups[loc].slice().sort((a, b) => a.name.localeCompare(b.name));
    if (list.length === 0) {
      lines.push(`${pad}${loc}?: never;`);
      continue;
    }
    const anyRequired = list.some((p) => p.required);
    const inner = list
      .map((p) => {
        const opt = p.required ? '' : '?';
        return `${docComment(p, depth + 2)}${indent(depth + 2)}${key(p.name)}${opt}: ${renderSchema(p.schema ?? true, depth + 2)};`;
      })
      .join('\n');
    lines.push(
      `${pad}${loc}${anyRequired ? '' : '?'}: {\n${inner}\n${pad}};`.replace(/\n\n/g, '\n'),
    );
  }
  return `{\n${lines.join('\n')}\n${indent(depth)}}`;
}

function renderContent(content, depth) {
  const mediaTypes = Object.keys(content ?? {}).sort();
  if (mediaTypes.length === 0) return null;
  const pad = indent(depth + 1);
  const body = mediaTypes
    .map((mt) => `${pad}${key(mt)}: ${renderSchema(content[mt].schema ?? true, depth + 1)};`)
    .join('\n');
  return `{\n${body}\n${indent(depth)}}`;
}

function renderResponses(op, depth) {
  const codes = Object.keys(op.responses ?? {}).sort();
  const pad = indent(depth + 1);
  const body = codes
    .map((code) => {
      const res = op.responses[code];
      const content = renderContent(res.content, depth + 2);
      const errorCodes = res['x-error-codes'];
      const doc = docComment(
        {
          description:
            (res.description ?? '') +
            (errorCodes ? `\n\nError codes: ${errorCodes.join(', ')}` : ''),
        },
        depth + 1,
      );
      const inner =
        content === null
          ? `{\n${indent(depth + 2)}content?: never;\n${pad}}`
          : `{\n${indent(depth + 2)}content: ${content};\n${pad}}`;
      return `${doc}${pad}${key(code)}: ${inner};`;
    })
    .join('\n');
  return `{\n${body}\n${indent(depth)}}`;
}

function renderOperation(op, depth) {
  const pad = indent(depth + 1);
  const lines = [];
  lines.push(`${pad}parameters: ${renderParameters(op, depth + 1)};`);

  const rb = op.requestBody;
  if (rb) {
    const content = renderContent(rb.content, depth + 1);
    const opt = rb.required ? '' : '?';
    lines.push(`${pad}requestBody${opt}: {\n${indent(depth + 2)}content: ${content};\n${pad}};`);
  } else {
    lines.push(`${pad}requestBody?: never;`);
  }
  lines.push(`${pad}responses: ${renderResponses(op, depth + 1)};`);
  return `{\n${lines.join('\n')}\n${indent(depth)}}`;
}

function generate(doc) {
  const out = [];
  out.push('/**');
  out.push(' * GENERATED FILE — DO NOT EDIT.');
  out.push(' *');
  out.push(` * ${doc.info?.title ?? 'API'} v${doc.info?.version ?? '?'}, from api/openapi.json.`);
  out.push(
    ' * Regenerate with `npm run gen:api`; CI runs `npm run gen:api:check` and fails on drift',
  );
  out.push(' * (DESIGN section 3, D43 — the types can never lie about the API).');
  out.push(' */');
  out.push('');
  out.push('/* eslint-disable */');
  out.push('');

  // ---- paths -------------------------------------------------------------
  out.push('export interface paths {');
  for (const p of Object.keys(doc.paths ?? {}).sort()) {
    const item = doc.paths[p];
    out.push(`    ${key(p)}: {`);
    for (const m of HTTP_METHODS) {
      if (!item[m]) {
        out.push(`        ${m}?: never;`);
        continue;
      }
      const opId = item[m].operationId;
      if (!opId) throw new Error(`${m.toUpperCase()} ${p} has no operationId`);
      const summary = item[m].summary;
      if (summary) out.push(`        /** ${summary.replace(/\*\//g, '*\\/')} */`);
      out.push(`        ${m}: operations[${JSON.stringify(opId)}];`);
    }
    out.push('    };');
  }
  out.push('}');
  out.push('');

  // ---- components --------------------------------------------------------
  out.push('export interface components {');
  out.push('    schemas: {');
  for (const name of Object.keys(doc.components?.schemas ?? {}).sort()) {
    const s = doc.components.schemas[name];
    const doccomment = docComment(s, 2);
    out.push(`${doccomment}        ${key(name)}: ${renderSchema(s, 2)};`.replace(/\n$/, ''));
  }
  out.push('    };');
  out.push('    responses: never;');
  out.push('    parameters: never;');
  out.push('    requestBodies: never;');
  out.push('    headers: never;');
  out.push('    pathItems: never;');
  out.push('}');
  out.push('');

  // ---- operations --------------------------------------------------------
  out.push('export interface operations {');
  const ops = [];
  for (const [p, item] of Object.entries(doc.paths ?? {})) {
    for (const m of HTTP_METHODS) {
      if (item[m]) ops.push([item[m].operationId, item[m], `${m.toUpperCase()} ${p}`]);
    }
  }
  ops.sort((a, b) => a[0].localeCompare(b[0]));
  const seen = new Set();
  for (const [id, op, where] of ops) {
    if (seen.has(id)) throw new Error(`duplicate operationId ${id}`);
    seen.add(id);
    out.push(`    /** ${where}${op.summary ? ` — ${op.summary.replace(/\*\//g, '*\\/')}` : ''} */`);
    out.push(`    ${key(id)}: ${renderOperation(op, 1)};`);
  }
  out.push('}');
  out.push('');

  return out.join('\n');
}

const doc = JSON.parse(readFileSync(SPEC, 'utf8'));
const generated = generate(doc);

if (process.argv.includes('--check')) {
  let current = '';
  try {
    current = readFileSync(OUT, 'utf8');
  } catch {
    console.error(`api types: ${OUT} does not exist — run \`npm run gen:api\``);
    process.exit(1);
  }
  if (current !== generated) {
    console.error(
      'api types: src/api/schema.d.ts is stale against api/openapi.json — run `npm run gen:api` and commit the result',
    );
    process.exit(1);
  }
  console.log('api types: src/api/schema.d.ts is up to date');
} else {
  writeFileSync(OUT, generated);
  console.log(`api types: wrote ${OUT}`);
}
