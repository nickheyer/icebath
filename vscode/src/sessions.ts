import * as vscode from 'vscode';
import { LoadSessionResponse, Mode, ModeState, Peer, PromptResponse, SessionEntry, SessionNotification, SessionsList, State, TextBlock, ToolCallUpdate } from './acp';

export type RunState = 'idle' | 'running' | 'waiting';

export interface SessionInfo {
  sessionId: string;
  title: string;
  state: RunState;
}

export interface UpdateEvent {
  connection: Connection;
  sessionId: string;
  update: ToolCallUpdate;
}

let nextId = 1;

// Tail of a session id, shown where no title is
export function shortId(id: string): string {
  return id.slice(-8);
}

// One TUI on its socket, with the sessions, mode and run state it reports
export class Connection {
  readonly id = `c${nextId++}`;
  ready = false;
  sessionId = '';
  modeId = '';
  availableModes: Mode[] = [];
  terminal: vscode.Terminal | undefined;
  readonly sessions = new Map<string, SessionInfo>();
  // Settles once the handshake bound the TUI to its terminal
  readonly whenReady: Promise<void>;
  private markReady!: () => void;
  private readonly updateEmitter = new vscode.EventEmitter<UpdateEvent>();
  private readonly closeEmitter = new vscode.EventEmitter<void>();
  readonly onUpdate = this.updateEmitter.event;
  readonly onDidClose = this.closeEmitter.event;

  constructor(readonly peer: Peer) {
    this.whenReady = new Promise((resolve) => {
      this.markReady = resolve;
    });
    peer.onNotify('session/update', (params) => this.update(params as SessionNotification));
    peer.onNotify('_icebath/state', (params) => this.state(params as State));
    peer.onDidClose(() => this.closed());
  }

  get title(): string {
    return this.sessions.get(this.sessionId)?.title ?? '';
  }

  get runState(): RunState {
    return this.sessions.get(this.sessionId)?.state ?? 'idle';
  }

  // Title of the current session, or the tail of its id
  get label(): string {
    return this.title || shortId(this.sessionId);
  }

  // Name of the mode in force
  get modeName(): string {
    return this.availableModes.find((m) => m.id === this.modeId)?.name ?? this.modeId;
  }

  adoptModes(modes: ModeState): void {
    this.modeId = modes.currentModeId;
    this.availableModes = modes.availableModes;
  }

  // Marks the handshake done and tells the listeners
  finishHandshake(): void {
    this.ready = true;
    this.markReady();
    changed.fire();
  }

  // Asks the TUI to open and focus the session
  async load(sessionId: string): Promise<void> {
    const reply = await this.peer.request<LoadSessionResponse>('session/load', { sessionId });
    this.adoptModes(reply.modes);
    this.sessionId = sessionId;
    if (!this.sessions.has(sessionId)) this.sessions.set(sessionId, { sessionId, title: '', state: 'idle' });
    changed.fire();
  }

  prompt(blocks: TextBlock[]): Promise<PromptResponse> {
    return this.peer.request<PromptResponse>('session/prompt', { sessionId: this.sessionId, prompt: blocks });
  }

  cancel(): void {
    this.peer.notify('session/cancel', { sessionId: this.sessionId });
  }

  async setMode(modeId: string): Promise<void> {
    await this.peer.request('session/set_mode', { sessionId: this.sessionId, modeId });
    this.modeId = modeId;
    changed.fire();
  }

  async listSessions(): Promise<SessionEntry[]> {
    const reply = await this.peer.request<SessionsList>('_icebath/sessions/list', {});
    return reply.sessions;
  }

  private update(note: SessionNotification): void {
    if (note.update.sessionUpdate === 'current_mode_update') {
      this.modeId = note.update.currentModeId;
      changed.fire();
      return;
    }
    this.updateEmitter.fire({ connection: this, sessionId: note.sessionId, update: note.update });
  }

  private state(state: State): void {
    const run: RunState = state.waiting ? 'waiting' : state.running ? 'running' : 'idle';
    this.sessions.set(state.sessionId, { sessionId: state.sessionId, title: state.title, state: run });
    if (state.current) this.sessionId = state.sessionId;
    changed.fire();
  }

  private closed(): void {
    this.closeEmitter.fire();
    remove(this);
  }
}

export const connections: Connection[] = [];

const changed = new vscode.EventEmitter<void>();
export const onDidChange = changed.event;

const connected = new vscode.EventEmitter<Connection>();
export const onDidConnect = connected.event;

export function add(conn: Connection): void {
  connections.push(conn);
  connected.fire(conn);
  changed.fire();
}

function remove(conn: Connection): void {
  const at = connections.indexOf(conn);
  if (at < 0) return;
  connections.splice(at, 1);
  changed.fire();
}

// Connection of the active terminal, else the most recently connected ready one
export function active(): Connection | undefined {
  const terminal = vscode.window.activeTerminal;
  if (terminal) {
    const match = connections.find((c) => c.ready && c.terminal === terminal);
    if (match) return match;
  }
  for (let i = connections.length - 1; i >= 0; i--) {
    if (connections[i].ready) return connections[i];
  }
  return undefined;
}
