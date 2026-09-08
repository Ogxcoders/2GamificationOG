import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      // Same-origin API calls in dev; the console talks to the control plane.
      '/v1': { target: 'http://localhost:8080', changeOrigin: true },
    },
  },
})
