import { createRootRoute, createRoute, createRouter, Outlet } from '@tanstack/react-router';
import { Home } from './screens/Home';

// Code-based routes (DESIGN section 4): filters, sort and comparison
// selections belong in typed search params, which is why this project does not
// use file-based routing. The seventeen screens of section 4 are added here as
// they land; for now there is one.

const rootRoute = createRootRoute({
  component: () => <Outlet />,
});

const indexRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  component: Home,
});

const routeTree = rootRoute.addChildren([indexRoute]);

export const router = createRouter({ routeTree });

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router;
  }
}
