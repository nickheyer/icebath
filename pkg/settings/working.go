package settings

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/config"
)

// Transcript parts whose fold state a details mode sets
var DetailSections = []string{"thinking", "tools", "subagents", "activity"}

// Fold states of a transcript part
var detailModeChoices = []Choice{
	{"hidden", "Hidden", "Not shown at all."},
	{"collapsed", "Folded", "One line you can open."},
	{"expanded", "Open", "Shown in full."},
}

// Tool progress mode and fold state each detail level stands for
var detailPresets = map[string][2]string{
	"minimal":    {"off", "hidden"},
	"normal":     {"all", "collapsed"},
	"detailed":   {"all", "expanded"},
	"everything": {"verbose", "expanded"},
}

// Detail level a tool progress mode, fold state and per part overrides spell, custom when they match none
func detailLevelOf(progress, details string, sections map[string]any) string {
	for _, part := range DetailSections {
		if v, set := sections[part]; set && v != details {
			return "custom"
		}
	}
	for level, pair := range detailPresets {
		if pair[0] == progress && pair[1] == details {
			return level
		}
	}
	return "custom"
}

// Highest of two sources
func higherSource(a, b Source) Source {
	rank := map[Source]int{SourceDefault: 0, SourceGlobal: 1, SourceWorkspace: 2, SourceSession: 3, SourceEnv: 4, SourceManaged: 5}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// How much of the agent's work the transcript shows, one control over tool progress and fold state
var DetailLevel = Register(&Enum{Meta: Meta{Key: "display.detail_level", Label: "Detail level",
	Short: "How much of the agent's tool work and thinking you see.",
	Help:  "Sets tool progress and how far each part of the transcript is opened. Change single parts under advanced.",
	Group: GroupWorking, Order: 10, Scopes: Global | Session, Slash: "detail",
	Validate: func(v any) error {
		if v == "custom" {
			return errors.New("custom is set by changing single parts")
		}
		return nil
	},
	Companions: func(v any) []config.KeyValue {
		pair := detailPresets[v.(string)]
		return []config.KeyValue{{Key: ToolProgress.Key, Value: pair[0]}, {Key: DetailsMode.Key, Value: pair[1]},
			{Key: DetailSectionModes.Key, Value: everySection(pair[1])}}
	},
	Derive: func(ctx context.Context) (any, Source) {
		progress, details := resolveEntry(ctx, ToolProgress), resolveEntry(ctx, DetailsMode)
		sections, _ := typedOr(DetailSectionModes, resolveEntry(ctx, DetailSectionModes).Value).(map[string]any)
		level := detailLevelOf(typedOr(ToolProgress, progress.Value).(string), typedOr(DetailsMode, details.Value).(string), sections)
		return level, higherSource(progress.Source, details.Source)
	}},
	Default: "normal",
	Values: []Choice{
		{"minimal", "Minimal", "Replies only."},
		{"normal", "Normal", "Each tool call as a folded line."},
		{"detailed", "Detailed", "Tool calls and thinking shown open."},
		{"everything", "Everything", "Every call with its full input and output."},
		{"custom", "Custom", "Parts set one by one under advanced."},
	}})

// Tool call lines in the transcript
var ToolProgress = Register(&Enum{Meta: Meta{Key: "display.tool_progress", Platforms: true, Label: "Tool progress",
	Short: "Which tool calls get a line in the transcript while the agent works.",
	Group: GroupWorking, Order: 100, Advanced: true, Scopes: Global | Session, Slash: "verbose"},
	Default: "all",
	Values: []Choice{
		{"off", "Off", "No tool lines."},
		{"new", "New", "A line only when the tool changes."},
		{"all", "All", "A line for every call."},
		{"verbose", "Verbose", "Every call with its arguments and results."},
		{"log", "Log", "Chat apps only: a running log message of tool names."},
	}})

// Fold state of every transcript part at once
var DetailsMode = Register(&Enum{Meta: Meta{Key: "display.details_mode", Label: "Fold parts",
	Short: "Whether thinking, tools and subagents start hidden, folded or open.",
	Group: GroupWorking, Order: 110, Advanced: true, Scopes: Global | Session, Slash: "details",
	Companions: func(v any) []config.KeyValue {
		return []config.KeyValue{{Key: DetailSectionModes.Key, Value: everySection(v.(string))}}
	}},
	Default: "collapsed", Values: detailModeChoices})

// The same fold state for every transcript part
func everySection(mode string) map[string]any {
	out := make(map[string]any, len(DetailSections))
	for _, part := range DetailSections {
		out[part] = mode
	}
	return out
}

// Fold state of single transcript parts, overriding the shared one
var DetailSectionModes = Register(&Map{Meta: Meta{Key: "display.sections", Label: "Fold per part",
	Short: "Fold state of thinking, tools, subagents or activity on its own.",
	Group: GroupWorking, Order: 120, Advanced: true, Scopes: Global | Session},
	Value: &Enum{Meta: Meta{Key: "display.sections.*"}, Values: detailModeChoices}})

// Whether the model's thinking shows in the transcript
var ShowThinking = Register(&Bool{Meta: Meta{Key: "display.show_reasoning", Platforms: true, Label: "Show thinking",
	Short: "Show the model's thinking in the transcript.",
	Help:  "Chat apps hide thinking unless their own display settings turn it on.",
	Group: GroupWorking, Order: 20, Scopes: Global | Session},
	Default: true})

// Whether thinking shows in full rather than clamped
var FullThinking = Register(&Bool{Meta: Meta{Key: "display.reasoning_full", Label: "Full thinking",
	Short: "Show the model's thinking in full instead of a few lines.",
	Group: GroupWorking, Order: 130, Advanced: true},
	Default: false})

// What Enter does while the agent is still working
var BusyMode = Register(&Enum{Meta: Meta{Key: "display.busy_input_mode", Label: "Enter while busy",
	Short: "What Enter does while the agent is still working.",
	Group: GroupWorking, Order: 30, Scopes: Global | Session, Slash: "busy"},
	Default: "queue",
	Values: []Choice{
		{"queue", "Queue it", "Send after the current turn ends."},
		{"steer", "Steer", "Add it to the current turn."},
		{"interrupt", "Interrupt", "Stop the turn and send it now."},
	}})

// Where the status bar sits
var StatusBar = Register(&Enum{Meta: Meta{Key: "display.tui_statusbar", Label: "Status bar",
	Short: "Where the line with model, context use and cost sits.",
	Group: GroupWorking, Order: 40, Slash: "statusbar"},
	Default: "top", On: "top", Off: "off",
	Values: []Choice{
		{"top", "Top", "Above the transcript."},
		{"bottom", "Bottom", "Below the input box."},
		{"off", "Off", "Hidden."},
	}})

// Facts the status bar shows
var StatusBarFields = Register(&List{Meta: Meta{Key: "display.status_bar.fields", Label: "Status bar fields",
	Short: "Which facts the status bar shows.",
	Help:  "Empty shows them all. Fields: model, context, context_pct, context_detail, cost, cwd, duration, latency, tps, cache_hit, compressions, bg_tasks, bg_subagents, battery, voice.",
	Group: GroupWorking, Order: 41}})

// Battery level in the status bar
var Battery = Register(&Bool{Meta: Meta{Key: "display.battery", Label: "Battery",
	Short: "Show the laptop battery level in the status bar.",
	Group: GroupWorking, Order: 42, Slash: "battery"},
	Default: false})

// Terminal bell when a turn finishes
var BellOnComplete = Register(&Bool{Meta: Meta{Key: "display.bell_on_complete", Label: "Bell when done",
	Short: "Ring the terminal bell when the agent finishes a turn.",
	Group: GroupWorking, Order: 50},
	Default: false})

// Terminal bell when the agent asks something
var BellOnPrompt = Register(&Bool{Meta: Meta{Key: "display.bell_on_prompt", Label: "Bell on question",
	Short: "Ring the terminal bell when the agent needs your answer.",
	Group: GroupWorking, Order: 51},
	Default: false})

// Reactions to messages sent to the agent as notes
var MessageReactions = Register(&Bool{Meta: Meta{Key: "display.message_reactions", Label: "Message reactions",
	Short: "Tell the agent about reactions you add to its messages.",
	Group: GroupWorking, Order: 150, Advanced: true, Apply: NextTurn},
	Default: false})
