package state

import (
	"context"
	"database/sql"
	"maps"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const LocalPolicyPrefix = "gateway.local_policy.v1:"

const promptResolvedSQL = "COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved"

var (
	cronPolicyJobSQL = "(SELECT CASE WHEN json_valid(r) THEN json_extract(r, '$.cron_job.id') END FROM" +
		" (SELECT CASE WHEN json_valid(value) THEN json_extract(value, '$.policy.request_json') END AS r" +
		" FROM state_meta WHERE key = '" + LocalPolicyPrefix + "' || target_session_id))"
	cronAdmissionSQL = "principal_id = '" + CronPrincipalID + "' AND request_id >= :request_lo AND request_id < :request_hi" +
		" AND ((instr(substr(request_id, " + strconv.Itoa(len(CronRequestPrefix)+1) + "), ':') = length(:request_lo) - " + strconv.Itoa(len(CronRequestPrefix)) +
		" AND instr(substr(request_id, length(:request_lo) + 1), ':') = 0)" +
		" OR " + cronPolicyJobSQL + " = :job_id)"
	cronRunIDsSQL = "SELECT id FROM sessions WHERE source = 'cron' AND id >= :prefix AND id < :prefix_hi" +
		" AND substr(id, length(:prefix) + 1) GLOB '[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]_[0-9][0-9][0-9][0-9][0-9][0-9]'" +
		" UNION ALL SELECT target_session_id FROM session_admissions WHERE " + cronAdmissionSQL
	cronExecutionSQL = "(SELECT substr(request_id, length(:request_lo) + 1) FROM session_admissions" +
		" WHERE target_session_id = s.id AND " + cronAdmissionSQL + " LIMIT 1)"
)

// List sessions rich shaped SELECT with resolved prompt, preview and last_active, a nil prompt dropping the join
func richSelect(selectCols, where, tail string, promptSelect *string) string {
	promptJoin := "LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash"
	prompt := ""
	if promptSelect == nil {
		promptJoin = ""
	} else {
		prompt = *promptSelect
	}
	return "\n            SELECT " + selectCols + prompt + ",\n                " + sqlPreviewRaw("s.id") + ",\n                " +
		sqlSessionLastActive("s") + " AS last_active\n            FROM sessions s\n            " + promptJoin +
		"\n            WHERE " + where + tail + "\n        "
}

// Session row dict with the raw preview shaped into preview
func richRow(row *pyjson.Object) *pyjson.Object {
	s := sessionRowDict(row)
	raw, ok := s.Get("_preview_raw")
	if !ok {
		raw = ""
	}
	s.Delete("_preview_raw")
	s.Set("preview", shapePreview(raw))
	return s
}

// Pure read query on a reader connection, never the writer lock
func (d *SessionDB) readRows(ctx context.Context, query string, args ...any) ([]*pyjson.Object, error) {
	var out []*pyjson.Object
	err := d.readCtx(ctx, true, func(conn SQLiteConn) error {
		rows, err := queryObjects(ctx, conn, query, args...)
		out = rows
		return err
	})
	return out, err
}

// Run listing query of one cron job and its named parameters
func cronJobRunsQuery(jobID string, limit, offset int) (string, []any, error) {
	prefix, prefixHi, err := PrefixRange("cron_" + jobID + "_")
	if err != nil {
		return "", nil, err
	}
	requestLo, requestHi := CronRequestRange(jobID)
	prompt := ",\n                " + promptResolvedSQL
	query := richSelect("s.*, "+cronExecutionSQL+" AS cron_execution_id", "s.source = 'cron' AND s.id IN ("+cronRunIDsSQL+")",
		"\n            ORDER BY s.started_at DESC, s.id DESC\n            LIMIT :limit OFFSET :offset", &prompt)
	return query, []any{sql.Named("prefix", prefix), sql.Named("prefix_hi", prefixHi), sql.Named("request_lo", requestLo),
		sql.Named("request_hi", requestHi), sql.Named("job_id", jobID), sql.Named("limit", limit), sql.Named("offset", offset)}, nil
}

// Run sessions of one cron job newest first in the rich row shape, stamped with the job and execution ids
func (d *SessionDB) ListCronJobRuns(ctx context.Context, jobID string, limit int) ([]*pyjson.Object, error) {
	return d.ListCronJobRunsFrom(ctx, jobID, limit, 0)
}

// Run sessions of one cron job from an offset, newest first in the rich row shape
func (d *SessionDB) ListCronJobRunsFrom(ctx context.Context, jobID string, limit, offset int) ([]*pyjson.Object, error) {
	query, args, err := cronJobRunsQuery(jobID, limit, offset)
	if err != nil {
		return nil, err
	}
	rows, err := d.readRows(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out := make([]*pyjson.Object, 0, len(rows))
	for _, row := range rows {
		r := richRow(row)
		r.Set("cron_job_id", jobID)
		out = append(out, r)
	}
	return out, nil
}

// Columns no list consumer renders, left out of the compact projection
var sessionCompactExcluded = map[string]bool{"system_prompt": true, "system_prompt_hash": true, "git_metadata_generation": true}

var (
	sessionCompactColsMu  sync.Mutex
	sessionCompactColsSQL string
)

// s prefixed SELECT list of every declared sessions column except the prompt storage internals
func compactSessionCols(ctx context.Context) (string, error) {
	sessionCompactColsMu.Lock()
	defer sessionCompactColsMu.Unlock()
	if sessionCompactColsSQL != "" {
		return sessionCompactColsSQL, nil
	}
	tables, err := parseSchemaColumns(ctx, SchemaSQL)
	if err != nil {
		return "", err
	}
	declared, _ := rowValue(tables, "sessions").(*pyjson.Object)
	if declared == nil {
		return "", &pyint.Error{Kind: "KeyError", Msg: "'sessions'"}
	}
	var cols []string
	for _, name := range declared.Keys {
		if !sessionCompactExcluded[name] {
			cols = append(cols, "s."+name)
		}
	}
	sessionCompactColsSQL = strings.Join(cols, ", ")
	return sessionCompactColsSQL, nil
}

type richRowSeams struct {
	single func(ctx context.Context, sessionID string, compactRows bool) (*pyjson.Object, error)
	batch  func(ctx context.Context, sessionIDs []string, compactRows bool) (map[string]*pyjson.Object, error)
}

// One session with the list enriched columns, nil when missing
func (d *SessionDB) GetSessionRichRow(ctx context.Context, sessionID string, compactRows bool) (*pyjson.Object, error) {
	if d.richRowSeams.single != nil {
		return d.richRowSeams.single(ctx, sessionID, compactRows)
	}
	return d.readSessionRichRow(ctx, sessionID, compactRows)
}

// Reads one enriched session row through the batched fetch
func (d *SessionDB) readSessionRichRow(ctx context.Context, sessionID string, compactRows bool) (*pyjson.Object, error) {
	rows, err := d.getSessionRichRowsBatch(ctx, []string{sessionID}, compactRows)
	if err != nil {
		return nil, err
	}
	return rows[sessionID], nil
}

// Enriched rows of many sessions keyed by id in one round trip per chunk, missing ids absent
func (d *SessionDB) getSessionRichRowsBatch(ctx context.Context, sessionIDs []string, compactRows bool) (map[string]*pyjson.Object, error) {
	if d.richRowSeams.batch != nil {
		return d.richRowSeams.batch(ctx, sessionIDs, compactRows)
	}
	return d.readSessionRichRowsBatch(ctx, sessionIDs, compactRows)
}

// Reads enriched rows in chunks of 900 with the prompt resolved unless compact
func (d *SessionDB) readSessionRichRowsBatch(ctx context.Context, sessionIDs []string, compactRows bool) (map[string]*pyjson.Object, error) {
	var ids []string
	for _, sid := range sessionIDs {
		if sid != "" {
			ids = append(ids, sid)
		}
	}
	result := map[string]*pyjson.Object{}
	if len(ids) == 0 {
		return result, nil
	}
	if len(ids) > sqlInChunk {
		for _, chunk := range idChunks(ids, sqlInChunk) {
			part, err := d.getSessionRichRowsBatch(ctx, chunk, compactRows)
			if err != nil {
				return nil, err
			}
			maps.Copy(result, part)
		}
		return result, nil
	}
	d.FlushTokenCounts(5.0)
	cols, prompt := "s.*", ", "+promptResolvedSQL
	promptSelect := &prompt
	if compactRows {
		var err error
		if cols, err = compactSessionCols(ctx); err != nil {
			return nil, err
		}
		promptSelect = nil
	}
	rows, err := d.readRows(ctx, richSelect(cols, "s.id IN ("+placeholders(len(ids))+")", "", promptSelect), stringsToAny(ids)...)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		s := richRow(row)
		result[pyStr(rowValue(s, "id"))] = s
	}
	return result, nil
}
