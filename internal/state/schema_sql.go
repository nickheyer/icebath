package state

// Schema version a writable open advances a store to, drift caught by the read probe instead of this number
const SchemaVersion = 32

// Fraction of reclaimable pages below which automatic maintenance skips VACUUM
const AutoVacuumMinFreelistRatio = 0.25

// Principal the profile scheduler admits every cron fire under
const CronPrincipalID = "cron-owner"

// Every table, index and column of the session store in the order a fresh store creates them
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
    runtime_revision INTEGER NOT NULL DEFAULT 0,
    runtime_generation INTEGER NOT NULL DEFAULT 0,
    source TEXT NOT NULL,
    created_source TEXT,
    user_id TEXT,
    session_key TEXT,
    chat_id TEXT,
    chat_type TEXT,
    thread_id TEXT,
    display_name TEXT,
    origin_json TEXT,
    expiry_finalized INTEGER DEFAULT 0,
    model TEXT,
    model_config TEXT,
    system_prompt TEXT,
    system_prompt_hash TEXT,
    parent_session_id TEXT,
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
    git_metadata_generation INTEGER NOT NULL DEFAULT 0,
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
    last_activity_provenance TEXT,
    api_call_count INTEGER DEFAULT 0,
    handoff_state TEXT,
    handoff_platform TEXT,
    handoff_error TEXT,
    compression_failure_cooldown_until REAL,
    compression_failure_error TEXT,
    compression_fallback_streak INTEGER NOT NULL DEFAULT 0,
    compression_ineffective_count INTEGER NOT NULL DEFAULT 0,
    compression_recovery_deadline REAL,
    compression_overload_streak INTEGER NOT NULL DEFAULT 0,
    profile_name TEXT,
    transport_profile TEXT,
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
    effect_disposition TEXT,
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
    _compressed_summary INTEGER NOT NULL DEFAULT 0,
    active INTEGER NOT NULL DEFAULT 1,
    compacted INTEGER NOT NULL DEFAULT 0,
    api_content TEXT,
    display_kind TEXT,
    display_metadata TEXT,
    display_identity BLOB,
    display_order INTEGER,
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

CREATE TABLE IF NOT EXISTS runtime_epoch (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    epoch INTEGER NOT NULL CHECK (epoch > 0),
    instance_id TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS session_admissions (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    admission_id TEXT NOT NULL UNIQUE,
    request_id TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    target_session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
    lineage_json TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    payload_digest TEXT NOT NULL,
    intent TEXT NOT NULL CHECK (intent IN ('queue','steer','redirect')),
    status TEXT NOT NULL CHECK (status IN ('queued','started','unknown','terminal')),
    outcome TEXT,
    owner_epoch INTEGER,
    generation INTEGER,
    UNIQUE (principal_id, target_session_id, request_id),
    CHECK (status != 'started' OR (owner_epoch IS NOT NULL AND generation IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS session_admissions_pending
    ON session_admissions(target_session_id, status, seq);
-- Cron run history finds a job's unified-runtime runs by the scheduler principal's request ids
-- (cron:<job>:<execution>); without this a lookup walked
-- every cron admission of every job. Partial: interactive turns, the bulk of admissions, pay
-- nothing for it. The constant leading column gives the planner equality + range over a covering
-- index, so it beats the UNIQUE autoindex's bare principal_id prefix even without ANALYZE stats.
-- The WHERE is built from CRON_PRINCIPAL_ID, the literal the run queries inline, so they cannot drift.
CREATE INDEX IF NOT EXISTS idx_session_admissions_cron_request
    ON session_admissions(principal_id, request_id, target_session_id) WHERE principal_id = '` + CronPrincipalID + `';
CREATE TABLE IF NOT EXISTS worker_executions (
    execution_id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT,
    kind TEXT NOT NULL CHECK (kind IN ('cron','child','compute','kanban')),
    owner_epoch INTEGER NOT NULL,
    generation INTEGER NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('registered','running','unknown','terminal')),
    adoption_digest TEXT NOT NULL,
    last_sequence INTEGER NOT NULL DEFAULT 0 CHECK (last_sequence >= 0)
);
-- Lineage-wide worker exclusion (claim/register/reset) probes every row of a conversation's lineage
-- inside the write transaction; without this each probe scanned the whole (only-growing) table.
CREATE INDEX IF NOT EXISTS idx_worker_executions_session_status
    ON worker_executions(session_id, status);
CREATE TABLE IF NOT EXISTS worker_receipts (
    execution_id TEXT NOT NULL REFERENCES worker_executions(execution_id) ON DELETE RESTRICT,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    payload_digest TEXT NOT NULL,
    result_json TEXT NOT NULL,
    PRIMARY KEY (execution_id, sequence)
);

CREATE TABLE IF NOT EXISTS state_meta (
    key TEXT PRIMARY KEY,
    value TEXT
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

-- Monotonic conversation generation per routing peer (#96811).
--
-- A host-declared conversation key (X-Icebath-Session-Key / build_session_key)
-- is per-CHAT and outlives any single conversation on it, so the prompt-cache
-- affinity scope derived from it must be qualified by which conversation is
-- currently live. Deriving that from the session rows themselves
-- (COUNT/MAX over _RESET_END_REASONS boundaries) cannot prove non-reuse:
-- delete_session() and bulk pruning remove ended rows, so an aggregate can
-- return a pair it already emitted and hand a new conversation a retired
-- affinity identity.
--
-- This counter lives outside prunable session history and only ever
-- increments, once per boundary actually written, so a generation can never
-- be reused for a peer even if every session row behind it is deleted.
--
-- These rows are deliberately NEVER garbage-collected, including when every
-- session row for the peer is gone. Collecting one resets that peer to "no
-- generation", so its next boundary writes generation = 1 again and re-issues
-- a gwk_ scope a retired conversation already used - exactly the ABA this
-- table exists to close. Do not add it to delete_session()'s cascade or to any
-- prune sweep. One (TEXT, TEXT, INTEGER) row per routing peer is the intended,
-- bounded cost.
CREATE TABLE IF NOT EXISTS conversation_generations (
    source TEXT NOT NULL,
    session_key TEXT NOT NULL,
    generation INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (source, session_key)
);

-- Per-backend liveness heartbeat (#94895). Each serve / tui_gateway process
-- registers a row at startup and refreshes last_heartbeat periodically.
-- The startup orphan sweep (sessions.startup_orphan_reap) consults this
-- table to avoid reaping rows whose owning backend is still alive but
-- just idle (multi-backend state.db shared by isolated serve processes).
-- A backend whose last_heartbeat is older than the heartbeat staleness
-- window is treated as dead; rows without ANY matching heartbeat fall back
-- to the original staleness predicate so legacy deployments keep working.
CREATE TABLE IF NOT EXISTS gateway_heartbeats (
    backend_id TEXT PRIMARY KEY,
    pid INTEGER NOT NULL,
    started_at REAL NOT NULL,
    last_heartbeat REAL NOT NULL,
    profile TEXT NOT NULL DEFAULT '',
    host TEXT NOT NULL DEFAULT ''
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

CREATE TABLE IF NOT EXISTS async_delegations (
    delegation_id TEXT PRIMARY KEY,
    origin_session TEXT NOT NULL,
    origin_ui_session_id TEXT NOT NULL DEFAULT '',
    parent_session_id TEXT,
    state TEXT NOT NULL,
    dispatched_at REAL NOT NULL,
    completed_at REAL,
    updated_at REAL NOT NULL,
    event_json TEXT,
    result_json TEXT,
    delivery_state TEXT NOT NULL DEFAULT 'pending',
    delivery_attempts INTEGER NOT NULL DEFAULT 0,
    delivered_at REAL,
    owner_pid INTEGER,
    owner_started_at INTEGER,
    task_json TEXT,
    -- Owner-side execution fence (unified runtime).
    owner_execution_id TEXT,
    delivery_claim TEXT,
    delivery_claimed_at REAL,
    -- Mirrors the delegation tool's own CREATE TABLE (tools/async_delegation.py
    -- _initialize_schema). Keeping the canonical fresh-install shape identical
    -- to the tool's avoids a silent schema drift: the tool's lazy
    -- ALTER TABLE ADD COLUMN used to be the only source of this column, so two
    -- databases at the same schema_version had different
    -- async_delegations shapes depending on whether the delegation tool had
    -- ever run, breaking rebuild/replay pipelines that reconstruct state.db
    -- from the canonical schema (#94691).
    origin_session_id TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_sessions_source ON sessions(source);
CREATE INDEX IF NOT EXISTS idx_sessions_source_id ON sessions(source, id);
CREATE INDEX IF NOT EXISTS idx_sessions_parent ON sessions(parent_session_id);
CREATE INDEX IF NOT EXISTS idx_sessions_started ON sessions(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id, id);
-- Partial index for the Insights assistant tool-call scan
-- (agent/insights.py _get_tool_usage / _get_skill_usage): those queries filter
-- messages by role='assistant' AND tool_calls IS NOT NULL, a small fraction of
-- rows on a large state.db. role and tool_calls are base columns, so this can
-- live in SCHEMA_SQL rather than DEFERRED_INDEX_SQL.
CREATE INDEX IF NOT EXISTS idx_messages_assistant_calls_by_session
    ON messages(session_id)
    WHERE role = 'assistant' AND tool_calls IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_compression_locks_expires ON compression_locks(expires_at);
CREATE INDEX IF NOT EXISTS idx_session_turn_leases_expires ON session_turn_leases(expires_at);
CREATE INDEX IF NOT EXISTS idx_session_model_usage_session ON session_model_usage(session_id);
CREATE INDEX IF NOT EXISTS idx_session_model_usage_model ON session_model_usage(model);
CREATE INDEX IF NOT EXISTS idx_async_delegations_delivery
    ON async_delegations(delivery_state, completed_at);
`

// Triggers and indexes on reconciler-added columns, run after column reconciliation so legacy stores accept them
const deferredIndexSQL = `
DROP TRIGGER IF EXISTS sessions_runtime_ledger_delete_guard;
CREATE TRIGGER IF NOT EXISTS sessions_runtime_ledger_delete_guard
BEFORE DELETE ON sessions
WHEN EXISTS (SELECT 1 FROM session_admissions WHERE target_session_id = old.id)
  OR EXISTS (SELECT 1 FROM worker_executions WHERE session_id = old.id)
BEGIN
    SELECT RAISE(ABORT, 'Refused to delete a session that has gateway runtime history: state.db was upgraded by a newer icebath, and this older icebath cannot delete such sessions. Downgrading is unsupported. Run icebath update to go back to the newer release, or restore the pre-update snapshot: https://icebath.app/docs/getting-started/updating#rolling-back-to-an-older-release');
END;
DROP TRIGGER IF EXISTS messages_transcript_revision_update;
CREATE TRIGGER IF NOT EXISTS messages_transcript_revision_update
AFTER UPDATE OF role, content, api_content, tool_calls, tool_call_id, tool_name, reasoning, reasoning_content,
    reasoning_details ON messages
BEGIN
    -- An in-place rewrite (prompt expansion, multimodal context, a redaction) moves neither the
    -- active row count nor the highest id; this write-side revision is what a transcript-derived
    -- cache keys on (conversation_revision). Whoever wrote the row, old builds included.
    UPDATE sessions SET transcript_revision = transcript_revision + 1 WHERE id = new.session_id;
END;
CREATE INDEX IF NOT EXISTS idx_messages_session_active
    ON messages(session_id, active, timestamp);
CREATE INDEX IF NOT EXISTS idx_messages_display_page
    ON messages(session_id, display_order, active DESC, id DESC)
    WHERE active = 1 OR compacted = 1;
CREATE INDEX IF NOT EXISTS idx_messages_display_backfill
    ON messages(session_id) WHERE (display_order IS NULL OR display_identity IS NULL)
    AND (active = 1 OR compacted = 1);
CREATE INDEX IF NOT EXISTS idx_messages_display_identity
    ON messages(session_id, display_identity, display_order)
    WHERE display_identity IS NOT NULL AND (active = 1 OR compacted = 1);
DROP TRIGGER IF EXISTS messages_message_uid_insert;
CREATE TRIGGER IF NOT EXISTS messages_message_uid_insert
AFTER INSERT ON messages WHEN new.message_uid IS NULL
BEGIN
    -- Every row carries a message_uid, whoever wrote it: a build that predates the column binds NULL, so
    -- the store mints one on its behalf (the current build always binds a uid; this never fires for it).
    -- The note lives inside the body: a comment before DROP/CREATE would defeat the settled-trigger skip.
    UPDATE messages SET message_uid = lower(hex(randomblob(16))) WHERE id = new.id;
END;
DROP TRIGGER IF EXISTS messages_display_order_insert;
CREATE TRIGGER IF NOT EXISTS messages_display_order_insert
AFTER INSERT ON messages WHEN new.display_order IS NULL
BEGIN
    UPDATE messages SET display_order = COALESCE((
        SELECT display_order FROM messages
        WHERE session_id = new.session_id AND id <> new.id
          AND (active = 1 OR compacted = 1)
          AND display_identity = new.display_identity AND display_order IS NOT NULL
        ORDER BY display_order LIMIT 1
    ), new.id) WHERE id = new.id;
END;
DROP TRIGGER IF EXISTS messages_display_visibility_update;
CREATE TRIGGER IF NOT EXISTS messages_display_visibility_update
AFTER UPDATE OF active, compacted ON messages
WHEN (new.active = 1 OR new.compacted = 1) <> (old.active = 1 OR old.compacted = 1)
BEGIN
    UPDATE messages SET display_order = MIN(new.id, COALESCE((
        SELECT display_order FROM messages
        WHERE session_id = new.session_id AND id <> new.id
          AND (active = 1 OR compacted = 1)
          AND display_identity = new.display_identity AND display_order IS NOT NULL
        ORDER BY display_order LIMIT 1
    ), new.id)) WHERE id = new.id
      AND (new.active = 1 OR new.compacted = 1);
    UPDATE messages SET display_order = (SELECT display_order FROM messages WHERE id = new.id)
    WHERE session_id = new.session_id AND id <> new.id AND (active = 1 OR compacted = 1)
      AND display_identity = new.display_identity
      AND (new.active = 1 OR new.compacted = 1);
    UPDATE messages SET display_order = (
        SELECT MIN(peer.id) FROM messages AS peer
        WHERE peer.session_id = old.session_id AND (peer.active = 1 OR peer.compacted = 1)
          AND peer.display_identity = old.display_identity
    ) WHERE session_id = old.session_id AND (active = 1 OR compacted = 1)
      AND display_identity = old.display_identity
      AND NOT (new.active = 1 OR new.compacted = 1);
END;
DROP TRIGGER IF EXISTS messages_display_identity_update;
CREATE TRIGGER IF NOT EXISTS messages_display_identity_update
AFTER UPDATE OF role, content, timestamp, tool_call_id, tool_calls, tool_name,
                display_kind ON messages
WHEN new.role IS NOT old.role
  OR new.content IS NOT old.content
  OR new.timestamp IS NOT old.timestamp
  OR new.tool_call_id IS NOT old.tool_call_id
  OR new.tool_calls IS NOT old.tool_calls
  OR new.tool_name IS NOT old.tool_name
  OR new.display_kind IS NOT old.display_kind
BEGIN
    UPDATE messages SET display_identity = NULL, display_order = NULL
    WHERE id = new.id OR (
        session_id = old.session_id AND display_identity = old.display_identity
        AND (active = 1 OR compacted = 1)
    );
END;
DROP TRIGGER IF EXISTS messages_display_identity_delete;
CREATE TRIGGER IF NOT EXISTS messages_display_identity_delete
AFTER DELETE ON messages WHEN old.active = 1 OR old.compacted = 1
BEGIN
    UPDATE messages SET display_order = (
        SELECT MIN(peer.id) FROM messages AS peer
        WHERE peer.session_id = old.session_id AND (peer.active = 1 OR peer.compacted = 1)
          AND peer.display_identity = old.display_identity
    ) WHERE session_id = old.session_id AND (active = 1 OR compacted = 1)
      AND display_identity = old.display_identity;
END;
CREATE INDEX IF NOT EXISTS idx_messages_active_null
    ON messages(active) WHERE active IS NULL;
-- Logical-identity recovery (agent.transcript_repair._active_logical_message_row) runs inside the write lock.
CREATE INDEX IF NOT EXISTS idx_messages_session_uid
    ON messages(session_id, message_uid) WHERE active = 1;
CREATE INDEX IF NOT EXISTS idx_sessions_session_key
    ON sessions(session_key, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_gateway_peer
    ON sessions(source, user_id, chat_id, chat_type, thread_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_handoff_state
    ON sessions(handoff_state, started_at);
CREATE INDEX IF NOT EXISTS idx_sessions_system_prompt_hash
    ON sessions(system_prompt_hash);
CREATE INDEX IF NOT EXISTS idx_sessions_tool_names
    ON sessions(tool_names);
-- Recent-session browsing must never derive recency by scanning messages.
-- This expression is the durable, indexable approximation used to preselect
-- a small candidate set before compression-chain and preview hydration.
CREATE INDEX IF NOT EXISTS idx_sessions_effective_activity
    ON sessions(COALESCE(last_activity_at, started_at) DESC, started_at DESC);
-- CLI/TUI listings match gateway rows stored as source='local' by created_source,
-- before LIMIT. created_source is reconciled onto legacy DBs, so this index lives
-- here rather than in SCHEMA_SQL. A source-only index would walk every local row
-- (Desktop and TUI included) to find one surface.
CREATE INDEX IF NOT EXISTS idx_sessions_local_created_started
    ON sessions(created_source, started_at DESC)
    WHERE source = 'local';
`
