package state

import (
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Words that keep a session listing a plain listing
var sessionListingListWords = map[string]bool{"list": true, "ls": true, "browse": true}

// Words that turn the rest of a session listing into a search query
var sessionListingSearchWords = map[string]bool{"search": true, "find": true}

// Sessions style arguments as all sources, unnamed kept, resume target and search query, nil when no search was asked
func ParseSessionListingArgs(rawArgs string) (includeAll, includeUnnamed bool, target string, searchQuery *string, err error) {
	parts, err := pyshlex.SplitPOSIX(rawArgs, false)
	if err != nil {
		return false, false, "", nil, err
	}
	var targetParts []string
	for i, part := range parts {
		lower := pystr.Lower(pystr.Strip(part))
		if len(targetParts) == 0 {
			if sessionListingListWords[lower] {
				continue
			}
			switch lower {
			case "all", "--all":
				includeAll = true
				continue
			case "full", "--full":
				includeUnnamed = true
				continue
			}
			if sessionListingSearchWords[lower] {
				query := pystr.Strip(strings.Join(parts[i+1:], " "))
				return includeAll, includeUnnamed, "", &query, nil
			}
		}
		targetParts = append(targetParts, part)
	}
	return includeAll, includeUnnamed, pystr.Strip(strings.Join(targetParts, " ")), nil, nil
}

// Compact markdown session list for gateway messengers, the notice above the footer when given
func FormatGatewaySessionListing(rows []*pyjson.Object, includeSource bool, title string, notice *string) string {
	field := func(row *pyjson.Object, key string) string {
		v, _ := row.Get(key)
		if !pyrepr.Truthy(v) {
			return ""
		}
		return pyrepr.Str(v)
	}
	if len(rows) == 0 {
		out := []string{"No sessions found.\nUse `/title My Session` to name this chat, or `/sessions full` to include unnamed sessions."}
		if notice != nil && *notice != "" {
			out = append(out, *notice)
		}
		return strings.Join(out, "\n")
	}
	lines := []string{"📋 **" + title + "**", ""}
	for idx, row := range rows {
		currentPart := ""
		if v, _ := row.Get("is_current_session"); pyrepr.Truthy(v) {
			currentPart = " (current)"
		}
		preview := pystr.Slice(field(row, "preview"), 0, 40)
		source := field(row, "source")
		sourcePart, previewPart := "", ""
		if includeSource && source != "" {
			sourcePart = " `" + source + "`"
		}
		if preview != "" {
			previewPart = " — _" + preview + "_"
		}
		rowTitle := field(row, "title")
		if rowTitle == "" {
			rowTitle = "—"
		}
		lines = append(lines, strconv.Itoa(idx+1)+". **"+rowTitle+"**"+currentPart+sourcePart+" — `"+field(row, "id")+"`"+previewPart)
	}
	lines = append(lines, "")
	if notice != nil && *notice != "" {
		lines = append(lines, *notice)
	}
	lines = append(lines, "Resume: `/resume <session id>` or `/resume <number>` from `/resume`.",
		"More: `/sessions all`, `/sessions full`, `/sessions search <query>`.")
	return strings.Join(lines, "\n")
}
