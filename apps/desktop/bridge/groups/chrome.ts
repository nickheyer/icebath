// chrome group bridge, its Go service ChromeService lands with the chrome lane
import * as ChromeService from '../bindings/github.com/nickheyer/icebath/apps/desktop/chromeservice.js'
import { fire } from '../runtime'

export const contextMenuEdit = (command) => ChromeService.ContextMenuEdit(command)
export const contextMenuCopyImage = () => ChromeService.ContextMenuCopyImage()
export const contextMenuSpellcheck = (action) => ChromeService.ContextMenuSpellcheck(action)
export const contextMenuGuestAddWord = (payload) => ChromeService.ContextMenuGuestAddWord(payload)
export const spellcheckDictionary = (op, word) => ChromeService.SpellcheckDictionary(op, word)
export const setDisableF12 = (blocked) => fire(ChromeService.SetDisableF12(blocked))
export const findInPage = (query, options) => ChromeService.FindInPage(query, options)
export const stopFindInPage = () => ChromeService.StopFindInPage()
