import { execFile } from 'node:child_process';
import * as fs from 'node:fs';
import * as net from 'node:net';
import * as os from 'node:os';
import * as path from 'node:path';
import * as vscode from 'vscode';
import { InitializeResponse, Peer } from './acp';
import { log } from './log';
import { offerOptionAsMeta } from './mac';
import { Connection, add } from './sessions';

const envVar = 'ICEBATH_ACP';

// Opens the per window listener, points every integrated terminal at it and serves each TUI that dials in
export function activateServer(context: vscode.ExtensionContext): void {
  const where = listenerPath();
  const unix = process.platform !== 'win32';
  if (unix) fs.rmSync(where, { force: true });
  const version = String((context.extension.packageJSON as { version: string }).version);
  const server = net.createServer(accept);
  let advertised: vscode.Disposable | undefined;
  server.on('error', (err) => log('info', `listener error ${err.message}`));
  server.listen(where, () => {
    if (unix) fs.chmodSync(where, 0o600);
    log('info', `listening on ${where}`);
    advertised = advertise(where, version);
  });
  const env = context.environmentVariableCollection;
  env.persistent = false;
  env.description = 'icebath VS Code bridge';
  env.replace(envVar, where);
  context.subscriptions.push({
    dispose: () => {
      advertised?.dispose();
      server.close();
      if (unix) fs.rmSync(where, { force: true });
      env.clear();
    },
  });
}

// Advertises the listening socket under the icebath configuration directory so a TUI already running in a terminal finds this window
function advertise(where: string, version: string): vscode.Disposable {
  const file = path.join(configDir(), 'ide', `${process.pid}.json`);
  const created = new Date().toISOString();
  const write = (): void => {
    try {
      fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
      const body = {
        pid: process.pid,
        socket: where,
        version,
        workspaceFolders: (vscode.workspace.workspaceFolders ?? []).map((f) => f.uri.fsPath),
        created,
      };
      fs.writeFileSync(file, JSON.stringify(body), { mode: 0o600 });
    } catch (err) {
      log('info', `advertise ${file}, ${err instanceof Error ? err.message : String(err)}`);
    }
  };
  write();
  const folders = vscode.workspace.onDidChangeWorkspaceFolders(write);
  return {
    dispose: () => {
      folders.dispose();
      fs.rmSync(file, { force: true });
    },
  };
}

// Configuration directory icebath reads, the override variable else the user configuration directory of the platform
function configDir(): string {
  const set = process.env.ICEBATH_CONFIG_DIR;
  if (set) return set;
  switch (process.platform) {
    case 'win32':
      return path.join(process.env.APPDATA || path.join(os.homedir(), 'AppData', 'Roaming'), 'icebath');
    case 'darwin':
      return path.join(os.homedir(), 'Library', 'Application Support', 'icebath');
    default:
      return path.join(process.env.XDG_CONFIG_HOME || path.join(os.homedir(), '.config'), 'icebath');
  }
}

// Socket or pipe path of this window
function listenerPath(): string {
  if (process.platform === 'win32') return `\\\\.\\pipe\\icebath-${process.pid}`;
  const dir = process.env.XDG_RUNTIME_DIR || os.tmpdir();
  return path.join(dir, `icebath-${process.pid}.sock`);
}

// Registers the connection at once so its first notifications reach the diff views, then completes the handshake
function accept(socket: net.Socket): void {
  const peer = new Peer(socket);
  const conn = new Connection(peer);
  add(conn);
  log('info', `${conn.id} connected`);
  peer.onDidClose(() => log('info', `${conn.id} closed`));
  handshake(conn).catch((err: unknown) => {
    log('info', `${conn.id} handshake failed, ${err instanceof Error ? err.message : String(err)}`);
    peer.close();
  });
}

// Initializes, binds the TUI to its terminal with its session and modes, then sees to Option on macOS
async function handshake(conn: Connection): Promise<void> {
  const init = await conn.peer.request<InitializeResponse>('initialize', { protocolVersion: 1 });
  conn.sessionId = init.sessionId;
  conn.adoptModes(init.modes);
  if (!conn.sessions.has(init.sessionId)) conn.sessions.set(init.sessionId, { sessionId: init.sessionId, title: '', state: 'idle' });
  conn.terminal = await terminalOf(init.pid);
  conn.finishHandshake();
  log('info', `${conn.id} icebath ${init.version} pid ${init.pid} session ${init.sessionId}`);
  if (conn.terminal) await offerOptionAsMeta();
}

// Integrated terminal whose shell is an ancestor of the TUI process, none when the TUI runs outside this window
async function terminalOf(pid: number): Promise<vscode.Terminal | undefined> {
  const line = await lineage(pid);
  for (const terminal of vscode.window.terminals) {
    const shell = await terminal.processId;
    if (shell !== undefined && line.has(shell)) return terminal;
  }
  return undefined;
}

// The process and every parent above it
async function lineage(pid: number): Promise<Set<number>> {
  const parents = await parentTable(pid);
  const out = new Set<number>();
  for (let p = pid; p > 1 && !out.has(p); p = parents.get(p) ?? 0) out.add(p);
  return out;
}

// Parent of every process on the way from the process up to init, as the platform reports it
function parentTable(pid: number): Promise<Map<number, number>> {
  switch (process.platform) {
    case 'linux':
      return procTable(pid);
    case 'darwin':
      return psTable();
    case 'win32':
      return cimTable(pid);
    default:
      return Promise.reject(new Error(`no process tree on ${process.platform}`));
  }
}

// Walks the stat file of each process up the chain, a process gone ending it
async function procTable(pid: number): Promise<Map<number, number>> {
  const table = new Map<number, number>();
  for (let p = pid; p > 1 && !table.has(p); ) {
    const stat = await fs.promises.readFile(`/proc/${p}/stat`, 'utf8').catch(() => '');
    const parent = Number(stat.slice(stat.lastIndexOf(')') + 2).split(' ')[1]);
    if (!Number.isFinite(parent)) break;
    table.set(p, parent);
    p = parent;
  }
  return table;
}

// Every process with its parent from one ps listing
async function psTable(): Promise<Map<number, number>> {
  return pairs(await run('ps', ['-axo', 'pid=,ppid=']));
}

// The chain from the process up, from one PowerShell run over the process class
async function cimTable(pid: number): Promise<Map<number, number>> {
  const script = `$t=@{};$p=${pid};while($p -gt 0 -and -not $t.ContainsKey($p)){$q=(Get-CimInstance Win32_Process -Filter "ProcessId=$p").ParentProcessId;if($null -eq $q){break};$t[$p]=$q;$p=$q};$t.GetEnumerator()|%{"$($_.Key) $($_.Value)"}`;
  return pairs(await run('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', script]));
}

// Lines of two numbers as a map of the first to the second
function pairs(text: string): Map<number, number> {
  const table = new Map<number, number>();
  for (const line of text.split(/\r?\n/)) {
    const [a, b] = line.trim().split(/\s+/).map(Number);
    if (Number.isFinite(a) && Number.isFinite(b)) table.set(a, b);
  }
  return table;
}

function run(cmd: string, args: string[]): Promise<string> {
  return new Promise((resolve, reject) => {
    execFile(cmd, args, { timeout: 10000 }, (err, stdout) => (err ? reject(err) : resolve(String(stdout))));
  });
}
