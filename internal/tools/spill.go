package tools

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Marks of the block that stands in for a tool result saved to a file
const (
	SpillTag      = "<persisted-output>"
	spillCloseTag = "</persisted-output>"
	// Folder of the profile results are saved in
	SpillDir = "cache/spillover"
)

const (
	defaultResultChars  = 100_000
	defaultTurnChars    = 200_000
	defaultPreviewChars = 1500
	// Shares of the window, in characters, one result and all the results of a reply may take, and the least each may
	resultWindowShare = 0.15
	turnWindowShare   = 0.30
	minResultChars    = 8000
	minTurnChars      = 16000
	charsPerToken     = 4
	// Characters of a result's call id a file name keeps
	maxStem = 120
	// Shortest time a saved result is kept
	spillAgeFloor = 24 * time.Hour
)

// Size of a result kept in the chat whatever its size, as read_file has to avoid saving what it reads back from a saved result
const Unlimited = math.MaxInt

// What a file name of a saved result can't hold
var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// Tools whose results are never saved to files
var pinned = map[string]bool{"read_file": true}

// How much of its tool results a turn keeps in the chat before the rest is saved to files: one result, all the results of a reply, the
// preview a saved one keeps, and one result of an MCP tool
type Budget struct {
	Result  int
	Turn    int
	Preview int
	MCP     int
}

// Budget for a window of that many tokens, the defaults capped to shares of it and floored; the defaults when the window is not known
func BudgetFor(cfg *config.Config, window int) Budget {
	mcp := cfg.GetInt(toolsconfig.BudgetMCPResult)
	if mcp <= 0 {
		mcp = 50000
	}
	if window <= 0 {
		return Budget{Result: defaultResultChars, Turn: defaultTurnChars, Preview: defaultPreviewChars, MCP: mcp}
	}
	chars := float64(window * charsPerToken)
	return Budget{
		Result:  max(minResultChars, min(int(chars*resultWindowShare), defaultResultChars)),
		Turn:    max(minTurnChars, min(int(chars*turnWindowShare), defaultTurnChars)),
		Preview: defaultPreviewChars,
		MCP:     mcp,
	}
}

// Largest result of the tool the chat keeps: none for the pinned tools, the MCP size for an MCP tool, the tool's own when smaller
func (b Budget) Threshold(def Def) int {
	switch {
	case pinned[def.Name]:
		return Unlimited
	case strings.HasPrefix(def.Name, "mcp_"):
		return min(b.MCP, b.Result)
	case def.MaxResultChars < 0:
		return Unlimited
	case def.MaxResultChars > 0:
		return min(def.MaxResultChars, b.Result)
	}
	return b.Result
}

// Folder the profile's saved results are in
func spillFolder(cfg *config.Config) string {
	return filepath.Join(cfg.ProfileDir(cfg.Profile()), filepath.FromSlash(SpillDir))
}

// File name of a result by its call id, made safe and short
func spillFile(id string) string {
	raw := cmp.Or(id, "tool_result")
	stem := strings.Trim(unsafeName.ReplaceAllString(raw, "_"), "._-")
	changed := stem != raw
	if stem == "" {
		stem = "tool_result"
	}
	if changed || len(stem) > maxStem {
		sum := sha256.Sum256([]byte(raw))
		stem = strings.TrimRight(stem[:min(len(stem), maxStem)], "._-")
		stem = cmp.Or(stem, "tool_result") + "_" + hex.EncodeToString(sum[:])[:12]
	}
	return stem + ".txt"
}

// Start of a text cut at its last line break within the limit, and whether more follows
func Preview(content string, limit int) (string, bool) {
	if utf8.RuneCountInString(content) <= limit {
		return content, false
	}
	head := string([]rune(content)[:limit])
	if cut := strings.LastIndex(head, "\n"); cut >= 0 && utf8.RuneCountInString(head[:cut]) > limit/2 {
		return head[:cut+1], true
	}
	return head, true
}

// Text an MCP result envelope wraps with its metadata after it, the content as it is for anything else
func pageable(content string) string {
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &payload) != nil || len(payload) == 0 {
		return content
	}
	extras := map[string]json.RawMessage{}
	for key, value := range payload {
		switch key {
		case "result":
		case "structuredContent", "_meta":
			extras[key] = value
		default:
			return content
		}
	}
	var text string
	if json.Unmarshal(payload["result"], &text) != nil || text == "" {
		return content
	}
	if len(extras) == 0 {
		return text
	}
	metadata, err := json.MarshalIndent(extras, "", " ")
	if err != nil {
		return text
	}
	return fmt.Sprintf("%s\n\n<mcp-result-metadata>\n%s\n</mcp-result-metadata>\n", text, metadata)
}

// Block that stands in for a saved result: its size, where it is and how to read it, and its start
func spilledBlock(preview string, more bool, size int, path string) string {
	kb := float64(size) / 1024
	human := fmt.Sprintf("%.1f KB", kb)
	if kb >= 1024 {
		human = fmt.Sprintf("%.1f MB", kb/1024)
	}
	tail := ""
	if more {
		tail = "\n..."
	}
	return SpillTag + "\n" +
		fmt.Sprintf("This tool result was too large (%s characters, %s).\n", strs.Thousands(size), human) +
		fmt.Sprintf("Full output saved to: %s\n", path) +
		"Use the read_file tool with offset and limit to access specific sections of this output.\n" +
		"Recovery: page through the saved file with read_file (offset/limit) or " +
		"process it with execute_code — do NOT re-request the same data from the " +
		"remote API; the archive is kept while this session's transcript references " +
		"it. If it is missing, inspect the current state before retrying — the call " +
		"that produced it may already have taken effect.\n\n" +
		fmt.Sprintf("Preview (first %d chars):\n", utf8.RuneCountInString(preview)) +
		preview + tail + "\n" + spillCloseTag
}

// Writes the text to the profile's folder of saved results, checked whole on disk; the path, empty when it could not be written
func writeSpill(cfg *config.Config, content, name string) string {
	dir := spillFolder(cfg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn(fmt.Sprintf("Spillover write failed for %s: %v", name, err))
		return ""
	}
	path := filepath.Join(dir, name)
	data := []byte(content)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		slog.Warn(fmt.Sprintf("Spillover write failed for %s: %v", name, err))
		return ""
	}
	if st, err := os.Stat(path); err != nil || st.Size() != int64(len(data)) {
		slog.Warn(fmt.Sprintf("Spillover write for %s is not lossless — discarding archive", name))
		os.Remove(path)
		return ""
	}
	return path
}

// Path of a saved result, of the tools' or of a command's spilled output, or of the folder results were saved to before
var archive = regexp.MustCompile(`(?:^|[/\\])(?:spillover|icebath-results|terminal-output)[/\\][^/\\]+$`)

// Hint for a path that is missing when it is that of a saved result, which the sweep pruned or its session took with it; empty otherwise
func Expired(path string) string {
	if !archive.MatchString(path) {
		return ""
	}
	return "This was a persisted tool result archive and it has expired (pruned with its session or by the cache sweep); the full output is no " +
		"longer recoverable. Inspect the current state before re-running the original tool — the call that produced it may already have " +
		"taken effect."
}

// Result in the chat: the text when it fits the threshold, else a block pointing at the file it was saved in with its start; its start
// alone with a note when it could not be saved
func Spill(env *Env, content, tool, id string, threshold, preview int) string {
	if threshold == Unlimited || utf8.RuneCountInString(content) <= threshold {
		return content
	}
	saved := pageable(content)
	head, more := Preview(saved, preview)
	if path := writeSpill(env.Config, saved, spillFile(id)); path != "" {
		shown := path
		if env.Visible != nil {
			shown = env.Visible(path)
		}
		suffix := ""
		if shown != path {
			suffix = " [host: " + path + "]"
		}
		slog.Info(fmt.Sprintf("Persisted large tool result: %s (%s, %d chars -> %s%s)", tool, id, utf8.RuneCountInString(saved), shown, suffix))
		return spilledBlock(head, more, utf8.RuneCountInString(saved), shown)
	}
	slog.Info(fmt.Sprintf("Inline-truncating large tool result: %s (%d chars, no sandbox write)", tool, utf8.RuneCountInString(content)))
	return fmt.Sprintf("%s\n\n[Truncated: tool response was %s chars. Full output could not be saved to sandbox.]", head, strs.Thousands(utf8.RuneCountInString(content)))
}

// Deletes the profile's saved results older than the retention, at least a day, that no transcript points at any more; how many went.
// Referenced tells whether a transcript names a file, and an error keeps it
func Sweep(cfg *config.Config, retention time.Duration, referenced func(name string) (bool, error)) int {
	entries, err := os.ReadDir(spillFolder(cfg))
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-max(retention, spillAgeFloor))
	removed := 0
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		if kept, err := referenced(entry.Name()); err != nil || kept {
			continue
		}
		if os.Remove(filepath.Join(spillFolder(cfg), entry.Name())) == nil {
			removed++
		}
	}
	if removed > 0 {
		slog.Debug(fmt.Sprintf("Pruned %d expired spillover file(s)", removed))
	}
	return removed
}
