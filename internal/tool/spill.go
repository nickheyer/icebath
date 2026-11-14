package tool

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/shell"
)

const (
	persistedOpenTag     = "<persisted-output>"
	persistedCloseTag    = "</persisted-output>"
	sandboxResultsDir    = "icebath-results"
	budgetToolName       = "__budget_enforcement__"
	envelopeMetaOpenTag  = "<mcp-result-metadata>"
	envelopeMetaCloseTag = "</mcp-result-metadata>"
	maxResultFilenameLen = 120
	containerCacheBase   = "/root/.icebath"
	SpilloverSQLFilter   = "(instr(content, 'spillover') > 0 OR instr(api_content, 'spillover') > 0)"
)

var (
	unsafeResultFilename = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
	spilloverNameRe      = regexp.MustCompile(`spillover(?:/|\\+)([A-Za-z0-9_.-]+)`)
	archivePathRe        = regexp.MustCompile(`(?:^|[/\\])(?:spillover|icebath-results)[/\\][^/\\]+$`)
	persistedPathRe      = regexp.MustCompile(`(?m)^Full output saved to: (.+)$`)
	envelopeKeys         = map[string]bool{"result": true, "structuredContent": true, "_meta": true}
)

type SpillReferences interface {
	SpilloverNames(ctx context.Context, home string) (names map[string]bool, known bool)
}

type SpillOptions struct {
	Home       string
	Retention  time.Duration
	Paths      CachePaths
	References SpillReferences
}

type Spill struct {
	home      string
	dir       string
	retention time.Duration
	paths     CachePaths
	refs      SpillReferences
	prune     sync.Once
}

func NewSpill(o SpillOptions) (*Spill, error) {
	switch {
	case o.Home == "":
		return nil, &UnfilledError{Name: "tool.SpillOptions.Home"}
	case o.Paths == nil:
		return nil, &UnfilledError{Name: "tool.CachePaths"}
	case o.References == nil:
		return nil, &UnfilledError{Name: "tool.SpillReferences"}
	}
	retention := o.Retention
	if retention <= 0 {
		retention = spilloverMinRetention
	}
	return &Spill{home: o.Home, dir: home.Spillover.In(o.Home), retention: retention, paths: o.Paths, refs: o.References}, nil
}

func (s *Spill) Dir() string { return s.dir }

func ExpiredArchiveFields(p string) []Field {
	if !archivePathRe.MatchString(p) {
		return nil
	}
	return []Field{
		{Key: "expired", Value: true},
		{Key: "hint", Value: "This was a persisted tool result archive and it has expired (pruned with its session or by the cache sweep); the full output is no longer recoverable. Inspect the current state before re-running the original tool — the call that produced it may already have taken effect."},
	}
}

func SpilloverNames(texts ...string) map[string]bool {
	names := map[string]bool{}
	for _, text := range texts {
		if !strings.Contains(text, "spillover") {
			continue
		}
		for _, m := range spilloverNameRe.FindAllStringSubmatch(text, -1) {
			names[m[1]] = true
		}
	}
	return names
}

func (s *Spill) Prune(ctx context.Context) int {
	cutoff := time.Now().Add(-s.retention)
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0
	}
	var expired []string
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(s.dir, e.Name()))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.ModTime().Before(cutoff) {
			expired = append(expired, e.Name())
		}
	}
	if len(expired) == 0 {
		return 0
	}
	referenced, known := s.refs.SpilloverNames(ctx, s.home)
	removed := 0
	for _, name := range expired {
		if !known || referenced[name] {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, name)); err == nil {
			removed++
		}
	}
	return removed
}

func (s *Spill) pruneOnce(ctx context.Context) {
	s.prune.Do(func() {
		if removed := s.Prune(ctx); removed > 0 {
			slog.Debug(fmt.Sprintf("Pruned %d expired spillover file(s)", removed))
		}
	})
}

func (s *Spill) writeToSpillover(ctx context.Context, content, filename string) (string, bool) {
	data := []byte(strings.ToValidUTF8(content, "�"))
	p := filepath.Join(s.dir, filename)
	if err := os.MkdirAll(s.dir, 0o777); err != nil {
		slog.Warn(fmt.Sprintf("Spillover write failed for %s: %v", filename, err))
		return "", false
	}
	if err := os.WriteFile(p, data, 0o666); err != nil {
		slog.Warn(fmt.Sprintf("Spillover write failed for %s: %v", filename, err))
		return "", false
	}
	info, err := os.Stat(p)
	if err != nil {
		slog.Warn(fmt.Sprintf("Spillover write failed for %s: %v", filename, err))
		return "", false
	}
	if info.Size() != int64(len(data)) {
		slog.Warn(fmt.Sprintf("Spillover write for %s is not lossless (%d bytes on disk, expected %d) — discarding archive", filename, info.Size(), len(data)))
		_ = os.Remove(p)
		return "", false
	}
	s.pruneOnce(ctx)
	return p, true
}

func (s *Spill) sandboxVisible(ctx context.Context, hostPath string, env Environment) string {
	visible, err := s.paths.ToAgentVisibleCachePath(ctx, hostPath, containerCacheBase)
	if err != nil {
		slog.Debug(fmt.Sprintf("Spillover path translation failed: %v", err))
		return ""
	}
	if err := env.Sync(ctx, true); err != nil {
		slog.Debug(fmt.Sprintf("Spillover sync failed: %v", err))
	}
	res, err := env.Execute(ctx, "test -r "+shell.Quote(visible), ExecOptions{Timeout: 15 * time.Second, RewriteCompoundBackground: true})
	if err != nil {
		slog.Debug(fmt.Sprintf("Spillover readability probe failed: %v", err))
		return ""
	}
	if res.ExitCode == 0 {
		return visible
	}
	return ""
}

func storageDir(env Environment) string {
	if t := env.TempDir(); t != "" {
		dir := strings.TrimRight(t, "/")
		if dir == "" {
			dir = "/"
		}
		return dir + "/" + sandboxResultsDir
	}
	return filepath.Join(os.TempDir(), sandboxResultsDir)
}

func safeResultFilename(id string) string {
	raw := id
	if raw == "" {
		raw = "tool_result"
	}
	stem := strings.Trim(unsafeResultFilename.ReplaceAllString(raw, "_"), "._-")
	changed := stem != raw
	if stem == "" {
		stem = "tool_result"
	}
	if changed || len(stem) > maxResultFilenameLen {
		digest := sha256Hex(raw)[:12]
		if len(stem) > maxResultFilenameLen {
			stem = stem[:maxResultFilenameLen]
		}
		stem = strings.TrimRight(stem, "._-")
		if stem == "" {
			stem = "tool_result"
		}
		stem += "_" + digest
	}
	return stem + ".txt"
}

func generatePreview(content string, maxChars int) (string, bool) {
	runes := []rune(content)
	if len(runes) <= maxChars {
		return content, false
	}
	last := -1
	for i := maxChars - 1; i >= 0; i-- {
		if runes[i] == '\n' {
			last = i
			break
		}
	}
	if last > maxChars/2 {
		return string(runes[:last+1]), true
	}
	return string(runes[:maxChars]), true
}

func pageableText(content string) string {
	n, err := parseNode([]byte(content))
	if err != nil || !n.isObj() || len(n.keys) == 0 {
		return content
	}
	for _, k := range n.keys {
		if !envelopeKeys[k] {
			return content
		}
	}
	text, ok := n.get("result").text()
	if !ok || text == "" {
		return content
	}
	extras := newObj()
	for i, k := range n.keys {
		if k != "result" {
			extras.set(k, n.vals[i])
		}
	}
	if len(extras.keys) == 0 {
		return text
	}
	var b strings.Builder
	writeIndented(&b, sortedNode(extras), 1, 0)
	return text + "\n\n" + envelopeMetaOpenTag + "\n" + b.String() + "\n" + envelopeMetaCloseTag + "\n"
}

func writeIndented(b *strings.Builder, n *jnode, step, depth int) {
	pad := func(d int) string { return "\n" + strings.Repeat(" ", step*d) }
	switch {
	case n.isArr():
		if len(n.arr) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[")
		for i, v := range n.arr {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(pad(depth + 1))
			writeIndented(b, v, step, depth+1)
		}
		b.WriteString(pad(depth) + "]")
	case n.isObj():
		if len(n.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{")
		for i, k := range n.keys {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(pad(depth + 1))
			b.Write(wire.Quote(k))
			b.WriteString(": ")
			writeIndented(b, n.vals[i], step, depth+1)
		}
		b.WriteString(pad(depth) + "}")
	default:
		b.Write(n.compact())
	}
}

func (s *Spill) writeToSandbox(ctx context.Context, content, remotePath string, env Environment) (bool, error) {
	dir := path.Dir(remotePath)
	cmd := "umask 077 && mkdir -p " + shell.Quote(dir) + " && chmod 700 " + shell.Quote(dir) + " && cat > " + shell.Quote(remotePath)
	stdin := content
	res, err := env.Execute(ctx, cmd, ExecOptions{Timeout: 30 * time.Second, Stdin: &stdin, RewriteCompoundBackground: true})
	if err != nil {
		return false, err
	}
	if res.ExitCode != 0 {
		return false, nil
	}
	expected := len(strings.ToValidUTF8(content, "�"))
	probe, err := env.Execute(ctx, "wc -c < "+shell.Quote(remotePath), ExecOptions{Timeout: 15 * time.Second, RewriteCompoundBackground: true})
	if err != nil {
		slog.Debug(fmt.Sprintf("Sandbox size probe failed for %s: %v", remotePath, err))
		return true, nil
	}
	if probe.ExitCode != 0 {
		return true, nil
	}
	fields := strings.Fields(probe.Output)
	if len(fields) == 0 {
		return true, nil
	}
	last := fields[len(fields)-1]
	size, perr := strconv.Atoi(last)
	if perr != nil || strings.TrimLeft(last, "0123456789") != "" {
		return true, nil
	}
	if size == expected {
		return true, nil
	}
	slog.Warn(fmt.Sprintf("Sandbox spill for %s is not lossless (%d bytes in sandbox, expected %d) — discarding archive", remotePath, size, expected))
	_, _ = env.Execute(ctx, "rm -f "+shell.Quote(remotePath), ExecOptions{Timeout: 15 * time.Second, RewriteCompoundBackground: true})
	return false, nil
}

func persistedMessage(preview string, more bool, original int, filePath string) string {
	kb := float64(original) / 1024
	size := strconv.FormatFloat(kb, 'f', 1, 64) + " KB"
	if kb >= 1024 {
		size = strconv.FormatFloat(kb/1024, 'f', 1, 64) + " MB"
	}
	tail := ""
	if more {
		tail = "\n..."
	}
	return persistedOpenTag + "\n" +
		"This tool result was too large (" + groupThousands(original) + " characters, " + size + ").\n" +
		"Full output saved to: " + filePath + "\n" +
		"Use the read_file tool with offset and limit to access specific sections of this output.\n" +
		"Recovery: page through the saved file with read_file (offset/limit) or process it with execute_code — do NOT re-request the same data from the remote API; the archive is kept while this session's transcript references it. If it is missing, inspect the current state before retrying — the call that produced it may already have taken effect.\n\n" +
		"Preview (first " + strconv.Itoa(utf8.RuneCountInString(preview)) + " chars):\n" +
		preview + tail + "\n" + persistedCloseTag
}

func PersistedPath(content string) (string, bool) {
	if !strings.Contains(content, persistedOpenTag) {
		return "", false
	}
	m := persistedPathRe.FindStringSubmatch(content)
	if m == nil {
		return "", false
	}
	p := trimWide(m[1])
	return p, p != ""
}

func (s *Spill) persist(ctx context.Context, content, toolName, callID string, threshold int, env Environment, b Budget) string {
	size := utf8.RuneCountInString(content)
	if threshold == Unlimited || size <= threshold {
		return content
	}
	persisted := pageableText(content)
	persistedSize := utf8.RuneCountInString(persisted)
	filename := safeResultFilename(callID)
	preview, more := generatePreview(persisted, b.PreviewChars)
	done := func(p, hostSuffix string) string {
		slog.Info(fmt.Sprintf("Persisted large tool result: %s (%s, %d chars -> %s%s)", toolName, callID, persistedSize, p, hostSuffix))
		return persistedMessage(preview, more, persistedSize, p)
	}
	hostPath, wrote := s.writeToSpillover(ctx, persisted, filename)
	hostSide := env == nil || env.HostFilesystem()
	if hostSide && wrote {
		return done(hostPath, "")
	}
	if !hostSide {
		if wrote {
			if visible := s.sandboxVisible(ctx, hostPath, env); visible != "" {
				return done(visible, " [host: "+hostPath+"]")
			}
		}
		remote := storageDir(env) + "/" + filename
		ok, err := s.writeToSandbox(ctx, persisted, remote, env)
		switch {
		case err != nil:
			slog.Warn(fmt.Sprintf("Sandbox write failed for %s: %v", callID, err))
		case ok:
			return done(remote, "")
		}
	}
	slog.Info(fmt.Sprintf("Inline-truncating large tool result: %s (%d chars, no sandbox write)", toolName, size))
	return preview + "\n\n[Truncated: tool response was " + groupThousands(size) + " chars. Full output could not be saved to sandbox.]"
}

func (s *Spill) Persist(ctx context.Context, r Result, toolName, callID string, threshold int, env Environment, b Budget) Result {
	if r.Parts == nil {
		r.Text = s.persist(ctx, r.Text, toolName, callID, threshold, env, b)
		return r
	}
	parts := slices.Clone(r.Parts)
	first, spilled := "", 0
	for i := range parts {
		if parts[i].Kind != wire.PartText {
			continue
		}
		text := parts[i].Text
		id := callID
		if spilled > 0 {
			id = fmt.Sprintf("%s_part%d", callID, spilled)
		}
		replaced := s.persist(ctx, text, toolName, id, threshold, env, b)
		if replaced != text {
			parts[i].Text = replaced
			if first == "" {
				first = replaced
			}
			spilled++
		}
	}
	if first == "" {
		return r
	}
	r.Parts = parts
	if r.Summary != "" && threshold != Unlimited && utf8.RuneCountInString(r.Summary) > threshold {
		r.Summary = first
	}
	return r
}

func messageTextSize(m *wire.Message) int {
	if m.Parts == nil {
		return utf8.RuneCountInString(m.Text)
	}
	n := 0
	for _, p := range m.Parts {
		if p.Kind == wire.PartText {
			n += utf8.RuneCountInString(p.Text)
		}
	}
	return n
}

func (s *Spill) EnforceTurn(ctx context.Context, msgs []*wire.Message, env Environment, b Budget) {
	sizes := make([]int, len(msgs))
	total := 0
	type candidate struct{ idx, size int }
	var candidates []candidate
	for i, m := range msgs {
		sizes[i] = messageTextSize(m)
		total += sizes[i]
		if m.Parts != nil || !strings.Contains(m.Text, persistedOpenTag) {
			candidates = append(candidates, candidate{i, sizes[i]})
		}
	}
	if total <= b.TurnChars {
		return
	}
	sort.SliceStable(candidates, func(a, c int) bool { return candidates[a].size > candidates[c].size })
	for _, c := range candidates {
		if total <= b.TurnChars {
			break
		}
		m := msgs[c.idx]
		id := m.ToolCallID
		if id == "" {
			id = fmt.Sprintf("budget_%d", c.idx)
		}
		if m.Parts != nil {
			best, bestLen := -1, -1
			for i, p := range m.Parts {
				if p.Kind != wire.PartText || strings.Contains(p.Text, persistedOpenTag) {
					continue
				}
				if l := utf8.RuneCountInString(p.Text); l > bestLen {
					best, bestLen = i, l
				}
			}
			if best < 0 {
				continue
			}
			text := m.Parts[best].Text
			replacement := s.persist(ctx, text, budgetToolName, fmt.Sprintf("%s_part%d", id, best), 0, env, b)
			if replacement == text {
				continue
			}
			parts := slices.Clone(m.Parts)
			parts[best].Text = replacement
			saved := bestLen - utf8.RuneCountInString(replacement)
			total -= saved
			m.Parts = parts
			slog.Info(fmt.Sprintf("Budget enforcement: persisted a text part of tool result %s (%d chars)", id, saved))
			continue
		}
		replacement := s.persist(ctx, m.Text, budgetToolName, id, 0, env, b)
		if replacement != m.Text {
			total += utf8.RuneCountInString(replacement) - c.size
			m.Text = replacement
			slog.Info(fmt.Sprintf("Budget enforcement: persisted tool result %s (%d chars)", id, c.size))
		}
	}
}
