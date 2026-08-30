import { useId, useState } from 'react';
import { useNavigate } from '@tanstack/react-router';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { KeyRound, Lock, ShieldCheck } from 'lucide-react';
import { Badge, Button, FormField, Input, Progress } from '../../components';
import { api } from '../../api/client';
import { ApiError } from '../../api/errors';
import { WizardStep } from '../../setup/WizardStep';
import { nextStepPath, useWizard } from '../../setup/useWizard';
import { MIN_PASSWORD_LENGTH, scorePassword, strengthPercent } from './passwordStrength';

/**
 * Wizard step `password` (DESIGN section 11.2): "argon2id hash with a strength meter, creates the
 * session, stamps the claim". Not skippable — this step is what claims the host.
 *
 * Three things are true here and nowhere else in the app:
 *
 *  - **There is no session yet.** `POST /api/v1/setup/password` is a `setup` route, and the 204 it
 *    answers with carries the session cookie. Everything cached before it was answered without one,
 *    so the whole cache is invalidated on success rather than a hand-picked key.
 *  - **A non-loopback browser must present `X-Setup-Token`** (D38). `GET /setup/state` says whether
 *    this caller needs one, so the field appears only for the browser that does — a user sitting at
 *    the machine never sees it.
 *  - **The claim is one-way.** `409 setup_already_claimed` means someone got here first; the honest
 *    answer is the login screen, not a retry.
 *
 * The meter advises and the button gates only on what the daemon would actually refuse: eight
 * characters, and a confirmation that matches. Section 11.2 asks for a meter, not a policy.
 */
export function PasswordStep() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const wizard = useWizard();
  const formId = useId();

  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [setupToken, setSetupToken] = useState('');

  const strength = scorePassword(password);
  const mismatch = confirm.length > 0 && confirm !== password;
  const ready = strength.acceptable && confirm === password && !mismatch;

  const claim = useMutation({
    mutationFn: () =>
      api.post('/api/v1/setup/password', {
        body: { password },
        ...(wizard.tokenRequired && setupToken
          ? { headers: { 'X-Setup-Token': setupToken.trim() } }
          : {}),
      }),
    onSuccess: async () => {
      // The response minted a session. Nothing read before it is still the whole truth.
      await queryClient.invalidateQueries();
      setPassword('');
      setConfirm('');
      void navigate({ to: nextStepPath('password') });
    },
  });

  const error = claim.error;
  const code = error instanceof ApiError ? error.code : null;

  // Already claimed — a resumed wizard, a second tab, or a host someone else set up first.
  if (wizard.claimed) {
    return (
      <WizardStep
        step="password"
        canContinue
        banner={
          <div className="flex items-start gap-3 rounded-[var(--lm-radius-lg)] border border-[var(--lm-ok)]/40 bg-[var(--lm-ok-soft)] p-3">
            <ShieldCheck aria-hidden className="mt-0.5 size-4 shrink-0 text-[var(--lm-ok)]" />
            <p className="text-sm text-[var(--lm-text-muted)]">
              This host is already claimed. The admin password is changed from Settings, and the
              wizard carries on from here.
            </p>
          </div>
        }
      >
        <div className="flex items-start gap-3">
          <Lock aria-hidden className="mt-0.5 size-4 shrink-0 text-[var(--lm-text-faint)]" />
          <p className="text-sm text-[var(--lm-text-muted)]">
            The password is stored as an argon2id hash and is the only credential for the management
            UI. If it is ever lost, <span className="lm-numeric">llamaman reset-password</span> run
            on the machine itself is the way back in — filesystem access to the database is the
            authorization, so no network path can do it.
          </p>
        </div>
      </WizardStep>
    );
  }

  return (
    <WizardStep
      step="password"
      primaryAction={
        <Button
          type="submit"
          form={formId}
          variant="primary"
          icon={<KeyRound />}
          loading={claim.isPending}
          disabled={!ready}
        >
          Claim this host
        </Button>
      }
      banner={
        code === 'setup_already_claimed' ? (
          <div className="flex items-start justify-between gap-3 rounded-[var(--lm-radius-lg)] border border-[var(--lm-warn)]/40 bg-[var(--lm-warn-soft)] p-3">
            <p className="text-sm text-[var(--lm-text-muted)]">
              Someone claimed this host while this page was open. The password that was set is the
              one that works now.
            </p>
            <Button variant="secondary" onClick={() => void navigate({ to: '/login' })}>
              Go to sign in
            </Button>
          </div>
        ) : null
      }
    >
      <form
        id={formId}
        className="space-y-4"
        onSubmit={(event) => {
          event.preventDefault();
          if (!ready || claim.isPending) return;
          claim.mutate();
        }}
      >
        {wizard.tokenRequired ? (
          <FormField
            label="Setup token"
            required
            error={code === 'setup_token_required' ? 'That token was not accepted.' : undefined}
            hint={
              <>
                This browser is not on the machine itself, so the first claim needs the token the
                daemon wrote at boot. Read it with{' '}
                <span className="lm-numeric text-[var(--lm-text)]">
                  sudo journalctl -u llamaman -b
                </span>
                , or from the machine&apos;s own console.
              </>
            }
          >
            {(field) => (
              <Input
                {...field}
                mono
                autoFocus
                autoComplete="off"
                spellCheck={false}
                value={setupToken}
                onChange={(event) => setSetupToken(event.target.value)}
              />
            )}
          </FormField>
        ) : null}

        <FormField
          label="Admin password"
          required
          error={
            code === 'password_invalid' && error instanceof ApiError ? error.message : undefined
          }
          hint={`At least ${MIN_PASSWORD_LENGTH} characters. Length does more for this than punctuation does.`}
        >
          {(field) => (
            <Input
              {...field}
              type="password"
              autoComplete="new-password"
              autoFocus={!wizard.tokenRequired}
              value={password}
              onChange={(event) => setPassword(event.target.value)}
            />
          )}
        </FormField>

        <div>
          <Progress
            value={password.length === 0 ? 0 : strengthPercent(strength)}
            tone={strength.tone}
            aria-label="Password strength"
            label="Strength"
            detail={<Badge tone={strength.tone}>{strength.label}</Badge>}
          />
          {strength.suggestions.length ? (
            <ul className="mt-2 space-y-1">
              {strength.suggestions.map((suggestion) => (
                <li key={suggestion} className="text-xs text-[var(--lm-text-muted)]">
                  {suggestion}
                </li>
              ))}
            </ul>
          ) : null}
        </div>

        <FormField
          label="Confirm password"
          required
          error={mismatch ? 'The two entries do not match.' : undefined}
        >
          {(field) => (
            <Input
              {...field}
              type="password"
              autoComplete="new-password"
              value={confirm}
              onChange={(event) => setConfirm(event.target.value)}
            />
          )}
        </FormField>

        {error && !code ? (
          <p className="text-xs text-[var(--lm-danger)]" role="alert">
            The daemon did not answer. Check that llamaman.service is running, then try again.
          </p>
        ) : null}
        {code &&
        code !== 'password_invalid' &&
        code !== 'setup_token_required' &&
        code !== 'setup_already_claimed' ? (
          <p className="text-xs text-[var(--lm-danger)]" role="alert">
            {error instanceof ApiError ? error.message : null}
          </p>
        ) : null}
      </form>
    </WizardStep>
  );
}
