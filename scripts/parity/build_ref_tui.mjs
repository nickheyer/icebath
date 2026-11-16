// Builds the reference ui-tui bundle from a scratch copy with the options of scripts/build/tui.mjs
import { cpSync, readFileSync, writeFileSync, mkdirSync } from 'node:fs'
import { resolve, join } from 'node:path'
import { pathToFileURL } from 'node:url'

const source = resolve(process.argv[2])
const root = join(source, 'ui-tui')
const { build } = await import(pathToFileURL(join(source, 'node_modules/esbuild/lib/main.js')).href)
const stubDevtools = {
  name: 'stub-react-devtools-core',
  setup(b) {
    b.onResolve({ filter: /^react-devtools-core$/ }, args => ({ path: args.path, namespace: 'stub-devtools' }))
    b.onLoad({ filter: /.*/, namespace: 'stub-devtools' }, () => ({
      contents: 'export default { initialize() {}, connectToDevTools() {} }',
      loader: 'js'
    }))
  }
}
const out = join(root, 'dist/entry.js')
await build({
  absWorkingDir: root,
  entryPoints: [resolve(root, 'src/entry.tsx')],
  bundle: true,
  platform: 'node',
  format: 'esm',
  target: 'node20',
  outfile: out,
  jsx: 'automatic',
  jsxImportSource: 'react',
  alias: { '@hermes/ink': resolve(root, 'packages/hermes-ink/src/entry-exports.ts') },
  plugins: [stubDevtools],
  banner: { js: "import { createRequire as __cr } from 'node:module'; const require = __cr(import.meta.url);" },
  logLevel: 'info'
})
const body = readFileSync(out, 'utf8')
if (body.startsWith('#!')) writeFileSync(out, body.slice(body.indexOf('\n') + 1))
mkdirSync(join(root, 'dist/scripts'), { recursive: true })
for (const name of ['gateway_bootstrap.py', 'clipboard_image.py']) cpSync(join(root, 'scripts', name), join(root, 'dist/scripts', name))
console.log('built ' + out)
