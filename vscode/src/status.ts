import * as vscode from 'vscode';
import { Connection, RunState, active, connections, onDidChange } from './sessions';

const states: Record<RunState, string> = { idle: 'idle', running: '$(sync~spin) running', waiting: '$(bell) waiting' };

// Shows a line in the status bar for a few seconds
export function flash(text: string): void {
  vscode.window.setStatusBarMessage(`icebath: ${text}`, 5000);
}

// Shows the session, run state and mode of the TUI on the active terminal in the status bar
export function activateStatus(context: vscode.ExtensionContext): void {
  const session = vscode.window.createStatusBarItem('icebath.session', vscode.StatusBarAlignment.Right, 100);
  session.name = 'icebath session';
  session.command = 'icebath.sessions';
  const mode = vscode.window.createStatusBarItem('icebath.mode', vscode.StatusBarAlignment.Right, 99);
  mode.name = 'icebath mode';
  mode.command = 'icebath.mode';
  const refresh = (): void => {
    const conn = active();
    if (!conn) {
      session.hide();
      mode.hide();
      return;
    }
    session.text = `$(terminal) ${conn.label} · ${states[conn.runState]}`;
    session.tooltip = tooltip();
    mode.text = `$(shield) ${conn.modeName}`;
    mode.tooltip = 'icebath mode, click to change it';
    session.show();
    mode.show();
  };
  context.subscriptions.push(session, mode, onDidChange(refresh), vscode.window.onDidChangeActiveTerminal(refresh));
  refresh();
}

// One line per connected TUI with its session, mode and run state
function tooltip(): vscode.MarkdownString {
  const md = new vscode.MarkdownString();
  md.appendMarkdown('icebath TUIs connected to this window\n\n');
  for (const conn of connections.filter((c: Connection) => c.ready)) {
    md.appendMarkdown(`- **${conn.label}** ${conn.modeName}, ${conn.runState}`);
    if (conn.terminal) md.appendMarkdown(` in ${conn.terminal.name}`);
    md.appendMarkdown('\n');
  }
  return md;
}
