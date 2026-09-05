import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  test: {
    // jsdom rather than the default: the defect these tests exist for is a table footer
    // that states the wrong number, and there is no way to see a rendered number without
    // rendering it.
    environment: 'jsdom',
    globals: false,
    setupFiles: ['./src/test-setup.ts'],
  },
})
