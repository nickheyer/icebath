package tui

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Collapsed paste or attached image the composer shows as a label in the draft
type composerToken struct {
	Kind  string
	Label string
	Text  string
	Path  string
	Mime  string
	Index int
}

// Outcome of a paste or attachment, the draft and the caret after it
type composerPasteResult struct {
	Value  string
	Cursor int
}

// Typed reply of command.dispatch, one of alias, exec, plugin, prefill, send or skill
type commandDispatch struct {
	Type    string
	Target  string
	Output  *string
	Message *string
	Notice  *string
	Display *string
	Name    string
}

// Gateway completion request the composer sends for its draft
type completionRequest struct {
	Method      string
	Params      map[string]any
	ReplaceFrom int
	SkillsOnly  bool
}

const (
	tokenMaxCount      = 32
	tokenMaxTotalUnits = 4 * 1024 * 1024
)

var (
	pasteTokenRunRe     = regexp.MustCompile(`[ \t]?(?:\[\[[^\n]*?\]\])`)
	hasInterpolationRe  = regexp.MustCompile(`\{!.+?\}`)
	slashCommandRe      = regexp.MustCompile(`^/[^/` + jsWS + `]*(?:[` + jsWS + `]|$)`)
	leadingSlashesRe    = regexp.MustCompile(`^/+`)
	slashPartsRe        = regexp.MustCompile(`(?s)^([^` + jsWS + `]+)(.*)$`)
	slashArgumentRe     = regexp.MustCompile(`(?s)^/[^` + jsWS + `]+[` + jsWS + `]+(.+)$`)
	inlineSlashRe       = regexp.MustCompile(`[` + jsWS + `]/([a-zA-Z][\w-]*)?$`)
	localCommandShapeRe = regexp.MustCompile(`^/[\w-]+$`)
	modelCommandRe      = regexp.MustCompile(`^/model(?:[` + jsWS + `]|$)`)
	tabPathRe           = regexp.MustCompile(`((?:["']?(?:[A-Za-z]:[\\/]|\.{1,2}/|~/|/|@|[^"'` + "`" + jsWS + `]+/))[^` + jsWS + `]*)$`)
	driveLetterRe       = regexp.MustCompile(`^[A-Za-z]:[/\\]`)
	quotedDriveLetterRe = regexp.MustCompile(`^["'][A-Za-z]:[/\\]`)
)

// Paste and image token of a draft, the double bracket label the composer inserts
var pasteSnippetRe = regexp.MustCompile(`\[\[[^\n]*?\]\]`)

// Label of the image token with that index
func imageToken(index int) string { return tr("libText.attachments.imageToken", index) }

// Images whose labels are still in the draft, with their staged path and mime
func imageAttachments(value string, tokens []composerToken) []imageAttachment {
	out := []imageAttachment{}
	for _, t := range tokens {
		if t.Kind == "image" && t.Mime != "" && strings.Contains(value, t.Label) {
			out = append(out, imageAttachment{Path: t.Path, Mime: t.Mime})
		}
	}
	return out
}

// Index the next image token takes, one past the highest so far
func nextImageIndex(tokens []composerToken) int {
	highest := 0
	for _, t := range tokens {
		if t.Kind == "image" {
			highest = max(highest, t.Index)
		}
	}
	return highest + 1
}

// Tokens whose labels no longer appear in the draft
func droppedTokens(tokens []composerToken, value string) []composerToken {
	live := map[string]bool{}
	for _, label := range pasteSnippetRe.FindAllString(value, -1) {
		live[label] = true
	}
	var out []composerToken
	for _, t := range tokens {
		if !live[t.Label] {
			out = append(out, t)
		}
	}
	return out
}

// Expander that swaps paste labels for their text and drops image labels, each label used once in order
func expandTokens(tokens []composerToken) func(string) string {
	byLabel := map[string][]composerToken{}
	for _, t := range tokens {
		byLabel[t.Label] = append(byLabel[t.Label], t)
	}
	return func(value string) string {
		out := pasteTokenRunRe.ReplaceAllStringFunc(value, func(match string) string {
			label := strings.TrimLeftFunc(match, isJSSpace)
			queue := byLabel[label]
			if len(queue) == 0 {
				return match
			}
			token := queue[0]
			byLabel[label] = queue[1:]
			if token.Kind == "paste" {
				return match[:len(match)-len(token.Label)] + token.Text
			}
			return ""
		})
		return strings.TrimFunc(out, isJSSpace)
	}
}

// Expander of the paste tokens only, image labels left in place
func expandPasteTokens(tokens []composerToken) func(string) string {
	var pastes []composerToken
	for _, t := range tokens {
		if t.Kind == "paste" {
			pastes = append(pastes, t)
		}
	}
	return expandTokens(pastes)
}

// Whether a draft holds a shell interpolation
func hasInterpolation(s string) bool { return hasInterpolationRe.MatchString(s) }

// Whether text starts with a slash command word
func looksLikeSlashCommand(text string) bool { return slashCommandRe.MatchString(text) }

// Lowercase command name and trimmed argument of a slash command, both empty when nothing follows the slashes
func parseSlashCommand(command string) (name, arg string) {
	m := slashPartsRe.FindStringSubmatch(leadingSlashesRe.ReplaceAllString(command, ""))
	if m == nil {
		return "", ""
	}
	return strings.ToLower(m[1]), strings.TrimFunc(m[2], isJSSpace)
}

func optionalString(row map[string]any, key string) *string {
	if s, isStr := row[key].(string); isStr {
		return &s
	}
	return nil
}

// Reply of command.dispatch read by its type, nil for any reply missing a required field
func parseCommandDispatch(raw any) *commandDispatch {
	row, isObject := raw.(map[string]any)
	if !isObject || row == nil {
		return nil
	}
	typ, _ := row["type"].(string)
	switch typ {
	case "alias":
		if target, isStr := row["target"].(string); isStr {
			return &commandDispatch{Type: "alias", Target: target}
		}
	case "exec", "plugin":
		return &commandDispatch{Type: typ, Output: optionalString(row, "output")}
	case "prefill":
		if message := optionalString(row, "message"); message != nil {
			return &commandDispatch{Type: "prefill", Message: message, Notice: optionalString(row, "notice")}
		}
	case "send":
		if message := optionalString(row, "message"); message != nil {
			return &commandDispatch{Type: "send", Display: optionalString(row, "display"), Message: message, Notice: optionalString(row, "notice")}
		}
	case "skill":
		if name, isStr := row["name"].(string); isStr {
			return &commandDispatch{Type: "skill", Display: optionalString(row, "display"), Message: optionalString(row, "message"), Name: name}
		}
	}
	return nil
}

// Model argument scoped to the session, the scope flags replaced by one trailing session flag
func sessionScopedModelArg(value string) string {
	var kept []string
	for _, part := range strings.FieldsFunc(value, isJSSpace) {
		if part != tuiSessionModelFlag && part != "--global" && part != "--session" {
			kept = append(kept, part)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, " ") + " --session"
}

// Skill query typed after a space and a slash, with the offset of the slash
func inlineSlashTrigger(text string) (query string, start int, ok bool) {
	m := inlineSlashRe.FindStringSubmatch(text)
	if m == nil {
		return "", 0, false
	}
	return m[1], unitLen(text) - unitLen(m[1]) - 1, true
}

// Draft with a completion row applied from the replace offset, one slash kept
func applyCompletion(value, rowText string, compReplace int) string {
	text := rowText
	if compReplace >= 1 && jsSlice(value, compReplace-1, compReplace) == "/" && strings.HasPrefix(rowText, "/") {
		text = rowText[1:]
	}
	return jsSlice(value, 0, compReplace) + text
}

// Completion a submit applies first, false for a typed local command or a row that changes nothing
func completionToApplyOnSubmit(value string, rowText *string, compReplace int, isLocalCommand func(string) bool) (string, bool) {
	typed := strings.TrimFunc(value, isJSSpace)
	if rowText == nil || *rowText == "" || (localCommandShapeRe.MatchString(typed) && isLocalCommand != nil && isLocalCommand(typed[1:])) {
		return "", false
	}
	next := applyCompletion(value, *rowText, compReplace)
	if next != value && strings.TrimRightFunc(next, isJSSpace) != strings.TrimRightFunc(value, isJSSpace) {
		return next, true
	}
	return "", false
}

// Completion request for a draft, slash names, inline skills or a path word, false when none applies
func (s *tuiStore) completionRequestForInput(input string) (completionRequest, bool) {
	isSlash := looksLikeSlashCommand(input)
	pathWord := ""
	if !isSlash {
		if m := tabPathRe.FindStringSubmatch(input); m != nil {
			pathWord = m[1]
		}
	}
	if isSlash && modelCommandRe.MatchString(input) {
		return completionRequest{}, false
	}
	if query, start, inline := inlineSlashTrigger(input); inline {
		return completionRequest{Method: "complete.slash", Params: map[string]any{"text": "/" + query}, ReplaceFrom: start + 1, SkillsOnly: true}, true
	}
	if isSlash {
		return completionRequest{Method: "complete.slash", Params: map[string]any{"text": input}, ReplaceFrom: 1}, true
	}
	if pathWord == "" {
		return completionRequest{}, false
	}
	params := map[string]any{"word": pathWord}
	if cwd := s.sessionCwd(); cwd != "" {
		params["cwd"] = cwd
	}
	if s.ui.Sid != "" {
		params["session_id"] = s.ui.Sid
	}
	return completionRequest{Method: "complete.path", Params: params, ReplaceFrom: unitLen(input) - unitLen(pathWord)}, true
}

// Whether the process runs inside an SSH session
func isRemoteShellSession(lookup func(string) (string, bool)) bool {
	for _, k := range []string{"SSH_CONNECTION", "SSH_TTY", "SSH_CLIENT"} {
		if v, _ := lookup(k); v != "" {
			return true
		}
	}
	return false
}

// Text a submit sends with its paste labels expanded and the images still referenced
func prepareSubmission(display string, tokens []composerToken) (string, []imageAttachment) {
	return expandTokens(tokens)(display), imageAttachments(display, tokens)
}

// Command a slash submit runs with pastes expanded, the compact display kept for the transcript
func prepareSlashSubmission(display string, tokens []composerToken) (command, shown string) {
	return expandPasteTokens(tokens)(display), display
}

// Whether a submission goes through shell interpolation first
func shouldInterpolateSubmission(display string) bool { return hasInterpolation(display) }

func slashArgument(command string) string {
	if m := slashArgumentRe.FindStringSubmatch(command); m != nil {
		return m[1]
	}
	return ""
}

// Queue entry of a slash command argument, nil when the command carries no argument
func queueItemFromSlash(displayCommand, expandedCommand string) *queueItem {
	display := slashArgument(displayCommand)
	if strings.TrimFunc(display, isJSSpace) == "" {
		return nil
	}
	item := newQueueItem(slashArgument(expandedCommand), display)
	return &item
}

// Newest tokens that fit the count and size caps, the same slice when all fit
func trimTokens(tokens []composerToken) []composerToken {
	total := 0
	var out []composerToken
	for i := len(tokens) - 1; i >= 0; i-- {
		size := unitLen(tokens[i].Text)
		if len(out) >= tokenMaxCount || total+size > tokenMaxTotalUnits {
			break
		}
		total += size
		out = append([]composerToken{tokens[i]}, out...)
	}
	if len(out) == len(tokens) {
		return tokens
	}
	return out
}

// Draft with text inserted at a byte caret, padded by a space against adjacent non space runes
func insertAtCursor(value string, cursor int, text string) composerPasteResult {
	lead, tail := "", ""
	if r, _ := utf8.DecodeLastRuneInString(value[:cursor]); cursor > 0 && !isJSSpace(r) {
		lead = " "
	}
	if r, _ := utf8.DecodeRuneInString(value[cursor:]); cursor < len(value) && !isJSSpace(r) {
		tail = " "
	}
	insert := lead + text + tail
	return composerPasteResult{Cursor: cursor + len(insert), Value: value[:cursor] + insert + value[cursor:]}
}

// Whether a paste reaches the configured line or character threshold for collapsing
func (s *tuiStore) shouldCollapsePaste(cleanedText string, lineCount int) bool {
	lines, chars := s.ui.PasteCollapseLines, s.ui.PasteCollapseChars
	return (lines > 0 && lineCount >= lines) || (chars > 0 && unitLen(cleanedText) >= chars)
}

// Whether pasted text is a single path or file URL dragged into the terminal
func looksLikeDroppedPath(text string) bool {
	trimmed := strings.TrimFunc(text, isJSSpace)
	if trimmed == "" || strings.Contains(trimmed, "\n") {
		return false
	}
	for _, prefix := range []string{"file://", "~/", "./", "../", `"/`, "'/", `"~`, "'~"} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	if driveLetterRe.MatchString(trimmed) || quotedDriveLetterRe.MatchString(trimmed) {
		return true
	}
	if rest, rooted := strings.CutPrefix(trimmed, "/"); rooted {
		return strings.Contains(rest, "/") || strings.Contains(rest, ".")
	}
	return false
}
