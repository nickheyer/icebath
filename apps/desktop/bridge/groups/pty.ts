// pty group bridge over TerminalService
import * as TerminalService from '../bindings/github.com/nickheyer/icebath/apps/desktop/terminalservice.js'

export const openSessionInTerminal = (sessionId, opts) => TerminalService.OpenSessionInTerminal(sessionId, opts)
export const terminalAttach = (id) => TerminalService.TerminalAttach(id)
export const terminalCwd = (id) => TerminalService.TerminalCwd(id)
export const terminalDispose = (id) => TerminalService.TerminalDispose(id)
export const terminalResize = (id, size) => TerminalService.TerminalResize(id, size)
export const terminalStart = (options) => TerminalService.TerminalStart(options)
export const terminalWrite = (id, data) => TerminalService.TerminalWrite(id, data)
