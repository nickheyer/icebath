import * as vscode from 'vscode';
import { allChanges, closeChange, refresh, showChange } from './diffs';
import { Connection, UpdateEvent, connections, onDidConnect } from './sessions';

// Opens the file changes of every TUI as diffs, the ones connected now and the ones to come
export function activateChanges(context: vscode.ExtensionContext): void {
  for (const conn of connections) watch(conn, context);
  context.subscriptions.push(onDidConnect((conn) => watch(conn, context)));
}

function watch(conn: Connection, context: vscode.ExtensionContext): void {
  const sub = conn.onUpdate((ev) => void updated(ev));
  context.subscriptions.push(sub);
  conn.onDidClose(() => {
    sub.dispose();
    void closed(conn);
  });
}

// Opens a proposed change as a diff, settles it with what was written, or closes it when the call failed
async function updated(ev: UpdateEvent): Promise<void> {
  const update = ev.update;
  const id = `${ev.connection.id}:${update.toolCallId}`;
  if (update.status === 'failed') {
    await closeChange(id);
    return;
  }
  const diff = update.content?.find((c) => c.type === 'diff');
  if (!diff) return;
  await ev.connection.whenReady;
  await showChange({ id, connection: ev.connection, path: diff.path, oldText: diff.oldText, newText: diff.newText, done: update.status === 'completed' });
}

// Closes the proposals of a TUI that went away, keeping the diffs of changes it wrote
async function closed(conn: Connection): Promise<void> {
  for (const change of allChanges()) {
    if (change.connection === conn && !change.done) await closeChange(change.id);
  }
  refresh();
}
