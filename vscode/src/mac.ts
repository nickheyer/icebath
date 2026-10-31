import * as vscode from 'vscode';
import { flash } from './status';

const section = 'terminal.integrated';
const setting = 'macOptionIsMeta';
const declinedKey = 'icebath.optionAsMeta.declined';

let memory: vscode.Memento | undefined;
let offered = false;

// Keeps the global state a refusal is remembered in
export function activateMac(context: vscode.ExtensionContext): void {
  memory = context.globalState;
}

// Reports whether the integrated terminal sends Option as Alt, which every shortcut of the TUI needs on macOS
function optionIsMeta(): boolean {
  return vscode.workspace.getConfiguration(section).get<boolean>(setting, false) === true;
}

// Turns Option into Alt for every integrated terminal of this user and forgets an earlier refusal
export async function enableOptionAsMeta(): Promise<void> {
  await vscode.workspace.getConfiguration(section).update(setting, true, vscode.ConfigurationTarget.Global);
  await memory?.update(declinedKey, undefined);
  flash('Option sends Alt in the integrated terminal now');
}

// Offers once per window to turn Option into Alt when a TUI runs in one of its terminals on macOS
export async function offerOptionAsMeta(): Promise<void> {
  if (process.platform !== 'darwin' || offered || optionIsMeta() || memory?.get<boolean>(declinedKey) === true) return;
  offered = true;
  const enable = 'Enable';
  const never = 'Never ask';
  const pick = await vscode.window.showInformationMessage(
    'icebath shortcuts use Option as Alt, which the integrated terminal only sends when "Option is Meta" is on',
    enable,
    never,
  );
  if (pick === enable) {
    await enableOptionAsMeta();
  } else if (pick === never) {
    await memory?.update(declinedKey, true);
  }
}
