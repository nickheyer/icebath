// storage group bridge, its Go service StorageService lands with the storage lane
import * as StorageService from '../bindings/github.com/nickheyer/icebath/apps/desktop/storageservice.js'
import { fire } from '../runtime'

export const uiPreferencesRead = () => StorageService.UiPreferencesRead()
export const uiPreferencesWrite = (payload) => StorageService.UiPreferencesWrite(payload)
export const readFileDataUrl = (filePath) => StorageService.ReadFileDataUrl(filePath)
export const readFileDataUrlForAttach = (filePath) => StorageService.ReadFileDataUrlForAttach(filePath)
export const dataUrlReadMaxGet = () => StorageService.DataUrlReadMaxGet()
// The handler clamps Number(maxMb), so the bridge sends that number as text to keep undefined apart from null
export const dataUrlReadMaxSet = (maxMb) => StorageService.DataUrlReadMaxSet(String(Number(maxMb)))
export const readFileText = (filePath) => StorageService.ReadFileText(filePath)
export const readPluginSource = (filePath) => StorageService.ReadPluginSource(filePath)
export const saveGatewayFile = (payload) => StorageService.SaveGatewayFile(payload)
export const saveImageFromUrl = (url) => StorageService.SaveImageFromUrl(url)
// Bytes of what Buffer.from accepts, null for the falsy values the handler refuses
function bufferBytes(data): Uint8Array | null {
  if (!data) {
    return null
  }

  if (typeof data === 'string') {
    return new TextEncoder().encode(data)
  }

  if (data instanceof ArrayBuffer) {
    return new Uint8Array(data)
  }

  if (ArrayBuffer.isView(data)) {
    return new Uint8Array(data.buffer, data.byteOffset, data.byteLength)
  }

  if (typeof data === 'object' && typeof (data as { length?: unknown }).length === 'number') {
    return Uint8Array.from(Array.from(data as ArrayLike<number>))
  }

  throw new TypeError(
    'The first argument must be of type string or an instance of Buffer, ArrayBuffer, or Array or an Array-like Object.'
  )
}

// Base64 text the Go []byte parameter decodes, null when there is no data
function base64Bytes(data): string | null {
  const bytes = bufferBytes(data)

  if (!bytes) {
    return null
  }

  let binary = ''

  for (let i = 0; i < bytes.length; i += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000))
  }

  return btoa(binary)
}

export const saveImageBuffer = (data, ext, name) => StorageService.SaveImageBuffer(base64Bytes(data), ext, name)
export const savePastedText = (text) => StorageService.SavePastedText(text)
export const settingsGetDefaultProjectDir = () => StorageService.SettingsGetDefaultProjectDir()
export const settingsSetDefaultProjectDir = (dir) => StorageService.SettingsSetDefaultProjectDir(dir)
export const settingsPickDefaultProjectDir = () => StorageService.SettingsPickDefaultProjectDir()
export const preparedSubmissionsRead = () => StorageService.PreparedSubmissionsRead()
export const preparedSubmissionsUpdate = (key, entry) => StorageService.PreparedSubmissionsUpdate(key, entry)
export const preparedSubmissionsCompareAndSet = (key, expected, entry) => StorageService.PreparedSubmissionsCompareAndSet(key, expected, entry)
export const revealLogs = () => StorageService.RevealLogs()
export const getRecentLogs = () => StorageService.GetRecentLogs()
export const reportRendererError = (report) => fire(StorageService.ReportRendererError(report))
export const logLine = (line) => fire(StorageService.LogLine(line))
