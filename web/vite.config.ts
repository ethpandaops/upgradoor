/// <reference types="vitest/config" />
import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

// The dev server proxies the API to a local controller. Override the target
// with UPGRADOOR_API_URL when it listens elsewhere.
const apiTarget = process.env.UPGRADOOR_API_URL ?? 'http://127.0.0.1:8080';

export default defineConfig({
  plugins: [react(), tailwindcss()],
  // Relative asset URLs, so the controller can serve the UI below any path.
  base: './',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/api': { target: apiTarget, changeOrigin: true },
    },
  },
  test: {
    globals: true,
    environment: 'jsdom',
    include: ['src/**/*.test.{ts,tsx}'],
    setupFiles: ['./src/test/setup.ts'],
    restoreMocks: true,
  },
});
