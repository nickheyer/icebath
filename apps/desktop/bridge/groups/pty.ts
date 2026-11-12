// pty group bridge, its Go service TerminalService lands with the pty lane
import * as TerminalService from '../bindings/github.com/nickheyer/icebath/apps/desktop/terminalservice.js'
import { fire } from '../runtime'

export const openSessionInTerminal = (sessionId, opts) => TerminalService.OpenSessionInTerminal(sessionId, opts)
export const terminalAttach = (id) => TerminalService.TerminalAttach(id)
export const terminalCwd = (id) => TerminalService.TerminalCwd(id)
export const terminalDispose = (id) => TerminalService.TerminalDispose(id)
export const terminalResize = (id, size) => TerminalService.TerminalResize(id, size)
export const terminalStart = (options) => TerminalService.TerminalStart(options)
export const terminalWrite = (id, data) => TerminalService.TerminalWrite(id, data)
