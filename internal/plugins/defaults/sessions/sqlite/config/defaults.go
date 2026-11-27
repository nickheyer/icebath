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
		About:  "The database file sessions are saved in",
		Order:  20,
		Settings: []config.Setting{
			{Key: CJKFTS, Title: "Search East Asian text", About: "Make Chinese, Japanese and Korean text in sessions searchable.", Default: true,
				Apply: config.Restart},
			{Key: Path, Title: "Database file", About: "The file sessions are saved in.", Default: filepath.Join(cfg.Dir(), "sessions.db"), Apply: config.Restart,
				Advanced: true},
			{Key: JournalMode, Title: "Journal mode", About: "How the database writes changes to disk.", Default: WAL,
				Help: "Pick Delete on network drives. A database in write-ahead mode stays so until `icebath sessions set-journal-mode delete`.",
				Options: config.Options(
					config.Option{Value: WAL, Title: "Write-ahead log", About: "Faster, reading never waits for writing"},
					config.Option{Value: Delete, Title: "Delete", About: "Slower, works on network drives"},
				), Apply: config.Restart, Advanced: true},
			{Key: Synchronous, Title: "Write safety", About: "How long the database waits for the disk after each write.",
				Help: "macOS always uses Full or Extra.", Default: "", Options: config.Options(
					config.Option{Title: "Database default", About: "What SQLite does on its own"},
					config.Option{Value: "OFF", Title: "Off", About: "Fastest, a crash may lose data"},
					config.Option{Value: "NORMAL", Title: "Normal", About: "Safe in most crashes"},
					config.Option{Value: "FULL", Title: "Full", About: "Waits for every write"},
					config.Option{Value: "EXTRA", Title: "Extra", About: "Also waits for the journal"},
				), Apply: config.Restart, Advanced: true},
			{Key: JournalSizeLimit, Title: "Journal size limit", About: "The largest the write log may stay after a cleanup.",
				Help: "A whole number of bytes, such as 67108864 for 64 MB.", Default: "", Zero: "The database's default", Apply: config.Restart, Advanced: true},
			{Key: WALAutocheckpoint, Title: "Checkpoint interval", About: "How many pages are written before the write log is merged back.",
				Help: "A whole number of pages.", Default: "", Zero: "The database's default", Apply: config.Restart, Advanced: true},
			{Key: CacheSize, Title: "Memory cache", About: "The memory the database keeps for recent pages.",
				Help: "A positive number counts pages. A negative one counts KiB, such as -2000 for about 2 MB.", Default: "", Zero: "The database's default",
				Apply: config.Restart, Advanced: true},
			{Key: MmapSize, Title: "Memory map size", About: "The part of the database file read through memory mapping.",
				Help: "A whole number of bytes. 0 turns memory mapping off.", Default: "", Zero: "The database's default", Apply: config.Restart, Advanced: true},
			{Key: TempStore, Title: "Temporary tables", About: "Where the database keeps temporary tables.", Default: "", Options: config.Options(
				config.Option{Title: "Database default", About: "What SQLite does on its own"},
				config.Option{Value: "0", Title: "Build default", About: "As the SQLite library was built"},
				config.Option{Value: "1", Title: "File", About: "In temporary files"},
				config.Option{Value: "2", Title: "Memory", About: "In memory"},
			), Apply: config.Restart, Advanced: true},
			{Key: FTSOptimizeNotice, Title: "Rebuild reminder", About: "Whether to remind you to finish an interrupted rebuild of the search index.",
				Default: Advise, Options: config.Options(
					config.Option{Value: Advise, Title: "Remind me", About: "When the database is larger than 512 MB"},
					config.Option{Value: Off, Title: "Off", About: "Never remind me"},
				), Apply: config.Restart, Advanced: true},
			{Key: SearchSlowMS, Title: "Slow search log", About: "How long a search may take before it is written to the log.", Default: time.Second,
				Unit: time.Millisecond, Zero: "Every search", Advanced: true},
		},
	}})
}
