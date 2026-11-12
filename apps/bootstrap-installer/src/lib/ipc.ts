import { Events, System } from '@wailsio/runtime'

import { SetupService } from '../bindings/github.com/nickheyer/icebath/apps/bootstrap-installer'

// Installer transport over the Wails v3 runtime under the command, event and window names the screens use

export type UnlistenFn = () => void
export type Theme = 'light' | 'dark'

interface CommandArgs {
  args?: {
    commit: string | null
    branch: string | null
    include_desktop: boolean
    hermes_home: string | null
  }
  installRoot?: string
}

/** Runs one installer command on the bound Go service */
export async function invoke<T>(cmd: string, args?: CommandArgs): Promise<T> {
  switch (cmd) {
    case 'get_mode':
      return (await SetupService.GetMode()) as T

    case 'start_bootstrap':
      if (!args?.args) {
        throw new Error('start_bootstrap needs args')
      }

      return (await SetupService.StartBootstrap(args.args)) as T

    case 'cancel_bootstrap':
      return (await SetupService.CancelBootstrap()) as T

    case 'get_bootstrap_status':
      return (await SetupService.GetBootstrapStatus()) as T

    case 'start_update':
      return (await SetupService.StartUpdate()) as T

    case 'launch_hermes_desktop':
      return (await SetupService.LaunchHermesDesktop(args?.installRoot ?? '')) as T

    case 'get_log_path':
      return (await SetupService.GetLogPath()) as T

    case 'get_hermes_home':
      return (await SetupService.GetHermesHome()) as T

    case 'open_log_dir':
      return (await SetupService.OpenLogDir()) as T
  }

  throw new Error(`unknown installer command: ${cmd}`)
}

/** Subscribes to one backend event channel with its payload under `payload` */
export async function listen<T>(event: string, handler: (event: { payload: T }) => void): Promise<UnlistenFn> {
  return Events.On(event, ev => handler({ payload: ev.data as T }))
}

async function systemTheme(): Promise<Theme> {
  return (await System.IsDarkMode()) ? 'dark' : 'light'
}

/** Installer window appearance, read from the system and followed live */
export function getCurrentWindow(): {
  theme: () => Promise<Theme | null>
  onThemeChanged: (handler: (event: { payload: Theme }) => void) => Promise<UnlistenFn>
} {
  return {
    theme: systemTheme,
    onThemeChanged: async handler =>
      Events.On(Events.Types.Common.ThemeChanged, () => {
        void systemTheme().then(payload => handler({ payload }))
      })
  }
}
