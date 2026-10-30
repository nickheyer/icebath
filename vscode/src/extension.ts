import * as vscode from 'vscode';
import { activateChanges } from './changes';
import { registerCommands } from './commands';
import { activateDiffs } from './diffs';
import { createLog, showLog } from './log';
import { activateServer } from './server';
import { activateStatus } from './status';

// Starts the listener, the diff views, the status bar and the commands
export function activate(context: vscode.ExtensionContext): void {
  createLog(context);
  context.subscriptions.push(vscode.commands.registerCommand('icebath.showLog', showLog));
  activateServer(context);
  activateDiffs(context);
  activateChanges(context);
  activateStatus(context);
  registerCommands(context);
}

export function deactivate(): void {}
