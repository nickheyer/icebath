/**
 * Which persisted keys are machine-level UI preferences (#145213).
 *
 * Only these leave localStorage for the machine's shared store, so native
 * Desktop and every browser that opens this machine's Webapp show the same
 * choices. Scoped families keep their scope inside the key name (a
 * `….profile.<name>` or per-connection suffix): only the location changes.
 *
 * Deliberately NOT here (they stay in this browser/window):
 * - credentials, grants and connection config: gateway registry, custom model
 *   endpoints, plugin decisions and preview-script grants, embed permissions;
 * - per-window or per-device runtime state: layout tree, open tiles and tabs,
 *   last route/session, scroll, zoom, pet and popout geometry, keep-awake;
 * - drafts, queues, journals and caches: composer queue, in-flight turns,
 *   pending/prepared submissions, previews, pricing, backend skin cache, the
 *   "provider configured" onboarding cache (each backend answers that itself);
 * - one-off notices: butterbar, update and real-profile prompts.
 */

const SHARED_KEYS: ReadonlySet<string> = new Set([
  // Appearance.
  'icebath-desktop-theme-v2',
  'icebath-desktop-mode-v1',
  'icebath-desktop-profile-themes-v1',
  'icebath-desktop-profile-modes-v1',
  'icebath-desktop-user-themes-v1',
  'icebath.desktop.backdrop.v1',
  'icebath.desktop.chat-text-scale.v1',
  'icebath.desktop.interfaceMode.v1',
  'icebath.desktop.textDirection',
  'icebath.desktop.translucency.v2',
  'icebath.desktop.user-bubble-transparency.v1',
  // Sessions sidebar and profile rail.
  'icebath.desktop.agentsGroupedByWorkspace',
  'icebath.desktop.profileColors',
  'icebath.desktop.profileOrder',
  'icebath.desktop.profileRailVisible',
  'icebath.desktop.sessionListDensity',
  'icebath.desktop.sessionOrder',
  'icebath.desktop.sessionOrder.manual',
  'icebath.desktop.showAllProfiles',
  'icebath.desktop.sidebarAgentsGrouped.allProfiles',
  'icebath.desktop.sidebarCardRows',
  'icebath.desktop.sidebarGrouping',
  'icebath.desktop.sidebarGrouping.allProfiles',
  'icebath.desktop.sidebarRowMeta',
  'icebath.desktop.sidebarShowAllSessions',
  'icebath.desktop.sidebarShowArchived',
  'icebath.desktop.sidebarSortKey',
  // Status bar.
  'icebath.desktop.statusbarHidden.v2',
  'icebath.desktop.statusbarVisible.v2',
  // Transcript and tools.
  'icebath.desktop.hideThreadTimeline',
  'icebath.desktop.reasoning.collapsedByDefault',
  'icebath.desktop.toolView.hideCodeDiffs',
  'icebath.desktop.toolView.technical',
  // Shell, panes and behaviour.
  'icebath.desktop.alwaysExternalLinks.v1',
  'icebath.desktop.capabilities.catalogCardView',
  'icebath.desktop.composerPopout.enabled',
  'icebath.desktop.composerPopout.gesturesEnabled',
  'icebath.desktop.files.showIgnored',
  'icebath.desktop.keybinds',
  'icebath.desktop.layoutPresets.v2',
  'icebath.desktop.panesFlipped',
  'icebath.desktop.tabStripDefault',
  'icebath.desktop.titlebarAppActions',
  // Guidance: what the user already skipped, saw or dismissed does not come
  // back in every new browser or origin (#145600).
  'icebath-onboarding-skipped-v1',
  'icebath.desktop.tips.last.v1',
  'icebath.desktop.tips.next.v1',
  'icebath.desktop.tips.retired.v1',
  'icebath.desktop.tips.rotation.v1',
  'icebath.desktop.tips.shownAt.v1',
  'icebath.desktop.tours.v1',
  // Sound and media.
  'icebath.desktop.autoSpeakReplies',
  'icebath.desktop.completionSoundVariantId',
  'icebath.desktop.hapticsMuted',
  'icebath.desktop.videoPlaybackSpeed',
  // Model picker.
  'icebath.desktop.collapsed-providers',
  'icebath.desktop.favorite-models',
  // visible-models is a diff against known-models: they only make sense together.
  'icebath.desktop.known-models',
  'icebath.desktop.model-presets',
  'icebath.desktop.visible-models'
])

/** Families whose scope rides in the key suffix (gateway aliases/order/collapsed). */
const SHARED_PREFIXES: readonly string[] = ['icebath.desktop.sidebar.gatewayGroups.v1.']

export function isSharedPreferenceKey(key: string): boolean {
  return SHARED_KEYS.has(key) || SHARED_PREFIXES.some(prefix => key.startsWith(prefix))
}
