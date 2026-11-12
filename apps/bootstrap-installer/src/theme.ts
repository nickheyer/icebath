import { getCurrentWindow, type Theme } from './lib/ipc'

/*
 * OS appearance follower.
 *
 * The installer ships no in-app theme switcher, so it tracks the system the
 * way the desktop overlays do. The webview's `prefers-color-scheme` is not
 * reliable across WebView2 / WebKitGTK, so the authoritative signal is the
 * system theme the Wails runtime reports, followed live through its theme
 * change event; the media query stays the fallback outside the Wails window
 * (e.g. plain `vite preview`).
 *
 * We only flip the `.dark` class + `color-scheme`; the dark seed values live in
 * styles.css (:root.dark), mirroring apps/desktop's applyTheme() palette.
 */

const prefersDark = (): boolean => window.matchMedia('(prefers-color-scheme: dark)').matches

function paint(theme: Theme): void {
  const dark = theme === 'dark'
  const root = document.documentElement
  root.classList.toggle('dark', dark)
  root.style.colorScheme = dark ? 'dark' : 'light'
}

// Best-effort synchronous first paint from the media query so the very first
// frame is already in the right mode. Refined below by the authoritative system
// theme once the runtime call resolves.
paint(prefersDark() ? 'dark' : 'light')

/** Adopt the system theme and keep tracking live OS appearance changes. */
export async function watchTheme(): Promise<void> {
  try {
    const win = getCurrentWindow()
    const current = await win.theme()

    if (current) {
      paint(current)
    }

    await win.onThemeChanged(({ payload }) => paint(payload))
  } catch {
    // Outside the Wails window (e.g. `vite preview`): keep the media query live.
    window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', e => paint(e.matches ? 'dark' : 'light'))
  }
}
