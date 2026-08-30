/**
 * The capabilities matrix (DESIGN section 3.3 `GET /system/capabilities`, section 11.1a).
 *
 * "The single object the UI reads to decide which controls to disable and which explanatory copy to
 * show" — this panel is that copy. The matrix renders every fact capabilities carries; the banners
 * below it are section 11.1a's rows, computed from the same object, each with the exact remediation
 * command a human would type.
 */

import { AlertTriangle, Info, ShieldQuestion } from 'lucide-react';
import { Badge, LoadingPanel, Panel, PanelHeader } from '../../components';
import type { Tone } from '../../components';
import { CommandLine } from '../../features/system/CommandBlock';
import { RestartControl } from '../../features/system/RestartControl';
import { useCapabilities, useSystemInfo } from '../../features/system/queries';
import type { Capabilities } from '../../features/system/types';

interface Cell {
  label: string;
  value: string;
  tone: Tone;
  hint: string;
}

function boolCell(label: string, value: boolean | null, hint: string): Cell {
  if (value === null)
    return {
      label,
      value: 'N/A',
      tone: 'neutral',
      hint: `${hint} Not applicable in this topology.`,
    };
  return { label, value: value ? 'Yes' : 'No', tone: value ? 'ok' : 'danger', hint };
}

function matrixOf(caps: Capabilities): Cell[] {
  return [
    {
      label: 'Systemd control',
      value: caps.systemd_control,
      tone:
        caps.systemd_control === 'dbus'
          ? 'ok'
          : caps.systemd_control === 'exec'
            ? 'warn'
            : 'danger',
      hint:
        caps.systemd_control === 'dbus'
          ? 'Talking to systemd over D-Bus — pushed status, typed errors.'
          : caps.systemd_control === 'exec'
            ? 'D-Bus is unusable; falling back to systemctl. Status is polled, not pushed.'
            : 'No service manager reachable at all (F10). The control plane is read-only.',
    },
    {
      label: 'Scope',
      value: caps.systemd_scope,
      tone: 'neutral',
      hint: 'System units, or a per-user manager (D2).',
    },
    {
      label: 'Polkit',
      value: caps.polkit_ok === null ? 'N/A' : caps.polkit_ok ? 'Granted' : 'Denied',
      tone: caps.polkit_ok === null ? 'neutral' : caps.polkit_ok ? 'ok' : 'danger',
      hint:
        caps.polkit_ok === null
          ? 'A user-scope manager authorizes its own owner unconditionally — there is no polkit rule to grant.'
          : caps.polkit_ok
            ? 'manage-units is granted for the llamaman unit set.'
            : 'manage-units was refused at boot (F9). Start/stop/restart on the daemon and instances are unavailable.',
    },
    {
      label: 'Unit-file management',
      value:
        caps.polkit_unit_files === null ? 'N/A' : caps.polkit_unit_files ? 'Granted' : 'Withheld',
      tone: caps.polkit_unit_files === null ? 'neutral' : caps.polkit_unit_files ? 'ok' : 'warn',
      hint: 'Enable/disable of instance units for autostart — installed with --no-autostart-grant to withhold it.',
    },
    boolCell('Instance control', caps.instance_control, 'Start, stop and restart instance units.'),
    boolCell(
      'Autostart control',
      caps.autostart_control,
      'Toggle whether an instance starts at boot.',
    ),
    boolCell('Self-update', caps.self_update, 'The privileged swap actor can be summoned.'),
    boolCell(
      'Self-update revert',
      caps.self_update_revert,
      'A failed update reverts itself automatically (D88).',
    ),
    {
      label: 'Journal read',
      value: caps.journal_read,
      tone: caps.journal_read === 'ok' ? 'ok' : caps.journal_read === 'denied' ? 'danger' : 'warn',
      hint: 'Whether this identity can read journald for the log panes.',
    },
    {
      label: 'Listener continuity',
      value: caps.listener_continuity,
      tone: caps.listener_continuity === 'fdstore' ? 'ok' : 'warn',
      hint: 'Whether gateway ports survive a daemon restart without dropping connections (D58).',
    },
  ];
}

interface Banner {
  tone: 'warn' | 'danger' | 'info';
  title: string;
  description: string;
  commands: string[];
}

function bannersFor(caps: Capabilities, identity: string): Banner[] {
  const banners: Banner[] = [];
  const id = identity || '<user>';

  if (caps.systemd_control === 'unavailable') {
    banners.push({
      tone: 'danger',
      title: 'No service manager (F10)',
      description:
        'systemd is not reachable at all. Models, downloads, the fit calculator, benchmarks, tokens and the gateway all still work; instance start/stop, autostart and self-update do not. There is no silent child-process fallback — configure instances here and run the printed command by hand or under another supervisor.',
      commands: [],
    });
  } else if (caps.polkit_ok === false) {
    banners.push({
      tone: 'danger',
      title: 'Polkit grant refused (F9)',
      description:
        'The daemon could not confirm it may manage the llamaman unit set. The control plane is read-only until this is repaired.',
      commands: ['sudo llamaman install-units --repair-polkit'],
    });
  }

  if (
    caps.systemd_control !== 'unavailable' &&
    caps.polkit_ok !== false &&
    caps.autostart_control === false
  ) {
    banners.push({
      tone: 'warn',
      title: 'Autostart management withheld',
      description:
        'This host was installed with --no-autostart-grant. Enabling or disabling an instance for boot needs a manual command; everything else keeps working.',
      commands: ['sudo systemctl enable llamaman-instance@<name>.service'],
    });
  }

  if (caps.self_update === false && caps.systemd_control !== 'unavailable') {
    banners.push({
      tone: 'warn',
      title: 'Self-update unsupported',
      description:
        'llamaman-selfupdate.service is absent or masked, so an update cannot be staged from here — this is normal on a pre-v1.0.0 install.',
      commands: [`sudo llamaman install-units --identity ${id}`],
    });
  }

  if (caps.self_update_revert === false) {
    banners.push({
      tone: 'warn',
      title: 'No automatic revert',
      description:
        'llamaman.service does not carry OnFailure=llamaman-update-verify.service, or the judge unit is missing. An update will never be staged without a working revert — this is what POST /update/apply refuses on rather than risk it.',
      commands: [`sudo llamaman install-units --identity ${id}`],
    });
  }

  if (caps.journal_read !== 'ok') {
    banners.push({
      tone: caps.journal_read === 'denied' ? 'danger' : 'warn',
      title: caps.journal_read === 'denied' ? 'Journal access denied (F23)' : 'No journal to read',
      description:
        caps.journal_read === 'denied'
          ? 'This identity cannot read journald. Log panes carry the fix rather than showing an empty stream.'
          : 'journalctl is not available on this host, so log panes will stay empty.',
      commands:
        caps.journal_read === 'denied'
          ? [`sudo usermod -aG systemd-journal ${id}`, 'sudo systemctl restart llamaman.service']
          : [],
    });
  }

  return banners;
}

const BANNER_ICON = { warn: AlertTriangle, danger: AlertTriangle, info: Info } as const;
const BANNER_CLASS: Record<Banner['tone'], string> = {
  warn: 'border-[var(--lm-warn)]/35 bg-[var(--lm-warn-soft)]',
  danger: 'border-[var(--lm-danger)]/35 bg-[var(--lm-danger-soft)]',
  info: 'border-[var(--lm-border)] bg-[var(--lm-surface-raised)]',
};

export function CapabilitiesPanel() {
  const capabilities = useCapabilities();
  const info = useSystemInfo();

  if (capabilities.isLoading) return <LoadingPanel>Reading capabilities…</LoadingPanel>;
  if (!capabilities.data) return null;

  const caps = capabilities.data;
  const cells = matrixOf(caps);
  const banners = bannersFor(caps, info.data?.identity ?? '');
  const allClear = banners.length === 0;

  return (
    <Panel className="space-y-4">
      <PanelHeader
        title="Capabilities"
        description={
          info.data
            ? `${info.data.identity} · ${caps.systemd_scope} scope · v${info.data.version}`
            : undefined
        }
        actions={<RestartControl capabilities={caps} size="sm" />}
      />

      <div className="grid grid-cols-2 gap-x-4 gap-y-3 sm:grid-cols-3 lg:grid-cols-5">
        {cells.map((cell) => (
          <div key={cell.label} title={cell.hint} className="min-w-0">
            <p className="text-[11px] tracking-wide text-[var(--lm-text-faint)] uppercase">
              {cell.label}
            </p>
            <Badge tone={cell.tone} className="mt-1">
              {cell.value}
            </Badge>
          </div>
        ))}
      </div>

      {allClear ? (
        <p className="flex items-center gap-1.5 text-xs text-[var(--lm-text-muted)]">
          <ShieldQuestion aria-hidden className="size-3.5 text-[var(--lm-ok)]" />
          Every capability is granted. Nothing on this host is degraded.
        </p>
      ) : (
        <div className="space-y-2">
          {banners.map((banner) => {
            const Icon = BANNER_ICON[banner.tone];
            return (
              <div
                key={banner.title}
                className={`space-y-2 rounded-[var(--lm-radius)] border p-3 ${BANNER_CLASS[banner.tone]}`}
              >
                <p className="flex items-start gap-2 text-sm font-medium text-[var(--lm-text)]">
                  <Icon aria-hidden className="mt-0.5 size-4 shrink-0" />
                  {banner.title}
                </p>
                <p className="pl-6 text-xs text-[var(--lm-text-muted)]">{banner.description}</p>
                {banner.commands.length > 0 ? (
                  <div className="space-y-1.5 pl-6">
                    {banner.commands.map((command) => (
                      <CommandLine key={command} command={command} />
                    ))}
                  </div>
                ) : null}
              </div>
            );
          })}
        </div>
      )}
    </Panel>
  );
}
