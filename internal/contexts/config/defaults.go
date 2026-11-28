package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

func Register(cfg *config.Config, engines func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Long sessions",
		About: "What happens when a session outgrows the model",
		Group: config.GroupSessions,
		Order: 20,
		Settings: []config.Setting{
			{Key: Engine, Title: "Method", About: "Which method keeps a long session within what the model can read.", Default: Compressor,
				Options: engines, Apply: config.NewSessions, Advanced: true},
		},
	}})
	cfg.Register(config.Sections{{
		Key:    CompressionKey,
		Parent: Key,
		Title:  "Summaries",
		About:  "When older messages are summarized and what is kept",
		Order:  10,
		Into:   Key,
		Settings: []config.Setting{
			{Key: Enabled, Title: "Shorten long sessions", About: "Summarize older messages when a session nears the limit of the model.", Default: true,
				Env: []string{"CONTEXT_COMPRESSION_ENABLED"}},
			{Key: Threshold, Title: "Start at", About: "How full the context window gets before older messages are summarized.", Default: 0.5,
				Counts: config.Share, Env: []string{"CONTEXT_COMPRESSION_THRESHOLD"}},
			{Key: IdleCompact, Title: "After idle time", About: "How long a session sits idle before it is summarized.", Default: time.Duration(0),
				Zero: "Never", Min: config.Bound(0)},
			{Key: ModelThresholds, Title: "Start at by model", About: "The point to summarize at for the models you pick.",
				Default: map[string]any{}, Advanced: true, Picks: providerconfig.PickModel,
				Entry: &config.Entry{Noun: "model", Name: "Model", Fields: []config.Setting{
					{Title: "Start at", About: "How full the context window gets before older messages are summarized.", Default: 0.0,
						Counts: config.Share}}}},
			{Key: ThresholdTokens, Title: "Token cap", About: "The session size that starts a summary whatever share of the window it fills.",
				Default: 0, Zero: "No limit", Counts: "tokens", Least: config.Num(0), Advanced: true},
			{Key: TargetRatio, Title: "Target size", About: "How small a summary is, as a part of the size that started it.", Default: 0.2,
				Counts: config.Share, Least: config.Num(0.1), Most: config.Num(0.8), Advanced: true},
			{Key: ProtectFirstN, Title: "First messages kept", About: "How many opening messages are never summarized.", Default: 3, Counts: "messages",
				Least: config.Num(0), Advanced: true},
			{Key: ProtectLastN, Title: "Last messages kept", About: "How many of the latest messages are never summarized.", Default: 20,
				Counts: "messages", Least: config.Num(0), Advanced: true},
			{Key: MinTailUsers, Title: "Your messages kept", About: "How many of your latest messages are always kept as you wrote them.", Default: 1,
				Counts: "messages", Least: config.Num(1), Advanced: true},
			{Key: TailMode, Title: "Recent history", About: "How recent messages are kept when a session is summarized.", Default: TailLean,
				Advanced: true, Options: config.Options(
					config.Option{Value: TailLean, Title: "Lean", About: "Keep a trimmed tail of recent messages"},
					config.Option{Value: TailLegacy, Title: "Fixed", About: "Keep the older tail of a fixed size"},
				)},
			{Key: InPlace, Title: "Keep the same session", About: "Keep the same session when old messages are summarized, rather than start a new one.",
				Default: true, Advanced: true},
			{Key: PrepareAhead, Title: "Prepare ahead", About: "Prepare the summary before the session reaches the point to summarize at.",
				Default: false, Advanced: true},
			{Key: MaxAttempts, Title: "Attempts per reply", About: "How many times one reply tries to summarize before it gives up.", Default: 3,
				Counts: "attempts", Least: config.Num(1), Advanced: true},
			{Key: AbortOnFailure, Title: "Keep all on failure", About: "Keep every message when a summary fails, rather than drop the old ones.",
				Default: false, Advanced: true},
			{Key: CheckpointRequired, Title: "Require a checkpoint", About: "Wait to summarize until a memory service has saved a checkpoint of the session.",
				Default: false, Advanced: true},
			{Key: ProgressNotices, Title: "Step notices", About: "Show a status message for each step while a session is summarized.", Default: false,
				Advanced: true},
			{Key: IdleTimeout, Title: "Stall limit", About: "How long the summary step may stay silent before it is given up.",
				Help: "The limit grows to fit the time limit of the summary model.", Default: 2 * time.Minute, Zero: "No limit", Advanced: true},
			{Key: TotalCeiling, Title: "Time limit", About: "How long a whole summary step may take.", Default: 10 * time.Minute,
				Zero: "10 minutes", Advanced: true},
			{Key: PruneTokens, Title: "Early trim point", About: "The session size at which old tool output is trimmed early.", Default: 0,
				Zero: "Off", Counts: "tokens", Least: config.Num(0), Advanced: true},
			{Key: PruneMinChars, Title: "Smallest trimmed output", About: "The shortest tool output that may be trimmed early.", Default: 8000,
				Counts: "characters", Least: config.Num(200), Advanced: true},
			{Key: PruneMinReclaim, Title: "Least trim gain", About: "The fewest tokens an early trim must free to be worth doing.", Default: 4096,
				Counts: "tokens", Least: config.Num(0), Advanced: true},
			{Key: MicroCompact, Title: "Running summary", About: "Fold the oldest exchange into a running summary after each reply.", Default: false,
				Advanced: true},
			{Key: MicroEveryNTurns, Title: "Running summary pace", About: "How many replies pass between folds into the running summary.", Default: 1,
				Counts: "replies", Least: config.Num(1), Advanced: true},
			{Key: MicroDefragTokens, Title: "Running summary repack", About: "The size of the running summary at which it is summarized again.",
				Default: 2000, Counts: "tokens", Least: config.Num(1), Advanced: true},
			{Key: CodexAutoraise, Title: "Later for Codex", About: "Summarize later for the newest Codex models, whose window is capped.",
				Default: true, Advanced: true},
			{Key: CodexAutoraiseNotice, Title: "Codex raise notice", About: "Show a note when the point to summarize at is raised for Codex.",
				Default: true, Advanced: true},
			{Key: CodexNative, Title: "Codex shortens itself", About: "Let Codex shorten long sessions itself over the Responses API.", Default: false,
				Advanced: true},
			{Key: CodexCompactThreshold, Title: "Codex shortening point", About: "The session size at which Codex starts to shorten it.", Default: 0,
				Zero: "Follows Start at", Counts: "tokens", Least: config.Num(0), Advanced: true},
		},
	}})
}
