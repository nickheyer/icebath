package state

import "strconv"

// Full-text storage layout version, tracked apart from the schema version under the fts_storage_version meta key
const FTSStorageVersion = 3

// Characters of a tool row the base word index keeps, the rest staying searchable through LIKE
const FTSToolContentPrefixChars = 8192

// Cap on user controlled full-text query input before sanitizing
const MaxFTS5QueryChars = 2048

// Meta keys of the full-text breadcrumbs, a detached cjk index, a detached base or trigram index, and a blocked recovery
const (
	ftsCJKStaleKey        = "fts_cjk_stale"
	ftsStaleKey           = "fts_stale"
	ftsRebuildDeferralKey = "fts_rebuild_deferral"
)

// Sync trigger names of the base and trigram indexes, and of the cjk index
var (
	ftsBaseTriggers    = []string{"messages_fts_insert", "messages_fts_delete", "messages_fts_update"}
	ftsTrigramTriggers = []string{"messages_fts_trigram_insert", "messages_fts_trigram_delete", "messages_fts_trigram_update"}
	ftsTriggers        = append(append([]string{}, ftsBaseTriggers...), ftsTrigramTriggers...)
	ftsCJKTriggers     = []string{"messages_fts_cjk_insert", "messages_fts_cjk_delete", "messages_fts_cjk_update"}
)

// Sources whose transcripts stay out of the trigram index
var ftsTrigramExcludedSources = []string{"cron", "subagent"}

// Sources whose transcripts stay out of the trigram index, in their canonical order
func FTSTrigramExcludedSources() []string { return append([]string{}, ftsTrigramExcludedSources...) }

// Tool rows truncated to the indexed prefix, every other row indexed whole, for the trigger row alias
func ftsIndexedContentSQL(alias string) string {
	return "CASE WHEN " + alias + ".role = 'tool'\n" +
		"         THEN substr(COALESCE(" + alias + ".content, ''), 1, " + strconv.Itoa(FTSToolContentPrefixChars) + ")\n" +
		"         ELSE " + alias + ".content END"
}

// Sessions whose rows belong in the trigram index, alias qualifying every column
func FTSTrigramSessionSQL(alias string) string {
	q := ""
	if alias != "" {
		q = alias + "."
	}
	return q + "source NOT IN (" + sqlLiteralList(ftsTrigramExcludedSources) + ") AND " + jsonExtractSQL(q+"model_config", "$._delegate_from") + " IS NULL"
}

// Rebuild gate every external content trigger shares, rows in the pending backfill window skipped
func ftsGateSQL(alias, highWater, progress string) string {
	return "(" + alias + ".id > COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta\n" +
		"                         WHERE key = '" + highWater + "'), -1)\n" +
		"   OR " + alias + ".id <= COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta\n" +
		"                          WHERE key = '" + progress + "'), -1))"
}

// Indented rebuild gate of the trigram and cjk triggers
func ftsIndentedGateSQL(alias, highWater, progress string) string {
	return "(" + alias + ".id > COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta\n" +
		"                           WHERE key = '" + highWater + "'), -1)\n" +
		"     OR " + alias + ".id <= COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta\n" +
		"                            WHERE key = '" + progress + "'), -1))"
}

// External content base word index over the stable projection view, with its sync triggers
var ftsSQL = `
-- Stable projection the base word index reads and writes through: the view
-- computes EXACTLY what the triggers/backfill insert, so 'rebuild' and the
-- integrity checker always agree with the stored index.
CREATE VIEW IF NOT EXISTS messages_fts_src AS
    SELECT id,
           CASE WHEN role = 'tool'
                THEN substr(COALESCE(content, ''), 1, ` + strconv.Itoa(FTSToolContentPrefixChars) + `)
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
WHEN ` + ftsGateSQL("new", "fts_rebuild_high_water", "fts_rebuild_progress") + `
BEGIN
    INSERT INTO messages_fts(rowid, content, tool_name, tool_calls)
    VALUES (
        new.id,
        ` + ftsIndexedContentSQL("new") + `,
        new.tool_name,
        new.tool_calls
    );
END;

CREATE TRIGGER IF NOT EXISTS messages_fts_delete AFTER DELETE ON messages
WHEN ` + ftsGateSQL("old", "fts_rebuild_high_water", "fts_rebuild_progress") + `
BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content, tool_name, tool_calls)
    VALUES (
        'delete',
        old.id,
        ` + ftsIndexedContentSQL("old") + `,
        old.tool_name,
        old.tool_calls
    );
END;

-- UPDATE OF skips the trigger entirely for non-content column writes
-- (status/compacted/observed/etc.), which is stronger than the WHEN gate
-- alone and avoids FTS I/O saturation on large state.db (#68858 / #73639).
CREATE TRIGGER IF NOT EXISTS messages_fts_update
AFTER UPDATE OF content, tool_name, tool_calls, role ON messages
WHEN (old.content IS NOT new.content
    OR old.tool_name IS NOT new.tool_name
    OR old.tool_calls IS NOT new.tool_calls
    OR old.role IS NOT new.role)
   AND ` + ftsIndentedGateSQL("old", "fts_rebuild_high_water", "fts_rebuild_progress") + `
BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content, tool_name, tool_calls)
    VALUES (
        'delete',
        old.id,
        ` + ftsIndexedContentSQL("old") + `,
        old.tool_name,
        old.tool_calls
    );
    INSERT INTO messages_fts(rowid, content, tool_name, tool_calls)
    VALUES (
        new.id,
        ` + ftsIndexedContentSQL("new") + `,
        new.tool_name,
        new.tool_calls
    );
END;
`

// Trigram index for CJK substring search over a view that skips tool rows and excluded sessions
var ftsTrigramSQL = `
CREATE VIEW IF NOT EXISTS messages_fts_trigram_src AS
    SELECT m.id, m.role, m.content, m.tool_name
    FROM messages AS m
    JOIN sessions AS s ON s.id = m.session_id
    WHERE m.role <> 'tool' AND ` + FTSTrigramSessionSQL("s") + `;

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts_trigram USING fts5(
    content,
    tool_name,
    content='messages_fts_trigram_src',
    content_rowid='id',
    tokenize='trigram'
);

CREATE TRIGGER IF NOT EXISTS messages_fts_trigram_insert AFTER INSERT ON messages
WHEN new.role <> 'tool'
   AND EXISTS (SELECT 1 FROM sessions
               WHERE id = new.session_id AND ` + FTSTrigramSessionSQL("") + `)
   AND ` + ftsIndentedGateSQL("new", "fts_rebuild_high_water", "fts_rebuild_progress") + `
BEGIN
    INSERT INTO messages_fts_trigram(rowid, content, tool_name)
    VALUES (new.id, new.content, new.tool_name);
END;

CREATE TRIGGER IF NOT EXISTS messages_fts_trigram_delete AFTER DELETE ON messages
WHEN old.role <> 'tool'
   AND EXISTS (SELECT 1 FROM sessions
               WHERE id = old.session_id AND ` + FTSTrigramSessionSQL("") + `)
   AND ` + ftsIndentedGateSQL("old", "fts_rebuild_high_water", "fts_rebuild_progress") + `
BEGIN
    INSERT INTO messages_fts_trigram(messages_fts_trigram, rowid, content, tool_name)
    VALUES ('delete', old.id, old.content, old.tool_name);
END;

CREATE TRIGGER IF NOT EXISTS messages_fts_trigram_update
AFTER UPDATE OF content, tool_name, role ON messages
WHEN (old.content IS NOT new.content
    OR old.tool_name IS NOT new.tool_name
    OR old.role IS NOT new.role)
   AND ` + ftsIndentedGateSQL("old", "fts_rebuild_high_water", "fts_rebuild_progress") + `
BEGIN
    INSERT INTO messages_fts_trigram(messages_fts_trigram, rowid, content, tool_name)
    SELECT 'delete', old.id, old.content, old.tool_name
    WHERE old.role <> 'tool'
      AND EXISTS (SELECT 1 FROM sessions
                  WHERE id = old.session_id AND ` + FTSTrigramSessionSQL("") + `);
    INSERT INTO messages_fts_trigram(rowid, content, tool_name)
    SELECT new.id, new.content, new.tool_name
    WHERE new.role <> 'tool'
      AND EXISTS (SELECT 1 FROM sessions
                  WHERE id = new.session_id AND ` + FTSTrigramSessionSQL("") + `);
END;
`

// Inline pre-v23 base index a legacy store keeps until it opts into optimize-storage
var legacyFTSSQL = `
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    content
);

CREATE TRIGGER IF NOT EXISTS messages_fts_insert AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, content) VALUES (
        new.id,
        COALESCE(` + ftsIndexedContentSQL("new") + `, '')
        || ' ' || COALESCE(new.tool_name, '') || ' ' || COALESCE(new.tool_calls, '')
    );
END;

CREATE TRIGGER IF NOT EXISTS messages_fts_delete AFTER DELETE ON messages BEGIN
    DELETE FROM messages_fts WHERE rowid = old.id;
END;

CREATE TRIGGER IF NOT EXISTS messages_fts_update
AFTER UPDATE OF content, tool_name, tool_calls, role ON messages BEGIN
    DELETE FROM messages_fts WHERE rowid = old.id;
    INSERT INTO messages_fts(rowid, content) VALUES (
        new.id,
        COALESCE(` + ftsIndexedContentSQL("new") + `, '')
        || ' ' || COALESCE(new.tool_name, '') || ' ' || COALESCE(new.tool_calls, '')
    );
END;
`

// Inline pre-v23 trigram index of a legacy store
var legacyFTSTrigramSQL = `
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts_trigram USING fts5(
    content,
    tokenize='trigram'
);

CREATE TRIGGER IF NOT EXISTS messages_fts_trigram_insert AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts_trigram(rowid, content) VALUES (
        new.id,
        COALESCE(` + ftsIndexedContentSQL("new") + `, '')
        || ' ' || COALESCE(new.tool_name, '') || ' ' || COALESCE(new.tool_calls, '')
    );
END;

CREATE TRIGGER IF NOT EXISTS messages_fts_trigram_delete AFTER DELETE ON messages BEGIN
    DELETE FROM messages_fts_trigram WHERE rowid = old.id;
END;

CREATE TRIGGER IF NOT EXISTS messages_fts_trigram_update
AFTER UPDATE OF content, tool_name, tool_calls, role ON messages BEGIN
    DELETE FROM messages_fts_trigram WHERE rowid = old.id;
    INSERT INTO messages_fts_trigram(rowid, content) VALUES (
        new.id,
        COALESCE(` + ftsIndexedContentSQL("new") + `, '')
        || ' ' || COALESCE(new.tool_name, '') || ' ' || COALESCE(new.tool_calls, '')
    );
END;
`

// CJK bigram index table and its tool row excluding view, safe to ensure at any time
const ftsCJKTableSQL = `
CREATE VIEW IF NOT EXISTS messages_fts_cjk_src AS
    SELECT id, role, content, tool_name, tool_calls
    FROM messages
    WHERE role <> 'tool';

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts_cjk USING fts5(
    content,
    tool_name,
    tool_calls,
    content='messages_fts_cjk_src',
    content_rowid='id',
    tokenize='cjk_unicode61'
);
`

// CJK index sync triggers, installed only while the index is complete or marker gated
var ftsCJKTriggerSQL = `
CREATE TRIGGER IF NOT EXISTS messages_fts_cjk_insert AFTER INSERT ON messages
WHEN new.role <> 'tool'
   AND ` + ftsIndentedGateSQL("new", "fts_cjk_rebuild_high_water", "fts_cjk_rebuild_progress") + `
BEGIN
    INSERT INTO messages_fts_cjk(rowid, content, tool_name, tool_calls)
    VALUES (new.id, new.content, new.tool_name, new.tool_calls);
END;

CREATE TRIGGER IF NOT EXISTS messages_fts_cjk_delete AFTER DELETE ON messages
WHEN old.role <> 'tool'
   AND ` + ftsIndentedGateSQL("old", "fts_cjk_rebuild_high_water", "fts_cjk_rebuild_progress") + `
BEGIN
    INSERT INTO messages_fts_cjk(messages_fts_cjk, rowid, content, tool_name, tool_calls)
    VALUES ('delete', old.id, old.content, old.tool_name, old.tool_calls);
END;

CREATE TRIGGER IF NOT EXISTS messages_fts_cjk_update
AFTER UPDATE OF content, tool_name, tool_calls, role ON messages
WHEN (old.content IS NOT new.content
    OR old.tool_name IS NOT new.tool_name
    OR old.tool_calls IS NOT new.tool_calls
    OR old.role IS NOT new.role)
   AND ` + ftsIndentedGateSQL("old", "fts_cjk_rebuild_high_water", "fts_cjk_rebuild_progress") + `
BEGIN
    INSERT INTO messages_fts_cjk(messages_fts_cjk, rowid, content, tool_name, tool_calls)
    SELECT 'delete', old.id, old.content, old.tool_name, old.tool_calls
    WHERE old.role <> 'tool';
    INSERT INTO messages_fts_cjk(rowid, content, tool_name, tool_calls)
    SELECT new.id, new.content, new.tool_name, new.tool_calls
    WHERE new.role <> 'tool';
END;
`

// Every stored definition a released version created per index, oldest first, the last being what this code creates
var ftsVTableLayouts = map[string][]string{
	"messages_fts": {
		"CREATE VIRTUAL TABLE messages_fts USING fts5(content,content=messages,content_rowid=id)",
		"CREATE VIRTUAL TABLE messages_fts USING fts5(content)",
		"CREATE VIRTUAL TABLE messages_fts USING fts5(content,tool_name,tool_calls,content='messages',content_rowid='id')",
		"CREATE VIRTUAL TABLE messages_fts USING fts5(content,tool_name,tool_calls,content='messages_fts_src',content_rowid='id')",
	},
	"messages_fts_trigram": {
		"CREATE VIRTUAL TABLE messages_fts_trigram USING fts5(content,content=messages,content_rowid=id,tokenize='trigram')",
		"CREATE VIRTUAL TABLE messages_fts_trigram USING fts5(content,tokenize='trigram')",
		"CREATE VIRTUAL TABLE messages_fts_trigram USING fts5(content,tool_name,tool_calls,content='messages_fts_trigram_src',content_rowid='id',tokenize='trigram')",
		"CREATE VIRTUAL TABLE messages_fts_trigram USING fts5(content,tool_name,content='messages_fts_trigram_src',content_rowid='id',tokenize='trigram')",
	},
}

// Tables the layout registry covers, base then trigram
var ftsDDLTables = []string{"messages_fts", "messages_fts_trigram"}

// Every full-text table merge and optimize act on, each probed before use
var ftsTables = []string{"messages_fts", "messages_fts_trigram", "messages_fts_cjk"}
