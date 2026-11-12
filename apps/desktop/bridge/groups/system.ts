// system group bridge, its Go service SystemService lands with the system lane
import * as SystemService from '../bindings/github.com/nickheyer/icebath/apps/desktop/systemservice.js'
import { fire } from '../runtime'

export const claimAmbientCue = (key) => SystemService.ClaimAmbientCue(key)
export const screenshotGetSettings = () => SystemService.ScreenshotGetSettings()
export const screenshotSetEnabled = (enabled) => SystemService.ScreenshotSetEnabled(enabled)
export const screenshotOpenPermissionSettings = (kind) => SystemService.ScreenshotOpenPermissionSettings(kind)
export const screenshotCapture = (requestId) => SystemService.ScreenshotCapture(requestId)
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
