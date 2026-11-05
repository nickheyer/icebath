package tools

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	_ "modernc.org/sqlite"
)

const (
	PersistedOutputTag        = "<persisted-output>"
	PersistedOutputClosingTag = "</persisted-output>"
	SpilloverSubdir           = "cache/spillover"
	// SpilloverMaxAgeHours is the legacy floor used when the retention config is unreadable
	SpilloverMaxAgeHours = 24
	budgetToolName       = "__budget_enforcement__"
	// ThresholdResolve asks MaybePersistToolResult to resolve the tool's own threshold
	ThresholdResolve = -1

	envelopeMetadataTag        = "<mcp-result-metadata>"
	envelopeMetadataClosingTag = "</mcp-result-metadata>"
	maxResultFilenameStem      = 120
)

// The exact key set the MCP call_tool renderer emits as its envelope
var mcpEnvelopeKeys = map[string]bool{"result": true, "structuredContent": true, "_meta": true}

var (
	unsafeResultFilenameChars = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
	spilloverNameRe           = regexp.MustCompile(`spillover(?:/|\\+)([A-Za-z0-9_.-]+)`)
	archivePathRe             = regexp.MustCompile(`(?:^|[/\\])(?:spillover|icebath-results)[/\\][^/\\]+$`)
	persistedPathRe           = regexp.MustCompile(`(?m)^Full output saved to: (.+)$`)
)

// SpilloverSQLFilter selects transcript rows that may reference a spill archive
const SpilloverSQLFilter = "(instr(content, 'spillover') > 0 OR instr(api_content, 'spillover') > 0)"

var (
	spilloverPruneMu     sync.Mutex
	spilloverPrunedHomes = map[string]bool{}
)

// StorageDir is the sandbox fallback results directory under the system temp root
func StorageDir() string {
	return filepath.Join(os.TempDir(), "icebath-results")
}

// SpilloverDir returns the profile's cache/spillover path without creating it
func SpilloverDir(env *Env) string {
	return filepath.Join(envHome(env), filepath.FromSlash(SpilloverSubdir))
}

// Spill archive age equals the session retention window floored at the legacy cache age
func spilloverRetentionHours(env *Env) int {
	sessions := cfgTree(env).Map("sessions")
	raw, ok := sessions["retention_days"]
	days := 90.0
	if ok && raw != nil {
		f, isNum := config.AsFloat(raw)
		if !isNum {
			if s, isStr := raw.(string); isStr {
				parsed, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
				if err != nil {
					return SpilloverMaxAgeHours
				}
				f = parsed
			} else {
				return SpilloverMaxAgeHours
			}
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return SpilloverMaxAgeHours
		}
		days = f
	}
	hours := int(days * 24)
	if hours < SpilloverMaxAgeHours {
		return SpilloverMaxAgeHours
	}
	return hours
}

// ExpiredArchiveFields adds the expired hint for a pruned persisted tool result path
func ExpiredArchiveFields(path string) map[string]any {
	if !archivePathRe.MatchString(path) {
		return map[string]any{}
	}
	return map[string]any{"expired": true, "hint": "This was a persisted tool result archive and it has expired (pruned with its " +
		"session or by the cache sweep); the full output is no longer recoverable. Inspect " +
		"the current state before re-running the original tool — the call that produced it " +
		"may already have taken effect."}
}

// SpilloverNamesInText returns spill archive basenames referenced by transcript text
func SpilloverNamesInText(texts ...string) map[string]bool {
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

// ReferencedSpilloverNames returns basenames still referenced by any transcript row, nil when unknown
func ReferencedSpilloverNames(spillDir string) (map[string]bool, bool) {
	dbPath := filepath.Join(filepath.Dir(filepath.Dir(spillDir)), "state.db")
	if _, err := os.Stat(dbPath); err != nil {
		return map[string]bool{}, true
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, false
	}
	defer db.Close()
	rows, err := db.Query("SELECT content, api_content FROM messages WHERE " + SpilloverSQLFilter + " LIMIT 20001")
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	names := map[string]bool{}
	count := 0
	for rows.Next() {
		var content, apiContent sql.NullString
		if err := rows.Scan(&content, &apiContent); err != nil {
			return nil, false
		}
		count++
		if count > 20000 {
			return nil, false
		}
		for n := range SpilloverNamesInText(content.String, apiContent.String) {
			names[n] = true
		}
	}
	if rows.Err() != nil {
		return nil, false
	}
	return names, true
}

// CleanupSpilloverCache deletes expired unreferenced spill files and returns the count removed
func CleanupSpilloverCache(env *Env, maxAgeHours int) int {
	ageHours := maxAgeHours
	if ageHours < 0 {
		ageHours = spilloverRetentionHours(env)
	}
	cutoff := time.Now().Add(-time.Duration(ageHours) * time.Hour)
	dir := SpilloverDir(env)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var expired []string
	for _, e := range entries {
		info, err := e.Info()
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
	referenced, known := ReferencedSpilloverNames(dir)
	removed := 0
	for _, name := range expired {
		if !known || referenced[name] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err == nil {
			removed++
		}
	}
	return removed
}

// Sweeps the profile's spillover at most once per process per home
func pruneSpilloverOnce(env *Env) {
	key := home.Key(envHome(env))
	spilloverPruneMu.Lock()
	already := spilloverPrunedHomes[key]
	spilloverPrunedHomes[key] = true
	spilloverPruneMu.Unlock()
	if already {
		return
	}
	if removed := CleanupSpilloverCache(env, -1); removed > 0 {
		toolsLogger(env).Debug("Pruned expired spillover file(s)", "count", removed)
	}
}

// ResetSpilloverPruneState forgets which homes were swept so tests can observe the first spill sweep
func ResetSpilloverPruneState() {
	spilloverPruneMu.Lock()
	spilloverPrunedHomes = map[string]bool{}
	spilloverPruneMu.Unlock()
}

// True when this process writes the spill file directly because the sandbox files are the host's
func isHostSideEnvironment(sandbox Environment) bool {
	return sandbox == nil || sandbox.UsesHostFilesystem()
}

// Writes host side under cache/spillover with the size verified before the model hears about it
func writeToSpillover(env *Env, content, filename string) string {
	data := []byte(content)
	dir := SpilloverDir(env)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		toolsLogger(env).Warn("Spillover write failed", "file", filename, "error", err)
		return ""
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		toolsLogger(env).Warn("Spillover write failed", "file", filename, "error", err)
		return ""
	}
	st, err := os.Stat(path)
	if err != nil {
		toolsLogger(env).Warn("Spillover write failed", "file", filename, "error", err)
		return ""
	}
	if st.Size() != int64(len(data)) {
		toolsLogger(env).Warn("Spillover write is not lossless, discarding archive", "file", filename, "on_disk", st.Size(), "expected", len(data))
		os.Remove(path)
		return ""
	}
	pruneSpilloverOnce(env)
	return path
}

// SandboxSyncer is implemented by file synced backends that must push host files before a probe
type SandboxSyncer interface {
	SyncFiles(ctx context.Context, force bool) error
}

// Path where a remote backend can read the host spill file or empty when it cannot
func sandboxVisibleSpilloverPath(ctx context.Context, env *Env, hostPath string, sandbox Environment) string {
	visible := toAgentVisibleCachePath(env, hostPath)
	if syncer, ok := sandbox.(SandboxSyncer); ok {
		if err := syncer.SyncFiles(ctx, true); err != nil {
			toolsLogger(env).Debug("Spillover sync failed", "error", err)
		}
	}
	res, err := sandbox.Execute(ctx, "test -r "+shQuote(visible), ExecOptions{Timeout: 15})
	if err != nil {
		toolsLogger(env).Debug("Spillover readability probe failed", "error", err)
		return ""
	}
	if res == nil || res.ReturnCode != 0 {
		return ""
	}
	return visible
}

// Best temp backed storage dir for a sandbox environment
func resolveStorageDir(sandbox Environment) string {
	if sandbox == nil {
		return StorageDir()
	}
	temp := strings.TrimRight(sandbox.TempDir(), "/")
	if sandbox.TempDir() == "" {
		return StorageDir()
	}
	if temp == "" {
		temp = "/"
	}
	return temp + "/icebath-results"
}

// Single safe filename for a tool result id
func safeResultFilename(toolUseID string) string {
	rawID := toolUseID
	if rawID == "" {
		rawID = "tool_result"
	}
	safeStem := strings.Trim(unsafeResultFilenameChars.ReplaceAllString(rawID, "_"), "._-")
	changed := safeStem != rawID
	if safeStem == "" {
		safeStem = "tool_result"
	}
	if changed || len(safeStem) > maxResultFilenameStem {
		sum := sha256.Sum256([]byte(rawID))
		digest := hex.EncodeToString(sum[:])[:12]
		if len(safeStem) > maxResultFilenameStem {
			safeStem = safeStem[:maxResultFilenameStem]
		}
		safeStem = strings.TrimRight(safeStem, "._-")
		if safeStem == "" {
			safeStem = "tool_result"
		}
		safeStem = safeStem + "_" + digest
	}
	return safeStem + ".txt"
}

// GeneratePreview truncates at the last newline within maxChars and reports whether more follows
func GeneratePreview(content string, maxChars int) (string, bool) {
	if len(content) <= maxChars {
		return content, false
	}
	lastNL := strings.LastIndex(content[:maxChars], "\n")
	if lastNL > maxChars/2 {
		return content[:lastNL+1], true
	}
	return content[:maxChars], true
}

// Rewrites an MCP handler result envelope into the text it wraps with metadata appended
func pageableText(content string) string {
	var payload map[string]any
	dec := json.NewDecoder(strings.NewReader(content))
	if err := dec.Decode(&payload); err != nil || payload == nil || len(payload) == 0 {
		return content
	}
	if _, trailing := dec.Token(); trailing == nil {
		return content
	}
	for k := range payload {
		if !mcpEnvelopeKeys[k] {
			return content
		}
	}
	text, ok := payload["result"].(string)
	if !ok || text == "" {
		return content
	}
	extras := map[string]any{}
	for k, v := range payload {
		if k != "result" {
			extras[k] = v
		}
	}
	if len(extras) == 0 {
		return text
	}
	metadata := jsonIndentOne(extras)
	return fmt.Sprintf("%s\n\n%s\n%s\n%s\n", text, envelopeMetadataTag, metadata, envelopeMetadataClosingTag)
}

// One space indented JSON with sorted keys and no HTML escaping
func jsonIndentOne(v any) string {
	b, err := pyjson.EncodeWTF8Indent(v, "", " ")
	if err != nil {
		return ErrorResult(err)
	}
	return string(b)
}

// Shell command creating the directory owner only on a shared host
func privateDirsCmd(root string) string {
	return "umask 077 && mkdir -p " + shQuote(root) + " && chmod 700 " + shQuote(root)
}

// Writes content into the sandbox through stdin and verifies the byte count round trip
func writeToSandbox(ctx context.Context, env *Env, content, remotePath string, sandbox Environment) bool {
	storageDir := remotePath
	if idx := strings.LastIndex(remotePath, "/"); idx >= 0 {
		storageDir = remotePath[:idx]
	}
	cmd := privateDirsCmd(storageDir) + " && cat > " + shQuote(remotePath)
	stdin := content
	res, err := sandbox.Execute(ctx, cmd, ExecOptions{Timeout: 30, Stdin: &stdin})
	if err != nil || res == nil || res.ReturnCode != 0 {
		return false
	}
	expected := len(content)
	probe, err := sandbox.Execute(ctx, "wc -c < "+shQuote(remotePath), ExecOptions{Timeout: 15})
	if err != nil {
		toolsLogger(env).Debug("Sandbox size probe failed", "path", remotePath, "error", err)
		return true
	}
	if probe == nil || probe.ReturnCode != 0 {
		return true
	}
	fields := strings.Fields(strings.TrimSpace(probe.Output))
	if len(fields) == 0 {
		return true
	}
	persisted, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil {
		return true
	}
	if persisted == expected {
		return true
	}
	toolsLogger(env).Warn("Sandbox spill is not lossless, discarding archive", "path", remotePath, "in_sandbox", persisted, "expected", expected)
	sandbox.Execute(ctx, "rm -f "+shQuote(remotePath), ExecOptions{Timeout: 15})
	return false
}

// BuildPersistedMessage builds the persisted output replacement block
func BuildPersistedMessage(preview string, hasMore bool, originalSize int, filePath string) string {
	sizeKB := float64(originalSize) / 1024
	sizeStr := fmt.Sprintf("%.1f KB", sizeKB)
	if sizeKB >= 1024 {
		sizeStr = fmt.Sprintf("%.1f MB", sizeKB/1024)
	}
	more := ""
	if hasMore {
		more = "\n..."
	}
	return PersistedOutputTag + "\n" +
		fmt.Sprintf("This tool result was too large (%s characters, %s).\n", commaInt(originalSize), sizeStr) +
		fmt.Sprintf("Full output saved to: %s\n", filePath) +
		"Use the read_file tool with offset and limit to access specific sections of this output.\n" +
		"Recovery: page through the saved file with read_file (offset/limit) or " +
		"process it with execute_code — do NOT re-request the same data from the " +
		"remote API; the archive is kept while this session's transcript references " +
		"it. If it is missing, inspect the current state before retrying — the call " +
		"that produced it may already have taken effect.\n\n" +
		fmt.Sprintf("Preview (first %d chars):\n", len(preview)) +
		preview + more + "\n" + PersistedOutputClosingTag
}

// ExtractPersistedPath returns the file path from a persisted output block or empty
func ExtractPersistedPath(content string) string {
	if !strings.Contains(content, PersistedOutputTag) {
		return ""
	}
	m := persistedPathRe.FindStringSubmatch(content)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// MaybePersistToolResult persists an oversized result and returns the preview plus path block
func MaybePersistToolResult(ctx context.Context, env *Env, sandbox Environment, content, toolName, toolUseID string, cfg BudgetConfig, threshold int) string {
	if threshold == ThresholdResolve {
		threshold = cfg.ResolveThreshold(env, toolName)
	}
	if threshold == BudgetUnlimited || len(content) <= threshold {
		return content
	}
	persistedContent := pageableText(content)
	filename := safeResultFilename(toolUseID)
	preview, hasMore := GeneratePreview(persistedContent, cfg.PreviewSize)
	persisted := func(path, hostSuffix string) string {
		toolsLogger(env).Info("Persisted large tool result", "tool", toolName, "id", toolUseID, "chars", len(persistedContent), "path", path+hostSuffix)
		return BuildPersistedMessage(preview, hasMore, len(persistedContent), path)
	}
	hostPath := writeToSpillover(env, persistedContent, filename)
	hostSide := isHostSideEnvironment(sandbox)
	if hostSide && hostPath != "" {
		return persisted(hostPath, "")
	}
	if !hostSide {
		visible := ""
		if hostPath != "" {
			visible = sandboxVisibleSpilloverPath(ctx, env, hostPath, sandbox)
		}
		if visible != "" {
			return persisted(visible, " [host: "+hostPath+"]")
		}
		remotePath := resolveStorageDir(sandbox) + "/" + filename
		if writeToSandbox(ctx, env, persistedContent, remotePath, sandbox) {
			return persisted(remotePath, "")
		}
	}
	toolsLogger(env).Info("Inline-truncating large tool result", "tool", toolName, "chars", len(content))
	return fmt.Sprintf("%s\n\n[Truncated: tool response was %s chars. Full output could not be saved to sandbox.]", preview, commaInt(len(content)))
}

// Characters of text a tool message carries, text parts summed for a multimodal list
func contentTextSize(content any) int {
	switch t := content.(type) {
	case string:
		return len(t)
	case []any:
		total := 0
		for _, p := range t {
			if part, ok := p.(map[string]any); ok {
				if kind, _ := part["type"].(string); kind == "text" {
					text, _ := part["text"].(string)
					total += len(text)
				}
			}
		}
		return total
	}
	return 0
}

// Persists the largest non persisted text part of a multimodal list and reports chars saved
func spillLargestTextPart(ctx context.Context, env *Env, sandbox Environment, parts []any, toolUseID string, cfg BudgetConfig) ([]any, int) {
	bestIdx, bestLen := -1, -1
	for i, p := range parts {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := part["type"].(string); kind != "text" {
			continue
		}
		text, ok := part["text"].(string)
		if !ok || strings.Contains(text, PersistedOutputTag) {
			continue
		}
		if len(text) > bestLen {
			bestIdx, bestLen = i, len(text)
		}
	}
	if bestIdx < 0 {
		return parts, 0
	}
	text := parts[bestIdx].(map[string]any)["text"].(string)
	replacement := MaybePersistToolResult(ctx, env, sandbox, text, budgetToolName, fmt.Sprintf("%s_part%d", toolUseID, bestIdx), cfg, 0)
	if replacement == text {
		return parts, 0
	}
	out := make([]any, len(parts))
	copy(out, parts)
	replaced := map[string]any{}
	for k, v := range parts[bestIdx].(map[string]any) {
		replaced[k] = v
	}
	replaced["text"] = replacement
	out[bestIdx] = replaced
	return out, len(text) - len(replacement)
}

// EnforceTurnBudget persists the largest non persisted results until the turn's aggregate fits
func EnforceTurnBudget(ctx context.Context, env *Env, sandbox Environment, toolMessages []*state.Message, cfg BudgetConfig) []*state.Message {
	sizes := make([]int, len(toolMessages))
	total := 0
	for i, msg := range toolMessages {
		sizes[i] = contentTextSize(msg.Content)
		total += sizes[i]
	}
	if total <= cfg.TurnBudget {
		return toolMessages
	}
	type candidate struct{ idx, size int }
	var candidates []candidate
	for i, msg := range toolMessages {
		switch c := msg.Content.(type) {
		case []any:
			candidates = append(candidates, candidate{i, sizes[i]})
		case string:
			if !strings.Contains(c, PersistedOutputTag) {
				candidates = append(candidates, candidate{i, sizes[i]})
			}
		default:
			candidates = append(candidates, candidate{i, sizes[i]})
		}
	}
	sort.SliceStable(candidates, func(a, b int) bool { return candidates[a].size > candidates[b].size })
	for _, c := range candidates {
		if total <= cfg.TurnBudget {
			break
		}
		msg := toolMessages[c.idx]
		toolUseID := msg.ToolCallID
		if toolUseID == "" {
			toolUseID = fmt.Sprintf("budget_%d", c.idx)
		}
		if parts, ok := msg.Content.([]any); ok {
			newParts, saved := spillLargestTextPart(ctx, env, sandbox, parts, toolUseID, cfg)
			if saved > 0 {
				total -= saved
				msg.Content = newParts
				toolsLogger(env).Info("Budget enforcement: persisted a text part of tool result", "id", toolUseID, "chars", saved)
			}
			continue
		}
		content, _ := msg.Content.(string)
		replacement := MaybePersistToolResult(ctx, env, sandbox, content, budgetToolName, toolUseID, cfg, 0)
		if replacement != content {
			total += len(replacement) - c.size
			msg.Content = replacement
			toolsLogger(env).Info("Budget enforcement: persisted tool result", "id", toolUseID, "chars", c.size)
		}
	}
	return toolMessages
}

// Hook output spill defaults
const (
	SpillDefaultMaxChars    = 10000
	SpillDefaultPreviewHead = 500
	SpillDefaultPreviewTail = 500
)

// SpillConfig is the resolved hooks.output_spill section
type SpillConfig struct {
	Enabled     bool
	MaxChars    int
	PreviewHead int
	PreviewTail int
	Directory   string
}

// SpillConfigFromEnv reads hooks.output_spill with defaults for every missing or invalid field
func SpillConfigFromEnv(env *Env) SpillConfig {
	section := map[string]any{}
	if hooks := cfgTree(env).Map("hooks"); hooks != nil {
		if s, ok := hooks["output_spill"].(map[string]any); ok {
			section = s
		}
	}
	enabled := true
	if raw, ok := section["enabled"]; ok && raw != nil {
		enabled = config.Truthy(raw)
	}
	directory, _ := section["directory"].(string)
	return SpillConfig{
		Enabled:     enabled,
		MaxChars:    coerceInt(section["max_chars"], SpillDefaultMaxChars, 1),
		PreviewHead: coerceInt(section["preview_head"], SpillDefaultPreviewHead, 0),
		PreviewTail: coerceInt(section["preview_tail"], SpillDefaultPreviewTail, 0),
		Directory:   directory,
	}
}

// Per session spill directory with the session id sanitized so it cannot escape the base
func resolveSpillDir(env *Env, directoryOverride, sessionID string) string {
	base := filepath.Join(envHome(env), "hook_outputs")
	if directoryOverride != "" {
		base = expandUser(directoryOverride)
	}
	segment := sessionID
	if segment == "" {
		segment = "no-session"
	}
	segment = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(segment, "/", "_"), "\\", "_"), "..", "_")
	return filepath.Join(base, segment)
}

// SpillIfOversized writes hook text over the cap to disk and returns a preview pointing at it
func SpillIfOversized(env *Env, text, sessionID, source string, cfg *SpillConfig) string {
	var c SpillConfig
	if cfg != nil {
		c = *cfg
	} else {
		c = SpillConfigFromEnv(env)
	}
	if !c.Enabled {
		return text
	}
	maxChars := c.MaxChars
	if maxChars <= 0 {
		maxChars = SpillDefaultMaxChars
	}
	if len(text) <= maxChars {
		return text
	}
	head, tail := c.PreviewHead, c.PreviewTail
	savedPath := ""
	spillDir := resolveSpillDir(env, c.Directory, sessionID)
	if err := EnsureSpillDir(spillDir, true); err != nil {
		toolsLogger(env).Warn("hook output spill failed", "error", err)
	} else {
		spillPath := filepath.Join(spillDir, strings.ReplaceAll(uuid.NewString(), "-", "")+".txt")
		body := text
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		if err := WriteTextExclusive(spillPath, body, true, false); err != nil {
			toolsLogger(env).Warn("hook output spill failed", "error", err)
		} else {
			savedPath = spillPath
		}
	}
	total := len(text)
	first := fmt.Sprintf("[%s output truncated — %s chars; full content ", source, commaInt(total))
	if savedPath != "" {
		first += "saved to " + savedPath + "]"
	} else {
		first += "unavailable — spill write failed]"
	}
	parts := []string{first}
	if head > 0 && len(text) > 0 {
		h := head
		if h > len(text) {
			h = len(text)
		}
		parts = append(parts, "--- head ---", text[:h])
	}
	if tail > 0 && total > head {
		t := tail
		if t > len(text) {
			t = len(text)
		}
		parts = append(parts, "--- tail ---", text[len(text)-t:])
	}
	return strings.Join(parts, "\n")
}

// EnsureSpillDir creates the directory refusing a symlinked leaf and tightening private leaves to 0700
func EnsureSpillDir(path string, private bool) error {
	mode := os.FileMode(0o777)
	if private {
		mode = 0o700
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsDir() {
		return fmt.Errorf("spill dir is not a directory (symlink?): %s", path)
	}
	if private && st.Mode().Perm() != 0o700 {
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// OpenExclusive creates the file exclusively never following a link, unlinking first when overwriting
func OpenExclusive(path string, private, overwrite bool) (*os.File, error) {
	if overwrite {
		st, err := os.Lstat(path)
		if err == nil {
			if st.IsDir() {
				return nil, fmt.Errorf("refusing to overwrite a directory: %s", path)
			}
			if err := os.Remove(path); err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	mode := os.FileMode(0o666)
	if private {
		mode = 0o600
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|spillNoFollowFlag, mode)
}

// WriteTextExclusive writes text through an exclusive create that refuses to follow symlinks
func WriteTextExclusive(path, text string, private, overwrite bool) error {
	f, err := OpenExclusive(path, private, overwrite)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
