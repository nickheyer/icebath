package tools

import (
	"fmt"
	"strings"
)

const (
	onboardingMaxPicks = 4
	onboardingMaxLabel = 32
	// The one fixed build row, always last on the card and never one of the model's picks
	onboardingFixedID = "icebath"
	// The value a task the user names in words records, never on the card
	onboardingOwnID          = "own"
	onboardingEveryTaskSkill = "humanizer"
)

// FirstTask is one row the model may put on the build card with the prompt its pick renders
type FirstTask struct {
	ID      string
	Label   string
	Gate    func(facts OnboardingFacts, picks map[string]any) bool
	Skills  []string
	Goal    string
	Steps   []string
	Output  string
	Quality string
	Done    string
}

func firstTaskAlways(OnboardingFacts, map[string]any) bool { return true }

// Gate passing when the plugins card recorded the named plugin as installed
func firstTaskPlugin(name string) func(OnboardingFacts, map[string]any) bool {
	return func(_ OnboardingFacts, picks map[string]any) bool {
		return containsString(anyStrings(picks["installed"]), name)
	}
}

func firstTaskConnected(_ OnboardingFacts, picks map[string]any) bool {
	return len(anyStrings(picks["connected"])) > 0
}

func firstTaskMacOS(facts OnboardingFacts, _ map[string]any) bool { return facts.OS == "macos" }

// Strings of a list value, nil for anything else
func anyStrings(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, s)
			} else {
				out = append(out, fmt.Sprint(item))
			}
		}
		return out
	}
	return nil
}

var firstTasks = []FirstTask{
	{ID: "icebath", Label: "Explain how Icebath works", Gate: func(OnboardingFacts, map[string]any) bool { return false },
		Skills: []string{"icebath-agent", "claude-design", "p5js"},
		Goal:   "build a living page that shows how Icebath works, from the real docs and this install",
		Steps: []string{
			"Read https://hermes-agent.nousresearch.com/docs/llms.txt once with web_extract. Open a page only to check a fact you show.",
			"Read this install: the model, how many skills are installed, the connected apps, and what this chat did (tools called, skills loaded, their name and picks).",
			"Within a minute, open it in the preview pane with the title, those facts and three screen headings. Then fill one screen at a time.",
			"Screen 1, the agent loop: a pulse runs model, tool, result, back to the model, labelled with the tools this chat called.",
			"Screen 2, learning: a procedure becomes a skill that loads next time; memory keeps facts across chats. Use the skills this chat loaded.",
			"Screen 3, out in the world: cron jobs, connectors and messaging, one example each built on their apps.",
		},
		Output:  "One HTML file in ~/icebath-first-task/, three screens that each fill the pane with one animated graphic; open in the preview pane.",
		Quality: "Every name and number comes from the docs or this install. A few short lines per screen.",
		Done:    "All three screens move and show real facts, and you said where the file is."},
	{ID: "games", Label: "Tune my games for this PC", Gate: firstTaskPlugin("nvidia-app"), Skills: []string{"nvidia-app:nvidia-app"},
		Goal: "optimise the games on this PC with NVIDIA App and show what changed",
		Steps: []string{
			"Its tools are mcp__nvidia_app__nvapp_*. If none are listed, check without changing anything whether NVIDIA App is installed and running. Not installed: open https://www.nvidia.com/en-us/software/nvidia-app/ in the preview pane and say how to install it. Not running: say how to start it. Check again each time they say done.",
			"List the installed games and show them at once. If the driver status shows a newer Game Ready driver, say so in one line.",
			"Ask in one clarify which preset to apply (Performance, Visual or Recommended), naming the games.",
			"Apply it, then read each game back.",
		},
		Output:  "A before and after table in chat: one row per game, what changed.",
		Quality: "Only settings NVIDIA App reports and really changed. Never guess a value.",
		Done:    "The changes are read back and in the table. Next-step idea: launch one of the games."},
	{ID: "broadcast", Label: "Make my mic and camera pro", Gate: firstTaskPlugin("nvidia-broadcast"), Skills: []string{"nvidia-broadcast:nvidia-broadcast"},
		Goal: "set up NVIDIA Broadcast so their mic and camera are ready for a call",
		Steps: []string{
			"Its tools are mcp__nvidia_broadcast__*. If none are listed, check without changing anything whether NVIDIA Broadcast is installed and running. Not installed or too old: open https://www.nvidia.com/en-us/geforce/broadcasting/broadcast-app/ in the preview pane and offer the install as the skill says. Not running: say how to start it. Check again each time they say done.",
			"Read the devices and the effects this machine offers, then ask in one clarify what it is for: a meeting, a stream or a noisy room.",
			"Turn the effects on one at a time. After each, say what to look at or listen for in the Broadcast window.",
		},
		Output:  "A short list in chat: each effect, its device, before and after.",
		Quality: "Only effects Broadcast reports on this machine.",
		Done:    "The effects are on and read back, and they know how to turn each one off."},
	{ID: "blender", Label: "Build a 3D scene in Blender", Gate: firstTaskPlugin("blender"), Skills: []string{"blender:blender"},
		Goal: "build a small 3D scene live in their open Blender, one visible step at a time",
		Steps: []string{
			"Drive the open Blender with its mcp__blender__* tools. Never run Blender from the terminal.",
			"If Blender is not connected (no tools, or get_objects_summary fails), this is a first run: open https://www.blender.org/lab/mcp-server/ in the preview pane and walk them through it in a short list: Blender 5.1 or newer open; drag the page's Drag and Drop into Blender button into Blender twice (the first adds the Blender Lab repository, the second installs the add-on); allow Online access; start the MCP bridge on port 9876. Check again each time they say done.",
			"Offer three scene ideas in one clarify: two that fit them, one that shows off this machine.",
			"First capture their Blender window with get_screenshot_of_window_as_image, copy the image it saved over preview.png and open that in the preview pane, so they watch Icebath work. Then build it in a new scene named Icebath, in camera view, in four calls they can watch: ground and camera, main objects, materials and light, framing. After each, capture the window over preview.png again; the pane reloads it.",
			"Render one 1280x720 EEVEE still with render_viewport_to_path, copy the filepath it returns to render.png and open it in the pane. Look at it before you call it done: lit, not black, framed. Fix and re-render once at most.",
			"Save only the new scene with bpy.data.libraries.write as scene.blend, and the code as scene.py.",
		},
		Output:  "scene.blend, scene.py, preview.png and render.png in ~/icebath-first-task/; the render open in the preview pane.",
		Quality: "Made in their open Blender, never headless, never a placeholder.",
		Done:    "The final still is shown and you named the files. Next-step idea: a turntable video or a GPU Cycles render."},
	{ID: "brief", Label: "Brief me on today", Gate: firstTaskConnected, Skills: []string{"claude-design"},
		Goal: "write today's brief from their connected apps",
		Steps: []string{
			"Check the connected apps with manage_connections(action=\"status\").",
			"Read today from them: mail, calendar, messages, tasks. Read only.",
			"Show a first version early, then fill it in: what needs attention, what changed, what is next.",
			"Offer a morning cron job that sends the brief. Schedule it only on yes.",
		},
		Output:  "One designed HTML page in ~/icebath-first-task/, open in the preview pane.",
		Quality: "Every item comes from a connected app and says which one. Nothing invented.",
		Done:    "The brief is shown and the morning job is set up or declined."},
	{ID: "week", Label: "Recap my week on one page", Gate: firstTaskConnected, Skills: []string{"weekly-review-planning", "claude-design"},
		Goal: "make one page that recaps their past seven days from the connected apps",
		Steps: []string{
			"Check the connected apps with manage_connections(action=\"status\").",
			"Read the past seven days from them. Read only.",
			"Show a first version early, then fill it in: meetings, threads, work shipped, open loops.",
			"End with a short plan for next week built from the open loops.",
		},
		Output:  "One designed HTML page in ~/icebath-first-task/, open in the preview pane.",
		Quality: "Every item comes from a connected app and says which one. Nothing invented.",
		Done:    "The page is shown with all five parts filled from real data."},
	{ID: "mac", Label: "Set up my Mac helpers", Gate: firstTaskMacOS, Skills: []string{"apple-reminders", "apple-notes", "icebath-agent"},
		Goal: "set up their Mac helpers: one real reminder, one real note and a morning routine",
		Steps: []string{
			"Ask in one clarify what the reminder and the note should be, with three concrete suggestions.",
			"Make the reminder in Reminders and the note in Notes with the skills.",
			"Propose a morning routine as a cron job: what it does and when. Schedule it only on yes.",
		},
		Output:  "The reminder and note in their apps and the cron job, each named in chat.",
		Quality: "Real items they asked for. No test items.",
		Done:    "The reminder and note exist, and the morning job is set up or declined."},
	{ID: "notes", Label: "Turn my notes into a wiki", Gate: firstTaskAlways, Skills: []string{"obsidian"},
		Goal: "turn their notes into a small linked wiki",
		Steps: []string{
			"Ask which folder holds their notes. Never search the disk for it.",
			"Read the notes there and find the three main topics.",
			"Write an index page and three linked topic pages, each citing the notes it came from.",
			"Show the index page.",
		},
		Output:  "Four Markdown pages in ~/icebath-first-task/wiki/, unless they ask for their notes folder.",
		Quality: "Only what their notes say. Every link resolves.",
		Done:    "The four pages exist, the links work and the index is shown."},
	{ID: "explain", Label: "Explain a topic I care about", Gate: firstTaskAlways, Skills: []string{"grounded-citations", "claude-design", "p5js", "blocked-page-recovery"},
		Goal: "explain a topic they care about on one page built around a figure they can play with",
		Steps: []string{
			"If the label they clicked names a topic, use it. Otherwise offer three guesses from what you know about them in one clarify.",
			"Within a minute, open it in the preview pane with the title and the figure already moving: the one mechanism that makes the topic click, which they can drag or tune.",
			"Research it with citations (load arxiv for a technical topic), then write short sections around the figure. End on the most surprising thing you found.",
		},
		Output:  "One HTML file in ~/icebath-first-task/, open in the preview pane, sources at the end.",
		Quality: "Every claim and every number in the figure is cited. Under 400 words of text.",
		Done:    "The figure responds to input and every source is listed."},
	{ID: "youtube", Label: "Turn a video into a page", Gate: firstTaskAlways, Skills: []string{"youtube-content", "claude-design", "p5js"},
		Goal: "turn one video into a page that lays its ideas out visually",
		Steps: []string{
			"Ask for the video link.",
			"Get the transcript with the skill.",
			"Pull out the main ideas, each with a timestamp and a quote.",
			"Show a first version early, then lay the ideas out visually, with one figure where it helps.",
		},
		Output:  "One self-contained HTML file in ~/icebath-first-task/, open in the preview pane; each timestamp links to that moment in the video.",
		Quality: "Designed with claude-design. Quotes are exact words from the transcript.",
		Done:    "The page is shown and every idea has its timestamp and quote."},
	{ID: "watch", Label: "Watch something for me daily", Gate: firstTaskAlways, Skills: []string{"competitor-news-monitor|product-price-monitor", "grounded-citations"},
		Goal: "watch something for them every day and report here",
		Steps: []string{
			"Ask what to watch in one clarify, with three concrete options from what you know about them.",
			"Run the first check now and report it with sources.",
			"Propose a daily cron job: when it runs and what it reports. Schedule it only on yes.",
		},
		Output:  "The first report in chat, and the daily cron job.",
		Quality: "Every finding has a source link. Nothing invented.",
		Done:    "The first report is shown and the daily job is set up or declined."},
	{ID: "site", Label: "Design a page for me", Gate: firstTaskAlways, Skills: []string{"claude-design", "popular-web-designs", "p5js"},
		Goal: "design one web page for them",
		Steps: []string{
			"If you do not know what the page is for, ask in one line.",
			"Offer three design directions in one clarify.",
			"Show a first version early with a p5js hero, then refine the same file.",
			"Offer one variation.",
		},
		Output:  "One self-contained HTML file in ~/icebath-first-task/, open in the preview pane.",
		Quality: "Designed with the skills. Real content they gave you, never filler text.",
		Done:    "The page is shown and a variation is offered."},
	{ID: "repo", Label: "Map one of my repos", Gate: firstTaskAlways, Skills: []string{"codebase-inspection", "architecture-diagram", "github"},
		Goal: "map one of their code repositories on an architecture page",
		Steps: []string{
			"Ask which repo, as a path or a URL, and ask before reading it.",
			"Show its language breakdown.",
			"Read its entry points and main modules.",
			"Draw the architecture page: the modules, how they connect, where to start reading.",
		},
		Output:  "One self-contained HTML file in ~/icebath-first-task/, open in the preview pane.",
		Quality: "Only modules that are really in the repo, named by their paths.",
		Done:    "The page is shown and every box on it maps to a real path."},
}

// A task the user names in words, the same prompt shape without a catalog row
var ownTask = FirstTask{ID: onboardingOwnID, Gate: func(OnboardingFacts, map[string]any) bool { return false }, Skills: []string{"icebath-agent"},
	Goal: "build the task they named in their own words",
	Steps: []string{
		"Also load the skills that fit what they asked.",
		"If you know what it is, say the first slice in one line and start. If not, ask one short question (a clarify with three concrete options is fine).",
		"Cut a big ask to a first slice and say so in one line.",
		"Show a first version early, then refine it.",
	},
	Output:  "Whatever fits the task; a page is one self-contained HTML file, open in the preview pane.",
	Quality: "Real data only, designed with the skills when it is a page.",
	Done:    "The first slice is shown and you said where its files are."}

// Catalog row by id, the own task for unknown ids
func firstTaskByID(id string) FirstTask {
	for _, t := range firstTasks {
		if t.ID == id {
			return t
		}
	}
	return ownTask
}

// Reports whether the id names a catalog row
func firstTaskKnown(id string) bool {
	for _, t := range firstTasks {
		if t.ID == id {
			return true
		}
	}
	return false
}

// EligibleFirstTasks are the rows the model may pick on this machine, in catalog order
func EligibleFirstTasks(facts OnboardingFacts, picks map[string]any) []FirstTask {
	var out []FirstTask
	for _, t := range firstTasks {
		if t.ID != onboardingFixedID && t.Gate(facts, picks) {
			out = append(out, t)
		}
	}
	return out
}

// Build card options, the model's picks as given then the fixed row
func firstTaskCardOptions(chosen any) []any {
	var out []any
	for _, option := range toAnyList(chosen) {
		if m, ok := option.(map[string]any); ok {
			out = append(out, copyMap(m))
		} else {
			out = append(out, option)
		}
	}
	return append(out, map[string]any{"id": onboardingFixedID, "label": firstTaskByID(onboardingFixedID).Label})
}

// Why one option is refused, empty when it is fine
func firstTaskOptionError(option any, eligible, seen map[string]bool) string {
	m, ok := option.(map[string]any)
	if !ok {
		return `each option is {"id": ..., "label": ...}`
	}
	id, ok := m["id"].(string)
	if !ok {
		return `each option is {"id": ..., "label": ...}`
	}
	if id == onboardingFixedID {
		return fmt.Sprintf("%s is added last by itself; do not pick it", onboardingFixedID)
	}
	if !eligible[id] {
		return fmt.Sprintf("%s is not an eligible id", pyRepr(id))
	}
	if seen[id] {
		return fmt.Sprintf("%s is picked twice", pyRepr(id))
	}
	label, ok := m["label"].(string)
	if !ok || strings.TrimSpace(label) == "" {
		return fmt.Sprintf("the label for %s is empty", id)
	}
	if n := len([]rune(label)); n > onboardingMaxLabel {
		return fmt.Sprintf("\"%s\" is %d characters; the limit is %d. Shorten it", label, n, onboardingMaxLabel)
	}
	return ""
}

// Why the model's build picks are refused, naming the eligible ids, empty when valid
func firstTaskPickError(options any, facts OnboardingFacts, picks map[string]any) string {
	var eligibleIDs []string
	eligible := map[string]bool{}
	for _, t := range EligibleFirstTasks(facts, picks) {
		eligibleIDs = append(eligibleIDs, t.ID)
		eligible[t.ID] = true
	}
	var problems []string
	list, isList := options.([]any)
	switch {
	case !isList || len(list) == 0:
		problems = append(problems, "options is required")
	case len(list) > onboardingMaxPicks:
		problems = append(problems, fmt.Sprintf("pick at most %d", onboardingMaxPicks))
	default:
		seen := map[string]bool{}
		for _, option := range list {
			if problem := firstTaskOptionError(option, eligible, seen); problem != "" {
				problems = append(problems, problem)
			} else if m, ok := option.(map[string]any); ok {
				seen[m["id"].(string)] = true
			}
		}
	}
	if len(problems) == 0 {
		return ""
	}
	return fmt.Sprintf("%s. Call setup_beat(beat=\"build\", options=[{\"id\": ..., \"label\": ...}]) with up to %d of these ids: %s.",
		strings.Join(problems, "; "), onboardingMaxPicks, strings.Join(eligibleIDs, ", "))
}

// The skills a task loads first, humanizer last, each by the name skill_view loads it with here
func firstTaskSkills(option string, resolveSkill func(string) string) []string {
	names := append(append([]string(nil), firstTaskByID(option).Skills...), onboardingEveryTaskSkill)
	var kept []string
	for _, entry := range names {
		var found []string
		for _, name := range strings.Split(entry, "|") {
			if resolved := resolveSkill(name); resolved != "" {
				found = append(found, resolved)
			}
		}
		if len(found) > 0 {
			kept = append(kept, strings.Join(found, " or "))
		}
	}
	return kept
}
