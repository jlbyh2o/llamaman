/**
 * Install a llama.cpp build (DESIGN section 4 screen 12, section 3.5, section 6.3).
 *
 * Channel tabs choose *what* to install; the plan preview underneath — `GET /llamacpp/plan` —
 * answers "what would happen" before anything is committed: source vs. prebuilt, the reason, the
 * detected CUDA architectures, missing toolchain items and a free-space check. `stable`/`nightly`
 * pick a resolved release tag; `custom` is a git URL and ref with no listing, resolved through
 * `git ls-remote` server-side.
 */

import { useEffect, useState } from 'react';
import { AlertTriangle, Download, GitBranch } from 'lucide-react';
import {
  Badge,
  Button,
  Dialog,
  DialogContent,
  FormField,
  Input,
  Select,
  Spinner,
  Switch,
  Tabs,
  TabsList,
  TabsTrigger,
  toast,
} from '../../components';
import { formatBytes, formatEstimate } from '../../format';
import { selectFieldProps } from '../../features/system/api';
import { useInstallLlamacpp, useLlamacppPlan, useLlamacppReleases } from './queries';
import type { LlamacppChannel } from './queries';

const BACKEND_OPTIONS = [
  {
    value: 'auto',
    label: 'Auto-detect',
    description: 'CUDA when a supported GPU is present, else CPU.',
  },
  { value: 'cpu', label: 'CPU only' },
  { value: 'cuda', label: 'CUDA' },
];

export interface InstallDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  defaultChannel?: LlamacppChannel;
}

export function InstallDialog({
  open,
  onOpenChange,
  defaultChannel = 'stable',
}: InstallDialogProps) {
  const [channel, setChannel] = useState<LlamacppChannel>(defaultChannel);
  const [tag, setTag] = useState('');
  const [gitUrl, setGitUrl] = useState('');
  const [gitRef, setGitRef] = useState('');
  const [backend, setBackend] = useState('auto');
  const [forceSource, setForceSource] = useState(false);
  const [cmakeExtra, setCmakeExtra] = useState('');

  useEffect(() => {
    if (open) setChannel(defaultChannel);
  }, [open, defaultChannel]);

  const releases = useLlamacppReleases(channel === 'custom' ? 'stable' : channel);
  const install = useInstallLlamacpp();

  const ready = channel === 'custom' ? gitUrl.trim() !== '' && gitRef.trim() !== '' : tag !== '';

  const plan = useLlamacppPlan(
    {
      channel,
      ...(backend !== 'auto' ? { backend: backend as 'cpu' | 'cuda' } : {}),
      ...(channel === 'custom' ? { git_url: gitUrl, git_ref: gitRef } : { tag }),
      force_source: forceSource,
    },
    open && ready,
  );

  const onSubmit = () => {
    install.mutate(
      {
        channel,
        force_source: forceSource,
        ...(backend !== 'auto' ? { backend: backend as 'cpu' | 'cuda' } : {}),
        ...(channel === 'custom' ? { git_url: gitUrl, git_ref: gitRef } : { tag }),
        ...(cmakeExtra.trim() ? { cmake_extra: cmakeExtra.trim().split(/\s+/) } : {}),
      },
      {
        onSuccess: (res) => {
          toast.success(res.reused ? 'Already installed' : 'Build queued', {
            description: res.reused
              ? 'This exact build already exists — nothing new was started.'
              : `${res.version.tag} is now ${res.version.state}.`,
          });
          onOpenChange(false);
        },
        onError: (err) => toast.error(err),
      },
    );
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        title="Install a llama.cpp build"
        description="A prebuilt binary when one exists for this platform; a source build otherwise."
        size="lg"
        footer={
          <>
            <Button
              variant="ghost"
              onClick={() => onOpenChange(false)}
              disabled={install.isPending}
            >
              Cancel
            </Button>
            <Button
              variant="primary"
              icon={<Download />}
              loading={install.isPending}
              disabled={!ready || (plan.data ? !plan.data.can_proceed : false)}
              onClick={onSubmit}
            >
              Install
            </Button>
          </>
        }
      >
        <div className="space-y-4">
          <Tabs value={channel} onValueChange={(v) => setChannel(v as LlamacppChannel)}>
            <TabsList>
              <TabsTrigger value="stable">Stable</TabsTrigger>
              <TabsTrigger value="nightly">Nightly</TabsTrigger>
              <TabsTrigger value="custom">Custom</TabsTrigger>
            </TabsList>
          </Tabs>

          {channel !== 'custom' ? (
            <FormField
              label="Release"
              hint={channel === 'nightly' ? 'Resolved to the latest nightly tag.' : undefined}
            >
              {(field) =>
                releases.isLoading ? (
                  <Spinner label="Loading releases" />
                ) : (
                  <Select
                    {...selectFieldProps(field)}
                    value={tag || undefined}
                    onValueChange={setTag}
                    placeholder="Choose a release…"
                    options={(releases.data?.releases ?? []).map((release) => ({
                      value: release.tag,
                      label: `${release.name || release.tag}${release.installed ? ' (installed)' : ''}`,
                      ...(release.prerelease ? { description: 'Pre-release' } : {}),
                    }))}
                  />
                )
              }
            </FormField>
          ) : (
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <FormField label="Git URL" required>
                {(field) => (
                  <Input
                    {...field}
                    mono
                    placeholder="https://github.com/ggml-org/llama.cpp"
                    value={gitUrl}
                    onChange={(e) => setGitUrl(e.target.value)}
                  />
                )}
              </FormField>
              <FormField label="Ref" hint="Tag, branch, or a 40-hex commit." required>
                {(field) => (
                  <Input
                    {...field}
                    mono
                    placeholder="main"
                    value={gitRef}
                    onChange={(e) => setGitRef(e.target.value)}
                  />
                )}
              </FormField>
            </div>
          )}

          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <FormField label="Backend">
              {(field) => (
                <Select
                  {...selectFieldProps(field)}
                  value={backend}
                  onValueChange={setBackend}
                  options={BACKEND_OPTIONS}
                />
              )}
            </FormField>
            <FormField label="Extra cmake flags" hint="Space-separated; passed through verbatim.">
              {(field) => (
                <Input
                  {...field}
                  mono
                  placeholder="-DGGML_CUDA_F16=ON"
                  value={cmakeExtra}
                  onChange={(e) => setCmakeExtra(e.target.value)}
                />
              )}
            </FormField>
          </div>

          {channel !== 'custom' ? (
            <label className="flex items-center gap-2 text-sm text-[var(--lm-text)]">
              <Switch checked={forceSource} onCheckedChange={setForceSource} />
              Build from source even if a prebuilt binary exists
            </label>
          ) : null}

          {ready ? (
            <div className="rounded-[var(--lm-radius)] border border-[var(--lm-border)] bg-[var(--lm-surface-sunken)] p-3">
              {plan.isLoading ? (
                <Spinner label="Estimating…" />
              ) : plan.data ? (
                <div className="space-y-2 text-sm">
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge tone={plan.data.acquisition === 'prebuilt' ? 'ok' : 'accent'}>
                      {plan.data.acquisition === 'prebuilt' ? 'Prebuilt binary' : 'Source build'}
                    </Badge>
                    <Badge tone="neutral">{plan.data.backend}</Badge>
                    <span className="text-xs text-[var(--lm-text-muted)]">{plan.data.reason}</span>
                  </div>
                  <p className="text-xs text-[var(--lm-text-muted)]">
                    Estimated {formatEstimate(plan.data.estimated_minutes)}
                    {plan.data.cuda_arch.length > 0
                      ? ` · CUDA arch ${plan.data.cuda_arch.join(', ')}`
                      : ''}
                  </p>
                  {!plan.data.free_space_ok ? (
                    <p className="flex items-center gap-1.5 text-xs text-[var(--lm-danger)]">
                      <AlertTriangle className="size-3.5" aria-hidden />
                      Not enough free space ({formatBytes(plan.data.free_bytes)} free,{' '}
                      {formatBytes(plan.data.required_bytes)} needed).
                    </p>
                  ) : null}
                  {plan.data.missing_tools.length > 0 ? (
                    <p className="flex items-center gap-1.5 text-xs text-[var(--lm-danger)]">
                      <AlertTriangle className="size-3.5" aria-hidden />
                      Missing: {plan.data.missing_tools.join(', ')} — see the System screen's
                      Toolchain tab.
                    </p>
                  ) : null}
                </div>
              ) : (
                <p className="flex items-center gap-1.5 text-xs text-[var(--lm-text-muted)]">
                  <GitBranch className="size-3.5" aria-hidden />
                  Choose a release to see the install plan.
                </p>
              )}
            </div>
          ) : null}
        </div>
      </DialogContent>
    </Dialog>
  );
}
