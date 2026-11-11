package dashboard

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	auditMaxFieldLength   = 256
	auditTruncationMarker = "...[truncated]"
)

// Size cap of the audit log and how many numbered backups rotation keeps
var (
	auditMaxBytes    int64 = 5 * 1024 * 1024
	auditBackupCount       = 3
)

// Dashboard auth audit event, its value the literal event field of the JSON line
type AuditEvent string

const (
	AuditLoginStart           AuditEvent = "login_start"
	AuditLoginSuccess         AuditEvent = "login_success"
	AuditLoginFailure         AuditEvent = "login_failure"
	AuditLogout               AuditEvent = "logout"
	AuditRefreshSuccess       AuditEvent = "refresh_success"
	AuditRefreshFailure       AuditEvent = "refresh_failure"
	AuditRevoke               AuditEvent = "revoke"
	AuditSessionVerifyFailure AuditEvent = "session_verify_failure"
	AuditSessionRejected      AuditEvent = "session_rejected"
	AuditWSTicketMinted       AuditEvent = "ws_ticket_minted"
	AuditWSTicketRejected     AuditEvent = "ws_ticket_rejected"
	AuditTokenAuthSuccess     AuditEvent = "token_auth_success"
	AuditTokenAuthFailure     AuditEvent = "token_auth_failure"
	AuditNativeAuthorizeStart AuditEvent = "native_authorize_start"
	AuditNativeCodeIssued     AuditEvent = "native_code_issued"
	AuditNativeTokenSuccess   AuditEvent = "native_token_success"
	AuditNativeTokenFailure   AuditEvent = "native_token_failure"
)

// Field names never written raw, their values dropped from the line
var auditRedactedFields = map[string]bool{"access_token": true, "refresh_token": true, "code": true,
	"code_verifier": true, "state": true, "ticket": true, "cookie": true, "Authorization": true, "authorization": true}

var auditWriteMu sync.Mutex

func auditLogger() *slog.Logger {
	return slog.Default().With("logger", "hermes_cli.dashboard_auth.audit")
}

// Audit log path under the active home's logs directory
func auditLogPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "logs", "dashboard-auth.log")
}

// Value with every string bounded, nested values included and other objects as their repr
func auditBoundedValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		obj := pyjson.NewObject()
		for k, x := range v {
			obj.Set(k, auditBoundedValue(x))
		}
		return obj
	case *pyjson.Object:
		obj := pyjson.NewObject()
		for _, k := range v.Keys {
			obj.Set(k, auditBoundedValue(v.Vals[k]))
		}
		return obj
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = auditBoundedValue(x)
		}
		return out
	case []string:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = auditBoundedValue(x)
		}
		return out
	case nil, bool, int, int64, float64:
		return v
	case string:
		if r := []rune(v); len(r) > auditMaxFieldLength {
			return string(r[:auditMaxFieldLength-len(auditTruncationMarker)]) + auditTruncationMarker
		}
		return v
	}
	return auditBoundedValue(pyrepr.Repr(value))
}

// Rotates the log to numbered backups once the pending append would reach the size cap
func auditRotateIfNeeded(path string, pendingBytes int) {
	info, err := os.Stat(path)
	if err != nil || info.Size()+int64(pendingBytes) < auditMaxBytes {
		return
	}
	for i := auditBackupCount - 1; i > 0; i-- {
		src := fmt.Sprintf("%s.%d", path, i)
		if _, err := os.Stat(src); err == nil {
			if os.Rename(src, fmt.Sprintf("%s.%d", path, i+1)) != nil {
				return
			}
		}
	}
	_ = os.Rename(path, path+".1")
}

// Appends one event line with token like fields dropped, never failing the caller
func auditLog(ctx context.Context, event AuditEvent, fields ...any) {
	entry := pyjson.ObjectOf("ts", pydatetime.ISOFormatUTC(time.Now()), "event", string(event))
	for i := 0; i+1 < len(fields); i += 2 {
		key, _ := fields[i].(string)
		if !auditRedactedFields[key] {
			entry.Set(key, auditBoundedValue(fields[i+1]))
		}
	}
	text, err := pyjson.Dumps(entry, pyjson.Separators(",", ":"))
	if err != nil {
		auditLogger().Warn(fmt.Sprintf("dashboard-auth audit log write failed: %s", err))
		return
	}
	line := []byte(text + "\n")
	path := auditLogPath(ctx)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		auditLogger().Warn(fmt.Sprintf("dashboard-auth audit log write failed: %s", err))
		return
	}
	auditWriteMu.Lock()
	defer auditWriteMu.Unlock()
	auditRotateIfNeeded(path, len(line))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err == nil {
		_, err = f.Write(line)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		auditLogger().Warn(fmt.Sprintf("dashboard-auth audit log write failed: %s", err))
	}
}
