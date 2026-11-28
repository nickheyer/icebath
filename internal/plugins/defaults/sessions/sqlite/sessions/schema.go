package sessions

import (
	"context"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/sqlitex"
)

// Characters of a tool result the search index holds; the rest of a long output is only found by scanning
const toolContentPrefix = 8192

// Sources whose sessions the trigram index leaves out: automation, whose text is rarely searched in other scripts
var trigramExcluded = []string{"cron", "subagent"}

const schema = `
CREATE TABLE IF NOT EXISTS state_meta (
	key TEXT PRIMARY KEY,
	value TEXT
);

CREATE TABLE IF NOT EXISTS system_prompts (
	hash TEXT PRIMARY KEY,
	prompt TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
	id TEXT PRIMARY KEY,
	profile TEXT NOT NULL,
	source TEXT NOT NULL,
	created_source TEXT,
	user_id TEXT,
	session_key TEXT,
	chat_id TEXT,
	chat_type TEXT,
	thread_id TEXT,
	display_name TEXT,
	origin_json TEXT,
	model TEXT,
	model_config TEXT,
	system_prompt_hash TEXT REFERENCES system_prompts(hash),
	tool_names TEXT,
	parent_session_id TEXT REFERENCES sessions(id),
	started_at REAL NOT NULL,
	ended_at REAL,
	end_reason TEXT,
	message_count INTEGER NOT NULL DEFAULT 0,
	tool_call_count INTEGER NOT NULL DEFAULT 0,
	api_call_count INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	reasoning_tokens INTEGER NOT NULL DEFAULT 0,
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
	handoff_state TEXT,
	handoff_platform TEXT,
	handoff_error TEXT,
	compression_failure_cooldown_until REAL,
	compression_failure_error TEXT,
	compression_fallback_streak INTEGER NOT NULL DEFAULT 0,
	compression_ineffective_count INTEGER NOT NULL DEFAULT 0,
	compression_recovery_deadline REAL,
	compression_overload_streak INTEGER NOT NULL DEFAULT 0,
	rewind_count INTEGER NOT NULL DEFAULT 0,
	archived INTEGER NOT NULL DEFAULT 0,
	auto_archived INTEGER NOT NULL DEFAULT 0,
	pinned INTEGER NOT NULL DEFAULT 0,
	hidden INTEGER NOT NULL DEFAULT 0,
	yolo INTEGER NOT NULL DEFAULT 0,
	last_read_at REAL,
	transcript_revision INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL REFERENCES sessions(id),
	role TEXT NOT NULL,
	content TEXT,
	tool_calls TEXT,
	tool_call_id TEXT,
	tool_name TEXT,
	timestamp REAL NOT NULL,
	token_count INTEGER,
	finish_reason TEXT,
	reasoning TEXT,
	failed INTEGER NOT NULL DEFAULT 0,
	payload BLOB,
	active INTEGER NOT NULL DEFAULT 1,
	compacted INTEGER NOT NULL DEFAULT 0,
	summary INTEGER NOT NULL DEFAULT 0,
	api_content TEXT,
	display_kind TEXT,
	model_only INTEGER NOT NULL DEFAULT 0,
	message_uid TEXT NOT NULL,
	absorbed_message_uids TEXT,
	platform_message_id TEXT,
	observed INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS session_model_usage (
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	model TEXT NOT NULL,
	billing_provider TEXT NOT NULL DEFAULT '',
	billing_base_url TEXT NOT NULL DEFAULT '',
	task TEXT NOT NULL DEFAULT '',
	api_call_count INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	reasoning_tokens INTEGER NOT NULL DEFAULT 0,
	estimated_cost_usd REAL NOT NULL DEFAULT 0,
	actual_cost_usd REAL NOT NULL DEFAULT 0,
	first_seen REAL,
	last_seen REAL,
	PRIMARY KEY (session_id, model, billing_provider, billing_base_url, task)
);

CREATE TABLE IF NOT EXISTS session_admissions (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	admission_id TEXT NOT NULL UNIQUE,
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	text TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('queued', 'started', 'unknown', 'terminal')),
	outcome TEXT,
	backend_id TEXT,
	created_at REAL NOT NULL,
	started_at REAL,
	settled_at REAL,
	attempts INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS session_turn_leases (
	session_id TEXT PRIMARY KEY,
	holder TEXT NOT NULL,
	acquired_at REAL NOT NULL,
	expires_at REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS compression_locks (
	session_id TEXT PRIMARY KEY,
	holder TEXT NOT NULL,
	acquired_at REAL NOT NULL,
	expires_at REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS backend_heartbeats (
	backend_id TEXT PRIMARY KEY,
	pid INTEGER NOT NULL,
	started_at REAL NOT NULL,
	last_heartbeat REAL NOT NULL,
	profile TEXT NOT NULL DEFAULT '',
	host TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS gateway_routing (
	scope TEXT NOT NULL DEFAULT '',
	session_key TEXT NOT NULL,
	entry_json TEXT NOT NULL,
	updated_at REAL NOT NULL,
	PRIMARY KEY (scope, session_key)
);

CREATE TABLE IF NOT EXISTS gateway_hygiene_state (
	session_key TEXT PRIMARY KEY,
	failure_streak INTEGER NOT NULL DEFAULT 0
);

-- Conversations of a chat count up here and never down, so a generation is never handed out twice even once its sessions are deleted;
-- these rows are never pruned
CREATE TABLE IF NOT EXISTS conversation_generations (
	source TEXT NOT NULL,
	session_key TEXT NOT NULL,
	generation INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (source, session_key)
);

CREATE INDEX IF NOT EXISTS idx_sessions_source ON sessions(source);
CREATE INDEX IF NOT EXISTS idx_sessions_profile ON sessions(profile);
CREATE INDEX IF NOT EXISTS idx_sessions_parent ON sessions(parent_session_id);
CREATE INDEX IF NOT EXISTS idx_sessions_started ON sessions(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_activity ON sessions(COALESCE(last_activity_at, started_at) DESC, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_session_key ON sessions(session_key, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_peer ON sessions(source, user_id, chat_id, chat_type, thread_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_handoff ON sessions(handoff_state, started_at);
CREATE INDEX IF NOT EXISTS idx_sessions_prompt ON sessions(system_prompt_hash);
CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_title_unique ON sessions(profile, title) WHERE title IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, id);
CREATE INDEX IF NOT EXISTS idx_messages_session_active ON messages(session_id, active, id);
CREATE INDEX IF NOT EXISTS idx_messages_session_uid ON messages(session_id, message_uid);
CREATE INDEX IF NOT EXISTS idx_admissions_pending ON session_admissions(session_id, status, seq);
CREATE INDEX IF NOT EXISTS idx_turn_leases_expires ON session_turn_leases(expires_at);
CREATE INDEX IF NOT EXISTS idx_compression_locks_expires ON compression_locks(expires_at);

-- An edit in place moves neither the count of messages nor the highest id; caches built from a transcript key on this revision
CREATE TRIGGER IF NOT EXISTS messages_revision AFTER UPDATE OF role, content, api_content, tool_calls, tool_call_id, tool_name, reasoning, active, compacted ON messages
BEGIN
	UPDATE sessions SET transcript_revision = transcript_revision + 1 WHERE id = new.session_id;
END;
`

// Word index over what the messages say, read through a view that cuts tool output to its indexed start, kept in step by triggers
var ftsSchema = ftsBody + ftsTriggers

// View and table of the word index
var ftsBody = `
CREATE VIEW IF NOT EXISTS messages_fts_src AS
	SELECT id,
		CASE WHEN role = 'tool' THEN substr(COALESCE(content, ''), 1, ` + strconv.Itoa(toolContentPrefix) + `) ELSE content END AS content,
		tool_name, tool_calls
	FROM messages;

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
	content,
	tool_name,
	tool_calls,
	content='messages_fts_src',
	content_rowid='id'
);
`

// Whether the triggers of an index keep the message of the id in step: one past the ids a backfill of it indexes itself, or one it
// already did; every message when no backfill runs
func untouched(prefix, id string) string {
	return "(" + id + " > COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta WHERE key = '" + prefix + "_high_water'), -1) OR " + id +
		" <= COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta WHERE key = '" + prefix + "_progress'), -1))"
}

var ftsTriggers = `
CREATE TRIGGER IF NOT EXISTS messages_fts_insert AFTER INSERT ON messages WHEN ` + untouched(wordBackfill, "new.id") + ` BEGIN
	INSERT INTO messages_fts(rowid, content, tool_name, tool_calls) VALUES (new.id,
		CASE WHEN new.role = 'tool' THEN substr(COALESCE(new.content, ''), 1, ` + strconv.Itoa(toolContentPrefix) + `) ELSE new.content END, new.tool_name, new.tool_calls);
END;

CREATE TRIGGER IF NOT EXISTS messages_fts_delete AFTER DELETE ON messages WHEN ` + untouched(wordBackfill, "old.id") + ` BEGIN
	INSERT INTO messages_fts(messages_fts, rowid, content, tool_name, tool_calls) VALUES ('delete', old.id,
		CASE WHEN old.role = 'tool' THEN substr(COALESCE(old.content, ''), 1, ` + strconv.Itoa(toolContentPrefix) + `) ELSE old.content END, old.tool_name, old.tool_calls);
END;

CREATE TRIGGER IF NOT EXISTS messages_fts_update AFTER UPDATE OF content, tool_name, tool_calls, role ON messages
WHEN (old.content IS NOT new.content OR old.tool_name IS NOT new.tool_name OR old.tool_calls IS NOT new.tool_calls OR old.role IS NOT new.role)
	AND ` + untouched(wordBackfill, "old.id") + `
BEGIN
	INSERT INTO messages_fts(messages_fts, rowid, content, tool_name, tool_calls) VALUES ('delete', old.id,
		CASE WHEN old.role = 'tool' THEN substr(COALESCE(old.content, ''), 1, ` + strconv.Itoa(toolContentPrefix) + `) ELSE old.content END, old.tool_name, old.tool_calls);
	INSERT INTO messages_fts(rowid, content, tool_name, tool_calls) VALUES (new.id,
		CASE WHEN new.role = 'tool' THEN substr(COALESCE(new.content, ''), 1, ` + strconv.Itoa(toolContentPrefix) + `) ELSE new.content END, new.tool_name, new.tool_calls);
END;
`

// Sessions whose messages the trigram index holds, over the sessions alias given
func trigramSessions(alias string) string {
	return alias + ".source NOT IN ('" + strings.Join(trigramExcluded, "', '") + "') AND json_extract(CASE WHEN json_valid(" + alias + ".model_config) THEN " + alias +
		".model_config ELSE '{}' END, '$._delegate_from') IS NULL"
}

// Index of three character runs, which finds Chinese, Japanese and Korean words the word index can't split
var trigramSchema = trigramBody + trigramTriggers

// View and table of the trigram index
var trigramBody = `
CREATE VIEW IF NOT EXISTS messages_fts_trigram_src AS
	SELECT m.id, m.content, m.tool_name
	FROM messages AS m JOIN sessions AS s ON s.id = m.session_id
	WHERE m.role <> 'tool' AND ` + trigramSessions("s") + `;

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts_trigram USING fts5(
	content,
	tool_name,
	content='messages_fts_trigram_src',
	content_rowid='id',
	tokenize='trigram'
);
`

var trigramTriggers = `
CREATE TRIGGER IF NOT EXISTS messages_fts_trigram_insert AFTER INSERT ON messages
WHEN new.role <> 'tool' AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = new.session_id AND ` + trigramSessions("s") + `)
	AND ` + untouched(trigramBackfill, "new.id") + `
BEGIN
	INSERT INTO messages_fts_trigram(rowid, content, tool_name) VALUES (new.id, new.content, new.tool_name);
END;

CREATE TRIGGER IF NOT EXISTS messages_fts_trigram_delete AFTER DELETE ON messages
WHEN old.role <> 'tool' AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = old.session_id AND ` + trigramSessions("s") + `)
	AND ` + untouched(trigramBackfill, "old.id") + `
BEGIN
	INSERT INTO messages_fts_trigram(messages_fts_trigram, rowid, content, tool_name) VALUES ('delete', old.id, old.content, old.tool_name);
END;

CREATE TRIGGER IF NOT EXISTS messages_fts_trigram_update AFTER UPDATE OF content, tool_name, role ON messages
WHEN (old.content IS NOT new.content OR old.tool_name IS NOT new.tool_name OR old.role IS NOT new.role) AND ` + untouched(trigramBackfill, "old.id") + `
BEGIN
	INSERT INTO messages_fts_trigram(messages_fts_trigram, rowid, content, tool_name)
		SELECT 'delete', old.id, old.content, old.tool_name
		WHERE old.role <> 'tool' AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = old.session_id AND ` + trigramSessions("s") + `);
	INSERT INTO messages_fts_trigram(rowid, content, tool_name)
		SELECT new.id, new.content, new.tool_name
		WHERE new.role <> 'tool' AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = new.session_id AND ` + trigramSessions("s") + `);
END;
`

// Triggers keeping the indexes in step, dropped when an index is found damaged so writes go on while search scans instead
var indexTriggers = append(wordIndex.triggerNames(), trigramIndex.triggerNames()...)

// Shape of a session database, read from the statements that make it, the trigram index's included
func declare(ctx context.Context) (*sqlitex.Schema, error) {
	return sqlitex.Declare(ctx, schema, ftsSchema, trigramSchema)
}
