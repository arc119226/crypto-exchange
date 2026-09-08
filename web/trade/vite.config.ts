import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The SPA never talks to the api or stream role directly: in development
// and under `vite preview` (the Playwright smoke) Vite proxies /v1, the
// JWKS and /ws to them, and a deployment puts the built assets behind the
// same origin. There is therefore no CORS anywhere in the Go code
// (docs/domain.md §25).
const api = process.env.API_URL ?? 'http://localhost:8080'
const ws = process.env.WS_URL ?? 'ws://localhost:8081'
const proxy = {
  '/v1': { target: api, changeOrigin: true },
  '/.well-known': { target: api, changeOrigin: true },
  '/ws': { target: ws, ws: true, changeOrigin: true },
}

export default defineConfig({
  plugins: [react()],
  server: { port: 5173, strictPort: true, proxy },
  preview: { port: 5173, strictPort: true, proxy },
  build: { sourcemap: false },
})
