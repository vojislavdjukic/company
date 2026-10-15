import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  build: {
    outDir: '../backend/web/dist',
    emptyOutDir: true,
  },
  server: {
    host: '127.0.0.1',
    port: 5173,
    strictPort: true,
    proxy: {
      '/auth': 'http://127.0.0.1:4000',
      '/codex': 'http://127.0.0.1:4000',
      '/healthz': 'http://127.0.0.1:4000',
      '/readyz': 'http://127.0.0.1:4000',
    },
  },
})
