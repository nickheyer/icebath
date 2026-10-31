import * as vscode from 'vscode';

export type Direction = 'in' | 'out' | 'info';

const marks: Record<Direction, string> = { in: '<-', out: '->', info: '##' };

// Longest string a logged message keeps, file bodies past it showing as their length
const longest = 200;

let channel: vscode.OutputChannel | undefined;

// Creates the ACP traffic channel and disposes it with the extension
export function createLog(context: vscode.ExtensionContext): void {
  channel = vscode.window.createOutputChannel('icebath ACP');
  context.subscriptions.push(channel);
}

// Appends one timestamped line of traffic or an event note
export function log(dir: Direction, text: string): void {
  channel?.appendLine(`${new Date().toISOString().slice(11, 23)} ${marks[dir]} ${text}`);
}

// JSON of a message with every long string replaced by its length, so file bodies stay out of the channel
export function brief(msg: unknown): string {
  return JSON.stringify(msg, (_key, value: unknown) => (typeof value === 'string' && value.length > longest ? `<${value.length} chars>` : value));
}

// Reveals the channel without taking focus
export function showLog(): void {
  channel?.show(true);
}
