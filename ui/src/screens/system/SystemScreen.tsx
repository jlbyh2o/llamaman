/**
 * `/system` — DESIGN section 4, screen 16.
 *
 * "Toolchain report cards with fix guidance, GPU details, disk, journal viewer, 'Download
 * diagnostics bundle'." The capabilities matrix sits above the tabs rather than inside one of them:
 * it is not a filterable list, it is the fact every other control on this screen — and the restart
 * button in the header — reads before deciding what to allow.
 *
 * The diagnostics bundle sits beside it, out of the tabs, for the same reason and one more: it is
 * the thing `features/instances/remediation.ts` sends a user here for by name, so it must be
 * findable without knowing which tab to open.
 */

import { useNavigate, useSearch } from '@tanstack/react-router';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '../../components';
import { CapabilitiesPanel } from './CapabilitiesPanel';
import { DiagnosticsPanel } from './DiagnosticsPanel';
import { DiskPanel } from './DiskPanel';
import { GpuPanel } from './GpuPanel';
import { JournalPanel } from './JournalPanel';
import { ToolchainPanel } from './ToolchainPanel';
import { UnitsPanel } from './UnitsPanel';

const TABS = [
  { value: 'toolchain', label: 'Toolchain' },
  { value: 'gpus', label: 'GPUs' },
  { value: 'disk', label: 'Disk' },
  { value: 'units', label: 'Units' },
  { value: 'journal', label: 'Journal' },
] as const;

export function SystemScreen() {
  const navigate = useNavigate({ from: '/system' });
  const search = useSearch({ from: '/app/system' });
  const tab = search.tab ?? 'toolchain';

  return (
    <div className="space-y-4">
      <h1 className="text-lg font-semibold tracking-tight text-[var(--lm-text)]">System</h1>

      <CapabilitiesPanel />
      <DiagnosticsPanel />

      <Tabs
        value={tab}
        onValueChange={(next) =>
          void navigate({ search: (prev) => ({ ...prev, tab: next as typeof tab }) })
        }
      >
        <TabsList>
          {TABS.map((item) => (
            <TabsTrigger key={item.value} value={item.value}>
              {item.label}
            </TabsTrigger>
          ))}
        </TabsList>

        <TabsContent value="toolchain">
          <ToolchainPanel />
        </TabsContent>
        <TabsContent value="gpus">
          <GpuPanel />
        </TabsContent>
        <TabsContent value="disk">
          <DiskPanel />
        </TabsContent>
        <TabsContent value="units">
          <UnitsPanel />
        </TabsContent>
        <TabsContent value="journal">
          <JournalPanel />
        </TabsContent>
      </Tabs>
    </div>
  );
}
