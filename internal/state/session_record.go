package state

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// One sessions row with its prompt storage resolved and its lineage markers lifted out of model_config
type Session struct {
	ID                string
	RuntimeRevision   int64
	RuntimeGeneration int64
	Source            string
	CreatedSource     sql.Null[string]
	UserID            sql.Null[string]
	SessionKey        sql.Null[string]
	ChatID            sql.Null[string]
	ChatType          sql.Null[string]
	ThreadID          sql.Null[string]
	DisplayName       sql.Null[string]
	OriginJSON        sql.Null[string]
	ExpiryFinalized   sql.Null[int64]
	Model             sql.Null[string]
	ModelConfig       sql.Null[string]
	// Resolved prompt text, the content addressed copy before the legacy inline column
	SystemPrompt     sql.Null[string]
	SystemPromptHash sql.Null[string]
	ParentSessionID  sql.Null[string]
	// Start stamp in epoch seconds, absent when the cell holds no number
	StartedAt                       sql.Null[float64]
	EndedAt                         sql.Null[float64]
	EndReason                       sql.Null[string]
	MessageCount                    sql.Null[int64]
	ToolCallCount                   sql.Null[int64]
	InputTokens                     sql.Null[int64]
	OutputTokens                    sql.Null[int64]
	CacheReadTokens                 sql.Null[int64]
	CacheWriteTokens                sql.Null[int64]
	ReasoningTokens                 sql.Null[int64]
	Cwd                             sql.Null[string]
	GitBranch                       sql.Null[string]
	GitRepoRoot                     sql.Null[string]
	GitMetadataGeneration           int64
	BillingProvider                 sql.Null[string]
	BillingBaseURL                  sql.Null[string]
	BillingMode                     sql.Null[string]
	EstimatedCostUSD                sql.Null[float64]
	ActualCostUSD                   sql.Null[float64]
	CostStatus                      sql.Null[string]
	CostSource                      sql.Null[string]
	PricingVersion                  sql.Null[string]
	Title                           sql.Null[string]
	TitleSource                     sql.Null[string]
	LastActivityAt                  sql.Null[float64]
	LastActivityDescription         sql.Null[string]
	LastActivityProvenance          sql.Null[string]
	APICallCount                    sql.Null[int64]
	HandoffState                    sql.Null[string]
	HandoffPlatform                 sql.Null[string]
	HandoffError                    sql.Null[string]
	CompressionFailureCooldownUntil sql.Null[float64]
	CompressionFailureError         sql.Null[string]
	CompressionFallbackStreak       int64
	CompressionIneffectiveCount     int64
	CompressionRecoveryDeadline     sql.Null[float64]
	CompressionOverloadStreak       int64
	ProfileName                     sql.Null[string]
	TransportProfile                sql.Null[string]
	RewindCount                     int64
	Archived                        int64
	AutoArchived                    int64
	Pinned                          int64
	Hidden                          int64
	LastReadAt                      sql.Null[float64]
	// Resolved tool pin, the stored array before a legacy inline name list
	ToolNames          sql.Null[string]
	TranscriptRevision int64
	// Lineage markers model_config carries, empty when absent
	ResetFrom    string
	BranchedFrom string
}

// Session row as a listing shows it, with its preview, recency and compression tip projection
type SessionListing struct {
	Session
	Preview    string
	LastActive sql.Null[float64]
	Unread     bool
	// Compression root the row was projected from, empty when the row is its own root
	LineageRootID string
	// Every chain id behind a projected row, nil when the row was not projected
	LineageIDs []string
	// The provenance of a projected row, compression for an automatic continuation
	ContinuationKind string
	lineageRoot      sql.NullString
}

// Destination of one sessions column, nil for a column the record does not carry
func (s *Session) dest(col string) any {
	switch col {
	case "id":
		return &s.ID
	case "runtime_revision":
		return &s.RuntimeRevision
	case "runtime_generation":
		return &s.RuntimeGeneration
	case "source":
		return &s.Source
	case "created_source":
		return &s.CreatedSource
	case "user_id":
		return &s.UserID
	case "session_key":
		return &s.SessionKey
	case "chat_id":
		return &s.ChatID
	case "chat_type":
		return &s.ChatType
	case "thread_id":
		return &s.ThreadID
	case "display_name":
		return &s.DisplayName
	case "origin_json":
		return &s.OriginJSON
	case "expiry_finalized":
		return &s.ExpiryFinalized
	case "model":
		return &s.Model
	case "model_config":
		return &s.ModelConfig
	case "system_prompt", "_system_prompt_resolved":
		return &s.SystemPrompt
	case "system_prompt_hash":
		return &s.SystemPromptHash
	case "parent_session_id":
		return &s.ParentSessionID
	case "started_at":
		return &s.StartedAt
	case "ended_at":
		return &s.EndedAt
	case "end_reason":
		return &s.EndReason
	case "message_count":
		return &s.MessageCount
	case "tool_call_count":
		return &s.ToolCallCount
	case "input_tokens":
		return &s.InputTokens
	case "output_tokens":
		return &s.OutputTokens
	case "cache_read_tokens":
		return &s.CacheReadTokens
	case "cache_write_tokens":
		return &s.CacheWriteTokens
	case "reasoning_tokens":
		return &s.ReasoningTokens
	case "cwd":
		return &s.Cwd
	case "git_branch":
		return &s.GitBranch
	case "git_repo_root":
		return &s.GitRepoRoot
	case "git_metadata_generation":
		return &s.GitMetadataGeneration
	case "billing_provider":
		return &s.BillingProvider
	case "billing_base_url":
		return &s.BillingBaseURL
	case "billing_mode":
		return &s.BillingMode
	case "estimated_cost_usd":
		return &s.EstimatedCostUSD
	case "actual_cost_usd":
		return &s.ActualCostUSD
	case "cost_status":
		return &s.CostStatus
	case "cost_source":
		return &s.CostSource
	case "pricing_version":
		return &s.PricingVersion
	case "title":
		return &s.Title
	case "title_source":
		return &s.TitleSource
	case "last_activity_at":
		return &s.LastActivityAt
	case "last_activity_description":
		return &s.LastActivityDescription
	case "last_activity_provenance":
		return &s.LastActivityProvenance
	case "api_call_count":
		return &s.APICallCount
	case "handoff_state":
		return &s.HandoffState
	case "handoff_platform":
		return &s.HandoffPlatform
	case "handoff_error":
		return &s.HandoffError
	case "compression_failure_cooldown_until":
		return &s.CompressionFailureCooldownUntil
	case "compression_failure_error":
		return &s.CompressionFailureError
	case "compression_fallback_streak":
		return &s.CompressionFallbackStreak
	case "compression_ineffective_count":
		return &s.CompressionIneffectiveCount
	case "compression_recovery_deadline":
		return &s.CompressionRecoveryDeadline
	case "compression_overload_streak":
		return &s.CompressionOverloadStreak
	case "profile_name":
		return &s.ProfileName
	case "transport_profile":
		return &s.TransportProfile
	case "rewind_count":
		return &s.RewindCount
	case "archived":
		return &s.Archived
	case "auto_archived":
		return &s.AutoArchived
	case "pinned":
		return &s.Pinned
	case "hidden":
		return &s.Hidden
	case "last_read_at":
		return &s.LastReadAt
	case "tool_names", "_tool_names_resolved":
		return &s.ToolNames
	case "transcript_revision":
		return &s.TranscriptRevision
	}
	return nil
}

// Destination of one listing column, the session columns first
func (l *SessionListing) dest(col string) any {
	switch col {
	case "_preview_raw":
		return &l.Preview
	case "last_active":
		return &l.LastActive
	case "_lineage_root_id":
		return &l.lineageRoot
	}
	return l.Session.dest(col)
}

// Scans every row through dest, a later column naming the same field winning and unknown columns dropped
func scanRecords[T any](rows *sql.Rows, dest func(r *T, col string) any) ([]T, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []T
	for rows.Next() {
		var r T
		args := make([]any, len(cols))
		last := map[any]int{}
		for i, col := range cols {
			if d := dest(&r, col); d != nil {
				args[i] = d
				last[d] = i
			}
		}
		for i := range args {
			if args[i] == nil || last[args[i]] != i {
				args[i] = new(any)
			} else {
				args[i] = lenient{dst: args[i]}
			}
		}
		if err := rows.Scan(args...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Every session row of a query with its markers lifted
func querySessions(rows *sql.Rows, err error) ([]Session, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanRecords(rows, func(s *Session, col string) any { return s.dest(col) })
	for i := range out {
		out[i].liftMarkers()
	}
	return out, err
}

// Every listing row of a query with markers lifted and previews shaped
func queryListings(rows *sql.Rows, err error) ([]SessionListing, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanRecords(rows, func(l *SessionListing, col string) any { return l.dest(col) })
	for i := range out {
		out[i].liftMarkers()
		out[i].Preview = shapePreview(out[i].Preview)
		out[i].LineageRootID = out[i].lineageRoot.String
	}
	return out, err
}

// Lifts the reset and branch markers out of model_config so payloads without it still tell forks apart
func (s *Session) liftMarkers() {
	if s.ResetFrom != "" && s.BranchedFrom != "" {
		return
	}
	cfg := ParseModelConfig(s.ModelConfig.V)
	if s.ResetFrom == "" {
		s.ResetFrom = markerText(cfg["_reset_from"])
	}
	if s.BranchedFrom == "" {
		s.BranchedFrom = markerText(cfg["_branched_from"])
	}
}

// Trimmed text of a JSON string marker, empty for anything else
func markerText(raw json.RawMessage) string {
	var v string
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

// Top level members of model_config JSON text, empty for text that is not a JSON object
func ParseModelConfig(text string) map[string]json.RawMessage {
	t := bytes.TrimSpace([]byte(text))
	if len(t) == 0 || t[0] != '{' {
		return map[string]json.RawMessage{}
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(t, &m) != nil || m == nil {
		return map[string]json.RawMessage{}
	}
	return m
}

// Reports whether a unread watermark predates the row's activity, an untracked row reading as read
func sessionUnread(lastReadAt, lastActive, startedAt sql.Null[float64]) bool {
	if !lastReadAt.Valid {
		return false
	}
	active := startedAt.V
	if lastActive.Valid && lastActive.V != 0 {
		active = lastActive.V
	}
	return active > lastReadAt.V
}

// Workspace grouping key of a row, its git root, else its cwd, empty when it has neither
func WorkspaceKey(gitRepoRoot, cwd sql.Null[string]) string {
	if root := strings.TrimSpace(gitRepoRoot.V); root != "" {
		return root
	}
	return strings.TrimSpace(cwd.V)
}

// Scan target that accepts any storage class, a cell its field cannot hold reading as absent
type lenient struct{ dst any }

// Stores the cell in the wrapped field, never failing on NULL or a mistyped cell
func (l lenient) Scan(src any) error {
	switch d := l.dst.(type) {
	case *string:
		*d, _ = cellText(src)
	case *sql.Null[string]:
		d.V, d.Valid = cellText(src)
	case *sql.NullString:
		d.String, d.Valid = cellText(src)
	case *int64:
		*d, _ = cellInt(src)
	case *sql.Null[int64]:
		d.V, d.Valid = cellInt(src)
	case *float64:
		*d, _ = cellFloat(src)
	case *sql.Null[float64]:
		d.V, d.Valid = cellFloat(src)
	default:
		return fmt.Errorf("session column has no lenient scan into %T", l.dst)
	}
	return nil
}

// Text of a cell, numbers in their shortest decimal form, false for NULL
func cellText(src any) (string, bool) {
	switch v := src.(type) {
	case nil:
		return "", false
	case string:
		return v, true
	case []byte:
		return string(v), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64), true
	case bool:
		return strconv.Itoa(boolInt(v)), true
	case time.Time:
		return v.Format(time.RFC3339Nano), true
	}
	return fmt.Sprint(src), true
}

// Integer of a cell, a real truncated toward zero and numeric text parsed, false for NULL and anything else
func cellInt(src any) (int64, bool) {
	switch v := src.(type) {
	case int64:
		return v, true
	case bool:
		return int64(boolInt(v)), true
	case float64:
		if math.IsNaN(v) || v < math.MinInt64 || v >= math.MaxInt64 {
			return 0, false
		}
		return int64(v), true
	case string:
		text := strings.TrimSpace(v)
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			return n, true
		}
		if f, err := strconv.ParseFloat(text, 64); err == nil {
			return cellInt(f)
		}
	case []byte:
		return cellInt(string(v))
	}
	return 0, false
}

// Real of a cell, an integer widened and numeric text parsed, false for NULL and anything else
func cellFloat(src any) (float64, bool) {
	switch v := src.(type) {
	case float64:
		return v, true
	case int64:
		return float64(v), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil || errors.Is(err, strconv.ErrRange)
	case []byte:
		return cellFloat(string(v))
	}
	return 0, false
}
