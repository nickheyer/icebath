package tools

import (
	"fmt"
	"strings"
)

// OnboardingChatTitle names the onboarding chat
const OnboardingChatTitle = "Getting started"

// OnboardingGreeting is the English scripted opening
const OnboardingGreeting = "Hi, I'm Icebath. Let's set things up for you. Then we'll get something cool done."

var onboardingGreetings = map[string]string{
	"en":      OnboardingGreeting,
	"ar":      "مرحبًا، أنا Icebath. لنجهّز كل شيء لك، ثم ننجز معًا شيئًا رائعًا.",
	"de":      "Hallo, ich bin Icebath. Richten wir alles für Sie ein. Danach machen wir etwas Cooles.",
	"es":      "Hola, soy Icebath. Vamos a dejarlo todo listo para ti. Luego haremos algo genial.",
	"fr":      "Bonjour, je suis Icebath. Configurons tout pour vous. Ensuite, nous ferons quelque chose de génial.",
	"ja":      "こんにちは、Icebath です。まずはあなたに合わせて設定しましょう。そのあと、何かすごいものを作りましょう。",
	"ru":      "Привет, я Icebath. Давайте всё настроим под вас. А потом сделаем что-нибудь классное.",
	"zh":      "你好，我是 Icebath。我们先帮你把一切设置好，然后一起做点很酷的事。",
	"zh-hant": "你好，我是 Icebath。我們先幫你把一切設定好，然後一起做點很酷的事。",
}

var onboardingNameQuestions = map[string]string{
	"ar":      "بماذا أناديك؟",
	"de":      "Wie darf ich Sie nennen?",
	"es":      "¿Cómo quieres que te llame?",
	"fr":      "Comment dois-je vous appeler ?",
	"ja":      "なんとお呼びすればいいですか？",
	"ru":      "Как к вам обращаться?",
	"zh":      "我该怎么称呼你？",
	"zh-hant": "我該怎麼稱呼你？",
}

var traditionalChinese = map[string]bool{"hant": true, "tw": true, "hk": true, "mo": true}

// OnboardingUILanguage maps a locale to the desktop language it reads in, English when none fits
func OnboardingUILanguage(locale string) string {
	tags := strings.Split(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(locale)), "_", "-"), "-")
	if tags[0] == "zh" {
		for _, t := range tags[1:] {
			if traditionalChinese[t] {
				return "zh-hant"
			}
		}
		return "zh"
	}
	if _, ok := onboardingGreetings[tags[0]]; ok {
		return tags[0]
	}
	return "en"
}

// OnboardingGreetingFor is the scripted greeting in the locale's language
func OnboardingGreetingFor(locale string) string {
	return onboardingGreetings[OnboardingUILanguage(locale)]
}

var onboardingQuestions = map[string]string{
	"name":       "What should I call you?",
	"theme":      "Pick a colour.",
	"layout":     "Which layout?",
	"connectors": "Which apps should I work with?",
	"plugins":    "Want any of these?",
	"tour":       "Want a look around first?",
	"build":      "Know what you'd like me to make?",
}

// OnboardingQuestion is the card's own question, the name card asking in the user's language
func OnboardingQuestion(beat, locale string) string {
	if beat == "name" {
		if q, ok := onboardingNameQuestions[OnboardingUILanguage(locale)]; ok {
			return q
		}
	}
	return onboardingQuestions[beat]
}

const (
	onbSetThemeCall = `tool_call {"calls":[{"name":"set_theme","arguments":{"accent":"violet"}}]}`
	onbTourCall     = `tool_call {"calls":[{"name":"gui_tour","arguments":{"action":"start","preset":"%s"}}]}`
)

var onboardingProfileDimensions = [][2]string{
	{"agent experience", "new to AI apps / chat apps only / has used local agents / builds agents"},
	{"technical depth", "non-developer / tinkerer / developer"},
	{"creative work", "none / hobby / regular"},
	{"gaming", "none / casual / enthusiast"},
	{"local compute", "cloud only / small local models / strong local (Spark, large GPU or unified memory)"},
	{"work context", "personal / work / mixed"},
	{"machine role", "daily driver / dedicated AI box / remote server"},
}

var onboardingNvidiaLines = map[string]string{
	"rtx_spark": "It is an NVIDIA RTX Spark (Windows on Arm), built to run local models.",
	"dgx_spark": "It is an NVIDIA DGX Spark (Linux on Arm with a GB10), built to run local models.",
	"discrete":  "It has an NVIDIA GPU.",
}

// OnboardingFactsBlock is the machine facts paragraph of the opening prompt
func OnboardingFactsBlock(facts OnboardingFacts) string {
	whose := "this computer"
	if facts.FactsSource == "remote" {
		whose = "the computer running Icebath, which is not the one in front of the user"
	}
	arch := facts.Arch
	if arch == "" {
		arch = "an unknown architecture"
	}
	lines := []string{fmt.Sprintf("What you know about %s: %s on %s, a %s.", whose, facts.OS, arch, facts.MachineKind)}
	if line, ok := onboardingNvidiaLines[facts.NvidiaPlatform]; ok {
		lines = append(lines, line)
	}
	if facts.Locale != "" {
		lines = append(lines, fmt.Sprintf("Locale: %s.", facts.Locale))
	}
	lines = append(lines, "Never recite these. A fact that is missing is unknown. When the user says otherwise, believe the user.")
	return strings.Join(lines, " ")
}

// OnboardingOpeningPrompt is the hidden first turn of the onboarding chat holding every rule of the run
func OnboardingOpeningPrompt(facts OnboardingFacts) string {
	var dims []string
	for _, d := range onboardingProfileDimensions {
		dims = append(dims, fmt.Sprintf("- %s: %s", d[0], d[1]))
	}
	return fmt.Sprintf(`[System note: This is the user's first time opening the Icebath desktop app. You run their first-run onboarding in this chat. The beats, in order: their name, the theme (accent colour; light or dark only if they ask), the layout, connectors, plugins, a tour, then the first thing you build for them.

How it works:
- setup_beat shows a beat's card and waits for the user. setup_beat(beat="next") shows the next beat. Its result carries their answer and a reminder of what to do next. Follow the reminder.
- Before a card, acknowledge in a few words and at most one more sentence. The card asks its own question and shows its own options: never ask it yourself, never list the options in text.
- After a pick, say it by its label in three words or fewer. No opinion, no praise, never the same words twice.
- If they write a message while a card waits, the card closes unanswered and their message comes as their next turn (words typed on the name card are the name). If the message answers the card, apply it yourself and move on. Otherwise answer it properly and briefly, then bring the card back in one line with setup_beat(beat=<that beat>). Never show a card again unless you are bringing it back.
- If they ask you to pick (a colour, a layout), pick from what you know and say why in one line.
- If they skip a beat in words, call setup_beat(beat=<that beat>, skip=true), take the default, and never ask it again in the same form. If they want to stop altogether, say in one line that the Skip setup button ends it, then carry on; only they can end it, with that button.
- Off-script requests are done now, then you bring the card back.
- You may look around this computer when they say yes: installed apps, developer tools, the OS and the hardware, read with the terminal. Never personal files, documents, browser data or messages without a specific ask. A yes to looking is an off-script request: act on it at any beat.

Voice: every word you write is spoken to the user. Never think out loud. Never mention beats, cards, tools, reminders or a script, and never call this setup or onboarding (the Skip setup button's name is the one exception). One or two short, plain sentences per message: warm, like the front desk of somewhere good. No thanks, no praise, no stock lines, no em dashes, no exclamation marks. Write in the user's language when the locale is not English, labels included.

%s
If they ask what you know about them, answer truthfully: these machine basics and what they picked.

Tools you reach through tool_call (their call shapes):
- %s. accent: a swatch id (%s) or a hex colour #rrggbb; mode: light, dark or system; layout: sidebar-left (Basic) or terminal-deck (Elite).
- %s (preset quick or full).

Local models: the model picker chooses one. Explain the download and how it fits this hardware first, then point to Settings, Providers, Local Models. Never install a model runtime or download a model yourself.
Ask before sending, deleting or scheduling anything. Never turn off a security setting.

As you go, form a coarse read of them on these seven dimensions, from what they say and pick and from what you see when they let you look. Use it to choose suggestions and words; never say it out loud. Presence of an app is not use, and absence is unknown.
%s

The app has already greeted them and is asking their name.]`, OnboardingFactsBlock(facts), onbSetThemeCall, strings.Join(accentSwatches, ", "), fmt.Sprintf(onbTourCall, "quick"), strings.Join(dims, "\n"))
}

const onbLookAround = "Before the card, offer once to look around this computer and suggest what to connect. Only on yes, and " +
	"only installed apps, developer tools, the OS and the hardware; never personal files, documents, browser " +
	"data or messages without a specific ask. Then show the card with what you suggest: " +
	`setup_beat(beat="connectors", suggest=[<connector slugs>]). If they say no, show it with ` +
	`setup_beat(beat="connectors").`

var onbTypedAnswer = map[string]string{
	"name":       `setup_beat(beat="name", value="<their name>")`,
	"theme":      strings.Replace(onbSetThemeCall, `"accent":"violet"`, `"accent":"<colour>"`, 1),
	"layout":     `tool_call {"calls":[{"name":"set_theme","arguments":{"layout":"sidebar-left" or "terminal-deck"}}]}`,
	"connectors": `setup_beat(beat="connectors", value="continue") for "done" or "continue", skip=true for "skip"`,
	"plugins":    `setup_beat(beat="plugins", value="continue") for "done" or "continue", skip=true for "skip"`,
	"tour":       `setup_beat(beat="tour", value="quick", "full" or "skip"), bucketing their words into one of the three`,
	"build": `setup_beat(beat="build", value=<id>) for an option on the card; a task of their own is ` +
		`setup_beat(beat="build", value="own"), then build it`,
}

const onbClose = `When the first thing is built and shown, ask with clarify: "How does this look?" with the choices ` +
	`"Looks right", "Change something" and "Take it further", exactly. Put the next-step idea in your text. ` +
	`After they answer, call setup_beat(beat="done").`

var onbHowToWork = []string{
	"Real data only: this computer, their connected apps, the live web. Never mock or invent data.",
	"First reply: one line, then a tool call. At most two checks and one question before you start. Something " +
		"visible within a minute, finished within five.",
	"Save every file in ~/icebath-first-task/ and say where. No test files.",
	"Install or connect anything once, with exact ids. Never search the whole disk.",
	"Write the words they read with humanizer: short, plain, specific.",
}

const onbPreviewCall = `tool_call {"calls":[{"name":"desktop_preview","arguments":{"action":"open","url":"<file>"}}]}`

// Every host shows results in the preview pane, which watches a local file but not a remote one
func onbPreviewLine(facts OnboardingFacts) string {
	reload := "Open it once, early: it reloads when the file changes, so keep improving the same file while they watch."
	if facts.FactsSource == "remote" {
		reload = "On this host it does not reload by itself: open it again after each change."
	}
	return fmt.Sprintf("Show results in the preview pane beside the chat with %s; it shows HTML, images and PDFs. %s Say it is open only after the call succeeds.", onbPreviewCall, reload)
}

const onbNPM = "https://cdn.jsdelivr.net/npm/"

var onbPage = []string{
	"One self-contained HTML file, laid out for the pane (often about 600px wide) and able to widen.",
	"A standalone page: it sets its own background, font and colours. The preview pane does not theme it; " +
		"the theme variables are only for ::preview widgets in the chat.",
	"It moves within a second: a diagram that draws itself, a dot that runs a path, numbers that count up to " +
		"real values.",
	"Libraries, only what the page needs, each pinned as " + onbNPM + "<package>@<version>/<file>: gsap@3.13.0 for " +
		"motion, with its DrawSVGPlugin, MotionPathPlugin and ScrollTrigger (register each); d3@7.9.0 for charts of " +
		"real numbers; p5@1.11.3 for a 2D figure they drag or tune; matter-js@0.20.0 for physics; katex@0.16.11 " +
		"with its CSS and auto-render for maths; tone@15.0.4 for sound, started on a click; roughjs@4.6.6 for a " +
		"hand-drawn look.",
	`3D: three@0.170.0 is ES modules only. Use an import map: "three" to its build/three.module.js, ` +
		`"three/addons/" to its examples/jsm/.`,
}

var onbPageTasks = []string{"icebath", "explain", "brief", "week", "youtube", "site", "repo", "own"}

const onbConnectors = "Never route around a connector. " +
	`Reading a connected app: check it with manage_connections(action="status"). Find its tools with tool_search ` +
	"using the app slug, then the slug plus an action (a bare slug misses actions). tool_describe every action you " +
	`need in one batch, then call them through tool_call(calls=[{"name": "connectors__<slug>__<ACTION>", ` +
	`"arguments": {...}}]). Resolve names to ids before acting. Read only; ask before any write, and say that a ` +
	"write may run as the connector's bot."

var onbConnectorTasks = []string{"brief", "week"}

var onbOSLines = map[string]string{
	"macos": "This Mac: check with `which brew` and `ls -d /Applications/<App>.app`; install with " +
		"`brew install --cask <app>`; on Apple silicon check `lipo -archs` for a binary's architecture.",
	"windows": "This PC: check with `winget list --accept-source-agreements`; install with `winget install <id> " +
		"--accept-source-agreements --accept-package-agreements` (without both flags a fresh profile hangs " +
		"on an agreements prompt).",
	"linux": "This Linux machine: check with `which`, and `flatpak list` when flatpak is there; install with " +
		"`flatpak install -y flathub <id>`. Anything that needs sudo is listed for the user, never run.",
}

const (
	onbArmWindows = "On Windows on Arm, read arm64 vs x64 from winget's install output, not the manifest, and say when " +
		"an app is x64 only."
	onbSparkLine  = "This machine is built for local models; say so once when it fits what they asked."
	onbRemoteLine = "Icebath runs on another computer than the user's: say which machine anything is installed on."
)

// The task prompt lines once a first task is picked
func onbTaskPrompt(option, label string, resolveSkill func(string) string) []string {
	task := firstTaskByID(option)
	picked := ""
	if label != "" || task.Label != "" {
		shown := label
		if shown == "" {
			shown = task.Label
		}
		picked = fmt.Sprintf("They picked \"%s\". ", shown)
	}
	lines := []string{fmt.Sprintf("%sYour job: %s.", picked, task.Goal),
		fmt.Sprintf("Load these skills with skill_view first: %s.", strings.Join(firstTaskSkills(option, resolveSkill), ", ")), "Steps:"}
	for i, step := range task.Steps {
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, step))
	}
	return append(lines, "Output: "+task.Output, "Quality bar: "+task.Quality, "Done when: "+task.Done)
}

// Titled bullet list
func onbBullets(title string, lines []string) string {
	out := []string{title + ":"}
	for _, l := range lines {
		out = append(out, "- "+l)
	}
	return strings.Join(out, "\n")
}

// OnboardingBuildGuidance is the prompt once a first task is picked, label being the card text they clicked
func OnboardingBuildGuidance(option string, facts OnboardingFacts, resolveSkill func(string) string, label string) string {
	if resolveSkill == nil {
		resolveSkill = func(name string) string { return name }
	}
	work := append(append([]string(nil), onbHowToWork...), onbPreviewLine(facts))
	if containsString(onbConnectorTasks, option) {
		work = append(work, onbConnectors)
	}
	parts := []string{strings.Join(onbTaskPrompt(option, label, resolveSkill), "\n"), onbBullets("How to work", work)}
	if containsString(onbPageTasks, option) {
		parts = append(parts, onbBullets("When you make a page", onbPage))
	}
	osLine, ok := onbOSLines[facts.OS]
	if !ok {
		osLine = onbOSLines["linux"]
	}
	host := []string{osLine}
	if facts.OS == "windows" && facts.Arch == "arm64" {
		host = append(host, onbArmWindows)
	}
	if facts.NvidiaPlatform == "rtx_spark" || facts.NvidiaPlatform == "dgx_spark" {
		host = append(host, onbSparkLine)
	}
	if facts.FactsSource == "remote" {
		host = append(host, onbRemoteLine)
	}
	parts = append(parts, onbBullets("This computer", host), "When it is shown:\n"+onbClose)
	return strings.Join(parts, "\n\n")
}

// Comma joined names or none
func onbNames(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}

// The rows the model picks the build card from
func onbBuildMenu(record map[string]any) string {
	facts := OnboardingFactsFromStored(asMapOrEmpty(record["facts"]))
	var listed []string
	for _, task := range EligibleFirstTasks(facts, onbPicks(record)) {
		listed = append(listed, fmt.Sprintf("- %s (%s): %s.", task.ID, task.Label, task.Goal))
	}
	return fmt.Sprintf("For the build card, pick up to %d of these that fit what you learned about them, earlier rows "+
		`first when they fit, and call setup_beat(beat="build", options=[{"id": ..., "label": ...}]). Reword `+
		"each label to fit them and keep its energy: a short, specific action they would want to click, "+
		"%d characters max. Ids only from this list. \"%s\" is added last "+
		"by itself.\n%s", onboardingMaxPicks, onboardingMaxLabel, firstTaskByID(onboardingFixedID).Label, strings.Join(listed, "\n"))
}

// Map view of a value, empty for anything else
func asMapOrEmpty(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// The next step line of a reminder
func onbNext(record map[string]any) string {
	beat := OnboardingNextBeat(record)
	if beat == "" {
		return `Every beat is settled; after the build is shown and its close card answered, call setup_beat(beat="done").`
	}
	line := fmt.Sprintf(`Next: setup_beat(beat="next") shows the %s card.`, beat)
	if beat == "build" {
		return line + " " + onbBuildMenu(record)
	}
	return line
}

// String pick of the record, empty when unset
func onbPickString(record map[string]any, key string) string {
	v := onbPicks(record)[key]
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func onbAfterLayout(record map[string]any, _ OnboardingFacts) string {
	layout := onbPickString(record, "layout")
	label := map[string]string{"sidebar-left": "Basic", "terminal-deck": "Elite"}[layout]
	if label == "" {
		label = layout
	}
	return fmt.Sprintf("Layout %s is on, already applied. Do not call set_theme for it again. %s", label, onbNext(record))
}

func onbAfterConnectors(record map[string]any, _ OnboardingFacts) string {
	picks := onbPicks(record)
	connected := anyStrings(picks["connected"])
	again := ""
	if len(connected) > 0 {
		again = " These are already connected; do not connect them again."
	}
	return fmt.Sprintf("Connected: %s; newly connected: %s.%s %s", onbNames(connected), onbNames(anyStrings(picks["newly_connected"])), again, onbNext(record))
}

func onbAfterPlugins(record map[string]any, _ OnboardingFacts) string {
	installed := anyStrings(onbPicks(record)["installed"])
	again := ""
	if len(installed) > 0 {
		again = " They are already installed; do not install them again."
	}
	return fmt.Sprintf("Installed plugins: %s.%s %s", onbNames(installed), again, onbNext(record))
}

// Reminder after a tour pick naming the preset to start
func onbTour(preset string) func(map[string]any, OnboardingFacts) string {
	return func(record map[string]any, _ OnboardingFacts) string {
		return fmt.Sprintf(`Say "Here's where things live." Then, in this same turn, call %s and then show the `+
			"build card: it waits under the tour. Never bring the tour up again. %s", fmt.Sprintf(onbTourCall, preset), onbBuildMenu(record))
	}
}

// The beat whose card is waiting, else the next beat
func onbWaitingBeat(record map[string]any) string {
	if waiting, ok := record["waiting"].(map[string]any); ok {
		if beat, ok := waiting["beat"].(string); ok && beat != "" {
			return beat
		}
	}
	return OnboardingNextBeat(record)
}

var onbReminders = map[string]func(map[string]any, OnboardingFacts) string{
	"after_name": func(r map[string]any, _ OnboardingFacts) string {
		return fmt.Sprintf("They go by %s. Greet them by name in a few words. %s", onbPickString(r, "name"), onbNext(r))
	},
	"after_theme": func(r map[string]any, _ OnboardingFacts) string {
		accent := onbPickString(r, "accent")
		if accent == "" {
			accent = "Nous blue"
		}
		return fmt.Sprintf("Accent %s is on, already applied. Do not call set_theme for it again. React in three words or fewer. %s", accent, onbNext(r))
	},
	"after_layout": onbAfterLayout,
	"offer_first": func(r map[string]any, _ OnboardingFacts) string {
		return "The connectors card is not shown yet. " + onbLookAround
	},
	"after_connectors": onbAfterConnectors,
	"after_plugins":    onbAfterPlugins,
	"after_tour": func(r map[string]any, _ OnboardingFacts) string {
		return "The tour is already running; do not start it again. " + onbNext(r)
	},
	"after_build": func(r map[string]any, _ OnboardingFacts) string { return onbNext(r) },
	"tour_quick":  onbTour("quick"),
	"tour_full":   onbTour("full"),
	"tour_skip": func(r map[string]any, _ OnboardingFacts) string {
		return "No tour. In this same turn, show the build card. " + onbBuildMenu(r)
	},
	"moved_on": func(r map[string]any, _ OnboardingFacts) string {
		return "That beat is skipped; take its default and do not ask it again. " + onbNext(r)
	},
	"skip_all": func(map[string]any, OnboardingFacts) string {
		return "They ended setup. In one line, say they can ask you anything any time, then stop " +
			"guiding. The onboarding rules no longer apply; this is a normal chat."
	},
	"complete": func(map[string]any, OnboardingFacts) string {
		return "Onboarding is complete. Its rules no longer apply; this is a normal chat. Close in " +
			"one short line: whenever they are stuck, they can just ask."
	},
}

// A repeat apply of a settled beat steers the model forward instead of drawing another receipt
func onbAlready(record map[string]any, beat string) string {
	return fmt.Sprintf("The %s was already set by the card or an earlier call. Do not apply it again unless they ask for a change. %s", beat, onbNext(record))
}

func onbWroteText(_ map[string]any, _ OnboardingFacts, beat string) string {
	text := fmt.Sprintf("The %s card closed unanswered: they wrote a message instead. It comes next as their own turn; write nothing until it does.", beat)
	if beat == "theme" || beat == "layout" {
		text += fmt.Sprintf(" If it asks what you think or which one fits, pick the %s yourself: look around first if they asked, then say your pick and why in one line and apply it (%s).", beat, onbTypedAnswer[beat])
	}
	return text
}

func onbTimedOutText(_ map[string]any, _ OnboardingFacts, beat string) string {
	return fmt.Sprintf("The %s card closed without an answer. If a message from them comes next, answer it first. "+
		"Otherwise say one short line that they can pick up whenever they are back, then stop. When they "+
		`return, bring it back with setup_beat(beat="%s").`, beat, beat)
}

// OnboardingReminder renders a reminder key, beat naming the card a wrote or timed out result is about
func OnboardingReminder(key string, record map[string]any, facts OnboardingFacts, beat string, resolveSkill func(string) string) string {
	if key == "" {
		return ""
	}
	if resolveSkill == nil {
		resolveSkill = func(name string) string { return name }
	}
	if key == "wrote" || key == "timed_out" {
		target := beat
		if target == "" {
			target = onbWaitingBeat(record)
		}
		if target == "" {
			target = "build"
		}
		if key == "wrote" {
			return onbWroteText(record, facts, target)
		}
		return onbTimedOutText(record, facts, target)
	}
	if strings.HasPrefix(key, "build_") {
		option := strings.TrimPrefix(key, "build_")
		label := ""
		for _, o := range toAnyList(onbPicks(record)["build_options"]) {
			if m, ok := o.(map[string]any); ok && m["id"] == option {
				label, _ = m["label"].(string)
				break
			}
		}
		return OnboardingBuildGuidance(option, facts, resolveSkill, label)
	}
	if strings.HasPrefix(key, "already_") {
		return onbAlready(record, strings.TrimPrefix(key, "already_"))
	}
	return onbReminders[key](record, facts)
}

// OnboardingTurnNote is the note on a user turn while no card waits, empty once every card is settled
func OnboardingTurnNote(record map[string]any) string {
	beat := onbWaitingBeat(record)
	if beat == "" {
		return ""
	}
	return fmt.Sprintf("[System note: Onboarding continues; the %s card is closed. If their message answers it, apply it "+
		"yourself (%s) and move on. Otherwise answer their message first, then bring the card "+
		`back in one line with setup_beat(beat="%s").]`, beat, onbTypedAnswer[beat], beat)
}

// OnboardingFinishReminder is the next step line once every beat is settled
func OnboardingFinishReminder(record map[string]any) string { return onbNext(record) }
