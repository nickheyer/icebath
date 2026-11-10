package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Write approval subsystems gating cross session writes
const (
	WriteApprovalMemory = "memory"
	WriteApprovalSkills = "skills"
)

var writeApprovalSubsystems = setOf(WriteApprovalMemory, WriteApprovalSkills)

// Per subsystem config key holding the single boolean gate
const writeApprovalConfigKey = "write_approval"

var writeApprovalTruthy = setOf("on", "true", "yes", "1", "approve", "enabled")

// Write origin of a regular foreground agent turn
const foregroundOrigin = "foreground"

// Installed skill directory lookup by name, registered by the skills package at init
var FindSkillDir func(env *Env, name string) string

// Subsystem write_approval flag from the live config, unset or invalid values leave the gate off
func WriteApprovalEnabled(env *Env, subsystem string) bool {
	if !writeApprovalSubsystems[subsystem] {
		return false
	}
	tree, err := config.Load(envHome(env), configLookup(env))
	if err != nil {
		return false
	}
	return normalizeWriteApprovalEnabled(tree.Get(subsystem + "." + writeApprovalConfigKey))
}

// Coerces a config value to the gate state, real booleans pass and only the truthy strings enable
func normalizeWriteApprovalEnabled(value any) bool {
	if b, ok := value.(bool); ok {
		return b
	}
	s, ok := value.(string)
	return ok && writeApprovalTruthy[strings.ToLower(pyStrip(s))]
}

// Directory holding one subsystem's staged write records
func PendingWriteDir(env *Env, subsystem string) string {
	return filepath.Join(home.PendingDir(envHome(env)), subsystem)
}

func pendingWritePath(env *Env, subsystem, pendingID string) string {
	return filepath.Join(PendingWriteDir(env, subsystem), pendingID+".json")
}

// Every json record entry in the subsystem pending dir, dotfiles included like pathlib glob
func pendingWriteFiles(env *Env, subsystem string) []string {
	dir := PendingWriteDir(env, subsystem)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if isWindowsHost {
			name = strings.ToLower(name)
		}
		if strings.HasSuffix(name, ".json") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// Ordered on disk shape of one staged write
type pendingRecordJSON struct {
	ID        string         `json:"id"`
	Subsystem string         `json:"subsystem"`
	Action    any            `json:"action"`
	Summary   string         `json:"summary"`
	Origin    string         `json:"origin"`
	CreatedAt float64        `json:"created_at"`
	Payload   map[string]any `json:"payload"`
}

// Writes indented JSON through a sibling temp file and an atomic replace, new files take the umask mode
func writeApprovalJSONAtomic(ctx context.Context, path string, data any) error {
	dir := filepath.Dir(path)
	if _, err := home.Mkdir(ctx, dir); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(data); err != nil {
		return err
	}
	body := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	var preserve os.FileMode
	if st, err := os.Stat(path); err == nil {
		preserve = st.Mode().Perm()
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	var tmp *os.File
	var tmpName string
	for {
		var suffix [6]byte
		rand.Read(suffix[:])
		tmpName = filepath.Join(dir, "."+stem+"_"+hex.EncodeToString(suffix[:])+".tmp")
		f, err := os.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		tmp = f
		break
	}
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if preserve != 0 && !isWindowsHost {
		if err := tmp.Chmod(preserve); err != nil {
			return fail(err)
		}
	}
	if _, err := tmp.Write(body); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// Persists a pending write and returns its record, a disk failure is logged and the write is lost
func StageWrite(ctx context.Context, env *Env, subsystem string, payload map[string]any, summary, origin string) map[string]any {
	var raw [16]byte
	rand.Read(raw[:])
	pid := hex.EncodeToString(raw[:])[:8]
	if origin == "" {
		origin = foregroundOrigin
	}
	action, ok := payload["action"]
	if !ok {
		action = ""
	}
	created := float64(time.Now().UnixNano()) / 1e9
	rec := pendingRecordJSON{ID: pid, Subsystem: subsystem, Action: action, Summary: pyStrip(summary), Origin: origin, CreatedAt: created, Payload: payload}
	if err := writeApprovalJSONAtomic(WithEnvHome(ctx, env), pendingWritePath(env, subsystem, pid), rec); err != nil {
		envLog(env).Error(fmt.Sprintf("Failed to stage pending %s write: %v", subsystem, err))
	}
	return map[string]any{"id": pid, "subsystem": subsystem, "action": action, "summary": rec.Summary, "origin": origin, "created_at": created, "payload": payload}
}

// Decodes one pending record file, BOM tolerant, nil unless it holds a JSON object
func readPendingRecord(p string) (map[string]any, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	record, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected a JSON object, got %s", pyJSONTypeNameOf(value))
	}
	return record, nil
}

// Python type name of a decoded JSON value, integral numbers reading as int like json.loads
func pyJSONTypeNameOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case float64:
		if x == float64(int64(x)) {
			return "int"
		}
		return "float"
	case int, int64:
		return "int"
	case string:
		return "str"
	case []any:
		return "list"
	}
	return "dict"
}

// Every readable pending record of the subsystem, oldest first
func ListPending(env *Env, subsystem string) []map[string]any {
	records := []map[string]any{}
	for _, p := range pendingWriteFiles(env, subsystem) {
		record, err := readPendingRecord(p)
		if err != nil {
			envLog(env).Warn("Skipping unreadable pending record: " + p)
			continue
		}
		records = append(records, record)
	}
	sort.SliceStable(records, func(i, j int) bool {
		return recordCreatedAt(records[i]) < recordCreatedAt(records[j])
	})
	return records
}

func recordCreatedAt(r map[string]any) float64 {
	if f, ok := config.AsFloat(r["created_at"]); ok {
		return f
	}
	return 0
}

// One pending record by id, nil when absent or unreadable
func GetPending(env *Env, subsystem, pendingID string) map[string]any {
	p := pendingWritePath(env, subsystem, pendingID)
	if _, err := os.Stat(p); err != nil {
		return nil
	}
	record, err := readPendingRecord(p)
	if err != nil {
		return nil
	}
	return record
}

// Deletes a pending record and reports whether it existed
func DiscardPending(env *Env, subsystem, pendingID string) bool {
	p := pendingWritePath(env, subsystem, pendingID)
	if _, err := os.Stat(p); err != nil {
		return false
	}
	if err := os.Remove(p); err != nil {
		envLog(env).Error(fmt.Sprintf("Failed to discard pending %s/%s: %v", subsystem, pendingID, err))
		return false
	}
	return true
}

// Count of pending record files for notification badges
func PendingCount(env *Env, subsystem string) int {
	return len(pendingWriteFiles(env, subsystem))
}

// Foreground or background_review as tagged on the session by the review fork
func CurrentOrigin(env *Env) string {
	if env != nil && env.Values != nil {
		if v, ok := env.Values.Load(writeOriginValuesKey); ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return foregroundOrigin
}

// Whether to write now, refuse after an inline denial, or stage for review
type GateDecision struct {
	Allow   bool
	Blocked bool
	Stage   bool
	Message string
}

// Reports whether a human on this turn can type the pending review slash command
func slashReviewSurface(env *Env) bool {
	if pyStrip(sessionValue(env, "kanban_task", "ICEBATH_KANBAN_TASK")) != "" {
		return false
	}
	if isCronApprovalContext(env) {
		return false
	}
	platform := getSessionPlatform(env)
	if platform == "" {
		platform = os.Getenv("ICEBATH_PLATFORM")
	}
	return !unattendedApprovalPlatforms[strings.ToLower(pyStrip(platform))]
}

// Stage decision whose message names the review command or the pending dir on headless surfaces
func stagedDecision(env *Env, subsystem string) GateDecision {
	command := "/" + subsystem + " pending"
	where := "review with " + command
	if !slashReviewSurface(env) {
		where = fmt.Sprintf("pending records live in %s (review from an interactive CLI session with %s)", PendingWriteDir(env, subsystem), command)
	}
	return GateDecision{Stage: true, Message: fmt.Sprintf("Staged for approval (%s.write_approval is on). Not yet saved — %s.", subsystem, where)}
}

// Allows with the gate off, stages skills and background writes, and asks inline for foreground memory
func CheckWriteApproval(ctx context.Context, env *Env, subsystem, inlineSummary, inlineDetail string) GateDecision {
	if !WriteApprovalEnabled(env, subsystem) {
		return GateDecision{Allow: true}
	}
	if subsystem == WriteApprovalSkills || CurrentOrigin(env) == backgroundReviewOrigin {
		return stagedDecision(env, subsystem)
	}
	granted, answered := promptInlineMemoryApproval(ctx, env, inlineSummary, inlineDetail)
	if !answered {
		return stagedDecision(env, WriteApprovalMemory)
	}
	if granted {
		return GateDecision{Allow: true}
	}
	return GateDecision{Blocked: true, Message: "Memory write denied by user. The change was not saved."}
}

// Asks the session human to save a memory write, unanswered when nobody can answer so the caller stages
func promptInlineMemoryApproval(ctx context.Context, env *Env, summary, detail string) (bool, bool) {
	if NoUserCanAnswer(env) {
		return false, false
	}
	if env == nil || env.Ask == nil {
		return false, false
	}
	header := pyStrip(summary)
	if header == "" {
		header = "Save to memory?"
	}
	text := pyStrip(detail)
	if text == "" {
		text = header
	}
	askCtx, cancel := context.WithTimeout(ctx, time.Duration(getApprovalTimeout(env))*time.Second)
	defer cancel()
	result, err := env.Ask.Ask(askCtx, []Question{{Header: "Save to memory?", Text: text, Choices: []string{"once", "session", "deny"}}})
	if err != nil {
		envLog(env).Error(fmt.Sprintf("Inline memory approval prompt failed: %v", err))
		return false, false
	}
	if result.Outcome == "cancelled" || len(result.Answers) == 0 {
		return false, false
	}
	choice := result.Answers[0].Text
	if len(result.Answers[0].Selected) > 0 {
		choice = result.Answers[0].Selected[0]
	}
	switch strings.ToLower(pyStrip(choice)) {
	case "once", "session":
		return true, true
	case "deny":
		return false, true
	}
	return false, false
}

var frontmatterDescriptionRe = regexp.MustCompile(`(?m)^description:\s*(.+)$`)

// One line heuristic description of a pending skill write without a model call
func SkillGist(action, name, content, filePath, oldString, newString string) string {
	if (action == "create" || action == "edit") && content != "" {
		desc := frontmatterDescription(content)
		n := utf8.RuneCountInString(content)
		size := fmt.Sprintf("%d chars", n)
		if n >= 1024 {
			size = fmt.Sprintf("%d KB", n/1024+1)
		}
		verb := "rewrite"
		if action == "create" {
			verb = "create"
		}
		tail := ""
		if desc != "" {
			tail = " — " + desc
		}
		return fmt.Sprintf("%s '%s'%s (%s)", verb, name, tail, size)
	}
	if action == "patch" {
		removed, added := 0, 0
		if oldString != "" {
			removed = strings.Count(oldString, "\n") + 1
		}
		if newString != "" {
			added = strings.Count(newString, "\n") + 1
		}
		target := filePath
		if target == "" {
			target = "SKILL.md"
		}
		return fmt.Sprintf("patch '%s' %s (+%d/-%d lines)", name, target, added, removed)
	}
	switch action {
	case "write_file":
		return fmt.Sprintf("write %s in '%s'", filePath, name)
	case "remove_file":
		return fmt.Sprintf("remove %s from '%s'", filePath, name)
	case "delete":
		return fmt.Sprintf("delete skill '%s'", name)
	}
	return fmt.Sprintf("%s '%s'", action, name)
}

// Frontmatter description value of a SKILL.md, unquoted and clipped to 140 characters
func frontmatterDescription(content string) string {
	m := frontmatterDescriptionRe.FindStringSubmatch(content)
	if m == nil {
		return ""
	}
	desc := strings.Trim(pyStrip(m[1]), `'"`)
	if pystr.Len(desc) > 140 {
		desc = runePrefix(desc, 140)
	}
	return desc
}

// Installed skill directory through the skills package lookup, empty when unknown
func findSkillPath(env *Env, name string) string {
	if FindSkillDir == nil {
		panic("tools: FindSkillDir is nil, pkg/skills must install the skill directory lookup")
	}
	return FindSkillDir(env, name)
}

// Content an op diffs against, the earlier batch ops' result for that file else the on disk copy
func stagedBase(env *Env, name, targetLabel string, staged map[string]map[string]string) string {
	if files, ok := staged[name]; ok && files != nil {
		if content, ok := files[targetLabel]; ok {
			return content
		}
	}
	skillDir := findSkillPath(env, name)
	if skillDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(skillDir, targetLabel))
	if err != nil {
		return ""
	}
	return strictUTF8Text(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
}

// Strict UTF-8 text of file bytes, empty when they do not decode
func strictUTF8Text(b []byte) string {
	if !utf8.Valid(b) {
		return ""
	}
	return string(b)
}

// Python dict get with a default for decoded payloads
func payloadGet(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Python value or empty string, the falsy case collapsing to empty
func payloadOrEmpty(m map[string]any, key string) string {
	v := m[key]
	if !truthy(v) {
		return ""
	}
	return pyStr(v)
}

// Full content or a unified diff against its base for a pending skill write
func SkillPendingDiff(env *Env, record map[string]any, staged map[string]map[string]string) string {
	payload, _ := payloadGet(record, "payload", map[string]any{}).(map[string]any)
	if payload == nil {
		payload = map[string]any{}
	}
	action := pyStr(payloadGet(payload, "action", ""))
	name := pyStr(payloadGet(payload, "name", ""))
	switch action {
	case "batch":
		return batchPendingDiff(env, payload)
	case "create":
		return payloadOrEmpty(payload, "content")
	case "edit", "patch", "write_file":
	case "remove_file":
		return fmt.Sprintf("remove file: %s from skill '%s'", pyStr(payload["file_path"]), name)
	case "delete":
		return fmt.Sprintf("delete skill '%s'", name)
	default:
		return fmt.Sprintf("(%s on '%s')", action, name)
	}
	targetLabel := "SKILL.md"
	if action != "edit" {
		if fp := payloadOrEmpty(payload, "file_path"); fp != "" {
			targetLabel = fp
		}
	}
	current := stagedBase(env, name, targetLabel, staged)
	var updated string
	if action == "patch" {
		oldS, newS := payloadOrEmpty(payload, "old_string"), payloadOrEmpty(payload, "new_string")
		if current == "" {
			updated = fmt.Sprintf("(patch %s → %s)", pyReprStr(oldS), pyReprStr(newS))
		} else {
			folded, patchErr := foldPatch(current, oldS, newS, truthy(payload["replace_all"]))
			if patchErr != "" {
				return "(patch would fail: " + patchErr + ")"
			}
			updated = folded
		}
	} else {
		key := "file_content"
		if action == "edit" {
			key = "content"
		}
		updated = payloadOrEmpty(payload, key)
	}
	diff := strings.Join(unifiedDiff(pySplitLinesKeepEnds(current), pySplitLinesKeepEnds(updated), "a/"+targetLabel, "b/"+targetLabel, 3), "")
	if diff == "" {
		return "(no textual change)"
	}
	return diff
}

// Folds one patch op through the same matcher approval runs, base and the error when it would fail
func foldPatch(base, oldString, newString string, replaceAll bool) (string, string) {
	folded, matchCount, _, errText := fuzzyFindAndReplace(base, oldString, newString, replaceAll, nil, nil)
	if errText != "" || matchCount == 0 {
		if errText == "" {
			errText = "Could not find a match for old_string in the file"
		}
		return base, errText
	}
	return folded, ""
}

// Records what one batch op leaves in the staged view the next op diffs against
func foldStaged(env *Env, staged map[string]map[string]string, action, name string, op map[string]any) {
	if name == "" {
		return
	}
	if action == "delete" {
		delete(staged, name)
		return
	}
	label := "SKILL.md"
	if action != "edit" {
		if fp := payloadOrEmpty(op, "file_path"); fp != "" {
			label = fp
		}
	}
	files, ok := staged[name]
	if !ok {
		files = map[string]string{}
		staged[name] = files
	}
	switch action {
	case "create", "edit":
		files["SKILL.md"] = payloadOrEmpty(op, "content")
	case "write_file":
		files[label] = payloadOrEmpty(op, "file_content")
	case "patch":
		base := stagedBase(env, name, label, staged)
		folded, patchErr := foldPatch(base, payloadOrEmpty(op, "old_string"), payloadOrEmpty(op, "new_string"), truthy(op["replace_all"]))
		if patchErr == "" {
			files[label] = folded
		}
	case "remove_file":
		delete(files, label)
	}
}

// Per op diffs of a staged batch, each op diffing against the content earlier ops left behind
func batchPendingDiff(env *Env, payload map[string]any) string {
	var ops []map[string]any
	if list, ok := payload["operations"].([]any); ok {
		for _, item := range list {
			if op, ok := item.(map[string]any); ok {
				ops = append(ops, op)
			}
		}
	}
	staged := map[string]map[string]string{}
	var parts []string
	for i, op := range ops {
		opAction := pyStr(payloadGet(op, "action", ""))
		opName := payloadOrEmpty(op, "name")
		gist := SkillGist(opAction, opName, payloadOrEmpty(op, "content"), payloadOrEmpty(op, "file_path"), payloadOrEmpty(op, "old_string"), payloadOrEmpty(op, "new_string"))
		merged := make(map[string]any, len(op)+1)
		for k, v := range op {
			merged[k] = v
		}
		merged["name"] = opName
		diff := SkillPendingDiff(env, map[string]any{"payload": merged}, staged)
		parts = append(parts, fmt.Sprintf("## op %d/%d: %s\n\n%s", i+1, len(ops), gist, diff))
		foldStaged(env, staged, opAction, opName, op)
	}
	if len(parts) == 0 {
		return "(empty batch)"
	}
	return strings.Join(parts, "\n\n")
}
