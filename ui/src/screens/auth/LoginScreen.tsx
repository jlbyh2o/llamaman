import { useEffect, useState } from 'react';
import { useNavigate, useSearch } from '@tanstack/react-router';
import { LogIn } from 'lucide-react';
import { Button, FormField, Input, Panel } from '../../components';
import { ApiError } from '../../api/errors';
import { formatSeconds } from '../../format';
import { lockoutFrom, useLogin, useSession } from '../../auth/session';
import { ThemeToggle } from '../../layout/ThemeToggle';

/**
 * The login screen (DESIGN section 4, screen 1: "password, lockout countdown").
 *
 * Three outcomes, and they are not the same thing:
 *
 *  - `401 bad_credentials` — the password is wrong, or this host has no admin account.
 *  - `429 locked_out` — this address has spent its attempts (SPEC section 4). `retry_after_sec` is
 *    counted down here and the form stays disabled for exactly that long, because a button that
 *    submits into a lockout teaches the wrong thing about what is happening.
 *  - anything else — the daemon is unwell, and saying "wrong password" would be a lie.
 *
 * On success the router goes to `?redirect`, which is where `AuthGate` recorded the user was
 * headed before the session was found missing.
 */
export function LoginScreen() {
  const navigate = useNavigate();
  const search = useSearch({ from: '/login' });
  const session = useSession();
  const login = useLogin();

  const [password, setPassword] = useState('');
  const [remainingMs, setRemainingMs] = useState(0);

  const lockout = lockoutFrom(login.error);

  // Start the countdown when a lockout arrives, and tick it down once a second.
  useEffect(() => {
    if (!lockout) return;
    setRemainingMs(lockout.retryAfterMs);
  }, [lockout]);

  useEffect(() => {
    if (remainingMs <= 0) return;
    const timer = setInterval(() => setRemainingMs((ms) => Math.max(0, ms - 1000)), 1000);
    return () => clearInterval(timer);
  }, [remainingMs]);

  // A session that already exists (a second tab, a back button) does not need a password.
  useEffect(() => {
    if (session.data?.authenticated) {
      void navigate({ to: search.redirect ?? '/', replace: true });
    }
  }, [session.data?.authenticated, navigate, search.redirect]);

  const lockedOut = remainingMs > 0;
  const error =
    login.error instanceof ApiError
      ? login.error.code === 'bad_credentials'
        ? 'That password was not accepted.'
        : login.error.code === 'locked_out'
          ? `Too many attempts. Try again in ${formatSeconds(Math.ceil(remainingMs / 1000))}.`
          : login.error.message
      : login.error
        ? 'The daemon did not answer. Check that llamaman.service is running.'
        : undefined;

  return (
    <div className="flex min-h-full flex-col bg-[var(--lm-bg)]">
      <header className="flex h-12 items-center gap-2 px-4">
        <span
          aria-hidden
          className="flex size-6 items-center justify-center rounded-[var(--lm-radius)] bg-[var(--lm-accent)] text-[11px] font-bold text-[var(--lm-accent-contrast)]"
        >
          LM
        </span>
        <span className="text-sm font-semibold tracking-tight text-[var(--lm-text)]">
          Llama Man
        </span>
        <div className="flex-1" />
        <ThemeToggle />
      </header>

      <div className="flex flex-1 items-center justify-center px-4 pb-16">
        <Panel className="w-full max-w-sm">
          <h1 className="text-base font-semibold text-[var(--lm-text)]">Sign in</h1>
          <p className="mt-1 text-xs text-[var(--lm-text-muted)]">
            One admin password for this host. Lost it? Run{' '}
            <span className="lm-numeric text-[var(--lm-text)]">llamaman reset-password</span> on the
            machine itself.
          </p>

          <form
            className="mt-4 space-y-4"
            onSubmit={(event) => {
              event.preventDefault();
              if (lockedOut || login.isPending) return;
              login.mutate(password, {
                onSuccess: () => void navigate({ to: search.redirect ?? '/', replace: true }),
              });
            }}
          >
            <FormField label="Password" error={error} required>
              {(field) => (
                <Input
                  {...field}
                  type="password"
                  autoComplete="current-password"
                  autoFocus
                  value={password}
                  disabled={lockedOut}
                  onChange={(event) => setPassword(event.target.value)}
                />
              )}
            </FormField>

            <Button
              type="submit"
              variant="primary"
              size="lg"
              icon={<LogIn />}
              className="w-full justify-center"
              loading={login.isPending}
              disabled={lockedOut || password.length === 0}
            >
              {lockedOut ? `Locked for ${formatSeconds(Math.ceil(remainingMs / 1000))}` : 'Sign in'}
            </Button>
          </form>
        </Panel>
      </div>
    </div>
  );
}
