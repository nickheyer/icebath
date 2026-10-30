import * as path from 'node:path';
import * as vscode from 'vscode';
import type { Connection } from './sessions';
import { flash } from './status';

export interface Change {
  id: string;
  connection: Connection;
  path: string;
  oldText: string | null;
  newText: string;
  // Written to disk, so the diff may be reverted
  done: boolean;
}

const beforeScheme = 'icebath-before';
const afterScheme = 'icebath-after';

const changes = new Map<string, Change>();

// Serves the before and after text of every change under its two schemes
class Provider implements vscode.TextDocumentContentProvider {
  readonly changed = new vscode.EventEmitter<vscode.Uri>();
  readonly onDidChange = this.changed.event;

  provideTextDocumentContent(uri: vscode.Uri): string {
    const change = changes.get(uri.query);
    if (!change) return '';
    return uri.scheme === beforeScheme ? (change.oldText ?? '') : change.newText;
  }
}

const provider = new Provider();

function uriOf(change: Change, scheme: string): vscode.Uri {
  return vscode.Uri.from({ scheme, path: '/' + path.basename(change.path), query: change.id });
}

// Every change with a diff open
export function allChanges(): Change[] {
  return [...changes.values()];
}

// Change behind the active editor, on either side of its diff
function activeChange(): Change | undefined {
  const uri = vscode.window.activeTextEditor?.document.uri;
  if (!uri || (uri.scheme !== afterScheme && uri.scheme !== beforeScheme)) return undefined;
  return changes.get(uri.query);
}

// Sets the context key the Revert button reads from the active diff
export function refresh(): void {
  void vscode.commands.executeCommand('setContext', 'icebath.revertable', activeChange()?.done === true);
}

// Opens the diff of a change or refreshes the one open
export async function showChange(next: Change): Promise<void> {
  const have = changes.get(next.id);
  changes.set(next.id, next);
  if (!have) {
    await open(next);
    return;
  }
  provider.changed.fire(uriOf(next, beforeScheme));
  provider.changed.fire(uriOf(next, afterScheme));
  refresh();
}

// Opens the diff, revealing the editor area even behind a maximized panel, then gives the keyboard back to the TUI
async function open(change: Change): Promise<void> {
  const title = `icebath: ${path.basename(change.path)}`;
  await vscode.commands.executeCommand('vscode.diff', uriOf(change, beforeScheme), uriOf(change, afterScheme), title, { preview: false, preserveFocus: false, viewColumn: columnFor() });
  change.connection.terminal?.show(false);
  refresh();
}

// Group icebath diffs already sit in, else beside a terminal shown as an editor, else the active group
function columnFor(): vscode.ViewColumn {
  for (const group of vscode.window.tabGroups.all) {
    for (const tab of group.tabs) {
      if (tab.input instanceof vscode.TabInputTextDiff && tab.input.modified.scheme === afterScheme) return group.viewColumn;
    }
  }
  if (vscode.window.tabGroups.activeTabGroup.activeTab?.input instanceof vscode.TabInputTerminal) return vscode.ViewColumn.Beside;
  return vscode.ViewColumn.Active;
}

// Forgets a change and closes its diff
export async function closeChange(id: string): Promise<void> {
  const change = changes.get(id);
  if (!change) return;
  changes.delete(id);
  const after = uriOf(change, afterScheme).toString();
  for (const group of vscode.window.tabGroups.all) {
    for (const tab of group.tabs) {
      if (tab.input instanceof vscode.TabInputTextDiff && tab.input.modified.toString() === after) {
        await vscode.window.tabGroups.close(tab);
      }
    }
  }
  refresh();
}

// Writes the before text back, or deletes a file the change created, then closes the diff
async function revert(change: Change): Promise<void> {
  const file = vscode.Uri.file(change.path);
  if (change.oldText === null) {
    await vscode.workspace.fs.delete(file);
  } else {
    await vscode.workspace.fs.writeFile(file, Buffer.from(change.oldText, 'utf8'));
  }
  await closeChange(change.id);
  flash(`reverted ${vscode.workspace.asRelativePath(change.path)}`);
}

// Registers the content provider, the Revert command and the context key tracking
export function activateDiffs(context: vscode.ExtensionContext): void {
  context.subscriptions.push(
    vscode.workspace.registerTextDocumentContentProvider(beforeScheme, provider),
    vscode.workspace.registerTextDocumentContentProvider(afterScheme, provider),
    vscode.window.onDidChangeActiveTextEditor(() => refresh()),
    vscode.commands.registerCommand('icebath.revert', async () => {
      const change = activeChange();
      if (change?.done) await revert(change);
    }),
  );
  refresh();
}
