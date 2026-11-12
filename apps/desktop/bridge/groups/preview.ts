// preview group bridge, its Go service PreviewService lands with the preview lane
import * as PreviewService from '../bindings/github.com/nickheyer/icebath/apps/desktop/previewservice.js'
import { fire } from '../runtime'

export const getEmbedHostOrigin = () => PreviewService.GetEmbedHostOrigin()
export const openSessionWindow = (sessionId, opts) => PreviewService.OpenSessionWindow(sessionId, opts)
export const openWindow = (options) => PreviewService.OpenWindow(options)
export const openBrowserWindow = (tabId) => PreviewService.OpenBrowserWindow(tabId)
export const windowRelaySend = (payload) => fire(PreviewService.WindowRelaySend(payload))
export const capturePreview = (payload) => PreviewService.CapturePreview(payload)
export const normalizePreviewTarget = (target, baseDir) => PreviewService.NormalizePreviewTarget(target, baseDir)
export const setPreviewGuestHidden = (webContentsId, hidden) => fire(PreviewService.SetPreviewGuestHidden(webContentsId, hidden))
export const openExternal = (url) => PreviewService.OpenExternal(url)
export const freeTierChallengeRun = (request) => PreviewService.FreeTierChallengeRun(request)
export const openPreviewInBrowser = (url) => PreviewService.OpenPreviewInBrowser(url)
export const reachPreviewUrl = (url) => PreviewService.ReachPreviewUrl(url)
export const fetchLinkTitle = (url) => PreviewService.FetchLinkTitle(url)
export const resolveFavicon = (url) => PreviewService.ResolveFavicon(url)
export const themesFetchMarketplace = (id) => PreviewService.ThemesFetchMarketplace(id)
export const themesSearchMarketplace = (query) => PreviewService.ThemesSearchMarketplace(query)
