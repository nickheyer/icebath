package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config, engines func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Context",
		About: "How long conversations are kept within the model's limit",
		Settings: []config.Setting{
			{Key: Engine, Title: "Context engine", About: "Which engine manages long conversations of new sessions; compressor is the built-in one, plugins can add others",
				Default: Compressor, Options: engines},
		},
	}})
	cfg.Register(config.Sections{{
		Key:    CompressionKey,
		Parent: Key,
		Title:  "Compression",
		About:  "When and how older messages are summarised as a chat nears the model's limit",
		Settings: []config.Setting{
			{Key: Enabled, Title: "Shorten long chats", About: "Summarize older messages when a chat nears the model's limit", Default: true,
				Env: []string{"CONTEXT_COMPRESSION_ENABLED"}},
			{Key: Threshold, Title: "Shorten at", About: "How full the chat gets before older messages are summarized, as a share of the model's context window: 0.5 is half full",
				Default: 0.5, Env: []string{"CONTEXT_COMPRESSION_THRESHOLD"}},
			{Key: ThresholdTokens, Title: "Trigger token cap", About: "Hard cap in tokens that triggers compression; 0 uses the share of the context window alone", Default: 0},
			{Key: ModelThresholds, Title: "Per model trigger", About: "Compression trigger share for chosen models, by model name or provider:model, each a fraction of the window such as 0.6",
				Default: map[string]any{}},
			{Key: TargetRatio, Title: "Summary size", About: "Share of the trigger size the summary is squeezed to, held between 0.1 and 0.8", Default: 0.2},
			{Key: ProtectFirstN, Title: "Keep first messages", About: "Number of opening messages never summarised", Default: 3},
			{Key: ProtectLastN, Title: "Keep last messages", About: "Number of latest messages never summarised", Default: 20},
			{Key: MinTailUsers, Title: "Keep user messages", About: "Fewest of your latest messages always kept as written", Default: 1},
			{Key: TailMode, Title: "Recent history mode", About: "How recent messages are kept when compressing", Default: TailLean, Options: config.Options(
				config.Option{Value: TailLean, Title: "Lean", About: "Keep a trimmed recent tail"},
				config.Option{Value: TailLegacy, Title: "Legacy", About: "Keep the older fixed-size tail"},
			)},
			{Key: InPlace, Title: "Compress in place", About: "Keeps the same session when old messages are summarised, rather than going on in a new one", Default: true},
			{Key: PrepareAhead, Title: "Summarise ahead", About: "Prepares the summary before the trigger is reached", Default: false},
			{Key: MaxAttempts, Title: "Summary attempts", About: "How many times one turn tries to compress before it gives up, 1 to 10", Default: 3},
			{Key: AbortOnFailure, Title: "Stop on failure", About: "Keeps every message when the summary of old messages fails, rather than dropping them under a placeholder", Default: false},
			{Key: CheckpointRequired, Title: "Require checkpoint", About: "Refuses to compress until a memory provider saved a checkpoint of the chat", Default: false},
			{Key: ProgressNotices, Title: "Compression notices", About: "Shows the status messages of each step while a chat is being compressed", Default: false},
			{Key: IdleTimeout, Title: "Summary idle limit", About: "Seconds the summary step may stay silent before it is given up; raised to fit the summary model's own timeout",
				Default: 2 * time.Minute},
			{Key: TotalCeiling, Title: "Summary time limit", About: "Seconds a whole summary step may take", Default: 10 * time.Minute},
			{Key: IdleCompact, Title: "Compact when idle", About: "Compresses a chat after it has sat idle this many seconds; 0 turns it off", Default: time.Duration(0),
				Min: config.Bound(0)},
			{Key: PruneTokens, Title: "Early trim size", About: "Token size at which old tool output is trimmed early; 0 turns early trimming off", Default: 0},
			{Key: PruneMinChars, Title: "Prune result size", About: "Tool output shorter than this many characters is never trimmed early", Default: 8000},
			{Key: PruneMinReclaim, Title: "Prune minimum gain", About: "Least tokens an early trim must free to be worth doing", Default: 4096},
			{Key: MicroCompact, Title: "Micro compaction", About: "Folds the oldest exchange into a running summary after each turn instead of all at once", Default: false},
			{Key: MicroEveryNTurns, Title: "Micro compact pace", About: "Folds an exchange once every this many turns", Default: 1},
			{Key: MicroDefragTokens, Title: "Micro compact repack", About: "Size in tokens of the running summary that has it summarised again", Default: 2000},
			{Key: CodexAutoraise, Title: "Raise Codex trigger", About: "Compresses later for the newest Codex models, whose window is capped", Default: true},
			{Key: CodexAutoraiseNotice, Title: "Codex raise notice", About: "Shows a note when the Codex compression trigger is raised", Default: true},
			{Key: CodexNative, Title: "Native Codex compaction", About: "Lets Codex compact long conversations itself on the Responses API", Default: false},
			{Key: CodexCompactThreshold, Title: "Codex compact size", About: "Size in tokens at which native Codex compaction starts; 0 chooses it from the local trigger", Default: 0},
		},
	}})
}
