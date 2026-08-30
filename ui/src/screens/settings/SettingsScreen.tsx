/**
 * `/settings` — DESIGN section 4, screen 15.
 *
 * "Grouped forms mirroring the settings registry ... A setting flagged restart_required shows the
 * 'Restart to apply' button wired to POST /system/restart. Secrets are not settings: the Hugging
 * Face and GitHub tokens have their own validating triples and are rendered inside the groups they
 * belong to."
 */

import { useState } from 'react';
import { useNavigate, useSearch } from '@tanstack/react-router';
import { AlertTriangle } from 'lucide-react';
import {
  Button,
  ConfirmDialog,
  LoadingPanel,
  Panel,
  PanelHeader,
  QueryError,
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
  toast,
} from '../../components';
import { RestartControl } from '../../features/system/RestartControl';
import { useCapabilities, useResetSettings, useSettings } from '../../features/system/queries';
import { SETTINGS_GROUPS } from '../../searchParams';
import type { SettingsGroup } from '../../searchParams';
import { SecretField } from './SecretField';
import { SettingsGroupForm } from './SettingsGroupForm';
import { SETTINGS_GROUP_LABELS, metaFor } from './settingsMeta';
import { UpdatesPanel } from '../update/UpdatesPanel';

export function SettingsScreen() {
  const navigate = useNavigate({ from: '/settings' });
  const search = useSearch({ from: '/app/settings' });
  const group: SettingsGroup = search.group ?? 'general';

  const settings = useSettings();
  const capabilities = useCapabilities();
  const reset = useResetSettings();
  const [restartNeeded, setRestartNeeded] = useState(false);
  const [resetTarget, setResetTarget] = useState<SettingsGroup | 'all' | null>(null);

  const keysForGroup = (target: SettingsGroup): string[] =>
    (settings.data?.schema ?? [])
      .filter((def) => metaFor(def.key).group === target)
      .map((def) => def.key);

  const onSaved = (needsRestart: boolean) => {
    if (needsRestart) setRestartNeeded(true);
  };

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-lg font-semibold tracking-tight text-[var(--lm-text)]">Settings</h1>
      </div>

      {restartNeeded ? (
        <Panel className="border-[var(--lm-warn)]/40 bg-[var(--lm-warn-soft)]">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <p className="flex items-center gap-2 text-sm text-[var(--lm-text)]">
              <AlertTriangle className="size-4 shrink-0" aria-hidden />A saved change needs a
              restart before it takes effect.
            </p>
            <RestartControl capabilities={capabilities.data} size="sm" label="Restart to apply" />
          </div>
        </Panel>
      ) : null}

      {settings.isPending ? (
        <LoadingPanel>Reading settings…</LoadingPanel>
      ) : settings.isError ? (
        // Without this the nine tabs rendered with no fields and no message at all — a settings
        // screen that looks configured and is simply empty.
        <QueryError
          title="Settings could not be read"
          error={settings.error}
          onRetry={() => void settings.refetch()}
        />
      ) : (
        <Tabs
          value={group}
          onValueChange={(next) =>
            void navigate({ search: (prev) => ({ ...prev, group: next as SettingsGroup }) })
          }
        >
          <TabsList className="flex-wrap">
            {SETTINGS_GROUPS.map((g) => (
              <TabsTrigger key={g} value={g}>
                {SETTINGS_GROUP_LABELS[g].title}
              </TabsTrigger>
            ))}
          </TabsList>

          {SETTINGS_GROUPS.map((g) => (
            <TabsContent key={g} value={g}>
              <Panel className="space-y-5">
                <PanelHeader
                  title={SETTINGS_GROUP_LABELS[g].title}
                  description={SETTINGS_GROUP_LABELS[g].description}
                  actions={
                    g !== 'danger' && g !== 'updates' && keysForGroup(g).length > 0 ? (
                      <Button size="sm" variant="ghost" onClick={() => setResetTarget(g)}>
                        Reset to defaults
                      </Button>
                    ) : undefined
                  }
                />

                {g === 'huggingface' ? <SecretField kind="hf" label="Access token" /> : null}

                {settings.data ? (
                  <SettingsGroupForm
                    group={g}
                    schema={settings.data.schema}
                    values={settings.data.values}
                    onSaved={onSaved}
                  />
                ) : null}

                {g === 'builds' ? <SecretField kind="github" label="GitHub token" /> : null}
                {g === 'updates' ? <UpdatesPanel /> : null}

                {g === 'danger' ? (
                  <div className="space-y-3">
                    <p className="text-sm text-[var(--lm-text-muted)]">
                      Reset any group's settings to their built-in defaults. This never touches
                      models, instances, downloads or tokens.
                    </p>
                    <Button variant="danger" onClick={() => setResetTarget('all')}>
                      Reset every setting to its default
                    </Button>
                  </div>
                ) : null}
              </Panel>
            </TabsContent>
          ))}
        </Tabs>
      )}

      <ConfirmDialog
        open={resetTarget !== null}
        onOpenChange={(open) => !open && setResetTarget(null)}
        title={
          resetTarget === 'all'
            ? 'Reset every setting?'
            : `Reset ${resetTarget ? SETTINGS_GROUP_LABELS[resetTarget].title : ''} to defaults?`
        }
        description="Built-in defaults resume immediately. Restart-flagged keys still need a restart to take effect."
        confirmLabel="Reset"
        busy={reset.isPending}
        onConfirm={() => {
          const keys =
            resetTarget === 'all'
              ? (settings.data?.schema ?? []).map((def) => def.key)
              : resetTarget
                ? keysForGroup(resetTarget)
                : [];
          reset.mutate(keys, {
            onSuccess: () => {
              toast.success('Reset to defaults');
              setResetTarget(null);
            },
            onError: (err) => toast.error(err),
          });
        }}
      />
    </div>
  );
}
