/**
 * Per-key presentation for the settings registry (DESIGN section 2.1, section 3.4).
 *
 * The registry (`internal/settings/registry.go`) is `{key, type, default, min, max, enum,
 * restart_required}` plus a mechanically-derived `group` — the dotted prefix, e.g. `hf` for
 * `hf.endpoint` — and carries no human-facing label yet. Screen 15's nine groups ("General, Network
 * & Ports, Hugging Face, Storage, Builds, Benchmarks, Security, Updates, Danger zone") do not line up
 * one-to-one with those prefixes: `hf.hub_dir`/`hf.home` are Storage, not Hugging Face; every
 * `gateway.*`, `ui.port_desired`/`ui.bind` and the internal port pool are Network & Ports; the rest
 * of `ui.*`, `gpu.*`, `fit.*`, `instances.*` (its two poll/timeout knobs) and `retention.*`/`log.*`
 * fall to General for want of a better-named home. This table is that mapping, plus a label and a
 * hint transcribed from the registry's own comments — and it is additive: an unlisted key still
 * renders, humanized, under General, so a registry addition is never invisible.
 */

import type { SettingsGroup } from '../../searchParams';

export interface SettingMeta {
  group: SettingsGroup;
  label: string;
  hint?: string;
  suffix?: string;
}

export const SETTINGS_GROUP_LABELS: Record<SettingsGroup, { title: string; description: string }> =
  {
    general: {
      title: 'General',
      description: 'Theme, retention, and the polling and margin knobs a few subsystems tune.',
    },
    network: {
      title: 'Network & Ports',
      description:
        'Where the web UI and the gateway bind, and the internal port pool instances draw from.',
    },
    huggingface: {
      title: 'Hugging Face',
      description:
        'The Hub endpoint, download concurrency, and the access token used for gated repos.',
    },
    storage: {
      title: 'Storage',
      description:
        'The primary cache directory models and llama.cpp versions are read from and written to.',
    },
    builds: {
      title: 'Builds',
      description: 'llama.cpp acquisition defaults and the GitHub token used for release checks.',
    },
    benchmarks: { title: 'Benchmarks', description: 'Defaults for a new sweep.' },
    security: { title: 'Security', description: 'Session lifetime and the login lockout rule.' },
    updates: { title: 'Updates', description: 'Automatic checks, and the release feed itself.' },
    danger: {
      title: 'Danger zone',
      description: 'Irreversible resets. Nothing here touches models, instances or tokens.',
    },
  };

export const SETTINGS_META: Record<string, SettingMeta> = {
  'ui.port_desired': {
    group: 'network',
    label: 'Web UI port',
    hint: 'Seeded once from the unit at first boot; the stored value wins after that.',
  },
  'ui.bind': { group: 'network', label: 'Web UI bind address' },
  'ui.theme': { group: 'general', label: 'Theme' },

  'security.session_ttl_hours': { group: 'security', label: 'Session lifetime', suffix: 'hours' },
  'security.idle_timeout_hours': { group: 'security', label: 'Idle timeout', suffix: 'hours' },
  'security.login_max_attempts': { group: 'security', label: 'Login attempts before lockout' },
  'security.login_window_sec': {
    group: 'security',
    label: 'Lockout counting window',
    suffix: 'seconds',
  },
  'security.lockout_sec': { group: 'security', label: 'Lockout duration', suffix: 'seconds' },

  'hf.endpoint': { group: 'huggingface', label: 'Hub endpoint' },
  'hf.hub_dir': {
    group: 'storage',
    label: 'Primary cache directory',
    hint: 'Changing this promotes a new primary hub directory and rescans it — the same write path as promoting a cache root.',
  },
  'hf.home': {
    group: 'storage',
    label: 'HF_HOME (derived)',
    hint: 'A courtesy projection of the cache directory above.',
  },
  'hf.download_concurrency': { group: 'huggingface', label: 'Parallel downloads' },
  'hf.rate_limit_bytes_sec': {
    group: 'huggingface',
    label: 'Download rate limit',
    hint: '0 = unlimited.',
    suffix: 'bytes/s',
  },
  'hf.verify_checksums': { group: 'huggingface', label: 'Verify checksums after download' },

  'llamacpp.channel': { group: 'builds', label: 'Default channel' },
  'llamacpp.build_jobs': {
    group: 'builds',
    label: 'Parallel build jobs',
    hint: '0 = auto, from CPU count and free memory.',
  },
  'llamacpp.cuda_arch_list': {
    group: 'builds',
    label: 'CUDA architectures',
    hint: 'Empty auto-detects from the GPUs present, e.g. "89;86".',
  },
  'llamacpp.prefer_prebuilt_cpu': { group: 'builds', label: 'Prefer a prebuilt CPU binary' },
  'llamacpp.extra_cmake_flags': {
    group: 'builds',
    label: 'Extra cmake flags',
    hint: 'Passed through verbatim to every build.',
  },
  'llamacpp.keep_previous': { group: 'builds', label: 'Keep the previous build for rollback' },

  'instances.internal_port_min': { group: 'network', label: 'Internal port pool — floor' },
  'instances.internal_port_max': { group: 'network', label: 'Internal port pool — ceiling' },
  'instances.health_poll_sec': {
    group: 'general',
    label: 'Instance health poll interval',
    suffix: 'seconds',
  },
  'instances.start_timeout_sec': {
    group: 'general',
    label: 'Instance start timeout',
    suffix: 'seconds',
  },

  'gateway.bind': { group: 'network', label: 'Gateway bind address' },
  'gateway.request_timeout_sec': {
    group: 'network',
    label: 'Gateway request timeout',
    hint: '0 = never cap a generation.',
    suffix: 'seconds',
  },
  'gateway.idle_timeout_sec': {
    group: 'network',
    label: 'Gateway idle timeout',
    suffix: 'seconds',
  },
  'gateway.max_body_mb': { group: 'network', label: 'Gateway max request body', suffix: 'MiB' },
  'gateway.usage_parse_kb': {
    group: 'network',
    label: 'Usage parsing limit',
    hint: '0 disables usage parsing.',
    suffix: 'KiB',
  },
  'gateway.drain_sec': { group: 'network', label: 'Restart drain time', suffix: 'seconds' },

  'gpu.poll_active_sec': {
    group: 'general',
    label: 'GPU poll interval — active',
    suffix: 'seconds',
  },
  'gpu.poll_idle_sec': { group: 'general', label: 'GPU poll interval — idle', suffix: 'seconds' },

  'fit.margin_mib': { group: 'general', label: 'Fit calculator VRAM margin', suffix: 'MiB' },
  'fit.use_calibration': { group: 'general', label: 'Calibrate fit estimates from bench history' },

  'bench.exclusive_gpu': { group: 'benchmarks', label: 'Require exclusive GPU access' },
  'bench.default_repetitions': { group: 'benchmarks', label: 'Default repetitions' },

  'update.channel': { group: 'updates', label: 'Release channel' },
  'update.auto_check': { group: 'updates', label: 'Check for updates automatically' },
  'update.check_interval_hours': { group: 'updates', label: 'Check interval', suffix: 'hours' },

  'retention.events_days': { group: 'general', label: 'Event log retention', suffix: 'days' },
  'retention.events_rows': { group: 'general', label: 'Event log row cap' },

  'log.level': { group: 'general', label: 'Log level' },
};

function humanize(key: string): string {
  const last = key.slice(key.lastIndexOf('.') + 1);
  return last.replace(/_/g, ' ').replace(/^./, (c) => c.toUpperCase());
}

export function metaFor(key: string): SettingMeta {
  return SETTINGS_META[key] ?? { group: 'general', label: humanize(key) };
}
