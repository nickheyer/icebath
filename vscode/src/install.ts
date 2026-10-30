import { execFile } from 'node:child_process';
import * as os from 'node:os';
import * as path from 'node:path';
import * as vscode from 'vscode';
import { flash } from './status';

const installUnix = 'curl -fsSL icebath.app | sh';
const installWindows = 'irm icebath.app | iex';

const pollInterval = 3000;
const pollDeadline = 5 * 60 * 1000;

// Binary the setting names, else icebath on PATH
export function configuredPath(): string {
  const set = vscode.workspace.getConfiguration('icebath').get<string>('path', '').trim();
  return set !== '' ? set : 'icebath';
}

// Places the site installer puts the binary when PATH does not reach it yet
function candidates(): string[] {
  const set = vscode.workspace.getConfiguration('icebath').get<string>('path', '').trim();
  if (set !== '') return [set];
  if (process.platform === 'win32') {
    const local = process.env.LOCALAPPDATA ?? path.join(os.homedir(), 'AppData', 'Local');
    return ['icebath', path.join(local, 'Programs', 'icebath', 'icebath.exe')];
  }
  return ['icebath', path.join(os.homedir(), '.local', 'bin', 'icebath')];
}

// Reports whether the binary at the path answers -version with its own name
function answers(binary: string): Promise<boolean> {
  return new Promise((resolve) => {
    execFile(binary, ['-version'], { timeout: 5000 }, (err, stdout) => {
      resolve(!err && String(stdout).trimStart().startsWith('icebath'));
    });
  });
}

// Runs the site installer in a terminal and reports in the status bar once the binary answers
export async function installIcebath(): Promise<void> {
  const terminal = vscode.window.createTerminal({ name: 'icebath install' });
  terminal.show();
  terminal.sendText(process.platform === 'win32' ? installWindows : installUnix, true);
  const end = Date.now() + pollDeadline;
  while (Date.now() < end) {
    await new Promise((r) => setTimeout(r, pollInterval));
    for (const binary of candidates()) {
      if (await answers(binary)) {
        flash(`installed at ${binary}`);
        return;
      }
    }
  }
  flash('the binary did not appear within five minutes, check the install terminal');
}
