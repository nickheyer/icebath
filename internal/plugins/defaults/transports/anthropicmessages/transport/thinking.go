package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Output ceilings of the model families, the longest name the model holds winning
var outputLimits = map[string]int{"claude-fable": 128_000, "claude-sonnet-5": 128_000, "claude-opus-4-8": 128_000, "claude-opus-4-7": 128_000,
	"claude-opus-4-6": 128_000, "claude-sonnet-4-6": 64_000, "claude-opus-4-5": 64_000, "claude-sonnet-4-5": 64_000, "claude-haiku-4-5": 64_000,
	"claude-opus-4": 32_000, "claude-sonnet-4": 64_000, "claude-3-7-sonnet": 128_000, "claude-3-5-sonnet": 8_192, "claude-3-5-haiku": 8_192,
	"claude-3-opus": 4_096, "claude-3-sonnet": 4_096, "claude-3-haiku": 4_096, "minimax": 131_072, "qwen3": 65_536}

const defaultOutputLimit = 128_000

// Output ceiling of a model, its dots read as dashes
func outputLimit(model string) int {
	m := strings.ReplaceAll(strings.ToLower(model), ".", "-")
	best := ""
	for key := range outputLimits {
		if strings.Contains(m, key) && (len(key) > len(best) || len(key) == len(best) && key < best) {
			best = key
		}
	}
	if best == "" {
		return defaultOutputLimit
	}
	return outputLimits[best]
}

// Routes whose thinking blocks carry signatures the API checks, where a refused signature is remembered for the session
var signedRoutes = []string{"native", "minimax"}

// Fingerprint of a signed thinking block, empty for any other block
func fingerprint(block map[string]any) string {
	kind, _ := block["type"].(string)
	field := "signature"
	switch kind {
	case "thinking":
	case "redacted_thinking":
		field = "data"
	default:
		return ""
	}
	value, _ := block[field].(string)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(kind + "\x00" + value))
	return hex.EncodeToString(sum[:])
}

// Thinking blocks the API refused the signatures of in each session, kept in a file of the session so a later run skips them too
type rejections struct {
	dir string
	mu  sync.Mutex
	// By session
	sessions map[string]map[string]bool
}

func (r *rejections) path(session string) string {
	sum := sha256.Sum256([]byte(session))
	return filepath.Join(r.dir, "anthropic", "rejected-thinking", hex.EncodeToString(sum[:12])+".json")
}

// Fingerprints refused in the session
func (r *rejections) of(session string) map[string]bool {
	if session == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if known, ok := r.sessions[session]; ok {
		return known
	}
	known := map[string]bool{}
	if raw, err := os.ReadFile(r.path(session)); err == nil {
		var list []string
		if json.Unmarshal(raw, &list) == nil {
			for _, fp := range list {
				known[fp] = true
			}
		}
	}
	r.sessions[session] = known
	return known
}

// Remembers the fingerprints as refused in the session
func (r *rejections) add(session string, fingerprints []string) {
	if session == "" || len(fingerprints) == 0 {
		return
	}
	known := r.of(session)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, fp := range fingerprints {
		known[fp] = true
	}
	list := make([]string, 0, len(known))
	for fp := range known {
		list = append(list, fp)
	}
	slices.Sort(list)
	raw, _ := json.Marshal(list)
	if err := fsx.Write(r.path(session), raw, 0o600); err != nil {
		slog.Debug("The refused thinking of a session could not be kept", "error", err)
	}
}

// Thinking blocks of the request's messages, those already refused taken out; the fingerprints of those sent, and how many were taken
func strip(messages []map[string]any, refused map[string]bool, every bool) ([]string, int) {
	var sent []string
	removed := 0
	for _, message := range messages {
		content, ok := message["content"].([]any)
		if !ok || message["role"] != "assistant" {
			continue
		}
		kept := content[:0:0]
		for _, b := range content {
			block, _ := b.(map[string]any)
			if fp := fingerprint(block); fp != "" {
				if every || refused[fp] {
					removed++
					continue
				}
				sent = append(sent, fp)
			}
			kept = append(kept, b)
		}
		if len(kept) == 0 {
			kept = []any{textBlock(emptyText)}
		}
		message["content"] = kept
	}
	return sent, removed
}

// Whether a refusal is the API rejecting a replayed thinking signature
func signatureRefused(failure *provider.APIError) bool {
	if failure == nil || failure.StatusCode != 400 {
		return false
	}
	text := strings.ToLower(failure.Message + " " + fmt.Sprint(failure.Err))
	return strings.Contains(text, "thinking_signature_invalid") || strings.Contains(text, "thinking") && provider.Contains(text, "signature",
		"cannot be modified", "must remain as they were") || strings.Contains(text, "redacted_thinking") && strings.Contains(text, "invalid")
}
