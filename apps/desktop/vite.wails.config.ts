import path from 'path'
import { fileURLToPath } from 'url'

import { defineConfig, mergeConfig, type Plugin, type UserConfig } from 'vite'

import electronConfig from './vite.config'

const __dirname: string = path.dirname(fileURLToPath(import.meta.url))

// Loads boot.js and the hermesDesktop bridge ahead of the renderer entry
function wailsBridge(): Plugin {
  return {
    name: 'icebath-wails-bridge',
    transformIndexHtml: {
      order: 'pre',
      handler: html =>
        html.replace(
          '<script type="module" src="/src/entry.ts"></script>',
          '<script src="/__icebath/boot.js"></script>\n    <script type="module" src="/bridge/index.ts"></script>\n    <script type="module" src="/src/entry.ts"></script>'
        )
    }
  }
}

export default defineConfig(env => {
  const base = (typeof electronConfig === 'function' ? electronConfig(env) : electronConfig) as UserConfig

  return mergeConfig(base, {
    plugins: [wailsBridge()],
    cacheDir: process.env.ICEBATH_WAILS_VITE_CACHE || path.join(__dirname, 'node_modules/.vite-wails'),
    server: {
      hmr: { host: '127.0.0.1' }
    },
    build: {
      outDir: path.join(__dirname, 'dist-wails'),
      emptyOutDir: true
    }
  })
})
