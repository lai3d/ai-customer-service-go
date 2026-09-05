import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The dev server does not proxy the API.
//
// A proxy would make development same-origin and production cross-origin, which is
// exactly the difference that hides a CORS mistake until deployment. Point
// VITE_API_BASE at the Go service and the browser does the same thing in both -- the
// preflight, the allowlist and the Vary header are all exercised from the first minute.
export default defineConfig({
  plugins: [react()],
  build: { outDir: 'dist', sourcemap: true },
  server: { port: 5173 },
})
