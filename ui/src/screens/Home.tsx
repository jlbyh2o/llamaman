import { useQuery } from '@tanstack/react-query';
import { getMeta } from '../api/client';

/**
 * Placeholder route. It exists to prove the whole chain — Vite build, Tailwind
 * tokens, router, query client, API client — and is replaced by the Dashboard
 * of DESIGN section 4, screen 3.
 */
export function Home() {
  const meta = useQuery({ queryKey: ['meta'], queryFn: getMeta, retry: false });

  return (
    <main className="mx-auto flex min-h-full max-w-3xl flex-col justify-center gap-4 px-6 py-16">
      <h1 className="text-3xl font-semibold tracking-tight text-[var(--lm-text)]">Llama Man</h1>
      <p className="text-sm text-[var(--lm-text-muted)]">
        {meta.isPending
          ? 'Contacting the daemon…'
          : meta.isError
            ? 'Daemon unreachable — the API is not implemented yet.'
            : `Daemon version ${meta.data.version} (${meta.data.commit})`}
      </p>
    </main>
  );
}
