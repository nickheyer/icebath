import * as vscode from 'vscode';
import { allChanges, closeChange, refresh, showChange } from './diffs';
import { Connection, UpdateEvent, connections, onDidConnect } from './sessions';

// Update listeners of the connections open now, each dropped when its connection closes
const watching = new Set<vscode.Disposable>();

// Opens the file changes of every TUI as diffs, the ones connected now and the ones to come
export function activateChanges(context: vscode.ExtensionContext): void {
  for (const conn of connections) watch(conn);
  context.subscriptions.push(onDidConnect(watch), {
    dispose: () => {
      for (const sub of watching) sub.dispose();
      watching.clear();
    },
  });
}

function watch(conn: Connection): void {
  const sub = conn.onUpdate((ev) => void updated(ev));
  watching.add(sub);
  conn.onDidClose(() => {
    sub.dispose();
    watching.delete(sub);
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
  if (!ev.connection.ready) return;
  await showChange({ id, connection: ev.connection, path: diff.path, oldText: diff.oldText, newText: diff.newText, done: update.status === 'completed' });
}

// Closes the proposals of a TUI that went away, keeping the diffs of changes it wrote
async function closed(conn: Connection): Promise<void> {
  for (const change of allChanges()) {
    if (change.connection === conn && !change.done) await closeChange(change.id);
  }
  refresh();
}
