package config

import sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"

const (
	Key  = sessionsconfig.Key + ".sqlite"
	Path = Key + ".path"
	// How the database writes to disk: "wal" or "delete"
	JournalMode = Key + ".journal_mode"
	// Pragmas of every connection, the database's own default when empty
	JournalSizeLimit  = Key + ".journal_size_limit"
	WALAutocheckpoint = Key + ".wal_autocheckpoint"
	CacheSize         = Key + ".cache_size"
	MmapSize          = Key + ".mmap_size"
	TempStore         = Key + ".temp_store"
	Synchronous       = Key + ".synchronous"
	// Index for searching Chinese, Japanese and Korean text by any three characters
	CJKFTS = Key + ".cjk_fts"
	// Suggests finishing an interrupted search index rebuild of a large database
	FTSOptimizeNotice = Key + ".fts_optimize_notice"
	// Searches slower than this many milliseconds are written to the log
	SearchSlowMS = Key + ".search_slow_ms"
)

const (
	WAL    = "wal"
	Delete = "delete"
	Advise = "advise"
	Off    = "off"
)
