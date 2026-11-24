package slack

import (
	"strconv"
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

const reactionMarkerMax = 8

var unsafeNameChars = mustPyRe(`[^\w+'\-:]+`)

// Reaction summary marker of a Slack message with names and counts, empty without reactions
func slackReactionsMarker(msg map[string]any) string {
	reactions, ok := msg["reactions"].([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, raw := range reactions {
		reaction, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := ""
		if anyutil.IsTrue(reaction["name"]) {
			name = anyutil.String(reaction["name"])
		}
		name = strings.Trim(replaceAll(unsafeNameChars, name, func(reMatch) string { return "" }), ":")
		if name == "" {
			continue
		}
		count, isInt := anyutil.Int(reaction["count"])
		if isInt && count > 1 {
			parts = append(parts, ":"+name+":\u00d7"+strconv.FormatInt(count, 10))
		} else {
			parts = append(parts, ":"+name+":")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	shown := parts
	if extra := len(parts) - reactionMarkerMax; extra > 0 {
		shown = append(append([]string{}, parts[:reactionMarkerMax]...), "+"+anyutil.String(extra)+" more")
	}
	return "[reactions: " + strings.Join(shown, " ") + "]"
}
