package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strconv"
	"strings"
)

// Prefix of the tool call ids one provider family mints and later rejects when replayed as a parallel batch
const providerToolIDPrefix = "chatcmpl-tool-"

// Call id derived from the call itself for a provider that sent none, stable across replays
func DeterministicCallID(name, arguments string, index int) string {
	sum := sha256.Sum256([]byte(name + ":" + arguments + ":" + strconv.Itoa(index)))
	return "call_" + hex.EncodeToString(sum[:])[:12]
}

// Every spelling of the given identifiers, each whole and split at the bar that joins call and item ids
func idVariants(values ...string) map[string]bool {
	variants := map[string]bool{}
	for _, raw := range values {
		value := trimSpace(raw)
		if value == "" {
			continue
		}
		variants[value] = true
		for _, part := range strings.Split(value, "|") {
			if part = trimSpace(part); part != "" {
				variants[part] = true
			}
		}
	}
	return variants
}

// Every identifier a tool result may use to name this call
func (c *ToolCall) IDVariants() map[string]bool {
	return idVariants(c.CallID, c.ID, c.ResponseItemID)
}

// Every identifier a tool result's tool_call_id may match a call under
func ResultIDVariants(toolCallID string) map[string]bool {
	return idVariants(toolCallID)
}

// Call id results pair with, the call_id before the id and only the call half of a joined id
func (c *ToolCall) EffectiveID() string {
	for _, raw := range [...]string{c.CallID, c.ID} {
		value := trimSpace(raw)
		if value == "" {
			continue
		}
		head, _, _ := strings.Cut(value, "|")
		if head = trimSpace(head); head != "" {
			return head
		}
		return value
	}
	return ""
}

// Renames calls whose id another call of the turn or the session already uses, with a numbered suffix
func UniquifyToolCallIDs(calls []ToolCall, taken []string) {
	seen, tried := make(map[string]bool, len(taken)+len(calls)), map[string]int{}
	for _, id := range taken {
		seen[id] = true
	}
	for i := range calls {
		c := &calls[i]
		raw := c.CallID
		if raw == "" {
			raw = c.ID
		}
		id, _, _ := strings.Cut(trimSpace(raw), "|")
		if id == "" {
			continue
		}
		if !seen[id] {
			seen[id] = true
			continue
		}
		n := max(tried[id], 2)
		for seen[id+"_d"+strconv.Itoa(n)] {
			n++
		}
		renamed := id + "_d" + strconv.Itoa(n)
		tried[id] = n + 1
		seen[renamed] = true
		if _, item, joined := strings.Cut(c.ID, "|"); joined {
			c.ID = renamed + "|" + item
		} else {
			c.ID = renamed
		}
		if c.CallID != "" {
			c.CallID = renamed
		}
		name := c.Name
		if name == "" {
			name = "?"
		}
		slog.Warn("model reused a tool call id, renamed the duplicate to keep call and result pairing", "id", id, "renamed", renamed, "tool", name)
	}
}

// Rewrites provider-minted ids of a parallel batch to digests the provider accepts on replay
func NormalizeProviderToolCallIDs(calls []ToolCall) {
	if len(calls) < 2 {
		return
	}
	for i := range calls {
		if !strings.HasPrefix(calls[i].EffectiveID(), providerToolIDPrefix) {
			return
		}
	}
	slog.Warn("normalized provider-minted parallel tool call ids for replay compatibility")
	for i := range calls {
		for _, id := range [...]*string{&calls[i].ID, &calls[i].CallID} {
			head, item, joined := strings.Cut(trimSpace(*id), "|")
			if head = trimSpace(head); !strings.HasPrefix(head, providerToolIDPrefix) {
				continue
			}
			sum := sha256.Sum256([]byte(head))
			*id = "call_" + hex.EncodeToString(sum[:])[:12]
			if joined {
				*id += "|" + item
			}
		}
	}
}

// Tool call whose arguments are a provider's JSON input, an object re-encoded in the spaced ASCII layout history stores
func BuildToolCall(id, name string, arguments []byte) ToolCall {
	arguments = trimJSON(arguments)
	text := string(arguments)
	if s, ok := stringValue(arguments); ok {
		text = s
	} else if formatted, ok := FormatJSON(arguments, TextLayout{Spaced: true}); ok {
		text = formatted
	}
	return ToolCall{ID: id, Name: name, Arguments: text}
}
