import { defineConfig, type Plugin } from 'vite';
import react from '@vitejs/plugin-react';

// The renderer is loaded from file:// in production (Electron), so all
// asset URLs must be relative.

// Dev serves react-refresh via an inline module script, which the strict
// CSP meta forbids. Strip the CSP tag while serving; production builds
// keep it intact.
function cspDevStrip(): Plugin {
  return {
    name: 'csp-dev-strip',
    apply: 'serve',
    transformIndexHtml(html) {
      return html.replace(/<meta http-equiv="Content-Security-Policy"[^>]*>/, '');
    },
  };
}

export default defineConfig({
  root: import.meta.dirname,
  base: './',
  plugins: [react(), cspDevStrip()],
  build: {
    outDir: '../dist-renderer',
    emptyOutDir: true,
    chunkSizeWarningLimit: 1200,
  },
  server: {
    port: 5173,
    strictPort: true,
  },
});
