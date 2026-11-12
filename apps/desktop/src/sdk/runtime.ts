/**
 * Runtime SDK injection — the other half of the vscode-module model. Bundled
 * plugins resolve `@hermes/plugin-sdk` through the vite alias; RUNTIME-loaded
 * plugins (disk / fetched) import the same specifier and get the same object:
 * the loader rewrites bare specifiers to shim modules that re-export the
 * live namespaces installed here. React ships as the app's singletons —
 * a second React instance would break hooks.
 */

import * as React from 'react'
import * as jsxDevRuntime from 'react/jsx-dev-runtime'
import * as jsxRuntime from 'react/jsx-runtime'

import * as sdk from './index'

// Resolved LAZILY, never as a module-scope literal. This module sits in an
// import cycle — `sdk/index` → `contrib/*` → `contrib/runtime-loader` →
// `sdk/runtime` — so a module-scope `{ __ICEBATH_PLUGIN_SDK__: sdk, … }` is
// evaluated BEFORE `sdk/index`'s own body runs. In the bundled app that read
// yields `undefined` (the bundler emits the namespace as a hoisted `var`), so
// `Object.keys(GLOBALS.__ICEBATH_PLUGIN_SDK__)` threw
// "Cannot convert undefined or null to object" and EVERY runtime (disk)
// plugin failed to load. Reading them at call time — installPluginSdk() and
// the shim builder only ever run once the app is up — gets the live
// namespaces.
function pluginNamespaces() {
  return {
    __ICEBATH_PLUGIN_SDK__: sdk,
    __ICEBATH_REACT__: React,
    __ICEBATH_REACT_JSX__: jsxRuntime,
    __ICEBATH_REACT_JSX_DEV__: jsxDevRuntime
  }
}

type PluginGlobalKey = keyof ReturnType<typeof pluginNamespaces>

export function installPluginSdk(): void {
  const namespaces = pluginNamespaces()

  // Plugins built for hermes reach the same namespaces under the hermes global names.
  const legacy = Object.fromEntries(
    Object.entries(namespaces).map(([key, value]) => [key.replace('__ICEBATH_', '__HERMES_'), value])
  )

  Object.assign(globalThis, namespaces, legacy)
}

/** Build a shim ESM blob that re-exports a global namespace's live members.
 *  Export names come from the namespace itself, so the list can't drift. */
function shimUrl(globalKey: PluginGlobalKey): string {
  const names = Object.keys(pluginNamespaces()[globalKey]).filter(
    name => name !== 'default' && /^[A-Za-z_$][\w$]*$/.test(name)
  )

  const source =
    `const m = globalThis.${globalKey};\n` +
    `export default m.default ?? m;\n` +
    // Guard the destructuring: `export const {  } = m` is a syntax error, so
    // only emit it when the namespace actually has named exports.
    (names.length ? `export const { ${names.join(', ')} } = m;\n` : '')

  return URL.createObjectURL(new Blob([source], { type: 'text/javascript' }))
}

let cached: Record<string, string> | null = null

/** Specifier -> shim URL map for the runtime loader (longest keys first). */
export function sdkImportMap(): Record<string, string> {
  cached ??= {
    '@icebath/plugin-sdk': shimUrl('__ICEBATH_PLUGIN_SDK__'),
    '@hermes/plugin-sdk': shimUrl('__ICEBATH_PLUGIN_SDK__'),
    'react/jsx-dev-runtime': shimUrl('__ICEBATH_REACT_JSX_DEV__'),
    'react/jsx-runtime': shimUrl('__ICEBATH_REACT_JSX__'),
    react: shimUrl('__ICEBATH_REACT__')
  }

  return cached
}
