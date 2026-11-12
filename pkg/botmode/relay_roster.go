package botmode

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// RelayDirName is the Desktop relay directory under an install or profile home
const RelayDirName = "bot_relay"

// RosterFile is the Desktop pushed remote roster inside the relay directory
const RosterFile = "roster.json"

var relayHandleRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// RemoteRosterRow is one validated agent on another connected machine
type RemoteRosterRow struct {
	Profile         string
	Handle          string
	ConnectionID    string
	ConnectionLabel string
	Title           string
	Description     string
	Online          *bool
}

// RelayRoot is the relay directory of a home
func RelayRoot(root string) string {
	return filepath.Join(root, RelayDirName)
}

// Text of a value as Python str of value or empty string gives it
func strOr(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// First n code points of text
func headRunes(text string, n int) string {
	runes := pystr.Runes(text)
	if len(runes) <= n {
		return text
	}
	return pystr.FromRunes(runes[:n])
}

// Validated minimal roster row from untrusted Desktop input, false when it is unusable
func normalizeRosterRow(row any) (RemoteRosterRow, bool) {
	obj, ok := row.(*pyjson.Object)
	if !ok {
		fields, isMap := row.(map[string]any)
		if !isMap {
			return RemoteRosterRow{}, false
		}
		obj = pyjson.NewObject()
		for key, value := range fields {
			obj.Set(key, value)
		}
	}
	get := func(key string) any {
		v, _ := obj.Get(key)
		return v
	}
	profile := pystr.Strip(strOr(get("profile")))
	handle := pystr.LstripChars(pystr.Strip(strOr(get("handle"))), "@")
	if handle == "" {
		handle = profile
		if profile == "default" {
			handle = "icebath"
		}
	}
	connectionID := pystr.Strip(strOr(get("connection_id")))
	if profile == "" || connectionID == "" {
		return RemoteRosterRow{}, false
	}
	for _, v := range []string{handle, profile, connectionID} {
		if !relayHandleRe.MatchString(v) {
			return RemoteRosterRow{}, false
		}
	}
	out := RemoteRosterRow{
		Profile:         profile,
		Handle:          handle,
		ConnectionID:    connectionID,
		ConnectionLabel: headRunes(pystr.Strip(strOr(get("connection_label"))), 80),
		Title:           headRunes(pystr.Strip(strOr(get("title"))), 120),
		Description:     headRunes(strings.Join(pystr.SplitWhitespace(strOr(get("description")), -1), " "), 160),
	}
	if online, ok := get("online").(bool); ok {
		out.Online = &online
	}
	return out, true
}

// ReadRemoteRoster is the current remote roster of a home, empty when absent or unreadable
func ReadRemoteRoster(root string) []RemoteRosterRow {
	rows, err := readRemoteRoster(root)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("bot_relay roster read failed", "error", err)
		}
		return nil
	}
	return rows
}

// Roster rows of the relay file, the read or parse error returned
func readRemoteRoster(root string) ([]RemoteRosterRow, error) {
	data, err := os.ReadFile(filepath.Join(RelayRoot(root), RosterFile))
	if err != nil {
		return nil, err
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size <= 1 {
			return nil, pystr.UTF8DecodeError(data, i)
		}
		i += size
	}
	doc, err := pyjson.Loads(string(data))
	if err != nil {
		return nil, err
	}
	obj, ok := doc.(*pyjson.Object)
	if !ok {
		return nil, nil
	}
	agents, _ := obj.Get("agents")
	list, ok := agents.([]any)
	if !ok {
		return nil, nil
	}
	var out []RemoteRosterRow
	for _, raw := range list {
		if row, ok := normalizeRosterRow(raw); ok {
			out = append(out, row)
		}
	}
	return out, nil
}

// Lower cased routing ids of a row, its handle and profile folder id
func targetIDs(row RemoteRosterRow) map[string]bool {
	return map[string]bool{pystr.Lower(row.Handle): true, pystr.Lower(row.Profile): true}
}

// Every lower cased bare form addressing a row, routing ids plus the title mention slugs
func targetAliases(row RemoteRosterRow) map[string]bool {
	out := targetIDs(row)
	for form := range AliasForms(row.Title) {
		out[form] = true
	}
	return out
}

// Slug form of the Bot Mode title, empty when the title is empty, reserved or not a valid handle
func titleSlug(row RemoteRosterRow) string {
	slug := strings.Trim(aliasNonSlugRe.ReplaceAllString(pystr.Lower(pystr.Strip(row.Title)), "-"), "-")
	if AliasForms(row.Title)[slug] {
		return slug
	}
	return ""
}

// RemoteTargetForms gives one unambiguous target per row, bare handle, then title slug, else handle at connection
func RemoteTargetForms(roster []RemoteRosterRow, localTaken map[string]bool) []string {
	taken := map[string]bool{}
	for form := range localTaken {
		taken[pystr.Lower(form)] = true
	}
	idClaims := map[string]int{}
	aliasClaims := map[string]int{}
	for _, row := range roster {
		for form := range targetIDs(row) {
			idClaims[form]++
		}
		for form := range targetAliases(row) {
			aliasClaims[form]++
		}
	}
	out := make([]string, 0, len(roster))
	for _, row := range roster {
		form := fmt.Sprintf("%s@%s", row.Handle, row.ConnectionID)
		for _, c := range []struct {
			candidate string
			claims    map[string]int
		}{{row.Handle, idClaims}, {titleSlug(row), aliasClaims}} {
			lower := pystr.Lower(c.candidate)
			if c.candidate != "" && !taken[lower] && c.claims[lower] == 1 {
				form = c.candidate
				break
			}
		}
		out = append(out, form)
	}
	return out
}
