import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import path from 'node:path'

// Icebath Setup — Vite config for the Wails v3 installer window.
//
// Port 5175 keeps us out of the way of:
//   web       (vite default 5173)
//   apps/desktop dev     (5174 per its package.json)
//
// `clearScreen: false` keeps errors visible when `wails3 dev` spawns vite as a
// child process.

const host = process.env.WAILS_VITE_HOST

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src')
    }
  },
  clearScreen: false,
  server: {
    port: 5175,
    strictPort: true,
    host: host || '127.0.0.1',
    hmr: host
      ? {
          protocol: 'ws',
          host,
          port: 5176
        }
      : undefined,
    watch: {
      // Don't watch the Go side or the packaging assets — wails3 dev handles them.
      ignored: ['**/*.go', '**/build/**', '**/bin/**']
    }
  },
  build: {
    target: 'esnext',
    outDir: 'dist',
    emptyOutDir: true
  }
})
