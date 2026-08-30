/**
 * The kit renders, in both themes, without ever naming a color.
 *
 * Rendering happens through `react-dom/server`, which needs no DOM environment (see
 * vitest.config.ts for why that is the deliberate choice rather than a limitation). What it proves
 * is the thing that actually matters for theming: the component tree is *identical* under
 * `data-theme="dark"` and `data-theme="light"`, because every color in this design system is a CSS
 * custom property resolved by the browser — not a value a component branches on. A component that
 * reached for a literal color, or for a JS-side palette lookup, would break one of these
 * assertions.
 */

import { renderToStaticMarkup } from 'react-dom/server';
import type { ReactElement } from 'react';
import { describe, expect, it } from 'vitest';

import { Badge } from './Badge';
import { Button } from './Button';
import { DataTable } from './DataTable';
import type { Column } from './DataTable';
import { EmptyState } from './EmptyState';
import { FieldGroup, FormField } from './FormField';
import { Input } from './Input';
import { Field, Mono, Panel, PanelHeader } from './Panel';
import { Meter, Progress } from './Progress';
import { Skeleton, Spinner } from './Spinner';
import { FlagBadge, STATE_MAPS, StatusBadge, stateStyle } from './StatusBadge';
import { Switch } from './Switch';
import { classifyLine, stripAnsi } from './LogViewer';
import { INSTANCE_STATES } from '../api/types';

function renderIn(theme: 'dark' | 'light', node: ReactElement): string {
  return renderToStaticMarkup(<div data-theme={theme}>{node}</div>);
}

/** Colors that are not tokens: a literal hex, rgb()/hsl() outside a var(), or a Tailwind palette class. */
const LITERAL_COLOR = /#[0-9a-f]{3,8}\b|(?<!var\([^)]*)\b(?:rgb|hsl)a?\(/i;
const TAILWIND_PALETTE_CLASS =
  /\b(?:bg|text|border|ring|fill)-(?:slate|gray|zinc|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose)-\d{2,3}\b/;

interface Row {
  id: string;
  name: string;
  port: number;
}

const rows: Row[] = [
  { id: 'a', name: 'qwen', port: 8081 },
  { id: 'b', name: 'gemma', port: 8082 },
];

const columns: Column<Row>[] = [
  { id: 'name', header: 'Name', cell: (row) => row.name, sortValue: (row) => row.name },
  { id: 'port', header: 'Port', cell: (row) => row.port, sortValue: (row) => row.port, mono: true },
];

const SPECIMENS: [string, ReactElement][] = [
  ['Button', <Button variant="primary">Start instance</Button>],
  ['Button (loading)', <Button loading>Working</Button>],
  [
    'Badge',
    <Badge tone="ok" dot>
      Ready
    </Badge>,
  ],
  ['StatusBadge', <StatusBadge kind="instance" state="crash-looping" />],
  ['FlagBadge', <FlagBadge flag="restart_required" />],
  [
    'Panel',
    <Panel>
      <PanelHeader title="Instance" description="qwen3-8b" />
      <Field label="Public port" mono>
        8081
      </Field>
      <Mono>sha256:0a1b2c3d</Mono>
    </Panel>,
  ],
  [
    'EmptyState',
    <EmptyState title="No instances yet" description="Create one to start serving." />,
  ],
  ['Progress', <Progress value={42} label="Downloading" detail="1.2 GiB of 4.9 GiB" />],
  ['Progress (indeterminate)', <Progress value={null} label="Verifying" />],
  ['Meter', <Meter used={7} total={24} label="VRAM" detail="7 of 24 GiB" />],
  ['Switch', <Switch checked aria-label="Autostart" />],
  ['Spinner', <Spinner />],
  ['Skeleton', <Skeleton className="w-24" />],
  [
    'DataTable',
    <DataTable columns={columns} rows={rows} rowKey={(row) => row.id} caption="Instances" />,
  ],
  [
    'FormField',
    <FieldGroup title="Model &amp; context">
      <FormField label="Context size" flag="--ctx-size" hint="Tokens per slot." error="Too large">
        {(field) => <Input {...field} mono defaultValue="8192" />}
      </FormField>
    </FieldGroup>,
  ],
];

describe('the kit renders in both themes', () => {
  for (const [name, element] of SPECIMENS) {
    it(`${name} produces identical markup in dark and light`, () => {
      const darkMarkup = renderIn('dark', element);
      const lightMarkup = renderIn('light', element);

      expect(darkMarkup.length).toBeGreaterThan(0);
      // The only difference between the two renders is the wrapper attribute itself. Anything else
      // would mean a component is deciding its own colors in JavaScript.
      expect(darkMarkup.replace('data-theme="dark"', 'data-theme="light"')).toBe(lightMarkup);
    });

    it(`${name} names no literal color`, () => {
      const markup = renderIn('dark', element);
      expect(markup).not.toMatch(LITERAL_COLOR);
      expect(markup).not.toMatch(TAILWIND_PALETTE_CLASS);
    });
  }
});

describe('accessibility affordances survive rendering', () => {
  it('a loading button is busy and disabled', () => {
    const markup = renderToStaticMarkup(<Button loading>Working</Button>);
    expect(markup).toContain('aria-busy="true"');
    expect(markup).toContain('disabled');
  });

  it('a sortable header is a button with an aria-sort cell', () => {
    const markup = renderToStaticMarkup(
      <DataTable columns={columns} rows={rows} rowKey={(row) => row.id} />,
    );
    expect(markup).toContain('aria-sort="none"');
    expect(markup.match(/<button/g)?.length).toBe(columns.length);
  });

  it('a field error is announced and tied to its control', () => {
    const markup = renderToStaticMarkup(
      <FormField label="Port" error="Already in use">
        {(field) => <Input {...field} />}
      </FormField>,
    );
    expect(markup).toContain('role="alert"');
    const described = /aria-describedby="([^"]+)"/.exec(markup);
    expect(described).not.toBeNull();
    expect(markup).toContain(`id="${described![1]!.split(' ')[0]}"`);
  });

  it('an empty table renders its empty state instead of an empty tbody', () => {
    const markup = renderToStaticMarkup(
      <DataTable
        columns={columns}
        rows={[]}
        rowKey={(row) => row.id}
        empty={<EmptyState title="No rows" />}
      />,
    );
    expect(markup).toContain('No rows');
    expect(markup).not.toContain('<table');
  });
});

describe('status badges cover every state enum', () => {
  it('has a style for all nine instance states of section 2.8', () => {
    for (const state of INSTANCE_STATES) {
      const style = stateStyle('instance', state);
      expect(style.label, `${state} has no label`).toBeTruthy();
      expect(style.label).not.toBe(state === 'unknown' ? '' : state);
    }
  });

  it('falls back to the raw value rather than hiding an unknown state', () => {
    expect(stateStyle('instance', 'quantum').label).toBe('quantum');
  });

  it('maps every kind without an empty table', () => {
    for (const [kind, map] of Object.entries(STATE_MAPS)) {
      expect(Object.keys(map).length, `${kind} is empty`).toBeGreaterThan(2);
    }
  });
});

describe('log line handling', () => {
  it('strips CSI colour sequences', () => {
    expect(stripAnsi('[1;31mFAILED[0m: link step')).toBe('FAILED: link step');
  });

  it('collapses carriage-return progress redraws', () => {
    expect(stripAnsi('10%\r50%\r100%')).toBe('10%50%100%');
  });

  it('classifies the shapes a build actually prints', () => {
    expect(classifyLine('[42/318] Building CXX object ggml.cpp.o')).toBe('step');
    expect(classifyLine('-- Configuring done')).toBe('step');
    expect(classifyLine('ggml-cuda.cu:12:3: error: no matching function')).toBe('error');
    expect(classifyLine('warning: unused variable')).toBe('warn');
    expect(classifyLine('linking llama-server')).toBe('plain');
  });
});
