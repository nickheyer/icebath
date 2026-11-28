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
			Title:  "Session storage",
			About:  "How sessions are saved, cleaned up and limited",
			Group:  config.GroupSessions,
			Order:  10,
			Settings: []config.Setting{
				{Key: AutoPrune, Title: "Delete old sessions", About: "Delete the sessions you have not used for a while each time the server starts.",
					Help: "Pinned sessions and open ones stay. It runs at most once per Time between cleanups.", Default: false, Slash: "autoprune",
					Apply: config.Restart},
				{Key: RetentionDays, Title: "Delete after", About: "How long a session sits unused before it is deleted.",
					Help: "This applies only while Delete old sessions is on.", Default: 90 * 24 * time.Hour, Unit: 24 * time.Hour,
					Zero: "Every ended session", Apply: config.Restart},
				{Key: AutoArchive, Title: "Archive idle sessions", About: "Archive the sessions that sit idle for a while each time the server starts.",
					Help: "Pinned sessions are never archived.", Default: false, Apply: config.Restart},
				{Key: AutoArchiveDays, Title: "Archive after", About: "How long a session sits idle before it is archived.",
					Help: "This applies only while Archive idle sessions is on.", Default: 3 * 24 * time.Hour, Unit: 24 * time.Hour,
					Apply: config.Restart},
				{Key: ShowSubagents, Title: "List subagent sessions", About: "Show the sessions that subagents started in lists of sessions.", Default: false},
				{Key: TerminalContinue, Title: "Continue per terminal", About: "Remember the session of each terminal so you can continue it there.", Default: true},
				{Key: Store, Title: "Storage", About: "Which store sessions are saved in.", Default: "sqlite", Options: stores, Apply: config.Restart,
					Advanced: true},
				{Key: PruneAsk, Title: "Low disk question", About: "When icebath may ask again about deleting old sessions on a low disk.", Default: "",
					Internal: true},
				{Key: VacuumAfterPrune, Title: "Shrink after deleting", About: "Shrink the session database after old sessions are deleted.", Default: true,
					Apply: config.Restart, Advanced: true},
				{Key: MinVacuumIntervalDays, Title: "Time between shrinks", About: "The least time between two shrinks of the session database.",
					Default: 30 * 24 * time.Hour, Unit: 24 * time.Hour, Zero: "Every cleanup", Apply: config.Restart, Advanced: true},
				{Key: MinIntervalHours, Title: "Time between cleanups", About: "The least time between two automatic cleanups.", Default: 24 * time.Hour,
					Unit: time.Hour, Zero: "Every start", Apply: config.Restart, Advanced: true},
				{Key: StartupOrphanSweep, Title: "Close left-open sessions", About: "Close sessions that a crash or shutdown left open when the server starts.",
					Default: true, Apply: config.Restart, Advanced: true},
				{Key: OrphanReapGrace, Title: "Kept after disconnect", About: "How long a session stays open after its last window goes away.",
					Help: "At 0, sessions left open by a crash are not closed when the server starts either.", Default: 20 * time.Second, Zero: "Until reopened",
					Advanced: true},
				{Key: OrphanActivityStale, Title: "Idle after disconnect", About: "How long a session with no window may sit idle after a reply before it closes.",
					Default: 10 * time.Minute, Zero: "Until reopened", Advanced: true},
				{Key: OrphanEndGrace, Title: "Wait before ending", About: "How long a closed session waits before it is marked ended.", Default: 5 * time.Minute,
					Zero: "Right away", Advanced: true},
				{Key: MaxLivePerConnection, Title: "Open per window", About: "The most sessions one window keeps open at once.", Default: 12, Counts: "sessions",
					Least: config.Num(1), Advanced: true},
				{Key: MaxLivePerProfile, Title: "Open per profile", About: "The most sessions open at once across the windows of one profile.", Default: 32,
					Counts: "sessions", Least: config.Num(1), Advanced: true},
				{Key: MaxLive, Title: "Kept open", About: "The most sessions kept open before the least recent ones close.", Default: 16, Zero: "No limit",
					Counts: "sessions", Advanced: true},
				{Key: MaxConcurrent, Title: "Working at once", About: "The most sessions that may work on a reply at the same time.", Default: 0, Zero: "No limit",
					Counts: "sessions", Advanced: true},
				{Key: MaxResumeMessages, Title: "Reopen limit", About: "The most messages a session may have and still be reopened.", Default: 20000,
					Zero: "No limit", Counts: "messages", Advanced: true},
				{Key: MaxExportMessages, Title: "Export limit", About: "The most messages a session may have for a JSON export.",
					Help: "A Markdown export has no limit.", Default: 20000, Zero: "No limit", Counts: "messages", Advanced: true},
			},
		},
	})
	cfg.Register(config.Sections{
		{
			Key:    AutoContinue,
			Parent: Key,
			Title:  "Interrupted replies",
			About:  "Pick up replies cut short when the server stopped",
			Order:  10,
			Settings: []config.Setting{
				{Key: AutoContinueEnabled, Title: "Resume replies", About: "Pick up a reply on its own when the server stopped in the middle of it.", Default: true,
					Apply: config.Restart},
				{Key: AutoContinueFreshness, Title: "Resume within", About: "How long after a reply started it may still be picked up.", Default: 15 * time.Minute,
					Unit: time.Minute, Apply: config.Restart},
				{Key: AutoContinueAttempts, Title: "Tries", About: "How many times one interrupted reply is picked up before icebath gives up.", Default: 2,
					Zero: "Never picked up", Counts: "tries", Apply: config.Restart},
			},
		},
	})
}
