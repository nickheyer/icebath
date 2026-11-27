package config

import (
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: sessionsconfig.Key,
		Title:  "SQLite",
		About:  "Sessions saved in a local database file",
		Settings: []config.Setting{
			{Key: Path, Title: "File", About: "Database file path", Default: filepath.Join(cfg.Dir(), "sessions.db")},
			{Key: JournalMode, Title: "Database journal mode", About: "How the session database writes to disk; use delete on network drives or file systems without write-ahead logs. " +
				"An existing write-ahead log database stays one until `icebath sessions set-journal-mode delete` converts it", Default: WAL, Options: config.Options(
				config.Option{Value: WAL, Title: "Write-ahead log", About: "Faster, readers do not block writers"},
				config.Option{Value: Delete, Title: "Delete", About: "Slower, works on network drives"},
			)},
			{Key: JournalSizeLimit, Title: "Journal size cap", About: "Largest size in bytes the database write log may keep after cleanup; empty for the database default", Default: ""},
			{Key: WALAutocheckpoint, Title: "Checkpoint interval", About: "Pages written before the database log is merged back; empty for the database default", Default: ""},
			{Key: CacheSize, Title: "Database cache", About: "Memory the session database keeps for recent pages: a positive number counts pages, a negative one KiB; empty for the database default", Default: ""},
			{Key: MmapSize, Title: "Database memory map", About: "Bytes of the session database read through memory mapping, at least 0; empty for the database default", Default: ""},
			{Key: TempStore, Title: "Database temp storage", About: "Where the session database keeps temporary tables", Default: "", Options: config.Options(
				config.Option{Title: "Database default"},
				config.Option{Value: "0", Title: "Default", About: "As the database was built"},
				config.Option{Value: "1", Title: "File", About: "In temporary files"},
				config.Option{Value: "2", Title: "Memory", About: "In memory"},
			)},
			{Key: Synchronous, Title: "Database write safety", About: "How carefully the session database waits for the disk after writes; macOS always keeps Full", Default: "", Options: config.Options(
				config.Option{Title: "Database default"},
				config.Option{Value: "OFF", Title: "Off", About: "Fastest, may lose data on a crash"},
				config.Option{Value: "NORMAL", Title: "Normal", About: "Safe in most crashes"},
				config.Option{Value: "FULL", Title: "Full", About: "Waits for every write"},
				config.Option{Value: "EXTRA", Title: "Extra", About: "Also waits for the journal"},
			)},
			{Key: CJKFTS, Title: "Search CJK text", About: "Make Chinese, Japanese and Korean text searchable in sessions", Default: true},
			{Key: FTSOptimizeNotice, Title: "Search tune-up notice", About: "Whether to suggest finishing an interrupted rebuild of a large session search index", Default: Advise, Options: config.Options(
				config.Option{Value: Advise, Title: "Advise", About: "Suggest it when the database is large"},
				config.Option{Value: Off, Title: "Off", About: "Never suggest it"},
			)},
			{Key: SearchSlowMS, Title: "Slow search mark", About: "Searches slower than this many milliseconds are written to the log", Default: time.Second, Unit: time.Millisecond},
		},
	}})
}
