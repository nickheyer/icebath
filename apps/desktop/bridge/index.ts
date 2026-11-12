// Rebuilds window.hermesDesktop over Wails bindings and events before the renderer loads
import * as LifecycleService from './bindings/github.com/nickheyer/icebath/apps/desktop/lifecycleservice.js'
import * as WindowService from './bindings/github.com/nickheyer/icebath/apps/desktop/windowservice.js'
import { boot, fire, filesGroupSync, lazyGroup, overlayBoot, storageBoot, subscribe, subscribeScreenshotRequests, zoomFactor } from './runtime'

const lazyStorage = lazyGroup(() => import('./groups/storage'))
const lazyBackend = lazyGroup(() => import('./groups/backend'))
const lazyPreview = lazyGroup(() => import('./groups/preview'))
const lazyConnections = lazyGroup(() => import('./groups/connections'))
const lazyPty = lazyGroup(() => import('./groups/pty'))
const lazySystem = lazyGroup(() => import('./groups/system'))
const lazyOverlays = lazyGroup(() => import('./groups/overlays'))
const lazyFiles = lazyGroup(() => import('./groups/files'))
const lazyChrome = lazyGroup(() => import('./groups/chrome'))
const lazyGit = lazyGroup(() => import('./groups/git'))
const lazyUpdater = lazyGroup(() => import('./groups/updater'))

const hermesDesktop = {
  glassSupported: boot.translucency?.glass === true,
  translucencySupported: boot.translucency?.translucency === true,
  localModelsEnabled: boot.featureFlags?.localModels === true,
  assumeFreshInstall: boot.featureFlags?.assumeFreshInstall === true,
  localSkin: storageBoot.localSkin && typeof storageBoot.localSkin === 'object' ? storageBoot.localSkin : null,
  uiPreferences: {
    snapshot: storageBoot.uiPreferencesSnapshot && typeof storageBoot.uiPreferencesSnapshot === 'object' ? storageBoot.uiPreferencesSnapshot : null,
    read: () => lazyStorage().then(m => m.uiPreferencesRead()),
    write: (payload) => lazyStorage().then(m => m.uiPreferencesWrite(payload)),
    onChanged: callback => subscribe('ui-preferences:changed', callback)
  },
  getConnection: (profile, opts) => lazyBackend().then(m => m.getConnection(profile, opts)),
  getEmbedHostOrigin: () => lazyPreview().then(m => m.getEmbedHostOrigin()),
  getConnectionFor: (payload) => lazyConnections().then(m => m.getConnectionFor(payload)),
  getProfileRoutes: (profiles) => lazyConnections().then(m => m.getProfileRoutes(profiles)),
  revalidateConnection: () => lazyBackend().then(m => m.revalidateConnection()),
  getGatewayWsUrl: (profile) => lazyBackend().then(m => m.getGatewayWsUrl(profile)),
  getGatewayWsUrlFor: (payload) => lazyConnections().then(m => m.getGatewayWsUrlFor(payload)),
  getPluginWsUrl: (payload) => lazyConnections().then(m => m.getPluginWsUrl(payload)),
  getAgentRoster: () => lazyConnections().then(m => m.getAgentRoster()),
  openSessionWindow: (sessionId, opts) => lazyPreview().then(m => m.openSessionWindow(sessionId, opts)),
  openSessionInTerminal: (sessionId, opts) => lazyPty().then(m => m.openSessionInTerminal(sessionId, opts)),
  openWindow: (options) => lazyPreview().then(m => m.openWindow(options)),
  openBrowserWindow: (tabId) => lazyPreview().then(m => m.openBrowserWindow(tabId)),
  windowRelay: {
    send: (payload) => lazyPreview().then(m => m.windowRelaySend(payload)),
    onMessage: callback => subscribe('window:relay', callback)
  },
  onBrowserPopoutClosed: callback => subscribe('browser-popout:closed', callback),
  claimAmbientCue: (key) => lazySystem().then(m => m.claimAmbientCue(key)),
  windowControls: {
    custom: boot.windowControlsCustom === true,
    minimize: () => fire(WindowService.WindowControl('minimize')),
    toggleMaximize: () => fire(WindowService.WindowControl('toggle-maximize')),
    close: () => fire(WindowService.WindowControl('close'))
  },
  wakeIndicator: {
    getState: () => lazyOverlays().then(m => m.wakeIndicatorGetState()),
    setState: (state) => lazyOverlays().then(m => m.wakeIndicatorSetState(state)),
    onState: callback => subscribe('wake-indicator:state', callback)
  },
  chatOnboarding: {
    size: mode => fire(WindowService.WindowSize(mode))
  },
  petOverlay: {
    open: (request) => lazyOverlays().then(m => m.petOverlayOpen(request)),
    close: () => lazyOverlays().then(m => m.petOverlayClose()),
    setBounds: (bounds) => lazyOverlays().then(m => m.petOverlaySetBounds(bounds)),
    setIgnoreMouse: (ignore) => lazyOverlays().then(m => m.petOverlaySetIgnoreMouse(ignore)),
    setFocusable: (focusable) => lazyOverlays().then(m => m.petOverlaySetFocusable(focusable)),
    pushState: (payload) => lazyOverlays().then(m => m.petOverlayPushState(payload)),
    control: (payload) => lazyOverlays().then(m => m.petOverlayControl(payload)),
    onState: callback => subscribe('pet-overlay:state', callback),
    onControl: callback => subscribe('pet-overlay:control', callback)
  },
  hud: {
    nativeDrag: overlayBoot.hudWindowing?.nativeDrag === true,
    windowing: {
      clientPlacement: overlayBoot.hudWindowing?.clientPlacement !== false,
      controlDrag: overlayBoot.hudWindowing?.controlDrag === true,
      nativeDrag: overlayBoot.hudWindowing?.nativeDrag === true,
      solid: overlayBoot.hudWindowing?.solid === true,
      workspaceTransfer: overlayBoot.hudWindowing?.workspaceTransfer === true
    },
    open: (request) => lazyOverlays().then(m => m.hudOpen(request)),
    close: () => lazyOverlays().then(m => m.hudClose()),
    setIgnoreMouse: (ignore) => lazyOverlays().then(m => m.hudSetIgnoreMouse(ignore)),
    beginMove: () => lazyOverlays().then(m => m.hudBeginMove()),
    endMove: () => lazyOverlays().then(m => m.hudEndMove()),
    moveBy: (delta) => lazyOverlays().then(m => m.hudMoveBy(delta)),
    setWorkspaceTransfer: (transferring) => lazyOverlays().then(m => m.hudSetWorkspaceTransfer(transferring)),
    setBounds: (bounds) => lazyOverlays().then(m => m.hudSetBounds(bounds)),
    resetLayout: () => lazyOverlays().then(m => m.hudResetLayout()),
    setFrost: (showing) => lazyOverlays().then(m => m.hudSetFrost(showing)),
    setSession: (sessionId) => lazyOverlays().then(m => m.hudSetSession(sessionId)),
    onGoto: callback => subscribe('hud:goto', callback),
    onChanged: callback => subscribe('hud:changed', callback),
    onCursor: callback => subscribe('hud:cursor', callback),
    onGameOverlay: callback => subscribe('hud:game-overlay', callback)
  },
  hudModifier: {
    getSettings: () => lazyOverlays().then(m => m.hudModifierGetSettings()),
    setEnabled: (enabled) => lazyOverlays().then(m => m.hudModifierSetEnabled(enabled)),
    openPermissionSettings: () => lazyOverlays().then(m => m.hudModifierOpenPermissionSettings()),
    onStatus: callback => subscribe('hud-modifier:status', callback)
  },
  screenshot: boot.platform === 'darwin' ? {
    getSettings: () => lazySystem().then(m => m.screenshotGetSettings()),
    setEnabled: (enabled) => lazySystem().then(m => m.screenshotSetEnabled(enabled)),
    openPermissionSettings: (kind) => lazySystem().then(m => m.screenshotOpenPermissionSettings(kind)),
    capture: (requestId) => lazySystem().then(m => m.screenshotCapture(requestId)),
    onStatus: callback => subscribe('screenshot:status', callback),
    onRequest: callback => subscribeScreenshotRequests(callback)
  } : undefined,
  quickEntry: {
    getSettings: () => lazyOverlays().then(m => m.quickEntryGetSettings()),
    setSettings: (patch) => lazyOverlays().then(m => m.quickEntrySetSettings(patch)),
    submit: (payload) => lazyOverlays().then(m => m.quickEntrySubmit(payload)),
    ackSubmit: (correlationId, result) => lazyOverlays().then(m => m.quickEntryAckSubmit(correlationId, result)),
    dismiss: () => lazyOverlays().then(m => m.quickEntryDismiss()),
    pushState: (payload) => lazyOverlays().then(m => m.quickEntryPushState(payload)),
    onState: callback => subscribe('quick-entry:state', callback),
    onSubmit: callback => subscribe('quick-entry:submit', callback),
    onShown: callback => subscribe('quick-entry:shown', callback),
    onLateResult: callback => subscribe('quick-entry:late-result', callback)
  },
  getBootProgress: () => lazyBackend().then(m => m.getBootProgress()),
  getConnectionConfig: (profile) => lazyConnections().then(m => m.getConnectionConfig(profile)),
  saveConnectionConfig: (payload) => lazyConnections().then(m => m.saveConnectionConfig(payload)),
  applyConnectionConfig: (payload) => lazyConnections().then(m => m.applyConnectionConfig(payload)),
  testConnectionConfig: (payload) => lazyConnections().then(m => m.testConnectionConfig(payload)),
  getSecretStorageEncryption: () => lazyConnections().then(m => m.getSecretStorageEncryption()),
  setSecretStorageEncryption: (on) => lazyConnections().then(m => m.setSecretStorageEncryption(on)),
  connections: {
    list: () => lazyConnections().then(m => m.connectionsList()),
    save: (payload) => lazyConnections().then(m => m.connectionsSave(payload)),
    remove: (id) => lazyConnections().then(m => m.connectionsRemove(id)),
    setPrimary: (id) => lazyConnections().then(m => m.connectionsSetPrimary(id)),
    setLaunchMode: (mode) => lazyConnections().then(m => m.connectionsSetLaunchMode(mode)),
    setLastUsed: (id) => lazyConnections().then(m => m.connectionsSetLastUsed(id)),
    test: (id) => lazyConnections().then(m => m.connectionsTest(id)),
    updateManaged: (id) => lazyConnections().then(m => m.connectionsUpdateManaged(id)),
    updateAll: (options) => lazyConnections().then(m => m.connectionsUpdateAll(options)),
    onChanged: callback => subscribe('connections:changed', callback)
  },
  sshConfigHosts: () => lazyConnections().then(m => m.sshConfigHosts()),
  sshResolveHost: (host) => lazyConnections().then(m => m.sshResolveHost(host)),
  probeConnectionConfig: (remoteUrl) => lazyConnections().then(m => m.probeConnectionConfig(remoteUrl)),
  oauthLoginConnectionConfig: (remoteUrl, options) => lazyConnections().then(m => m.oauthLoginConnectionConfig(remoteUrl, options)),
  oauthLogoutConnectionConfig: (remoteUrl) => lazyConnections().then(m => m.oauthLogoutConnectionConfig(remoteUrl)),
  cloud: {
    status: () => lazyConnections().then(m => m.cloudStatus()),
    login: () => lazyConnections().then(m => m.cloudLogin()),
    logout: () => lazyConnections().then(m => m.cloudLogout()),
    discover: (org) => lazyConnections().then(m => m.cloudDiscover(org)),
    agentSignIn: (dashboardUrl) => lazyConnections().then(m => m.cloudAgentSignIn(dashboardUrl))
  },
  profile: {
    getDefault: () => lazyConnections().then(m => m.profileGetDefault()),
    setDefault: (route) => lazyConnections().then(m => m.profileSetDefault(route)),
    onDefaultChanged: callback => subscribe('profile:default:changed', callback),
    get: () => lazyConnections().then(m => m.profileGet()),
    remember: (name) => lazyConnections().then(m => m.profileRemember(name)),
    importConnections: (record) => lazyConnections().then(m => m.profileImportConnections(record)),
    rememberConnection: (connectionId, profile) => lazyConnections().then(m => m.profileRememberConnection(connectionId, profile)),
    set: (name) => lazyConnections().then(m => m.profileSet(name))
  },
  api: (request) => lazyConnections().then(m => m.api(request)),
  notify: (payload) => lazySystem().then(m => m.notify(payload)),
  claimStartupLatency: () => lazySystem().then(m => m.claimStartupLatency()),
  requestMicrophoneAccess: () => lazySystem().then(m => m.requestMicrophoneAccess()),
  readWindowBelow: () => lazyOverlays().then(m => m.readWindowBelow()),
  readFileDataUrl: (filePath) => lazyStorage().then(m => m.readFileDataUrl(filePath)),
  readFileDataUrlForAttach: (filePath) => lazyStorage().then(m => m.readFileDataUrlForAttach(filePath)),
  dataUrlReadMax: {
    get: () => lazyStorage().then(m => m.dataUrlReadMaxGet()),
    set: (maxMb) => lazyStorage().then(m => m.dataUrlReadMaxSet(maxMb))
  },
  readFileText: (filePath) => lazyStorage().then(m => m.readFileText(filePath)),
  readPluginSource: (filePath) => lazyStorage().then(m => m.readPluginSource(filePath)),
  selectPaths: (options) => lazyFiles().then(m => m.selectPaths(options)),
  selectSavePath: (options) => lazyFiles().then(m => m.selectSavePath(options)),
  saveTextFileAs: (request) => lazyFiles().then(m => m.saveTextFileAs(request)),
  writeClipboard: (text) => lazySystem().then(m => m.writeClipboard(text)),
  readClipboard: () => lazySystem().then(m => m.readClipboard()),
  saveGatewayFile: (payload) => lazyStorage().then(m => m.saveGatewayFile(payload)),
  saveImageFromUrl: (url) => lazyStorage().then(m => m.saveImageFromUrl(url)),
  contextMenuEdit: (command) => lazyChrome().then(m => m.contextMenuEdit(command)),
  contextMenuCopyImage: () => lazyChrome().then(m => m.contextMenuCopyImage()),
  contextMenuSpellcheck: (action) => lazyChrome().then(m => m.contextMenuSpellcheck(action)),
  contextMenuGuestAddWord: (payload) => lazyChrome().then(m => m.contextMenuGuestAddWord(payload)),
  spellcheckDictionary: (op, word) => lazyChrome().then(m => m.spellcheckDictionary(op, word)),
  onContextMenuSpellcheck: callback => subscribe('context-menu-spellcheck', callback),
  saveImageBuffer: (data, ext, name) => lazyStorage().then(m => m.saveImageBuffer(data, ext, name)),
  capturePreview: (payload) => lazyPreview().then(m => m.capturePreview(payload)),
  savePastedText: (text) => lazyStorage().then(m => m.savePastedText(text)),
  saveClipboardImage: () => lazySystem().then(m => m.saveClipboardImage()),
  getPathForFile: file => filesGroupSync().getPathForFile(file),
  normalizePreviewTarget: (target, baseDir) => lazyPreview().then(m => m.normalizePreviewTarget(target, baseDir)),
  watchPreviewFile: (url) => lazyFiles().then(m => m.watchPreviewFile(url)),
  watchDirectory: (dir) => lazyFiles().then(m => m.watchDirectory(dir)),
  stopPreviewFileWatch: (id) => lazyFiles().then(m => m.stopPreviewFileWatch(id)),
  setActiveWork: (payload) => lazySystem().then(m => m.setActiveWork(payload)),
  setTitleBarTheme: payload => fire(WindowService.SetTitleBarTheme(payload)),
  setNativeTheme: mode => fire(WindowService.SetNativeTheme(mode)),
  setTranslucency: payload => fire(WindowService.SetTranslucency(payload)),
  setKeepAwake: (mode) => lazySystem().then(m => m.setKeepAwake(mode)),
  onKeepAwakeOffer: callback => subscribe('keep-awake-offer', callback, true),
  minimizeToTray: {
    get: () => WindowService.MinimizeToTrayGet(),
    set: on => WindowService.MinimizeToTraySet(on),
    onChanged: callback => subscribe('minimize-to-tray:changed', callback)
  },
  setDisableF12: (blocked) => lazyChrome().then(m => m.setDisableF12(blocked)),
  setF12ShortcutActive: (active) => lazyOverlays().then(m => m.setF12ShortcutActive(active)),
  onF12Shortcut: callback => subscribe('f12-shortcut', callback),
  setPreviewShortcutActive: (active) => lazyOverlays().then(m => m.setPreviewShortcutActive(active)),
  setPreviewGuestHidden: (webContentsId, hidden) => lazyPreview().then(m => m.setPreviewGuestHidden(webContentsId, hidden)),
  openExternal: (url) => lazyPreview().then(m => m.openExternal(url)),
  freeTierChallenge: {
    run: (request) => lazyPreview().then(m => m.freeTierChallengeRun(request))
  },
  mcpOauth: {
    listen: () => lazyConnections().then(m => m.mcpOauthListen()),
    wait: (id, timeoutMs) => lazyConnections().then(m => m.mcpOauthWait(id, timeoutMs)),
    cancel: (id) => lazyConnections().then(m => m.mcpOauthCancel(id))
  },
  openPreviewInBrowser: (url) => lazyPreview().then(m => m.openPreviewInBrowser(url)),
  reachPreviewUrl: (url) => lazyPreview().then(m => m.reachPreviewUrl(url)),
  setActiveConnectionRoute: (route) => lazyBackend().then(m => m.setActiveConnectionRoute(route)),
  fetchLinkTitle: (url) => lazyPreview().then(m => m.fetchLinkTitle(url)),
  resolveFavicon: (url) => lazyPreview().then(m => m.resolveFavicon(url)),
  sanitizeWorkspaceCwd: (cwd) => lazyFiles().then(m => m.sanitizeWorkspaceCwd(cwd)),
  settings: {
    getDefaultProjectDir: () => lazyStorage().then(m => m.settingsGetDefaultProjectDir()),
    setDefaultProjectDir: (dir) => lazyStorage().then(m => m.settingsSetDefaultProjectDir(dir)),
    pickDefaultProjectDir: () => lazyStorage().then(m => m.settingsPickDefaultProjectDir())
  },
  preparedSubmissions: {
    read: () => lazyStorage().then(m => m.preparedSubmissionsRead()),
    update: (key, entry) => lazyStorage().then(m => m.preparedSubmissionsUpdate(key, entry)),
    compareAndSet: (key, expected, entry) => lazyStorage().then(m => m.preparedSubmissionsCompareAndSet(key, expected, entry))
  },
  zoom: {
    get: () => WindowService.ZoomGet(),
    factor: () => zoomFactor(),
    setPercent: percent => fire(WindowService.ZoomSetPercent(percent)),
    onChanged: callback => subscribe('zoom:changed', callback)
  },
  revealLogs: () => lazyStorage().then(m => m.revealLogs()),
  getRecentLogs: () => lazyStorage().then(m => m.getRecentLogs()),
  reportRendererError: (report) => lazyStorage().then(m => m.reportRendererError(report)),
  logLine: (line) => lazyStorage().then(m => m.logLine(line)),
  readDir: (dirPath) => lazyFiles().then(m => m.readDir(dirPath)),
  gitRoot: (startPath) => lazyFiles().then(m => m.gitRoot(startPath)),
  revealPath: (targetPath) => lazyFiles().then(m => m.revealPath(targetPath)),
  openDir: (dirPath) => lazyFiles().then(m => m.openDir(dirPath)),
  desktopPluginsRoot: () => lazyFiles().then(m => m.desktopPluginsRoot()),
  reconcileDesktopPlugins: () => lazyFiles().then(m => m.reconcileDesktopPlugins()),
  reconcileDesktopPluginsReport: () => lazyFiles().then(m => m.reconcileDesktopPluginsReport()),
  logsRoot: (profile) => lazyFiles().then(m => m.logsRoot(profile)),
  renamePath: (targetPath, newName) => lazyFiles().then(m => m.renamePath(targetPath, newName)),
  writeTextFile: (filePath, content) => lazyFiles().then(m => m.writeTextFile(filePath, content)),
  trashPath: (targetPath) => lazyFiles().then(m => m.trashPath(targetPath)),
  pluginFiles: {
    request: (pluginId, labels) => lazyFiles().then(m => m.pluginFilesRequest(pluginId, labels)),
    grants: (pluginId) => lazyFiles().then(m => m.pluginFilesGrants(pluginId)),
    revoke: (pluginId, folderId) => lazyFiles().then(m => m.pluginFilesRevoke(pluginId, folderId)),
    op: (pluginId, request) => lazyFiles().then(m => m.pluginFilesOp(pluginId, request)),
    onChanged: callback => subscribe('pluginFiles:changed', callback)
  },
  git: {
    worktreeList: (repoPath) => lazyGit().then(m => m.gitWorktreeList(repoPath)),
    worktreeAdd: (repoPath, options) => lazyGit().then(m => m.gitWorktreeAdd(repoPath, options)),
    worktreeRemove: (repoPath, worktreePath, options) => lazyGit().then(m => m.gitWorktreeRemove(repoPath, worktreePath, options)),
    branchSwitch: (repoPath, branch) => lazyGit().then(m => m.gitBranchSwitch(repoPath, branch)),
    branchList: (repoPath) => lazyGit().then(m => m.gitBranchList(repoPath)),
    baseBranchList: (repoPath) => lazyGit().then(m => m.gitBaseBranchList(repoPath)),
    repoStatus: (repoPath) => lazyGit().then(m => m.gitRepoStatus(repoPath)),
    fileDiff: (repoPath, filePath) => lazyGit().then(m => m.gitFileDiff(repoPath, filePath)),
    scanRepos: (roots, options) => lazyGit().then(m => m.gitScanRepos(roots, options)),
    review: {
      list: (repoPath, scope, baseRef) => lazyGit().then(m => m.gitReviewList(repoPath, scope, baseRef)),
      diff: (repoPath, filePath, scope, baseRef, staged) => lazyGit().then(m => m.gitReviewDiff(repoPath, filePath, scope, baseRef, staged)),
      stage: (repoPath, filePath) => lazyGit().then(m => m.gitReviewStage(repoPath, filePath)),
      unstage: (repoPath, filePath) => lazyGit().then(m => m.gitReviewUnstage(repoPath, filePath)),
      revert: (repoPath, filePath) => lazyGit().then(m => m.gitReviewRevert(repoPath, filePath)),
      revParse: (repoPath, ref) => lazyGit().then(m => m.gitReviewRevParse(repoPath, ref)),
      commit: (repoPath, message, push) => lazyGit().then(m => m.gitReviewCommit(repoPath, message, push)),
      commitContext: (repoPath) => lazyGit().then(m => m.gitReviewCommitContext(repoPath)),
      push: (repoPath) => lazyGit().then(m => m.gitReviewPush(repoPath)),
      shipInfo: (repoPath) => lazyGit().then(m => m.gitReviewShipInfo(repoPath)),
      prList: (repoPath, branches, numbers) => lazyGit().then(m => m.gitReviewPrList(repoPath, branches, numbers)),
      createPr: (repoPath) => lazyGit().then(m => m.gitReviewCreatePr(repoPath))
    }
  },
  terminal: {
    attach: (id) => lazyPty().then(m => m.terminalAttach(id)),
    cwd: (id) => lazyPty().then(m => m.terminalCwd(id)),
    dispose: (id) => lazyPty().then(m => m.terminalDispose(id)),
    resize: (id, size) => lazyPty().then(m => m.terminalResize(id, size)),
    start: (options) => lazyPty().then(m => m.terminalStart(options)),
    write: (id, data) => lazyPty().then(m => m.terminalWrite(id, data)),
    onData: (id, callback) => subscribe(`terminal:${id}:data`, callback),
    onExit: (id, callback) => subscribe(`terminal:${id}:exit`, callback)
  },
  onClosePreviewRequested: callback => subscribe('close-preview-requested', callback),
  onPreviewNav: callback => subscribe('preview-nav', callback),
  onOpenFolderRequested: callback => subscribe('open-folder-requested', callback),
  onOpenUpdatesRequested: callback => subscribe('open-updates', callback),
  onDeepLink: callback => subscribe('deep-link', callback),
  signalDeepLinkReady: () => LifecycleService.SignalDeepLinkReady(),
  probePluginRepo: (payload) => lazyFiles().then(m => m.probePluginRepo(payload)),
  installDesktopPlugin: (payload) => lazyFiles().then(m => m.installDesktopPlugin(payload)),
  removeDesktopPlugin: (payload) => lazyFiles().then(m => m.removeDesktopPlugin(payload)),
  onWindowStateChanged: callback => subscribe('window-state-changed', callback),
  onFocusSession: callback => subscribe('focus-session', callback),
  onNotificationAction: callback => subscribe('notification-action', callback),
  onNotificationActivate: callback => subscribe('notification-activate', callback),
  onExternalOpenFailed: callback => subscribe('external-open-failed', callback),
  onPreviewFileChanged: callback => subscribe('preview-file-changed', callback),
  onBackendExit: callback => subscribe('backend-exit', callback),
  onConnectionApplied: callback => subscribe('connection:applied', callback, true),
  onPowerResume: callback => subscribe('power-resume', callback),
  getOnBattery: () => lazySystem().then(m => m.getOnBattery()),
  onBatteryChanged: callback => subscribe('power-battery', callback),
  onBootProgress: callback => subscribe('boot-progress', callback),
  getBootstrapState: () => lazyUpdater().then(m => m.getBootstrapState()),
  probeLocalBackend: () => lazyBackend().then(m => m.probeLocalBackend()),
  continueBootstrapLocal: () => lazyUpdater().then(m => m.continueBootstrapLocal()),
  recycleBackend: (profile) => lazyBackend().then(m => m.recycleBackend(profile)),
  resetBootstrap: () => lazyUpdater().then(m => m.resetBootstrap()),
  updateHold: {
    recheck: () => lazyUpdater().then(m => m.updateHoldRecheck()),
    quit: () => lazyUpdater().then(m => m.updateHoldQuit()),
    startAnyway: (request) => lazyUpdater().then(m => m.updateHoldStartAnyway(request))
  },
  repairBootstrap: () => lazyUpdater().then(m => m.repairBootstrap()),
  cancelBootstrap: () => lazyUpdater().then(m => m.cancelBootstrap()),
  onBootstrapEvent: callback => subscribe('bootstrap:event', callback),
  getVersion: (scope) => lazyUpdater().then(m => m.getVersion(scope)),
  relaunchApp: () => lazyUpdater().then(m => m.relaunchApp()),
  getMachineProfile: () => lazyConnections().then(m => m.getMachineProfile()),
  getRemoteDisplayReason: () => lazyConnections().then(m => m.getRemoteDisplayReason()),
  uninstall: {
    summary: () => lazyUpdater().then(m => m.uninstallSummary()),
    run: (mode) => lazyUpdater().then(m => m.uninstallRun(mode)),
    openAppsSettings: () => lazyUpdater().then(m => m.uninstallOpenAppsSettings())
  },
  updates: {
    check: (opts) => lazyUpdater().then(m => m.updatesCheck(opts)),
    apply: (opts) => lazyUpdater().then(m => m.updatesApply(opts)),
    setChannel: (name) => lazyUpdater().then(m => m.updatesSetChannel(name)),
    onProgress: callback => subscribe('updates:progress', callback),
    takePendingRun: () => lazyUpdater().then(m => m.updatesTakePendingRun()),
    ackPendingRun: (sent) => lazyUpdater().then(m => m.updatesAckPendingRun(sent)),
    onPendingRun: callback => subscribe('updates:metric:pending', callback, true)
  },
  desktopMetrics: {
    setEnabled: (on, profile) => lazyUpdater().then(m => m.desktopMetricsSetEnabled(on, profile)),
    takeRendererCrashes: () => lazyUpdater().then(m => m.desktopMetricsTakeRendererCrashes()),
    ackRendererCrashes: (sent) => lazyUpdater().then(m => m.desktopMetricsAckRendererCrashes(sent))
  },
  themes: {
    fetchMarketplace: (id) => lazyPreview().then(m => m.themesFetchMarketplace(id)),
    searchMarketplace: (query) => lazyPreview().then(m => m.themesSearchMarketplace(query))
  },
  findInPage: (query, options) => lazyChrome().then(m => m.findInPage(query, options)),
  stopFindInPage: () => lazyChrome().then(m => m.stopFindInPage()),
  onFoundInPage: callback => subscribe('found-in-page', callback),
  onOpenFindBarRequested: callback => subscribe('open-find-bar', callback, true)
}

Object.defineProperty(window, 'hermesDesktop', { value: Object.freeze(hermesDesktop), configurable: false, writable: false })
