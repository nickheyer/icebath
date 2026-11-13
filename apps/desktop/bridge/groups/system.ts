// system group bridge over SystemService, the screenshot PNG turned back from base64 into bytes
import * as SystemService from '../bindings/github.com/nickheyer/icebath/apps/desktop/systemservice.js'
import { fire } from '../runtime'

// Decodes the base64 PNG of a capture result into the Uint8Array the renderer reads
function screenshotBytes(result) {
  if (!result?.ok || typeof result.png !== 'string') {
    return result
  }

  const raw = atob(result.png)
  const png = new Uint8Array(raw.length)

  for (let i = 0; i < raw.length; i += 1) {
    png[i] = raw.charCodeAt(i)
  }

  return { ok: true, png }
}

export const claimAmbientCue = (key) => SystemService.ClaimAmbientCue(key)
export const screenshotGetSettings = () => SystemService.ScreenshotGetSettings()
export const screenshotSetEnabled = (enabled) => SystemService.ScreenshotSetEnabled(enabled)
export const screenshotOpenPermissionSettings = (kind) => SystemService.ScreenshotOpenPermissionSettings(kind)
export const screenshotCapture = (requestId) => SystemService.ScreenshotCapture(requestId).then(screenshotBytes)
export const notify = (payload) => SystemService.Notify(payload)
export const claimStartupLatency = () => SystemService.ClaimStartupLatency()
export const requestMicrophoneAccess = () => SystemService.RequestMicrophoneAccess()
export const writeClipboard = (text) => SystemService.WriteClipboard(text)
export const readClipboard = () => SystemService.ReadClipboard()
export const saveClipboardImage = () => SystemService.SaveClipboardImage()
export const setActiveWork = (payload) => fire(SystemService.SetActiveWork(payload))
export const setKeepAwake = (mode) => fire(SystemService.SetKeepAwake(mode))
export const getOnBattery = () => SystemService.GetOnBattery()
export const screenshotSubscribe = (on) => SystemService.ScreenshotSubscribe(on)
