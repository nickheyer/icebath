package tui

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/txt"
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

// Gateway completion request the composer sends for its draft
type completionRequest struct {
	Slash       *tuigateway.CompleteSlashParams
	Path        *tuigateway.CompletePathParams
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
	tabPathRe           = regexp.MustCompile(`((?:["']?(?:[A-Za-z]:[\\/]|\.{1,2}/|~/|/|@|[^"'` + "`" + jsWS + `]+/))[^` + jsWS + `]*)$`)
	driveLetterRe       = regexp.MustCompile(`^[A-Za-z]:[/\\]`)
	quotedDriveLetterRe = regexp.MustCompile(`^["'][A-Za-z]:[/\\]`)
)

// Paste and image token of a draft, the double bracket label the composer inserts
var pasteSnippetRe = regexp.MustCompile(`\[\[[^\n]*?\]\]`)

// Label of the image token with that index
func imageToken(index int) string { return tr(txt.LibTextAttachmentsImageToken, index) }

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

// Completion request for a draft, slash names, inline skills or a path word, false when none applies. The popup opens by itself for commands, skills
// and @ references; a plain path at the end of the draft completes only on tab, so enter always sends a sentence that ends in one
func (s *tuiStore) completionRequestForInput(input string, tab bool) (completionRequest, bool) {
	isSlash := looksLikeSlashCommand(input)
	pathWord := ""
	if !isSlash {
		if m := tabPathRe.FindStringSubmatch(input); m != nil && (tab || strings.HasPrefix(m[1], "@")) {
			pathWord = m[1]
		}
	}
	if isSlash && strings.TrimFunc(input, isJSSpace) == "/model" {
		return completionRequest{}, false
	}
	if query, start, inline := inlineSlashTrigger(input); inline {
		return completionRequest{Slash: &tuigateway.CompleteSlashParams{Text: ptr("/" + query)}, ReplaceFrom: start + 1, SkillsOnly: true}, true
	}
	if isSlash {
		return completionRequest{Slash: &tuigateway.CompleteSlashParams{Text: &input}, ReplaceFrom: 1}, true
	}
	if pathWord == "" {
		return completionRequest{}, false
	}
	return completionRequest{Path: s.pathCompletionParams(pathWord), ReplaceFrom: unitLen(input) - unitLen(pathWord)}, true
}

// Path completion params of a word, rooted at the session in view and its folder when known
func (s *tuiStore) pathCompletionParams(word string) *tuigateway.CompletePathParams {
	params := &tuigateway.CompletePathParams{Word: &word}
	if cwd := s.sessionCwd(); cwd != "" {
		params.Cwd = &cwd
	}
	if s.ui.Sid != "" {
		params.SessionID = ptr(s.ui.Sid)
	}
	return params
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

// Collapsed paste a person expanded back into the draft: the runes from start up to end are the pasted text, editable like the rest, and whatever
// they hold folds back into a chip
type pasteSpan struct {
	start, end int
}

// Span kept on the same text across one edit of the draft from prev to next: typing inside it or at its very end grows it, typing before it moves
// it, and an edit that replaces all of it or leaves it empty ends it
func shiftSpan(prev, next []rune, span pasteSpan) *pasteSpan {
	if slices.Equal(prev, next) {
		return &span
	}
	limit := min(len(prev), len(next))
	head := 0
	for head < limit && prev[head] == next[head] {
		head++
	}
	tail := 0
	for tail < limit-head && prev[len(prev)-1-tail] == next[len(next)-1-tail] {
		tail++
	}
	oldEnd, newEnd := len(prev)-tail, len(next)-tail
	start, end := span.start, span.end
	switch {
	case oldEnd <= start:
		start, end = start+newEnd-oldEnd, end+newEnd-oldEnd
	case head > end:
	case head >= start && oldEnd <= end:
		end += newEnd - oldEnd
	case head <= start && oldEnd >= end:
		return nil
	case head < start:
		start, end = head, end+newEnd-oldEnd
	default:
		end = newEnd
	}
	if end <= start {
		return nil
	}
	return &pasteSpan{start: start, end: end}
}

// Chip of a token in a draft, the rune offset its label starts at and the place of the token in the list
type tokenChip struct {
	at, index int
	token     composerToken
}

// Runes the label of the chip takes in the draft
func (c tokenChip) width() int { return utf8.RuneCountInString(c.token.Label) }

// Chips of the tokens in a draft in text order, tokens of one label taking its occurrences in list order, the order the expander pairs them in
func tokenChips(value string, tokens []composerToken) []tokenChip {
	from := map[string]int{}
	var chips []tokenChip
	for i, token := range tokens {
		found := strings.Index(value[from[token.Label]:], token.Label)
		if token.Label == "" || found < 0 {
			continue
		}
		at := from[token.Label] + found
		from[token.Label] = at + len(token.Label)
		chips = append(chips, tokenChip{at: utf8.RuneCountInString(value[:at]), index: i, token: token})
	}
	slices.SortFunc(chips, func(a, b tokenChip) int { return a.at - b.at })
	return chips
}

// Chips of the collapsed pastes in a draft in text order
func pasteChips(value string, tokens []composerToken) []tokenChip {
	return slices.DeleteFunc(tokenChips(value, tokens), func(c tokenChip) bool { return c.token.Kind != "paste" })
}

// Whether a draft holds the chip of a collapsed paste
func hasPasteChip(value string, tokens []composerToken) bool {
	return len(pasteChips(value, tokens)) > 0
}

// Draft after a paste chip was expanded or an expanded paste folded back, with the cursor, the tokens left and, after an expansion, the span of the text
type pasteEdit struct {
	value  string
	cursor int
	span   pasteSpan
	tokens []composerToken
}

// Expands the paste chip under the cursor, or the last one of the draft, into its text, the token leaving the list since its text is the draft's own,
// false when the draft holds no paste chip
func expandPasteChip(value string, cursor int, tokens []composerToken) (pasteEdit, bool) {
	chips := pasteChips(value, tokens)
	if len(chips) == 0 {
		return pasteEdit{}, false
	}
	chip := chips[len(chips)-1]
	for _, c := range chips {
		if cursor >= c.at && cursor <= c.at+c.width() {
			chip = c
			break
		}
	}
	runes, text := []rune(value), []rune(chip.token.Text)
	end := chip.at + len(text)
	return pasteEdit{value: string(runes[:chip.at]) + chip.token.Text + string(runes[chip.at+chip.width():]), cursor: end,
		span: pasteSpan{start: chip.at, end: end}, tokens: slices.Delete(slices.Clone(tokens), chip.index, chip.index+1)}, true
}

// Whether the cursor sits inside the expanded paste, where typed and pasted text joins it
func insideSpan(span *pasteSpan, cursor int) bool {
	return span != nil && cursor > span.start && cursor <= span.end
}

// Folds the expanded span back into a chip whose text is what the span holds, the token taking its place among those of the same label by where
// its chip stands so every chip still pairs with its own text. A span holding another chip, an image attached inside it, is not folded: the
// outer text would carry that chip's label in place of its content
func collapsePasteSpan(value string, span pasteSpan, tokens []composerToken) (pasteEdit, bool) {
	runes := []rune(value)
	if span.start < 0 || span.end > len(runes) || span.end <= span.start {
		return pasteEdit{}, false
	}
	for _, c := range tokenChips(value, tokens) {
		if c.at < span.end && c.at+c.width() > span.start {
			return pasteEdit{}, false
		}
	}
	text := string(runes[span.start:span.end])
	label := pasteTokenLabel(text, strings.Count(text, "\n")+1)
	before, seen, at := strings.Count(string(runes[:span.start]), label), 0, len(tokens)
	for i, token := range tokens {
		if token.Label != label {
			continue
		}
		if seen == before {
			at = i
			break
		}
		seen++
	}
	return pasteEdit{value: string(runes[:span.start]) + label + string(runes[span.end:]), cursor: span.start + utf8.RuneCountInString(label),
		tokens: slices.Insert(slices.Clone(tokens), at, composerToken{Kind: "paste", Label: label, Text: text})}, true
}
