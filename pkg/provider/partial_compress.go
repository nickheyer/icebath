package provider

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	// Recent exchanges preserved verbatim when compress here has no explicit count
	DefaultKeepLast = 2
	// Ceiling a fat fingered keep count clamps to instead of turning the split into a no op
	MaxKeepLast = 100
)

// Partial flag, keep count and focus topic from the text after compress, nil focus for the partial forms
func ParsePartialCompressArgs(rawArgs string) (bool, int, *string) {
	text := pystr.Strip(rawArgs)
	if text == "" {
		return false, DefaultKeepLast, nil
	}
	lowered := pystr.Lower(text)
	if strings.HasPrefix(lowered, "up to here") {
		lowered = lowered[len("up to "):]
		text = text[len("up to "):]
	}
	tokens := pystr.SplitWhitespace(lowered, -1)
	head := ""
	if len(tokens) > 0 {
		head = tokens[0]
	}
	switch {
	case head == "here":
		keep := DefaultKeepLast
		if len(tokens) >= 2 {
			keep = coerceKeep(tokens[1])
		}
		return true, keep, nil
	case (head == "--keep" || head == "-k") && len(tokens) >= 2:
		return true, coerceKeep(tokens[1]), nil
	case strings.HasPrefix(head, "--keep="):
		return true, coerceKeep(strings.SplitN(head, "=", 2)[1]), nil
	}
	if text == "" {
		return false, DefaultKeepLast, nil
	}
	return false, DefaultKeepLast, &text
}

// Remaining args with preview, dry run and aggressive tokens removed anywhere, plus whether each was present
func ExtractCompressFlags(rawArgs string) (string, bool, bool) {
	preview, aggressive := false, false
	kept := []string{}
	for _, tok := range pystr.SplitWhitespace(rawArgs, -1) {
		switch pystr.Lower(tok) {
		case "--preview", "--dry-run", "--dryrun":
			preview = true
		case "--aggressive":
			aggressive = true
		default:
			kept = append(kept, tok)
		}
	}
	return strings.Join(kept, " "), preview, aggressive
}

// Preview report with head, tail and total counts, the effective partial flag and printable lines, history untouched
func SummarizeCompressPreview(history []map[string]any, partial bool, keepLast int, focusTopic *string, approxTokens int) map[string]any {
	total := len(history)
	head, tail := append([]map[string]any{}, history...), []map[string]any{}
	effectivePartial := false
	if partial {
		head, tail = SplitHistoryForPartialCompress(history, keepLast)
		effectivePartial = len(tail) > 0
	}
	lines := []string{
		"Preview — no changes made.",
		fmt.Sprintf("Would compress %d of %d message(s) (~%s tokens currently in context).", len(head), total, pystr.GroupThousands(int64(approxTokens))),
	}
	if effectivePartial {
		lines = append(lines, fmt.Sprintf("Boundary: keeping the last %d exchange(s) (%d message(s)) verbatim.", keepLast, len(tail)))
	} else if partial {
		lines = append(lines, "Boundary: 'here' split would keep everything — falling back to full compression.")
	}
	if focusTopic != nil && *focusTopic != "" {
		lines = append(lines, fmt.Sprintf("Focus topic: \"%s\"", *focusTopic))
	}
	lines = append(lines, "Run the command again without --preview to apply.")
	return map[string]any{
		"head_count": len(head),
		"tail_count": len(tail),
		"total":      total,
		"partial":    effectivePartial,
		"lines":      lines,
	}
}

// Keep count token clamped to one through the ceiling, the default when int would raise
func coerceKeep(value string) int {
	n, ok := pystr.IntText(value)
	if !ok {
		return DefaultKeepLast
	}
	if n.Cmp(big.NewInt(MaxKeepLast)) > 0 {
		return MaxKeepLast
	}
	if n.Cmp(big.NewInt(1)) < 0 {
		return 1
	}
	return int(n.Int64())
}

// Summarized head and verbatim tail of the last keep exchanges, whole history and no tail when the head is empty
func SplitHistoryForPartialCompress(history []map[string]any, keepLast int) ([]map[string]any, []map[string]any) {
	keepLast = max(keepLast, 1)
	if len(history) == 0 {
		return []map[string]any{}, []map[string]any{}
	}
	boundary, seen := 0, 0
	for idx := len(history) - 1; idx >= 0; idx-- {
		if history[idx]["role"] == "user" {
			boundary = idx
			seen++
			if seen >= keepLast {
				break
			}
		}
	}
	if boundary == 0 {
		return append([]map[string]any{}, history...), []map[string]any{}
	}
	return append([]map[string]any{}, history[:boundary]...), append([]map[string]any{}, history[boundary:]...)
}

// Compressed head followed by the verbatim tail, merging or bridging a same role user or assistant seam
func RejoinCompressedHeadAndTail(compressedHead, tail []map[string]any) []map[string]any {
	if len(tail) == 0 {
		return append([]map[string]any{}, compressedHead...)
	}
	if len(compressedHead) == 0 {
		return append([]map[string]any{}, tail...)
	}
	head := append([]map[string]any{}, compressedHead...)
	rest := tail
	last, first := head[len(head)-1], rest[0]
	lastRole, firstRole := last["role"], first["role"]
	if (lastRole == "user" || lastRole == "assistant") && lastRole == firstRole {
		lastContent, lastIsStr := last["content"].(string)
		firstContent, firstIsStr := first["content"].(string)
		if lastIsStr && firstIsStr {
			merged := make(map[string]any, len(last)+1)
			for k, v := range last {
				merged[k] = v
			}
			merged["content"] = lastContent + "\n\n" + firstContent
			head[len(head)-1] = merged
			rest = rest[1:]
		} else {
			bridgeRole := "user"
			if firstRole == "user" {
				bridgeRole = "assistant"
			}
			head = append(head, map[string]any{"role": bridgeRole, "content": ""})
		}
	}
	return append(head, rest...)
}
