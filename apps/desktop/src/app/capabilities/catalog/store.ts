import { Codecs, persistentAtom } from '@/lib/persisted'

// One browsing layout across Skills and Plugins; Installed keeps its own list.
export const $catalogCardView = persistentAtom('icebath.desktop.capabilities.catalogCardView', true, Codecs.bool)
