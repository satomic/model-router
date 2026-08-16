import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  // No `base`: the console is served from the site root, so assets resolve from '/'.
  server: {
    proxy: {
      '/v1': 'http://127.0.0.1:8000',
      '/healthz': 'http://127.0.0.1:8000',
    },
  },
})
