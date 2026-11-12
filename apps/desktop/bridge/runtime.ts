// Shared plumbing of the hermesDesktop bridge, boot payload, events and lazy group loading
import { Events } from '@wailsio/runtime'

export interface DesktopBoot {
  platform?: string
  translucency?: { glass?: boolean; translucency?: boolean }
  featureFlags?: { localModels?: boolean; assumeFreshInstall?: boolean }
  windowControlsCustom?: boolean
  localSkin?: unknown
  uiPreferencesSnapshot?: unknown
  hudWindowing?: {
    clientPlacement?: boolean
    controlDrag?: boolean
    nativeDrag?: boolean
    solid?: boolean
    workspaceTransfer?: boolean
  }
}

declare global {
  interface Window {
    __ICEBATH_DESKTOP_BOOT__?: DesktopBoot
  }
}

export const boot: DesktopBoot = window.__ICEBATH_DESKTOP_BOOT__ ?? {}

// Pre paint values of the storage group, published by its Go boot provider
export const storageBoot: Pick<DesktopBoot, 'localSkin' | 'uiPreferencesSnapshot'> = boot

// Pre paint values of the overlays group, published by its Go boot provider
export const overlayBoot: Pick<DesktopBoot, 'hudWindowing'> = boot

// Runs a send style call whose result the renderer never reads, logging a failure
export function fire(call: Promise<unknown>): void {
  call.catch((error: unknown) => console.error('[hermesDesktop]', error))
}

// Loads a group module once and hands every caller the same promise
export function lazyGroup<T>(load: () => Promise<T>): () => Promise<T> {
  let pending: Promise<T> | null = null

  return () => (pending ??= load())
}

// Subscribes callback to a main process event and returns the unsubscribe function
export function subscribe(event: string, callback: (payload?: unknown) => void, noPayload = false): () => void {
  return Events.On(event, (e: { data: unknown }) => (noPayload ? callback() : callback(e.data)))
}

let screenshotListeners = 0

// Subscribes to screenshot requests, telling main when the first listener arrives and the last leaves
export function subscribeScreenshotRequests(callback: (requestId: unknown) => void): () => void {
  if (screenshotListeners === 0) {
    fire(import('./groups/system').then(m => m.screenshotSubscribe(true)))
  }

  screenshotListeners += 1
  const off = subscribe('screenshot:request', callback)

  return () => {
    off()
    screenshotListeners -= 1

    if (screenshotListeners === 0) {
      fire(import('./groups/system').then(m => m.screenshotSubscribe(false)))
    }
  }
}

let factor = 1

Events.On('zoom:changed', (e: { data: { level?: number } }) => {
  const level = Number(e.data?.level)

  factor = Number.isFinite(level) ? Math.pow(1.2, level) : 1
})

// Answers webFrame.getZoomFactor from the last zoom the main process applied
export function zoomFactor(): number {
  return factor
}

let filesModule: typeof import('./groups/files') | null = null

import('./groups/files').then(
  m => {
    filesModule = m
  },
  (error: unknown) => console.error('[hermesDesktop] files group', error)
)

// Returns the files group for synchronous calls such as getPathForFile
export function filesGroupSync(): typeof import('./groups/files') {
  if (!filesModule) {
    throw new Error('hermesDesktop files group is not loaded')
  }

  return filesModule
}
