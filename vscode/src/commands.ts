import * as vscode from 'vscode';
import { TextBlock } from './acp';
import { configuredPath, installIcebath } from './install';
import { enableOptionAsMeta } from './mac';
import { Connection, active, shortId } from './sessions';
import { flash } from './status';

// Registers the palette commands that drive the connected TUI
export function registerCommands(context: vscode.ExtensionContext): void {
  context.subscriptions.push(
    vscode.commands.registerCommand('icebath.open', open),
    vscode.commands.registerCommand('icebath.install', installIcebath),
    vscode.commands.registerCommand('icebath.optionAsMeta', enableOptionAsMeta),
    vscode.commands.registerCommand('icebath.mode', () => withConnection(pickMode)),
    vscode.commands.registerCommand('icebath.sessions', () => withConnection(pickSession)),
    vscode.commands.registerCommand('icebath.prompt', () => withConnection(prompt)),
    vscode.commands.registerCommand('icebath.cancel', () => withConnection((conn) => conn.cancel())),
  );
}

// Starts icebath in a new integrated terminal at the workspace root
function open(): void {
  const terminal = vscode.window.createTerminal({ name: 'icebath', cwd: vscode.workspace.workspaceFolders?.[0]?.uri });
  terminal.show();
  terminal.sendText(shellWord(configuredPath()), true);
}

// Runs the action on the active connection, reporting in the status bar when there is none or it fails
async function withConnection(fn: (conn: Connection) => Promise<void> | void): Promise<void> {
  const conn = active();
  if (!conn) {
    flash('no TUI is connected to this window, run icebath in a terminal');
    return;
  }
  try {
    await fn(conn);
  } catch (err) {
    flash(err instanceof Error ? err.message : String(err));
  }
}

async function pickMode(conn: Connection): Promise<void> {
  const items = conn.availableModes.map((m) => ({
    label: m.name,
    description: m.id === conn.modeId ? 'current' : undefined,
    detail: m.description,
    id: m.id,
  }));
  const pick = await vscode.window.showQuickPick(items, { placeHolder: 'icebath mode' });
  if (pick && pick.id !== conn.modeId) await conn.setMode(pick.id);
}

// Lists the stored sessions of the project and asks the TUI to open the chosen one
async function pickSession(conn: Connection): Promise<void> {
  const list = await conn.listSessions();
  if (list.length === 0) {
    flash('no stored sessions for this project');
    return;
  }
  const items = list.map((s) => ({
    label: s.title || shortId(s.sessionId),
    description: `${ago(s.updated)}${s.busy ? ' · open' : ''}`,
    detail: s.sessionId,
    id: s.sessionId,
  }));
  const pick = await vscode.window.showQuickPick(items, { placeHolder: 'icebath sessions', matchOnDetail: true });
  if (pick) await conn.load(pick.id);
}

// Sends a typed prompt, with the editor selection beside it only when the setting allows
async function prompt(conn: Connection): Promise<void> {
  const text = await vscode.window.showInputBox({ prompt: `Prompt for ${conn.label}`, placeHolder: 'Ask icebath', ignoreFocusOut: true });
  if (!text || text.trim() === '') return;
  const blocks: TextBlock[] = [{ type: 'text', text }];
  if (vscode.workspace.getConfiguration('icebath').get<boolean>('editorContext', false)) {
    const context = editorContext();
    if (context) blocks.push({ type: 'text', text: context });
  }
  const reply = await conn.prompt(blocks);
  if (reply.stopReason !== 'end_turn') flash(`the run ended with ${reply.stopReason}`);
}

// Path and selection of the active file, undefined without a file editor
function editorContext(): string | undefined {
  const editor = vscode.window.activeTextEditor;
  if (!editor || editor.document.uri.scheme !== 'file') return undefined;
  const file = vscode.workspace.asRelativePath(editor.document.uri);
  const selection = editor.selection;
  if (selection.isEmpty) return `Editor context: ${file}`;
  return `Editor context: ${file} lines ${selection.start.line + 1}-${selection.end.line + 1}\n${editor.document.getText(selection)}`;
}

// Words for how long ago a time was
function ago(iso: string): string {
  const ms = Date.now() - Date.parse(iso);
  if (!Number.isFinite(ms)) return iso;
  const minutes = Math.round(ms / 60000);
  if (minutes < 1) return 'just now';
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours} h ago`;
  return `${Math.round(hours / 24)} d ago`;
}

function shellWord(text: string): string {
  return /\s/.test(text) ? JSON.stringify(text) : text;
}
