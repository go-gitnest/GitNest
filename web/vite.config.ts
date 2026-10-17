import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The build output is embedded into the Go binary, so `web/dist` is the one
// directory the two toolchains share. Asset names carry a content hash, which is
// what lets the Go side serve them as immutable: a changed file is a changed URL.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: 'dist',
    // The placeholder the repository commits must survive a build, so the output
    // directory is not emptied. Stale hashed bundles are removed by the clean
    // step that runs first and are unreferenced in the meantime.
    emptyOutDir: false,
    assetsDir: 'assets',
    sourcemap: false,
    rollupOptions: {
      output: {
        entryFileNames: 'assets/[name]-[hash].js',
        chunkFileNames: 'assets/[name]-[hash].js',
        assetFileNames: 'assets/[name]-[hash][extname]',
      },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.test.{ts,tsx}'],
    globals: true,
  },
})
