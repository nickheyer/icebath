package approval

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
)

const (
	MemoryWrites = "memory"
	SkillWrites  = "skills"

	OriginForeground       = "foreground"
	OriginBackgroundReview = "background_review"
)

type PendingWrite struct {
	ID        string          `json:"id"`
	Subsystem string          `json:"subsystem"`
	Action    string          `json:"action"`
	Summary   string          `json:"summary"`
	Origin    string          `json:"origin"`
	CreatedAt float64         `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
}

type WriteDecision struct {
	Allow   bool
	Blocked bool
	Stage   bool
	Message string
}

func (g *Gate) WriteApprovalEnabled(ctx context.Context, subsystem string) bool {
	if subsystem != MemoryWrites && subsystem != SkillWrites {
		return false
	}
	cfg, err := g.o.Config.Load(ctx)
	if err != nil {
		return false
	}
	if subsystem == MemoryWrites {
		return cfg.Memory.WriteApproval
	}
	return cfg.Skills.WriteApproval
}

func (g *Gate) pendingDir(ctx context.Context, c *tool.Call, subsystem string) (string, error) {
	profileHome, err := g.profileHome(ctx, c)
	if err != nil {
		return "", err
	}
	return filepath.Join(home.PendingDir.In(profileHome), subsystem), nil
}

func slashReviewSurface(c *tool.Call) bool {
	if pyStrip(c.Origin.KanbanTask) != "" || c.Presence.Cron {
		return false
	}
	platform := c.Origin.Platform
	if platform == "" {
		platform = os.Getenv("ICEBATH_PLATFORM")
	}
	return !unattendedPlatforms[pyLower(pyStrip(platform))]
}

func (g *Gate) staged(ctx context.Context, c *tool.Call, subsystem string) (WriteDecision, error) {
	command := "/" + subsystem + " pending"
	where := "review with " + command
	if !slashReviewSurface(c) {
		dir, err := g.pendingDir(ctx, c, subsystem)
		if err != nil {
			return WriteDecision{}, err
		}
		where = "pending records live in " + dir + " (review from an interactive CLI session with " + command + ")"
	}
	return WriteDecision{Stage: true, Message: "Staged for approval (" + subsystem + ".write_approval is on). Not yet saved — " + where + "."}, nil
}

func (g *Gate) EvaluateWrite(ctx context.Context, c *tool.Call, subsystem, origin, inlineSummary, inlineDetail string) (WriteDecision, error) {
	if !g.WriteApprovalEnabled(ctx, subsystem) {
		return WriteDecision{Allow: true}, nil
	}
	if subsystem == SkillWrites || origin == OriginBackgroundReview {
		return g.staged(ctx, c, subsystem)
	}
	granted, answered := g.promptInlineMemory(ctx, c, inlineSummary, inlineDetail)
	switch {
	case !answered:
		return g.staged(ctx, c, MemoryWrites)
	case granted:
		return WriteDecision{Allow: true}, nil
	}
	return WriteDecision{Blocked: true, Message: "Memory write denied by user. The change was not saved."}, nil
}

func (g *Gate) promptInlineMemory(ctx context.Context, c *tool.Call, summary, detail string) (bool, bool) {
	if NoUserCanAnswer(c) || c.Surface == nil {
		return false, false
	}
	header := pyStrip(summary)
	if header == "" {
		header = "Save to memory?"
	}
	command := pyStrip(detail)
	if command == "" {
		command = header
	}
	choice, ok, err := c.Surface.PromptApproval(ctx, tool.ApprovalPrompt{ID: newRequestID(), SessionKey: c.SessionKey,
		Title: "Save to memory?", Command: command, Description: "Save to memory: " + header, AllowSession: true, AllowPermanent: false,
		Timeout: time.Duration(approvalTimeout(ctx, g.o.Config)) * time.Second, Surface: "cli"})
	if err != nil {
		slog.ErrorContext(ctx, "Inline memory approval prompt failed", "error", err)
		return false, false
	}
	if !ok {
		return false, false
	}
	switch choice.Answer {
	case "once", "session":
		return true, true
	case "deny":
		return false, true
	}
	return false, false
}

func (g *Gate) StageWrite(ctx context.Context, c *tool.Call, subsystem string, payload json.RawMessage, summary, origin string) (PendingWrite, error) {
	var head struct {
		Action any `json:"action"`
	}
	if err := json.Unmarshal(payload, &head); err != nil {
		return PendingWrite{}, fmt.Errorf("pending %s write payload: %w", subsystem, err)
	}
	action, _ := head.Action.(string)
	if origin == "" {
		origin = OriginForeground
	}
	id := newRequestID()[:8]
	record := PendingWrite{ID: id, Subsystem: subsystem, Action: action, Summary: pyStrip(summary), Origin: origin,
		CreatedAt: float64(time.Now().UnixNano()) / 1e9, Payload: payload}
	dir, err := g.pendingDir(ctx, c, subsystem)
	if err != nil {
		return record, err
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		slog.ErrorContext(ctx, "Failed to stage pending write", "subsystem", subsystem, "error", err)
		return record, nil
	}
	if err := fileutil.WriteJSON(ctx, filepath.Join(dir, id+".json"), record, fileutil.JSONStyle{Indent: 2}); err != nil {
		slog.ErrorContext(ctx, "Failed to stage pending write", "subsystem", subsystem, "error", err)
	}
	return record, nil
}

func decodePending(data []byte) (PendingWrite, bool) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return PendingWrite{}, false
	}
	var rec PendingWrite
	text := func(k string) string {
		var s string
		if v, ok := raw[k]; ok && json.Unmarshal(v, &s) == nil {
			return s
		}
		return ""
	}
	rec.ID, rec.Subsystem, rec.Action, rec.Summary, rec.Origin = text("id"), text("subsystem"), text("action"), text("summary"), text("origin")
	if v, ok := raw["created_at"]; ok {
		_ = json.Unmarshal(v, &rec.CreatedAt)
	}
	rec.Payload = raw["payload"]
	return rec, true
}

func (g *Gate) ListPending(ctx context.Context, c *tool.Call, subsystem string) ([]PendingWrite, error) {
	dir, err := g.pendingDir(ctx, c, subsystem)
	if err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var records []PendingWrite
	for _, p := range paths {
		data, err := os.ReadFile(p)
		rec, ok := decodePending(data)
		if err != nil || !ok {
			slog.WarnContext(ctx, "Skipping unreadable pending record", "path", p)
			continue
		}
		records = append(records, rec)
	}
	slices.SortStableFunc(records, func(a, b PendingWrite) int { return cmp.Compare(a.CreatedAt, b.CreatedAt) })
	return records, nil
}

func (g *Gate) GetPending(ctx context.Context, c *tool.Call, subsystem, id string) (PendingWrite, bool, error) {
	dir, err := g.pendingDir(ctx, c, subsystem)
	if err != nil {
		return PendingWrite{}, false, err
	}
	data, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return PendingWrite{}, false, nil
	}
	rec, ok := decodePending(data)
	return rec, ok, nil
}

func (g *Gate) DiscardPending(ctx context.Context, c *tool.Call, subsystem, id string) (bool, error) {
	dir, err := g.pendingDir(ctx, c, subsystem)
	if err != nil {
		return false, err
	}
	err = os.Remove(filepath.Join(dir, id+".json"))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	slog.ErrorContext(ctx, "Failed to discard pending write", "subsystem", subsystem, "id", id, "error", err)
	return false, nil
}

func (g *Gate) PendingWriteCount(ctx context.Context, c *tool.Call, subsystem string) int {
	dir, err := g.pendingDir(ctx, c, subsystem)
	if err != nil {
		return 0
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return 0
	}
	return len(paths)
}

var frontmatterDescriptionRe = regexp.MustCompile(`(?m)^description:[` + pySpaceClass + `]*(.+)$`)

func frontmatterDescription(content string) string {
	m := frontmatterDescriptionRe.FindStringSubmatch(content)
	if m == nil {
		return ""
	}
	return firstRunes(strings.Trim(pyStrip(m[1]), "'\""), 140)
}

func SkillGist(action, name, content, filePath, oldString, newString string) string {
	if (action == "create" || action == "edit") && content != "" {
		desc := frontmatterDescription(content)
		size := strconv.Itoa(runeCount(content)) + " chars"
		if n := runeCount(content); n >= 1024 {
			size = strconv.Itoa(n/1024+1) + " KB"
		}
		verb := "rewrite"
		if action == "create" {
			verb = "create"
		}
		descPart := ""
		if desc != "" {
			descPart = " — " + desc
		}
		return verb + " '" + name + "'" + descPart + " (" + size + ")"
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
		return "patch '" + name + "' " + target + " (+" + strconv.Itoa(added) + "/-" + strconv.Itoa(removed) + " lines)"
	}
	switch action {
	case "write_file":
		return "write " + filePath + " in '" + name + "'"
	case "remove_file":
		return "remove " + filePath + " from '" + name + "'"
	case "delete":
		return "delete skill '" + name + "'"
	}
	return action + " '" + name + "'"
}

type skillOp map[string]any

func (o skillOp) str(k string) string {
	if v, ok := o[k].(string); ok {
		return v
	}
	return ""
}

func (o skillOp) truthy(k string) bool {
	switch v := o[k].(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

type stagedFiles map[string]map[string]string

func (g *Gate) stagedBase(ctx context.Context, name, label string, staged stagedFiles) (string, error) {
	if files, ok := staged[name]; ok {
		if content, ok := files[label]; ok {
			return content, nil
		}
	}
	dir, found, err := g.o.SkillDir(ctx, name)
	if err != nil {
		return "", err
	}
	if !found || dir == "" {
		return "", nil
	}
	data, err := os.ReadFile(filepath.Join(dir, label))
	if err != nil || !utf8.Valid(data) {
		return "", nil
	}
	return strings.TrimPrefix(string(data), "\xef\xbb\xbf"), nil
}

func (g *Gate) foldPatch(base string, op skillOp) (string, string) {
	folded, err := g.o.FoldPatch(base, op.str("old_string"), op.str("new_string"), op.truthy("replace_all"))
	if err != nil {
		return base, err.Error()
	}
	return folded, ""
}

func (g *Gate) SkillPendingDiff(ctx context.Context, record PendingWrite) (string, error) {
	var payload skillOp
	if len(record.Payload) > 0 {
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			return "", fmt.Errorf("pending skill write %s payload: %w", record.ID, err)
		}
	}
	return g.skillDiff(ctx, payload, nil)
}

func (g *Gate) skillDiff(ctx context.Context, payload skillOp, staged stagedFiles) (string, error) {
	action, name := payload.str("action"), payload.str("name")
	switch action {
	case "batch":
		return g.batchDiff(ctx, payload)
	case "create":
		return payload.str("content"), nil
	case "edit", "patch", "write_file":
	case "remove_file":
		return fmt.Sprintf("remove file: %s from skill '%s'", pyStr(payload["file_path"]), name), nil
	case "delete":
		return "delete skill '" + name + "'", nil
	default:
		return "(" + action + " on '" + name + "')", nil
	}
	label := "SKILL.md"
	if action != "edit" && payload.str("file_path") != "" {
		label = payload.str("file_path")
	}
	current, err := g.stagedBase(ctx, name, label, staged)
	if err != nil {
		return "", err
	}
	var updated string
	switch action {
	case "patch":
		oldS, newS := payload.str("old_string"), payload.str("new_string")
		if current == "" {
			updated = "(patch " + pyStrRepr(oldS) + " → " + pyStrRepr(newS) + ")"
		} else {
			folded, perr := g.foldPatch(current, payload)
			if perr != "" {
				return "(patch would fail: " + perr + ")", nil
			}
			updated = folded
		}
	case "edit":
		updated = payload.str("content")
	default:
		updated = payload.str("file_content")
	}
	diff := unifiedDiff(splitLinesKeep(current), splitLinesKeep(updated), "a/"+label, "b/"+label)
	if diff == "" {
		return "(no textual change)", nil
	}
	return diff, nil
}

func (g *Gate) foldStaged(ctx context.Context, staged stagedFiles, action, name string, op skillOp) error {
	if name == "" {
		return nil
	}
	if action == "delete" {
		delete(staged, name)
		return nil
	}
	label := "SKILL.md"
	if action != "edit" && op.str("file_path") != "" {
		label = op.str("file_path")
	}
	files := staged[name]
	if files == nil {
		files = map[string]string{}
		staged[name] = files
	}
	switch action {
	case "create", "edit":
		files["SKILL.md"] = op.str("content")
	case "write_file":
		files[label] = op.str("file_content")
	case "patch":
		base, err := g.stagedBase(ctx, name, label, staged)
		if err != nil {
			return err
		}
		if folded, perr := g.foldPatch(base, op); perr == "" {
			files[label] = folded
		}
	case "remove_file":
		delete(files, label)
	}
	return nil
}

func (g *Gate) batchDiff(ctx context.Context, payload skillOp) (string, error) {
	var ops []skillOp
	if list, ok := payload["operations"].([]any); ok {
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				ops = append(ops, skillOp(m))
			}
		}
	}
	staged := stagedFiles{}
	var parts []string
	for i, op := range ops {
		action, name := op.str("action"), op.str("name")
		gist := SkillGist(action, name, op.str("content"), op.str("file_path"), op.str("old_string"), op.str("new_string"))
		merged := skillOp{}
		for k, v := range op {
			merged[k] = v
		}
		merged["name"] = name
		diff, err := g.skillDiff(ctx, merged, staged)
		if err != nil {
			return "", err
		}
		parts = append(parts, "## op "+strconv.Itoa(i+1)+"/"+strconv.Itoa(len(ops))+": "+gist+"\n\n"+diff)
		if err := g.foldStaged(ctx, staged, action, name, op); err != nil {
			return "", err
		}
	}
	if len(parts) == 0 {
		return "(empty batch)", nil
	}
	return strings.Join(parts, "\n\n"), nil
}

func pyStrRepr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, "\"") {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(quote):
			b.WriteByte('\\')
			b.WriteByte(quote)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r != ' ' && !unicode.IsPrint(r) && r <= 0xff:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r != ' ' && !unicode.IsPrint(r) && r <= 0xffff:
			fmt.Fprintf(&b, `\`+"u%04x", r)
		case r != ' ' && !unicode.IsPrint(r):
			fmt.Fprintf(&b, `\`+"U%08x", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	}
	data, _ := json.Marshal(v)
	return string(data)
}
