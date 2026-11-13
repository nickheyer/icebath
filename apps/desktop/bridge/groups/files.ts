// files group bridge over FilesService, plus the synchronous getPathForFile fed by native drop paths
import * as FilesService from '../bindings/github.com/nickheyer/icebath/apps/desktop/filesservice.js'

export const selectPaths = (options) => FilesService.SelectPaths(options)
export const selectSavePath = (options) => FilesService.SelectSavePath(options)
export const saveTextFileAs = (request) => FilesService.SaveTextFileAs(request)
export const watchPreviewFile = (url) => FilesService.WatchPreviewFile(url)
export const watchDirectory = (dir) => FilesService.WatchDirectory(dir)
export const stopPreviewFileWatch = (id) => FilesService.StopPreviewFileWatch(id)
export const sanitizeWorkspaceCwd = (cwd) => FilesService.SanitizeWorkspaceCwd(cwd)
export const readDir = (dirPath) => FilesService.ReadDir(dirPath)
export const gitRoot = (startPath) => FilesService.GitRoot(startPath)
export const revealPath = (targetPath) => FilesService.RevealPath(targetPath)
export const openDir = (dirPath) => FilesService.OpenDir(dirPath)
export const desktopPluginsRoot = () => FilesService.DesktopPluginsRoot()
export const reconcileDesktopPlugins = async () => (await FilesService.ReconcileDesktopPlugins()).published
export const reconcileDesktopPluginsReport = () => FilesService.ReconcileDesktopPlugins()
export const logsRoot = (profile?: string) => FilesService.LogsRoot(profile)
export const renamePath = (targetPath, newName) => FilesService.RenamePath(targetPath, newName)
export const writeTextFile = (filePath, content) => FilesService.WriteTextFile(filePath, content)
export const trashPath = (targetPath) => FilesService.TrashPath(targetPath)
export const pluginFilesRequest = (pluginId, labels) => FilesService.PluginFilesRequest(pluginId, labels)
export const pluginFilesGrants = (pluginId) => FilesService.PluginFilesGrants(pluginId)
// A revoke naming no folder removes every grant of the plugin, as preload's undefined folderId does
export const pluginFilesRevoke = (pluginId, folderId?) =>
  FilesService.PluginFilesRevoke(pluginId, folderId ?? null, folderId === undefined)
export const pluginFilesOp = (pluginId, request) => FilesService.PluginFilesOp(pluginId, request)
export const probePluginRepo = (payload) => FilesService.ProbePluginRepo(payload)
export const installDesktopPlugin = (payload) => FilesService.InstallDesktopPlugin(payload)
export const removeDesktopPlugin = (payload) => FilesService.RemoveDesktopPlugin(payload)

interface DroppedEntry {
  path: string
  name: string
  size: number
  lastModified: number
  directory: boolean
}

declare global {
  interface Window {
    __icebathDroppedFiles?: (entries: DroppedEntry[]) => void
    chrome?: { webview?: { postMessageWithAdditionalObjects?: (message: string, objects: unknown[]) => void } }
    _wails?: { handlePlatformFileDrop?: (filenames: string[], x: number, y: number) => void }
  }
}

const filePaths = new WeakMap<File, string>()
let dragged: DroppedEntry[] = []
const claimed = new Set<DroppedEntry>()

// The native side reports each dragged OS file before the web view dispatches the drag
window.__icebathDroppedFiles = entries => {
  dragged = Array.isArray(entries) ? entries : []
  claimed.clear()
}

// Path of an OS file the user dropped, empty for any other File as webUtils.getPathForFile answers
export function getPathForFile(file: File): string {
  try {
    const known = filePaths.get(file)

    if (known) {
      return known
    }

    const match = dragged.find(
      entry =>
        !claimed.has(entry) &&
        entry.name === file.name &&
        (entry.directory || (entry.size === file.size && Math.abs(entry.lastModified - file.lastModified) < 1000))
    )

    if (!match) {
      return ''
    }

    claimed.add(match)
    filePaths.set(file, match.path)

    return match.path
  } catch {
    return ''
  }
}

const hasFiles = (event: DragEvent) => Boolean(event.dataTransfer?.types?.includes('Files'))

// The renderer's verdict on a file drag, read after its handlers and before the Wails runtime overrides it
const verdicts = new WeakMap<Event, { prevented: boolean; effect: DataTransfer['dropEffect'] }>()

function recordVerdict(event: DragEvent) {
  if (hasFiles(event) && event.dataTransfer) {
    verdicts.set(event, { prevented: event.defaultPrevented, effect: event.dataTransfer.dropEffect })
  }
}

// Restores the drop effect the renderer chose, refusing the drop where no renderer handler accepted it
function restoreVerdict(event: DragEvent) {
  const verdict = verdicts.get(event)

  if (verdict && event.dataTransfer) {
    event.dataTransfer.dropEffect = verdict.prevented ? verdict.effect : 'none'
  }
}

function installVerdictGuard() {
  for (const type of ['dragenter', 'dragover'] as const) {
    document.body.addEventListener(type, recordVerdict)
    document.addEventListener(type, restoreVerdict)
  }

  window.addEventListener('drop', event => {
    if (hasFiles(event)) {
      setTimeout(() => {
        dragged = []
        claimed.clear()
      })
    }
  })
}

if (document.body) {
  installVerdictGuard()
} else {
  document.addEventListener('DOMContentLoaded', installVerdictGuard, { once: true })
}

interface PendingDrop {
  files: File[]
  strings: [string, string][]
  target: EventTarget
  init: DragEventInit
  timer: ReturnType<typeof setTimeout>
}

let pendingDrop: PendingDrop | null = null

// Re-dispatches a captured WebView2 drop with the same File objects once their paths are known
function releaseDrop(paths: string[]) {
  const drop = pendingDrop

  if (!drop) {
    return
  }

  pendingDrop = null
  clearTimeout(drop.timer)
  drop.files.forEach((file, i) => {
    if (paths[i]) {
      filePaths.set(file, paths[i])
    }
  })

  const transfer = new DataTransfer()

  for (const file of drop.files) {
    transfer.items.add(file)
  }

  for (const [type, value] of drop.strings) {
    transfer.setData(type, value)
  }

  drop.target.dispatchEvent(new DragEvent('drop', { ...drop.init, dataTransfer: transfer }))
}

// WebView2 resolves dropped File paths only in the host, so the drop waits for the host to answer
function installWebView2Drop() {
  const webview = window.chrome?.webview
  const post = webview?.postMessageWithAdditionalObjects

  if (!webview || typeof post !== 'function') {
    return
  }

  const runtimeDrop = window._wails?.handlePlatformFileDrop

  if (window._wails) {
    window._wails.handlePlatformFileDrop = (filenames, x, y) => {
      if (pendingDrop) {
        releaseDrop(filenames)

        return
      }

      runtimeDrop?.call(window._wails, filenames, x, y)
    }
  }

  window.addEventListener(
    'drop',
    event => {
      if (!event.isTrusted || !hasFiles(event) || !event.dataTransfer?.files.length) {
        return
      }

      event.preventDefault()
      event.stopImmediatePropagation()
      const transfer = event.dataTransfer
      const files = Array.from(transfer.files)

      const strings = Array.from(transfer.types)
        .filter(type => type !== 'Files')
        .map(type => [type, transfer.getData(type)] as [string, string])

      const init: DragEventInit = {
        bubbles: true,
        cancelable: true,
        composed: true,
        clientX: event.clientX,
        clientY: event.clientY,
        screenX: event.screenX,
        screenY: event.screenY,
        altKey: event.altKey,
        ctrlKey: event.ctrlKey,
        metaKey: event.metaKey,
        shiftKey: event.shiftKey
      }

      pendingDrop = {
        files,
        strings,
        target: event.target ?? document,
        init,
        timer: setTimeout(() => releaseDrop([]), 2000)
      }
      post.call(webview, `file:drop:${Math.round(event.clientX)}:${Math.round(event.clientY)}`, files)
    },
    true
  )
}

installWebView2Drop()
