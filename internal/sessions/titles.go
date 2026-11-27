package sessions

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Longest title a session keeps
	MaxTitle = 100
	// Characters of the opening message a title is made from
	titleInput = 1000
	// Longest instant title, and longest one the model writes
	derivedChars = 48
	modelChars   = 80
	// More words than this is an answer, not a title
	titleWords     = 12
	titleMaxTokens = 512
	pasteLabel     = "\n\nPasted content:\n"
)

// Helper task that titles sessions, and the settings it keeps besides those of every task
const (
	TitleTask       = "title_generation"
	TitleEnabled    = "enabled"
	TitleUpgrade    = "model_upgrade_enabled"
	TitlePreferFast = "prefer_fast_model"
	TitleLanguage   = "language"
)

var (
	titleControl   = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)
	titleInvisible = regexp.MustCompile(`[\x{200b}-\x{200f}\x{2028}-\x{202e}\x{2060}-\x{2069}\x{feff}\x{fffc}\x{fff9}-\x{fffb}]`)
	numberedTitle  = regexp.MustCompile(`^(.*?) #(\d+)$`)
	attachmentRef  = regexp.MustCompile(`@(?:file|folder):\S+`)
	contextFooter  = regexp.MustCompile(`(?s)\n+--- (?:Context Warnings|Attached Context) ---\n.*`)
	// @file: and @folder: references, quoted or bare, with a line range
	contextReference = regexp.MustCompile("(^|[^\\w/])@(?:file|folder):(?:(?:`[^`\\n]+`|\"[^\"\\n]+\"|'[^'\\n]+')(?::\\d+(?:-\\d+)?)?|\\S+)")
	fencedTitle      = regexp.MustCompile("(?s)^```(?:json)?\\s*(.*?)\\s*```$")
	looseTitle       = regexp.MustCompile(`"title"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	titleEdge        = regexp.MustCompile(`^[\W_]+|[\W_]+$`)
	thinkBlock       = regexp.MustCompile(`(?s)<think>.*?</think>|<thinking>.*?</thinking>|<reasoning>.*?</reasoning>|<thought>.*?</thought>|` +
		`<REASONING_SCRATCHPAD>.*?</REASONING_SCRATCHPAD>|<思考>.*?</思考>|<反思>.*?</反思>|<推理>.*?</推理>|<推敲>.*?</推敲>`)
)

// Tags of the wrappers a slash command or an editor puts around what was typed
var controlWrappers = []string{"command-message", "command-name", "command-args", "local-command-caveat", "local-command-stderr", "local-command-stdout",
	"task-notification", "system-reminder", "ide_opened_file", "ide_selection"}

// Starts of user messages icebath writes itself, which are never titled
var machinePrefixes = []string{"[CONTEXT COMPACTION", "[CONTEXT SUMMARY]:", "[Runtime note:", "[System note:", "[SYSTEM]",
	"[System: The active model for this chat has changed to "}

var (
	goodExamples = []string{"Fix login button on mobile", "Postgres connection pool exhaustion", "Friendly greeting"}
	vagueExample = "Code changes"
	// Titles the model echoed from the prompt, the greeting excepted
	echoes = []string{"code changes", "fix login button on mobile", "postgres connection pool exhaustion"}
	// Title asked for on a bare greeting, which a later message may replace
	greetings = []string{"friendly greeting", "friendly greeting in chat"}
)

// Helper task that titles sessions, registered with the helper models by the sessions host
type Titles struct{}

// Tasks session titling runs on, with the settings titles keep
func (Titles) Tasks() []auxiliary.Task {
	return []auxiliary.Task{{
		Name: TitleTask, Title: "Session titles", About: "The model that names new sessions", Order: 10, Timeout: 30, Critical: true,
		Settings: []config.Setting{
			{Key: TitleEnabled, Title: "Name new sessions", About: "Give each new session a title made from its first message.", Default: true},
			{Key: TitleUpgrade, Title: "Improve with the model", About: "Replace the quick first title with one that the model writes.", Default: true},
			{Key: TitleLanguage, Title: "Language", About: "The language that session titles are written in.", Default: "", Zero: "Your message's language"},
			{Key: TitlePreferFast, Title: "Prefer a fast model", About: "Use a quicker model of the session's provider when there is one.", Default: false,
				Advanced: true},
		},
	}}
}

// Title as a session keeps it: control and invisible characters dropped, whitespace collapsed. Empty clears the title
func SanitizeTitle(title string) (string, error) {
	title = strings.ToValidUTF8(title, "\uFFFD")
	title = titleInvisible.ReplaceAllString(titleControl.ReplaceAllString(title, ""), "")
	title = strings.Join(strings.Fields(title), " ")
	if n := utf8.RuneCountInString(title); n > MaxTitle {
		return "", fmt.Errorf("A title can be at most %s, and this one has %s.", strs.Counted(MaxTitle, "characters"), strs.Number(float64(n)))
	}
	return title, nil
}

// Rank of a title source, an unknown one ranking lowest
func TitleRank(source string) int {
	return slices.Index([]string{TitleDerived, TitleLLM, TitleUser}, source)
}

// Base of a numbered title and its number, false for one not numbered
func NumberedTitle(title string) (string, int, bool) {
	match := numberedTitle.FindStringSubmatch(title)
	if match == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(match[2])
	return match[1], n, err == nil
}

// Whether a person wrote the message, rather than icebath or an attachment alone
func Titleable(message string) bool {
	trimmed := strings.TrimLeftFunc(message, unicode.IsSpace)
	if trimmed == "" {
		return false
	}
	for _, prefix := range machinePrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return false
		}
	}
	return strings.TrimSpace(typed(message)) != "" && !attachmentOnly(message)
}

// What the user typed: a skill turn as its command and instruction, without the wrappers a slash command puts around it
func typed(message string) string {
	if described, ok := DescribeSkill(message, " — "); ok {
		message = described
	}
	current := strings.TrimSpace(message)
	for range len(controlWrappers) * 2 {
		stripped := unwrap(current)
		if stripped == current {
			break
		}
		current = stripped
	}
	return current
}

// Text without one leading wrapper, what follows it kept over what it holds
func unwrap(text string) string {
	lowered := strings.ToLower(text)
	for _, tag := range controlWrappers {
		open, closing := "<"+tag+">", "</"+tag+">"
		if !strings.HasPrefix(lowered, open) {
			continue
		}
		at := strings.Index(lowered, closing)
		if at < 0 {
			return strings.TrimSpace(text[len(open):])
		}
		if kept := strings.TrimSpace(text[at+len(closing):]); kept != "" {
			return kept
		}
		return strings.TrimSpace(text[len(open):at])
	}
	return text
}

// Whether only references to files and their attached text are left of the message
func attachmentOnly(message string) bool {
	return strings.TrimSpace(contextReference.ReplaceAllString(contextFooter.ReplaceAllString(message, ""), "$1")) == ""
}

// Text a title is made from: what was typed, with the start of a paste after it when there is one
func titleSource(message, preview string) string {
	message = typed(message)
	if preview = strings.TrimSpace(preview); preview == "" {
		return strs.Head(message, titleInput)
	}
	message = strings.TrimSpace(contextFooter.ReplaceAllString(message, ""))
	if strings.TrimSpace(attachmentRef.ReplaceAllString(message, "")) == "" {
		return strs.Head(preview, titleInput)
	}
	budget := min(utf8.RuneCountInString(message), titleInput/2)
	rest := titleInput - budget - utf8.RuneCountInString(pasteLabel)
	if rest <= 0 {
		return strs.Head(message, titleInput)
	}
	return strs.Head(message, budget) + pasteLabel + strs.Head(preview, rest)
}

// Instant title: the first line worth titling, cut at a word near 48 characters. Empty when nothing can be titled
func DeriveTitle(message, preview string) string {
	if strings.TrimSpace(preview) == "" && attachmentOnly(message) {
		return ""
	}
	line := ""
	for _, candidate := range strings.Split(titleSource(message, preview), "\n") {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			line = strings.Join(strings.Fields(candidate), " ")
			break
		}
	}
	if utf8.RuneCountInString(line) <= derivedChars {
		return line
	}
	cut := strs.Head(line, derivedChars)
	if at := strings.LastIndex(cut, " "); at >= 0 && utf8.RuneCountInString(cut[:at]) > derivedChars/2 {
		cut = cut[:at]
	}
	return strings.TrimRight(cut, " ,.;:—-") + "…"
}

// System prompt of the title model, in the language asked for or the user's
func titlePrompt(language string) string {
	rule := "- Write the title in the same language as the user's message."
	if language = strings.TrimSpace(language); language != "" {
		rule = "- Write the title in " + language + "."
	}
	var b strings.Builder
	b.WriteString("You name chat sessions. Given the user's opening message, write a title that lets them find this conversation again in a list.\n\n" +
		"Rules:\n" +
		"- 3 to 7 words, sentence case (capitalize only the first word and proper nouns).\n" +
		"- Name what the user wants DONE, not that they asked a question.\n" +
		"- Keep technical terms, filenames, numbers, and error codes exact.\n" +
		"- Drop filler words: the, this, my, a, an.\n" +
		"- No trailing punctuation, no quotes, no tool names, no 'Title:' prefix.\n" +
		"- Never answer the message. Name it.\n" +
		"- Always produce something, even for a bare greeting.\n" +
		rule + "\n")
	for _, title := range goodExamples {
		b.WriteString(`Good: {"title": "` + title + "\"}\n")
	}
	b.WriteString(`Too vague: {"title": "` + vagueExample + "\"}\n" +
		`Too long: {"title": "Investigate and fix the issue where the login button does not respond on mobile devices"}` + "\n\n" +
		`Reply with JSON only: {"title": "..."}`)
	return b.String()
}

// Title of a JSON reply, strictly parsed then by its field alone, false when it holds none
func jsonTitle(raw string) (string, bool) {
	var parsed struct {
		Title *string `json:"title"`
	}
	if json.Unmarshal([]byte(raw), &parsed) == nil && parsed.Title != nil {
		return strings.TrimSpace(*parsed.Title), true
	}
	match := looseTitle.FindStringSubmatch(raw)
	if match == nil {
		return "", false
	}
	var unquoted string
	if json.Unmarshal([]byte(`"`+match[1]+`"`), &unquoted) == nil {
		return strings.TrimSpace(unquoted), true
	}
	return strings.TrimSpace(match[1]), true
}

// Title in the model's reply: JSON, then the first line of prose, nothing of a reply cut short
func replyTitle(text string) string {
	raw := strings.TrimSpace(text)
	if match := fencedTitle.FindStringSubmatch(raw); match != nil {
		raw = strings.TrimSpace(match[1])
	}
	if raw != "" {
		if title, ok := jsonTitle(raw); ok {
			return title
		}
		truncated := strings.HasPrefix(raw, `{"`) || strings.HasPrefix(raw, `["`) || strings.HasPrefix(raw, "[{") ||
			strings.HasPrefix(raw, "```") && strings.Count(raw, "```")%2 == 1
		if !truncated {
			raw = strings.TrimSpace(thinkBlock.ReplaceAllString(raw, ""))
			for _, line := range strings.Split(raw, "\n") {
				if line = strings.TrimSpace(line); line != "" {
					if title := strings.TrimSpace(strings.Trim(dropTitlePrefix(line), `"'`)); title != "" {
						return title
					}
					break
				}
			}
		}
	}
	return ""
}

func dropTitlePrefix(text string) string {
	if strings.HasPrefix(strings.ToLower(text), "title:") {
		return strings.TrimSpace(text[len("title:"):])
	}
	return text
}

// Model title tidied: whitespace collapsed, quotes, a Title: prefix and trailing punctuation dropped, cut at 80 characters
func cleanTitle(text string) string {
	title := strings.TrimRight(dropTitlePrefix(strings.TrimSpace(strings.Trim(strings.Join(strings.Fields(text), " "), `"'`))), ".!,;:")
	if utf8.RuneCountInString(title) > modelChars {
		title = strings.TrimRightFunc(strs.Head(title, modelChars-3), unicode.IsSpace) + "..."
	}
	return title
}

func titleEdges(title string) string {
	return strings.ToLower(titleEdge.ReplaceAllString(strings.TrimSpace(title), ""))
}

// What asks for a title
type TitleRequest struct {
	// Message the user sent and the start of what they pasted with it
	Message, Preview string
	// Model to write the title on in place of the task's, such as a fast one of the session's provider
	Model string
}

// Title the helper model writes for the message, with the authority it is stored at: a greeting's stays derived, so a later message can
// replace it. Empty when titles are off, the message can't be titled or the model gave nothing usable
func GenerateTitle(ctx context.Context, aux *auxiliary.Client, request TitleRequest) (string, string, error) {
	task := auxiliary.Task{Name: TitleTask}
	if !aux.Config.GetBool(task.Key(TitleEnabled)) {
		return "", "", nil
	}
	snippet := titleSource(request.Message, request.Preview)
	if strings.TrimSpace(snippet) == "" || strings.TrimSpace(request.Preview) == "" && attachmentOnly(snippet) {
		return "", "", nil
	}
	reply, err := aux.Complete(ctx, auxiliary.Request{
		Task:      TitleTask,
		Messages:  auxiliary.Prompt(titlePrompt(aux.Config.GetString(task.Key(TitleLanguage))), snippet),
		Model:     request.Model,
		MaxTokens: titleMaxTokens,
		Reasoning: new("none"),
		Format: &provider.Format{Name: "session_title", Strict: true, Schema: map[string]any{
			"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string"}}, "required": []string{"title"}, "additionalProperties": false,
		}},
	})
	if err != nil {
		return "", "", err
	}
	title := cleanTitle(replyTitle(reply))
	switch {
	case title == "", len(strings.Fields(title)) > titleWords, slices.Contains(echoes, titleEdges(title)):
		return "", "", nil
	case slices.Contains(greetings, titleEdges(title)):
		return title, TitleDerived, nil
	}
	return title, TitleLLM, nil
}

// Families of fast models, the most wanted first, and words of models that are not plain chat models
var (
	fastFamilies = []string{"gpt-mini-latest", "gpt-nano-latest", "claude-haiku-latest", "gemini-flash-latest", "gpt-5.4-nano", "gpt-5.4-mini", "gpt-5-mini",
		"haiku-4.5", "gemini-3.6-flash", "flash-lite", "-nano", "-mini", "-flash", "haiku"}
	notFast = []string{"thinking", "reason", "-r1", "minilm", ":batch", ":free", "o1-", "o3-", "o4-", "codex", "audio", "-vl", "embed", "-tts",
		"-transcribe", "-realtime", "-image", "-search-preview"}
	versionDigits = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)?`)
)

// Newest model of the first fast family the list holds, empty when it holds none
func FastModel(ids []string) string {
	ids = slices.Clone(ids)
	slices.SortStableFunc(ids, func(a, b string) int { return -compareVersions(a, b) })
	for _, family := range fastFamilies {
		for _, id := range ids {
			lowered := strings.ToLower(id)
			if strings.Contains(lowered, family) && !slices.ContainsFunc(notFast, func(word string) bool { return strings.Contains(lowered, word) }) {
				return id
			}
		}
	}
	return ""
}

// Order of two model ids with the numbers in them compared as numbers, so a family's newest release sorts last
func compareVersions(a, b string) int {
	type part struct {
		number bool
		value  float64
		text   string
	}
	split := func(id string) []part {
		id = strings.ToLower(id)
		var parts []part
		at := 0
		for _, span := range versionDigits.FindAllStringIndex(id, -1) {
			if span[0] > at {
				parts = append(parts, part{text: id[at:span[0]]})
			}
			value, _ := strconv.ParseFloat(id[span[0]:span[1]], 64)
			parts = append(parts, part{number: true, value: value})
			at = span[1]
		}
		if at < len(id) {
			parts = append(parts, part{text: id[at:]})
		}
		return parts
	}
	x, y := split(a), split(b)
	for i := range min(len(x), len(y)) {
		switch {
		case x[i].number != y[i].number:
			if x[i].number {
				return 1
			}
			return -1
		case x[i].value != y[i].value:
			return cmp.Compare(x[i].value, y[i].value)
		case x[i].text != y[i].text:
			return strings.Compare(x[i].text, y[i].text)
		}
	}
	return cmp.Compare(len(x), len(y))
}
