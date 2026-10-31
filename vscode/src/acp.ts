import * as net from 'node:net';
import * as vscode from 'vscode';
import { brief, log } from './log';

type RpcId = number | string;

interface RpcErrorObject {
  code: number;
  message: string;
  data?: unknown;
}

interface RpcMessage {
  jsonrpc: '2.0';
  id?: RpcId | null;
  method?: string;
  params?: unknown;
  result?: unknown;
  error?: RpcErrorObject;
}

type NotificationHandler = (params: unknown) => void;

interface Waiting {
  resolve: (value: unknown) => void;
  reject: (err: Error) => void;
}

// JSON-RPC 2.0 client over a socket, one JSON object per line, refusing any request the agent sends
export class Peer {
  private next = 1;
  private buffer = '';
  private closed = false;
  private readonly pending = new Map<RpcId, Waiting>();
  private readonly notifications = new Map<string, NotificationHandler>();
  private readonly closeEmitter = new vscode.EventEmitter<void>();
  readonly onDidClose = this.closeEmitter.event;

  constructor(private readonly socket: net.Socket) {
    socket.setEncoding('utf8');
    socket.on('data', (chunk: string) => this.receive(chunk));
    socket.on('close', () => this.end());
    socket.on('error', (err) => {
      log('info', `socket error ${err.message}`);
      this.end();
    });
  }

  onNotify(method: string, fn: NotificationHandler): void {
    this.notifications.set(method, fn);
  }

  // Sends a request and settles with its result or error, every open one failing when the connection ends
  request<T>(method: string, params: unknown): Promise<T> {
    if (this.closed) return Promise.reject(new Error('connection closed'));
    const id = this.next++;
    return new Promise<T>((resolve, reject) => {
      this.pending.set(id, { resolve: (value) => resolve(value as T), reject });
      this.send({ jsonrpc: '2.0', id, method, params });
    });
  }

  notify(method: string, params: unknown): void {
    this.send({ jsonrpc: '2.0', method, params });
  }

  close(): void {
    this.socket.destroy();
    this.end();
  }

  private send(msg: RpcMessage): void {
    if (this.closed) return;
    log('out', brief(msg));
    this.socket.write(JSON.stringify(msg) + '\n');
  }

  // Fails every open request, tells the listeners once and frees them
  private end(): void {
    if (this.closed) return;
    this.closed = true;
    for (const waiting of this.pending.values()) waiting.reject(new Error('connection closed'));
    this.pending.clear();
    this.notifications.clear();
    this.closeEmitter.fire();
    this.closeEmitter.dispose();
  }

  private receive(chunk: string): void {
    this.buffer += chunk;
    let at = this.buffer.indexOf('\n');
    while (at >= 0) {
      const line = this.buffer.slice(0, at).trim();
      this.buffer = this.buffer.slice(at + 1);
      if (line !== '') this.dispatch(line);
      at = this.buffer.indexOf('\n');
    }
  }

  // Routes one line to a notification handler or the waiting caller
  private dispatch(line: string): void {
    let msg: RpcMessage;
    try {
      msg = JSON.parse(line) as RpcMessage;
    } catch (err) {
      log('info', `unparseable line of ${line.length} chars, ${err instanceof Error ? err.message : String(err)}`);
      return;
    }
    log('in', brief(msg));
    if (typeof msg.method === 'string') {
      if (msg.id === undefined || msg.id === null) {
        this.notifications.get(msg.method)?.(msg.params);
        return;
      }
      this.send({ jsonrpc: '2.0', id: msg.id, error: { code: -32601, message: `method not found ${msg.method}` } });
      return;
    }
    if (msg.id === undefined || msg.id === null) return;
    const waiting = this.pending.get(msg.id);
    if (!waiting) return;
    this.pending.delete(msg.id);
    if (msg.error) {
      waiting.reject(new Error(msg.error.message));
    } else {
      waiting.resolve(msg.result);
    }
  }
}

export interface InitializeResponse {
  protocolVersion: number;
  sessionId: string;
  version: string;
  pid: number;
  modes: ModeState;
}

export interface Mode {
  id: string;
  name: string;
  description: string;
}

export interface ModeState {
  currentModeId: string;
  availableModes: Mode[];
}

export interface LoadSessionResponse {
  modes: ModeState;
}

export interface TextBlock {
  type: 'text';
  text: string;
}

export interface PromptResponse {
  stopReason: string;
}

export interface DiffContent {
  type: 'diff';
  path: string;
  oldText: string | null;
  newText: string;
}

// File change proposed while the TUI asks, then completed with what was written or failed
export interface ToolCallUpdate {
  sessionUpdate: 'tool_call' | 'tool_call_update';
  toolCallId: string;
  status: 'pending' | 'completed' | 'failed';
  content?: DiffContent[];
}

export interface ModeUpdate {
  sessionUpdate: 'current_mode_update';
  currentModeId: string;
}

export interface SessionNotification {
  sessionId: string;
  update: ToolCallUpdate | ModeUpdate;
}

export interface State {
  sessionId: string;
  title: string;
  running: boolean;
  waiting: boolean;
  current: boolean;
}

export interface SessionEntry {
  sessionId: string;
  title: string;
  updated: string;
  busy: boolean;
}

export interface SessionsList {
  sessions: SessionEntry[];
}
