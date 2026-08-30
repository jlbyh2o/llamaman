/**
 * `GET /bench/runs/{id}/export` — a file, not a JSON body.
 *
 * The generated response type collapses to `string` for every format (`schema.d.ts` declares one
 * content object carrying all three media types), which is right for `csv`/`md` but not for `json`
 * — so this bypasses the typed client entirely and reads the response as a blob, using whatever
 * filename the `Content-Disposition` header carries (falling back to the run's own name) rather than
 * inventing one client-side.
 */

import { buildPath, buildQuery, readCookie } from '../../api/client';

export type BenchExportFormat = 'json' | 'csv' | 'md';

const EXTENSIONS: Record<BenchExportFormat, string> = { json: 'json', csv: 'csv', md: 'md' };

function filenameFrom(header: string | null, fallback: string): string {
  if (!header) return fallback;
  // `attachment; filename="qwen3-8b-sweep.csv"` — the quoted form the server always sends.
  const match = /filename\*?=(?:UTF-8'')?"?([^";]+)"?/i.exec(header);
  return match?.[1] ? decodeURIComponent(match[1]) : fallback;
}

/**
 * Fetch the export and hand the browser a save dialog. Throws on a non-2xx response — callers
 * report that through `toast.error`.
 */
export async function downloadBenchExport(
  runId: string,
  format: BenchExportFormat,
  runName: string,
): Promise<void> {
  const path = buildPath('/api/v1/bench/runs/{id}/export', { id: runId });
  const query = buildQuery({ format });
  const csrf = readCookie('lm_csrf');

  const res = await fetch(`${path}${query}`, {
    method: 'GET',
    credentials: 'same-origin',
    headers: csrf ? { 'X-CSRF-Token': csrf } : {},
  });

  if (!res.ok) {
    const text = await res.text().catch(() => '');
    throw new Error(text.slice(0, 300) || `export failed: HTTP ${res.status}`);
  }

  const blob = await res.blob();
  const fallback = `${runName || 'bench-run'}.${EXTENSIONS[format]}`;
  const filename = filenameFrom(res.headers.get('Content-Disposition'), fallback);

  const url = URL.createObjectURL(blob);
  try {
    const anchor = document.createElement('a');
    anchor.href = url;
    anchor.download = filename;
    document.body.appendChild(anchor);
    anchor.click();
    anchor.remove();
  } finally {
    URL.revokeObjectURL(url);
  }
}
