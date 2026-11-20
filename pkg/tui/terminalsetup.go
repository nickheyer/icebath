package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Sequences the IDE keybindings send, the modified keys of the kitty keyboard protocol the composer acts on
const (
	ideCopySequence       = "\x1b[99;13u"
	ideShiftEnterSequence = "\x1b[13;2u"
	ideCtrlEnterSequence  = "\x1b[13;5u"
	ideSuperEnterSequence = "\x1b[13;9u"
	ideUndoSequence       = "\x1b[122;9u"
	ideRedoSequence       = "\x1b[122;10u"
	// Multiline sequence earlier setups wrote, a backslash and a line break the composer saw as two keys
	ideLegacyMultilineSequence = "\\\r\n"
	ideSendSequenceCommand     = "workbench.action.terminal.sendSequence"
)

// IDE whose integrated terminal the setup configures, by the name of its settings directory and the name shown
type ideTerminal struct{ appName, label string }

var ideTerminals = map[string]ideTerminal{"vscode": {"Code", "VS Code"}, "cursor": {"Cursor", "Cursor"}, "windsurf": {"Windsurf", "Windsurf"}}

// One entry of an IDE keybindings file as far as the setup reads it, a field that is absent or not text left unset
type ideBinding struct {
	key, command, when, text *string
}

// Outcome of a setup, the line to show and whether the IDE terminal has to be restarted
type terminalSetupResult struct {
	message         string
	success         bool
	requiresRestart bool
}

// Member of a JSON object in file order
type jsonMember struct {
	name  string
	value json.RawMessage
}

var ideWhenToken = regexp.MustCompile(`!?[A-Za-z_][\w.]*`)

// Bindings the setup adds for a platform, macOS also sending the copy key while text is selected
func ideTargetBindings(goos string) []ideBinding {
	send := func(key, when, text string) ideBinding {
		command := ideSendSequenceCommand
		return ideBinding{key: &key, command: &command, when: &when, text: &text}
	}
	base := []ideBinding{
		send("shift+enter", "terminalFocus", ideShiftEnterSequence),
		send("ctrl+enter", "terminalFocus", ideCtrlEnterSequence),
		send("cmd+enter", "terminalFocus", ideSuperEnterSequence),
		send("cmd+z", "terminalFocus", ideUndoSequence),
		send("shift+cmd+z", "terminalFocus", ideRedoSequence),
	}
	if goos == "darwin" {
		return append([]ideBinding{send("cmd+c", "terminalFocus && terminalTextSelected", ideCopySequence)}, base...)
	}
	return base
}

// IDE whose integrated terminal the process runs in, empty outside one
func detectIDETerminal(env func(string) string) string {
	askpass := strings.ToLower(env("VSCODE_GIT_ASKPASS_MAIN"))
	switch {
	case env("CURSOR_TRACE_ID") != "" || strings.Contains(askpass, "cursor"):
		return "cursor"
	case strings.Contains(askpass, "windsurf"):
		return "windsurf"
	case env("TERM_PROGRAM") == "vscode" || env("VSCODE_GIT_IPC_HANDLE") != "":
		return "vscode"
	}
	return ""
}

// End of the JSON string that opens at a position, the end of the content for one left open
func jsonStringEnd(content string, open int) int {
	j := open + 1
	for j < len(content) {
		if content[j] == '\\' {
			j += 2
			continue
		}
		j++
		if content[j-1] == '"' {
			break
		}
	}
	return min(j, len(content))
}

// JSON with the line comments, block comments and trailing commas of an IDE settings file removed, text inside strings untouched
func stripJSONComments(content string) string {
	var out strings.Builder
	for i := 0; i < len(content); {
		switch {
		case content[i] == '"':
			j := jsonStringEnd(content, i)
			out.WriteString(content[i:j])
			i = j
		case strings.HasPrefix(content[i:], "//"):
			if eol := strings.IndexByte(content[i:], '\n'); eol >= 0 {
				i += eol
			} else {
				i = len(content)
			}
		case strings.HasPrefix(content[i:], "/*"):
			if end := strings.Index(content[i+2:], "*/"); end >= 0 {
				i += end + 4
			} else {
				i = len(content)
			}
		default:
			out.WriteByte(content[i])
			i++
		}
	}
	bare := out.String()
	out.Reset()
	for i := 0; i < len(bare); {
		switch {
		case bare[i] == '"':
			j := jsonStringEnd(bare, i)
			out.WriteString(bare[i:j])
			i = j
			continue
		case bare[i] == ',':
			if rest := strings.TrimLeftFunc(bare[i+1:], isJSSpace); strings.HasPrefix(rest, "}") || strings.HasPrefix(rest, "]") {
				i++
				continue
			}
		}
		out.WriteByte(bare[i])
		i++
	}
	return out.String()
}

// Environment lookup that reports an empty variable as unset
func setLookup(env func(string) string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value := env(name)
		return value, value != ""
	}
}

// User settings directory of an IDE on a platform, empty when the platform gives no place for it
func ideConfigDir(appName, goos string, env func(string) string, homeDir string) string {
	switch goos {
	case "darwin":
		return filepath.Join(homeDir, "Library", "Application Support", appName, "User")
	case "windows":
		if appData := env("APPDATA"); appData != "" {
			return filepath.Join(appData, appName, "User")
		}
		return ""
	}
	if config := env("XDG_CONFIG_HOME"); filepath.IsAbs(config) {
		return filepath.Join(config, appName, "User")
	}
	return filepath.Join(homeDir, ".config", appName, "User")
}

// Members of a JSON object in file order, false for anything that is not an object
func jsonMembers(raw json.RawMessage) ([]jsonMember, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var members []jsonMember
	for dec.More() {
		tok, err := dec.Token()
		name, isName := tok.(string)
		if err != nil || !isName {
			return nil, false
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false
		}
		members = append(members, jsonMember{name: name, value: value})
	}
	return members, true
}

// Text of a JSON value, unset for anything that is not a string
func jsonText(raw json.RawMessage) *string {
	var text string
	if json.Unmarshal(raw, &text) != nil || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte(`"`)) {
		return nil
	}
	return &text
}

// Reads one entry of a keybindings file, false for an entry that is not an object
func readIDEBinding(raw json.RawMessage) (ideBinding, bool) {
	members, isObject := jsonMembers(raw)
	if !isObject {
		return ideBinding{}, false
	}
	var b ideBinding
	for _, m := range members {
		switch m.name {
		case "key":
			b.key = jsonText(m.value)
		case "command":
			b.command = jsonText(m.value)
		case "when":
			b.when = jsonText(m.value)
		case "args":
			b.text = nil
			if args, ok := jsonMembers(m.value); ok {
				for _, arg := range args {
					if arg.name == "text" {
						b.text = jsonText(arg.value)
					}
				}
			}
		}
	}
	return b, true
}

func sameText(a, b *string) bool { return a != nil && b != nil && *a == *b }

// Whether an entry of the file is the binding the setup wants
func sameIDEBinding(existing, target ideBinding) bool {
	return sameText(existing.key, target.key) && sameText(existing.command, target.command) && sameText(existing.when, target.when) && sameText(existing.text, target.text)
}

// Whether two when clauses can hold together: an empty clause holds everywhere, a context one requires and the other negates keeps them apart, and
// one shared required context is taken as overlap without parsing the whole clause language
func ideWhensOverlap(a, b string) bool {
	if a == b || a == "" || b == "" {
		return true
	}
	parse := func(when string) (required, forbidden map[string]bool) {
		required, forbidden = map[string]bool{}, map[string]bool{}
		for _, token := range ideWhenToken.FindAllString(when, -1) {
			if name, negated := strings.CutPrefix(token, "!"); negated {
				forbidden[name] = true
			} else {
				required[token] = true
			}
		}
		return required, forbidden
	}
	leftRequired, leftForbidden := parse(a)
	rightRequired, rightForbidden := parse(b)
	for token := range leftRequired {
		if rightForbidden[token] {
			return false
		}
	}
	for token := range rightRequired {
		if leftForbidden[token] {
			return false
		}
	}
	for token := range leftRequired {
		if rightRequired[token] {
			return true
		}
	}
	return false
}

// Whether an entry of the file would shadow a binding the setup wants: the same key in an overlapping context doing something else
func ideBindingsConflict(existing, target ideBinding) bool {
	if !sameText(existing.key, target.key) {
		return false
	}
	when := ""
	if existing.when != nil {
		when = *existing.when
	}
	return ideWhensOverlap(when, *target.when) && !sameIDEBinding(existing, target)
}

// JSON of a value as an IDE writes it, the characters of a when clause left as they are
func ideJSON(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// Entry the setup writes for a binding
func ideBindingJSON(b ideBinding) json.RawMessage {
	var out bytes.Buffer
	out.WriteString(`{"key":`)
	out.Write(ideJSON(*b.key))
	out.WriteString(`,"command":`)
	out.Write(ideJSON(*b.command))
	out.WriteString(`,"when":`)
	out.Write(ideJSON(*b.when))
	out.WriteString(`,"args":{"text":`)
	out.Write(ideJSON(*b.text))
	out.WriteString(`}}`)
	return out.Bytes()
}

// Rewrites the entries an earlier setup wrote with the legacy multiline sequence to the sequences sent now, every other member of an entry kept in place
func migrateLegacyIDEBindings(entries []json.RawMessage) int {
	replacements := map[string]string{"shift+enter": ideShiftEnterSequence, "ctrl+enter": ideCtrlEnterSequence, "cmd+enter": ideSuperEnterSequence}
	command, when, legacy := ideSendSequenceCommand, "terminalFocus", ideLegacyMultilineSequence
	migrated := 0
	for i, raw := range entries {
		b, ok := readIDEBinding(raw)
		if !ok || b.key == nil || !sameText(b.command, &command) || !sameText(b.when, &when) || !sameText(b.text, &legacy) {
			continue
		}
		replacement, known := replacements[*b.key]
		if !known {
			continue
		}
		members, _ := jsonMembers(raw)
		var out bytes.Buffer
		out.WriteByte('{')
		for n, m := range members {
			if n > 0 {
				out.WriteByte(',')
			}
			out.Write(ideJSON(m.name))
			out.WriteByte(':')
			if m.name == "args" {
				out.WriteString(`{"text":`)
				out.Write(ideJSON(replacement))
				out.WriteByte('}')
				continue
			}
			out.Write(m.value)
		}
		out.WriteByte('}')
		entries[i] = out.Bytes()
		migrated++
	}
	return migrated
}

// Entries of a keybindings file, a file that does not exist or holds nothing but comments read as none
func readIDEKeybindings(path string) (entries []json.RawMessage, exists bool, isArray bool, err error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, true, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	stripped := strings.TrimFunc(stripJSONComments(string(content)), isJSSpace)
	if stripped == "" {
		return nil, true, true, nil
	}
	if err := json.Unmarshal([]byte(stripped), &entries); err != nil {
		var mismatch *json.UnmarshalTypeError
		if errors.As(err, &mismatch) {
			return nil, true, false, nil
		}
		return nil, true, false, err
	}
	return entries, true, strings.HasPrefix(stripped, "["), nil
}

// Adds the multiline, undo, redo and copy bindings to the keybindings file of an IDE on this machine, the file backed up before it changes
func configureTerminalKeybindings(terminal, goos string, env func(string) string, homeDir string, now time.Time) terminalSetupResult {
	meta := ideTerminals[terminal]
	if isRemoteShellSession(setLookup(env)) {
		return terminalSetupResult{message: tr(txt.LibTextTerminalSetupMustRunLocally, meta.label)}
	}
	configDir := ideConfigDir(meta.appName, goos, env, homeDir)
	if configDir == "" {
		return terminalSetupResult{message: tr(txt.LibTextTerminalSetupSettingsPathUnknown, meta.label)}
	}
	failed := func(err error) terminalSetupResult {
		return terminalSetupResult{message: tr(txt.LibTextTerminalSetupConfigureFailed, meta.label, err.Error())}
	}
	file := filepath.Join(configDir, "keybindings.json")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return failed(err)
	}
	entries, exists, isArray, err := readIDEKeybindings(file)
	switch {
	case err != nil:
		return terminalSetupResult{message: tr(txt.LibTextTerminalSetupReadFailed, meta.label, err.Error())}
	case !isArray:
		return terminalSetupResult{message: tr(txt.LibTextTerminalSetupKeybindingsNotArray, meta.label, file)}
	}
	migrated := migrateLegacyIDEBindings(entries)
	existing := make([]ideBinding, 0, len(entries))
	for _, raw := range entries {
		if b, ok := readIDEBinding(raw); ok {
			existing = append(existing, b)
		}
	}
	targets := ideTargetBindings(goos)
	var conflicts []string
	var missing []json.RawMessage
	for _, target := range targets {
		present := false
		for _, b := range existing {
			if ideBindingsConflict(b, target) {
				conflicts = append(conflicts, *target.key)
				break
			}
			present = present || sameIDEBinding(b, target)
		}
		if !present {
			missing = append(missing, ideBindingJSON(target))
		}
	}
	if len(conflicts) > 0 {
		return terminalSetupResult{message: tr(txt.LibTextTerminalSetupConflicts, file, strings.Join(conflicts, ", "))}
	}
	added := len(missing)
	if added == 0 && migrated == 0 {
		return terminalSetupResult{success: true, message: tr(txt.LibTextTerminalSetupAlreadyConfigured, meta.label)}
	}
	if exists {
		stamp := strings.NewReplacer(":", "-", ".", "-").Replace(now.UTC().Format("2006-01-02T15:04:05.000Z"))
		if err := copyFileBeside(file, file+".backup."+stamp); err != nil {
			return failed(err)
		}
	}
	var compact bytes.Buffer
	compact.WriteByte('[')
	for i, raw := range append(missing, entries...) {
		if i > 0 {
			compact.WriteByte(',')
		}
		compact.Write(raw)
	}
	compact.WriteByte(']')
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, compact.Bytes(), "", "  "); err != nil {
		return failed(err)
	}
	pretty.WriteByte('\n')
	if err := os.WriteFile(file, pretty.Bytes(), 0o644); err != nil {
		return failed(err)
	}
	var parts []string
	if added > 0 {
		key := txt.LibTextTerminalSetupAddedOther
		if added == 1 {
			key = txt.LibTextTerminalSetupAddedOne
		}
		parts = append(parts, tr(key, strconv.Itoa(added), meta.label))
	}
	if migrated > 0 {
		key := txt.LibTextTerminalSetupMigratedOther
		if migrated == 1 {
			key = txt.LibTextTerminalSetupMigratedOne
		}
		parts = append(parts, tr(key, strconv.Itoa(migrated)))
	}
	return terminalSetupResult{success: true, requiresRestart: true, message: tr(txt.LibTextTerminalSetupSummaryIn, strings.Join(parts, ", "), file)}
}

// Copies a file under another name with the same permissions
func copyFileBeside(from, to string) error {
	content, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	info, err := os.Stat(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, content, info.Mode().Perm())
}

// Whether the IDE terminal the process runs in still lacks a binding the setup adds, false outside an IDE terminal and over SSH
func shouldPromptForTerminalSetup(goos string, env func(string) string, homeDir string) bool {
	detected := detectIDETerminal(env)
	if detected == "" || isRemoteShellSession(setLookup(env)) {
		return false
	}
	configDir := ideConfigDir(ideTerminals[detected].appName, goos, env, homeDir)
	if configDir == "" {
		return false
	}
	entries, exists, isArray, err := readIDEKeybindings(filepath.Join(configDir, "keybindings.json"))
	if err != nil || !exists || !isArray {
		return true
	}
	for _, target := range ideTargetBindings(goos) {
		present := false
		for _, raw := range entries {
			if b, ok := readIDEBinding(raw); ok && sameIDEBinding(b, target) {
				present = true
				break
			}
		}
		if !present {
			return true
		}
	}
	return false
}

// Hint about the terminal the UI runs in, shown once in the activity feed
type terminalHint struct{ key, message, tone string }

// Hints about the host terminal: an IDE terminal still to be set up, Apple Terminal, tmux and an SSH session, each with what it changes for keys and the clipboard
func terminalParityHints(goos string, env func(string) string, homeDir string) []terminalHint {
	var hints []terminalHint
	if ide := detectIDETerminal(env); ide != "" && shouldPromptForTerminalSetup(goos, env, homeDir) {
		hints = append(hints, terminalHint{"ide-setup", tr(txt.LibTextTerminalParityIdeSetup, ide), "info"})
	}
	if env("TERM_PROGRAM") == "Apple_Terminal" || env("TERM_SESSION_ID") != "" {
		hints = append(hints, terminalHint{"apple-terminal", tr(txt.LibTextTerminalParityAppleTerminal), "warn"})
	}
	if env("TMUX") != "" {
		hints = append(hints, terminalHint{"tmux", tr(txt.LibTextTerminalParityTmux), "warn"})
	}
	if isRemoteShellSession(setLookup(env)) {
		hints = append(hints, terminalHint{"remote", tr(txt.LibTextTerminalParityRemote), "warn"})
	}
	return hints
}

// Hints about the host terminal read off the update loop
type terminalHintsMsg struct{ hints []terminalHint }

// Reads the hints about the host terminal once the UI starts
func terminalHintsCmd() tea.Msg {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return terminalHintsMsg{hints: terminalParityHints(runtime.GOOS, os.Getenv, home)}
}

// Runs /terminal-setup for the IDE named, or the one the terminal belongs to, and prints the outcome
func (r *slashRun) terminalSetup() tea.Cmd {
	target := strings.ToLower(trimJS(r.arg))
	if _, known := ideTerminals[target]; target != "" && target != "auto" && !known {
		r.sys(tr(txt.SlashCmdCoreTerminalSetupUsage))
		return nil
	}
	return func() tea.Msg {
		result := func() terminalSetupResult {
			terminal := target
			if terminal == "" || terminal == "auto" {
				if terminal = detectIDETerminal(os.Getenv); terminal == "" {
					return terminalSetupResult{message: tr(txt.LibTextTerminalSetupNoSupportedIde)}
				}
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return terminalSetupResult{message: tr(txt.SlashCmdCoreTerminalSetupFailed, err.Error())}
			}
			return configureTerminalKeybindings(terminal, runtime.GOOS, os.Getenv, home, time.Now())
		}()
		return asyncDoneMsg{run: func() tea.Cmd {
			if r.stale() {
				return nil
			}
			r.sys(result.message)
			if result.success && result.requiresRestart {
				r.sys(tr(txt.SlashCmdCoreTerminalSetupRestartIde))
			}
			return nil
		}}
	}
}
