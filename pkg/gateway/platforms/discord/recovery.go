package discord

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
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	recoveryDBFilename = "discord_message_recovery.db"
	recoveryRetention  = 30 * 24 * time.Hour
)

const recoverySchema = `
            CREATE TABLE IF NOT EXISTS discord_messages (
                message_id TEXT PRIMARY KEY, channel_id TEXT, thread_id TEXT, parent_channel_id TEXT,
                author_id TEXT, created_at TEXT, status TEXT NOT NULL,
                replied INTEGER NOT NULL DEFAULT 0, emoji_ack INTEGER NOT NULL DEFAULT 0,
                outage_response INTEGER NOT NULL DEFAULT 0, response_message_id TEXT,
                attempts INTEGER NOT NULL DEFAULT 0, last_attempt_at TEXT, last_error TEXT,
                updated_at TEXT NOT NULL
            );
            CREATE TABLE IF NOT EXISTS discord_recovery_scans (
                scan_id TEXT PRIMARY KEY, started_at TEXT NOT NULL, completed_at TEXT, status TEXT NOT NULL,
                channels TEXT NOT NULL, window_seconds REAL NOT NULL, limit_count INTEGER NOT NULL,
                scanned INTEGER NOT NULL DEFAULT 0, missed INTEGER NOT NULL DEFAULT 0,
                dispatched INTEGER NOT NULL DEFAULT 0, error TEXT
            );
            CREATE TABLE IF NOT EXISTS discord_recovery_cursors (
                channel_id TEXT PRIMARY KEY, last_message_id TEXT NOT NULL, updated_at TEXT NOT NULL
            );
        `

// Profile scoped SQLite ledger of completed Discord messages, recovery scans and channel cursors
type DiscordRecoveryStore struct {
	mu          sync.Mutex
	initialized bool
	icebathHome string
}

// Store rooted at icebathHome, the home directory in effect when icebathHome is empty
func NewDiscordRecoveryStore(ctx context.Context, icebathHome string) *DiscordRecoveryStore {
	if icebathHome == "" {
		icebathHome = home.Dir(ctx)
	}
	return &DiscordRecoveryStore{icebathHome: icebathHome}
}

// Ledger file under the gateway directory of the home, creating that directory when missing
func (s *DiscordRecoveryStore) Path() (string, error) {
	directory := filepath.Join(s.icebathHome, "gateway")
	if err := os.MkdirAll(directory, 0o777); err != nil {
		return "", err
	}
	return filepath.Join(directory, recoveryDBFilename), nil
}

// Result of fn inside one committed transaction, def with a logged warning when any step fails
func (s *DiscordRecoveryStore) Call(ctx context.Context, fn func(conn state.SQLiteConn) (any, error), def any) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := func() (any, error) {
		path, err := s.Path()
		if err != nil {
			return nil, err
		}
		conn, err := state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 0.1})
		if err != nil {
			return nil, err
		}
		var result any
		err = state.Transaction(ctx, conn, false, func(tx state.SQLiteConn) error {
			if !s.initialized {
				if err := s.initialize(ctx, tx); err != nil {
					return err
				}
				s.initialized = true
				_ = os.Chmod(path, 0o600)
			}
			var err error
			result, err = fn(tx)
			return err
		})
		return result, err
	}()
	if err != nil {
		slog.Warn(fmt.Sprintf("Discord recovery ledger unavailable: %s", err))
		return def
	}
	return result
}

// Applies the configured journal mode, creates the ledger tables and prunes rows older than the retention window
func (s *DiscordRecoveryStore) initialize(ctx context.Context, conn state.SQLiteConn) error {
	if _, err := state.ApplyWALWithFallback(ctx, conn, "discord_recovery.db", false); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, recoverySchema); err != nil {
		return err
	}
	cutoff := pydatetime.ISOFormatUTC(time.Now().UTC().Add(-recoveryRetention))
	for _, query := range []string{
		"DELETE FROM discord_messages WHERE updated_at < ?",
		"DELETE FROM discord_recovery_scans WHERE COALESCE(completed_at, started_at) < ?",
		"DELETE FROM discord_recovery_cursors WHERE updated_at < ?",
	} {
		if _, err := conn.ExecContext(ctx, query, cutoff); err != nil {
			return err
		}
	}
	return nil
}
