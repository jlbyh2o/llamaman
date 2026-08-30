import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider } from '@tanstack/react-router';
import { createQueryClient } from './api/queryClient';
import { router } from './routes';
import { initTheme } from './theme/useTheme';
import './theme.css';

// Before the first paint, so a stored dark/light choice is never a flash of the other one.
initTheme();

// Every screen is a projection of database rows, so server state lives in TanStack Query and SSE
// frames patch its cache rather than triggering a refetch (DESIGN section 4).
const queryClient = createQueryClient();

const container = document.getElementById('root');
if (!container) throw new Error('#root missing from index.html');

createRoot(container).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
);
