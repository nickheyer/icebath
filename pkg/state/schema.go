// Session database of a home, sessions and messages in SQLite with full text search
package state

// Version the pre port schema stamps until the schema slice replaces initSchema
const goSchemaVersion = 1

// Tables and indexes every writable open applies
const schemaSQL = `
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS system_prompts (
    hash TEXT PRIMARY KEY,
    prompt TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    user_id TEXT,
    model TEXT,
    model_config TEXT,
    system_prompt_hash TEXT,
    parent_session_id TEXT,
    session_key TEXT,
    started_at REAL NOT NULL,
    ended_at REAL,
    end_reason TEXT,
    message_count INTEGER DEFAULT 0,
    tool_call_count INTEGER DEFAULT 0,
    input_tokens INTEGER DEFAULT 0,
    output_tokens INTEGER DEFAULT 0,
    cache_read_tokens INTEGER DEFAULT 0,
    cache_write_tokens INTEGER DEFAULT 0,
    reasoning_tokens INTEGER DEFAULT 0,
    cwd TEXT,
    git_branch TEXT,
    git_repo_root TEXT,
    billing_provider TEXT,
    billing_base_url TEXT,
    billing_mode TEXT,
    estimated_cost_usd REAL,
    actual_cost_usd REAL,
    cost_status TEXT,
    cost_source TEXT,
    pricing_version TEXT,
    title TEXT,
    title_source TEXT,
    last_activity_at REAL,
    last_activity_description TEXT,
    api_call_count INTEGER DEFAULT 0,
    compression_failure_cooldown_until REAL,
    compression_failure_error TEXT,
    compression_fallback_streak INTEGER NOT NULL DEFAULT 0,
    compression_ineffective_count INTEGER NOT NULL DEFAULT 0,
    profile_name TEXT,
    rewind_count INTEGER NOT NULL DEFAULT 0,
    archived INTEGER NOT NULL DEFAULT 0,
    auto_archived INTEGER NOT NULL DEFAULT 0,
    pinned INTEGER NOT NULL DEFAULT 0,
    hidden INTEGER NOT NULL DEFAULT 0,
    last_read_at REAL,
    tool_names TEXT,
    transcript_revision INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (parent_session_id) REFERENCES sessions(id),
    FOREIGN KEY (system_prompt_hash) REFERENCES system_prompts(hash)
);

CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    role TEXT NOT NULL,
    content TEXT,
    tool_call_id TEXT,
    tool_calls TEXT,
    tool_name TEXT,
    timestamp REAL NOT NULL,
    token_count INTEGER,
    finish_reason TEXT,
    reasoning TEXT,
    reasoning_content TEXT,
    reasoning_details TEXT,
    codex_reasoning_items TEXT,
    codex_message_items TEXT,
    platform_message_id TEXT,
    observed INTEGER DEFAULT 0,
    compressed_summary INTEGER NOT NULL DEFAULT 0,
    active INTEGER NOT NULL DEFAULT 1,
    compacted INTEGER NOT NULL DEFAULT 0,
    api_content TEXT,
    display_kind TEXT,
    display_metadata TEXT,
    message_uid TEXT,
    absorbed_message_uids TEXT,
    tool_call_uids TEXT,
    tool_call_uid TEXT
);

CREATE TABLE IF NOT EXISTS session_model_usage (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    model TEXT NOT NULL,
    billing_provider TEXT NOT NULL DEFAULT '',
    billing_base_url TEXT NOT NULL DEFAULT '',
    billing_mode TEXT NOT NULL DEFAULT '',
    task TEXT NOT NULL DEFAULT '',
    api_call_count INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    estimated_cost_usd REAL NOT NULL DEFAULT 0,
    actual_cost_usd REAL NOT NULL DEFAULT 0,
    cost_status TEXT,
    cost_source TEXT,
    first_seen REAL,
    last_seen REAL,
    PRIMARY KEY (session_id, model, billing_provider, billing_base_url, billing_mode, task)
);

CREATE TABLE IF NOT EXISTS state_meta (
    key TEXT PRIMARY KEY,
    value TEXT
);

CREATE TABLE IF NOT EXISTS compression_locks (
    session_id TEXT PRIMARY KEY,
    holder TEXT NOT NULL,
    acquired_at REAL NOT NULL,
    expires_at REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS session_turn_leases (
    conversation_id TEXT PRIMARY KEY,
    holder TEXT NOT NULL,
    acquired_at REAL NOT NULL,
    expires_at REAL NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_source ON sessions(source);
CREATE INDEX IF NOT EXISTS idx_sessions_parent ON sessions(parent_session_id);
CREATE INDEX IF NOT EXISTS idx_sessions_started ON sessions(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_effective_activity
    ON sessions(COALESCE(last_activity_at, started_at) DESC, started_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_title_unique ON sessions(title) WHERE title IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id, id);
CREATE INDEX IF NOT EXISTS idx_messages_session_active ON messages(session_id, active, timestamp);
CREATE INDEX IF NOT EXISTS idx_messages_session_uid ON messages(session_id, message_uid) WHERE active = 1;
CREATE INDEX IF NOT EXISTS idx_messages_assistant_calls_by_session
    ON messages(session_id) WHERE role = 'assistant' AND tool_calls IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_compression_locks_expires ON compression_locks(expires_at);
CREATE INDEX IF NOT EXISTS idx_session_turn_leases_expires ON session_turn_leases(expires_at);
CREATE INDEX IF NOT EXISTS idx_session_model_usage_session ON session_model_usage(session_id);
CREATE INDEX IF NOT EXISTS idx_session_model_usage_model ON session_model_usage(model);

CREATE TRIGGER IF NOT EXISTS messages_transcript_revision_update
AFTER UPDATE OF role, content, api_content, tool_calls, tool_call_id, tool_name, reasoning, reasoning_content,
    reasoning_details ON messages
BEGIN
    UPDATE sessions SET transcript_revision = transcript_revision + 1 WHERE id = new.session_id;
END;

CREATE TRIGGER IF NOT EXISTS messages_message_uid_insert
AFTER INSERT ON messages WHEN new.message_uid IS NULL
BEGIN
    UPDATE messages SET message_uid = lower(hex(randomblob(16))) WHERE id = new.id;
END;
`

// Word index over messages with a bounded projection of tool results
const ftsSQL = `
CREATE VIEW IF NOT EXISTS messages_fts_src AS
    SELECT id,
           CASE WHEN role = 'tool'
                THEN substr(COALESCE(content, ''), 1, 8192)
                ELSE content END AS content,
           tool_name, tool_calls
    FROM messages;

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    content,
    tool_name,
    tool_calls,
    content='messages_fts_src',
    content_rowid='id'
);

CREATE TRIGGER IF NOT EXISTS messages_fts_insert AFTER INSERT ON messages
BEGIN
    INSERT INTO messages_fts(rowid, content, tool_name, tool_calls)
    VALUES (
        new.id,
        CASE WHEN new.role = 'tool' THEN substr(COALESCE(new.content, ''), 1, 8192) ELSE new.content END,
        new.tool_name,
        new.tool_calls
    );
END;

CREATE TRIGGER IF NOT EXISTS messages_fts_delete AFTER DELETE ON messages
BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content, tool_name, tool_calls)
    VALUES (
        'delete',
        old.id,
        CASE WHEN old.role = 'tool' THEN substr(COALESCE(old.content, ''), 1, 8192) ELSE old.content END,
        old.tool_name,
        old.tool_calls
    );
END;

CREATE TRIGGER IF NOT EXISTS messages_fts_update
AFTER UPDATE OF content, tool_name, tool_calls, role ON messages
WHEN (old.content IS NOT new.content
    OR old.tool_name IS NOT new.tool_name
    OR old.tool_calls IS NOT new.tool_calls
    OR old.role IS NOT new.role)
BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content, tool_name, tool_calls)
    VALUES (
        'delete',
        old.id,
        CASE WHEN old.role = 'tool' THEN substr(COALESCE(old.content, ''), 1, 8192) ELSE old.content END,
        old.tool_name,
        old.tool_calls
    );
    INSERT INTO messages_fts(rowid, content, tool_name, tool_calls)
    VALUES (
        new.id,
        CASE WHEN new.role = 'tool' THEN substr(COALESCE(new.content, ''), 1, 8192) ELSE new.content END,
        new.tool_name,
        new.tool_calls
    );
END;
`

// Column list every message writer binds, in insert order
var messageColumns = []string{
	"session_id", "role", "content", "tool_call_id", "tool_calls", "tool_name", "timestamp", "token_count",
	"finish_reason", "reasoning", "reasoning_content", "reasoning_details", "codex_reasoning_items",
	"codex_message_items", "platform_message_id", "observed", "compressed_summary", "active", "compacted",
	"api_content", "display_kind", "display_metadata", "message_uid", "absorbed_message_uids",
	"tool_call_uids", "tool_call_uid",
}
