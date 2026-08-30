import { AuthGate } from '../auth/AuthGate';
import { EventStreamProvider } from '../events/EventStreamProvider';
import { AppShell } from './AppShell';

/**
 * Everything behind the session, in the order the layers depend on each other.
 *
 * The event stream is mounted *inside* the gate on purpose: `GET /api/v1/events` is a session
 * endpoint, so opening it before there is a session earns a 401 and — through the client's
 * broadcast — a redirect loop. Login and the wizard render outside this route and have no stream.
 */
export function AppLayout() {
  return (
    <AuthGate>
      <EventStreamProvider>
        <AppShell />
      </EventStreamProvider>
    </AuthGate>
  );
}
