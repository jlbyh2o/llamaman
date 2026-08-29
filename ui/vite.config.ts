import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';

// The build emits plain static assets into ui/dist. `make ui` syncs that
// directory into internal/web/dist, which the Go binary embeds (DESIGN
// sections 4 and 16.1). Nothing here may reference a CDN: every asset is
// served from the binary (SPEC section 5.4).
export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // Content-hashed names are what lets internal/web serve assets/ with
    // Cache-Control: immutable.
    assetsDir: 'assets',
  },
  server: {
    port: 5173,
    // `make dev` runs this beside `go run ./cmd/llamaman serve` and points the
    // daemon at it with LLAMAMAN_DEV_UI; API calls go to the daemon.
    proxy: {
      '/api': 'http://localhost:8080',
      '/healthz': 'http://localhost:8080',
    },
  },
});
