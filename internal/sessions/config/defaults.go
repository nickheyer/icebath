package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config, stores func() []config.Option) {
	cfg.Register(config.Sections{
		{
			Key:    Key,
			Global: true,
			Title:  "Sessions",
			About:  "Session storage, cleanup and the sessions kept open",
			Settings: []config.Setting{
				{Key: Store, Title: "Store", About: "Where sessions are saved", Default: "sqlite", Options: stores},
				{Key: AutoPrune, Title: "Delete old sessions", About: "Delete sessions older than the keep time when the server starts", Default: false},
				{Key: RetentionDays, Title: "Keep old sessions", About: "Days after which old sessions are deleted by automatic cleanup; applies only while Delete old sessions is on",
					Default: 90 * 24 * time.Hour, Unit: 24 * time.Hour},
				{Key: MinIntervalHours, Title: "Cleanup spacing", About: "Least hours between automatic archive or cleanup runs", Default: 24 * time.Hour, Unit: time.Hour},
				{Key: VacuumAfterPrune, Title: "Compact after cleanup", About: "Shrink the session database after old sessions are deleted", Default: true},
				{Key: MinVacuumIntervalDays, Title: "Compact spacing", About: "Least days between compactions of the session database", Default: 30 * 24 * time.Hour,
					Unit: 24 * time.Hour},
				{Key: AutoArchive, Title: "Auto-archive sessions", About: "Archive sessions that have sat idle for a while", Default: false},
				{Key: AutoArchiveDays, Title: "Archive after idle", About: "Days a session must be idle before it is archived; pinned sessions are never archived", Default: 3 * 24 * time.Hour,
					Unit: 24 * time.Hour},
				{Key: StartupOrphanSweep, Title: "Close stale sessions", About: "At startup, close sessions left open by a crash or shutdown", Default: true},
				{Key: OrphanEndGrace, Title: "Abandoned session grace", About: "Seconds an abandoned session waits before it is marked ended; zero ends it right away", Default: 5 * time.Minute},
				{Key: OrphanReapGrace, Title: "Disconnect grace", About: "Seconds a session is kept after its window disconnects; zero keeps it parked until it is reopened",
					Default: 20 * time.Second},
				{Key: OrphanActivityStale, Title: "Idle disconnect limit", About: "Seconds a disconnected session may sit idle before it is closed", Default: 10 * time.Minute},
				{Key: MaxExportMessages, Title: "Export size limit", About: "Largest session, in messages, that can be exported in memory; zero turns the guard off", Default: 20000},
				{Key: MaxResumeMessages, Title: "Resume size limit", About: "Largest session, in messages, that can be resumed; zero turns the guard off", Default: 20000},
				{Key: ShowSubagents, Title: "List subagent sessions", About: "Include sessions started by subagents in session lists", Default: false},
				{Key: MaxLivePerConnection, Title: "Sessions per window", About: "Most sessions one window keeps open at once, at least 1", Default: 12},
				{Key: MaxLivePerProfile, Title: "Open session limit", About: "Most sessions open at once across every window of one profile, at least 1", Default: 32},
				{Key: MaxLive, Title: "Live session limit", About: "Most sessions kept open before the least recent are closed; zero removes the limit", Default: 16},
				{Key: MaxConcurrent, Title: "Active session limit", About: "Most sessions that may run a turn at once; zero for no limit", Default: 0},
				{Key: TerminalContinue, Title: "Continue per terminal", About: "Remember each terminal's session so it can be continued there", Default: true},
			},
		},
	})
	cfg.Register(config.Sections{
		{
			Key:    AutoContinue,
			Parent: Key,
			Title:  "Interrupted turns",
			About:  "Turns cut short when the server stopped, picked up again when it starts",
			Settings: []config.Setting{
				{Key: AutoContinueEnabled, Title: "Resume interrupted turns", About: "Pick up a turn automatically after the server stopped mid-turn", Default: true},
				{Key: AutoContinueFreshness, Title: "Resume window", About: "Minutes since an interrupted turn started within which it resumes itself", Default: 15 * time.Minute,
					Unit: time.Minute},
				{Key: AutoContinueAttempts, Title: "Resume attempts", About: "Times one interrupted turn is resumed before giving up", Default: 2},
			},
		},
	})
}
