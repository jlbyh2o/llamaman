import { useEffect } from 'react';
import { Outlet, useNavigate, useRouterState } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { Toaster, TooltipProvider } from '../components';
import { onAuthEvent } from '../api/client';
import { queryKeys } from '../api/keys';

/**
 * The root route's component.
 *
 * It mounts the two app-wide providers and wires the client's structural responses to the router.
 * That wiring lives here rather than in the client so that `api/client.ts` never imports the router
 * and stays testable on its own: the client broadcasts, the shell decides where to go.
 *
 *  - `401` anywhere → the login screen, carrying where the user was.
 *  - `409 setup_required` anywhere → the wizard. Section 3: the SPA routes on that code alone.
 */
export function RootLayout() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const href = useRouterState({ select: (state) => state.location.href });
  const pathname = useRouterState({ select: (state) => state.location.pathname });

  useEffect(
    () =>
      onAuthEvent((event) => {
        if (event === 'setup-required') {
          // `claimed` and `setup_complete` are different questions (section 3.1): this code
          // means `admin_account` does not exist yet, so both are false.
          queryClient.setQueryData(queryKeys.auth.session(), {
            authenticated: false,
            claimed: false,
            setup_complete: false,
          });
          if (!pathname.startsWith('/setup')) void navigate({ to: '/setup', replace: true });
          return;
        }
        // The session is gone; nothing cached under it is still trustworthy.
        // A `401` rather than a `409 setup_required` means the host IS claimed and set up; only
        // this browser's session is gone. Seeding both as true is what keeps the gate sending the
        // user to the login screen instead of bouncing them into a finished wizard, and the next
        // read of `/auth/session` replaces the guess with the daemon's own answer either way.
        queryClient.setQueryData(queryKeys.auth.session(), {
          authenticated: false,
          claimed: true,
          setup_complete: true,
        });
        if (pathname !== '/login') {
          void navigate({ to: '/login', search: { redirect: href }, replace: true });
        }
      }),
    [navigate, queryClient, href, pathname],
  );

  return (
    <TooltipProvider delayDuration={200} skipDelayDuration={300}>
      <Outlet />
      <Toaster />
    </TooltipProvider>
  );
}
