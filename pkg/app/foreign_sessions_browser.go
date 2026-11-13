package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Largest foreign log the desktop browser previews or imports
const foreignMaxLogBytes = 32 * 1024 * 1024

// Foreign sources in their declaration order
var foreignSourceOrder = []string{"claude", "codex"}

// Origin tool name each foreign source records on an imported session
var foreignSourceDBNames = map[string]string{"claude": "claude-code", "codex": "codex-cli"}

var foreignMyRequestRE = pyre.MustCompile(`(?im)^#{1,6}\s*My request:\s*$`)

// One browsable foreign log, its handle the only identifier a client ever sees
type foreignCandidate struct {
	mtime  float64
	handle string
	source string
	path   string
	size   int64
}

// Browser title, the request after a codex attachment header or the title guess, else the source label
func foreignDisplayTitle(parsed foreignParsed, source string) string {
	firstUser := ""
	for _, turn := range parsed.turns {
		if turn.role == "user" {
			firstUser = turn.content
			break
		}
	}
	title := ""
	if m, err := foreignMyRequestRE.FindStringMatch(firstUser); err == nil && m != nil {
		runes := []rune(firstUser)
		after := pystr.Strip(string(runes[m.RuneIndex+m.RuneLength:]))
		if after != "" {
			title = pystr.Splitlines(after, false)[0]
		} else if parsed.titleGuess != nil {
			title = *parsed.titleGuess
		}
	} else if parsed.titleGuess != nil {
		title = *parsed.titleGuess
	}
	if title == "" {
		title = foreignSourceLabels[source]
	}
	return runePrefix(pystr.LstripChars(title, "# "), 180)
}

// Candidate logs across sources newest first, an unknown source a ValueError
func foreignCandidates(source any) ([]foreignCandidate, error) {
	switch source.(type) {
	case []any, map[string]any:
		return nil, &pyrepr.FormatError{Class: "TypeError", Msg: "unhashable type: '" + pyrepr.TypeName(source) + "'"}
	}
	if source != nil {
		name, isText := source.(string)
		if _, known := foreignSources[name]; !isText || !known {
			return nil, &appValueError{"Unknown session source"}
		}
	}
	var rows []foreignCandidate
	for _, name := range foreignSourceOrder {
		if source != nil && source != name {
			continue
		}
		files, err := foreignWalk(name, "")
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			sum := sha256.Sum256([]byte(name + ":" + f.path))
			rows = append(rows, foreignCandidate{f.mtime, hex.EncodeToString(sum[:]), name, f.path, f.size})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].mtime != rows[j].mtime {
			return rows[i].mtime > rows[j].mtime
		}
		return rows[i].handle > rows[j].handle
	})
	return rows, nil
}

// Parsed conversation of a candidate, oversized or empty logs a ValueError
func parseForeignCandidate(c foreignCandidate) (foreignParsed, error) {
	if c.size > foreignMaxLogBytes {
		return foreignParsed{}, &appValueError{"This log exceeds the 32 MB preview and import limit"}
	}
	parsed := foreignParser(c.source)(c.path)
	if len(parsed.turns) == 0 {
		return foreignParsed{}, &appValueError{"This session has no readable conversation messages"}
	}
	return parsed, nil
}

// Candidate and parse of a browser handle, a stale or malformed handle a ValueError
func resolveForeignSession(handle any) (foreignCandidate, foreignParsed, error) {
	text, isText := handle.(string)
	if !isText || len([]rune(text)) != 64 {
		return foreignCandidate{}, foreignParsed{}, &appValueError{"Unknown session. Refresh the list and try again"}
	}
	candidates, err := foreignCandidates(nil)
	if err != nil {
		return foreignCandidate{}, foreignParsed{}, err
	}
	for _, c := range candidates {
		if c.handle == text {
			parsed, err := parseForeignCandidate(c)
			return c, parsed, err
		}
	}
	return foreignCandidate{}, foreignParsed{}, &appValueError{"Session no longer available. Refresh the list and try again"}
}

// Python int of a page bound when isinstance int holds, saturated into the Go range
func foreignPageInt(v any) (int, bool) {
	n, isInt := pyint.Instance(v)
	if !isInt {
		return 0, false
	}
	if !n.IsInt64() {
		if n.Sign() > 0 {
			return math.MaxInt, true
		}
		return math.MinInt, true
	}
	return int(n.Int64()), true
}

// One page of browsable foreign sessions of this backend, parsing only the rows of the page
func ListForeignSessions(source, offset, limit any) (map[string]any, error) {
	off, offOK := foreignPageInt(offset)
	lim, limOK := foreignPageInt(limit)
	if !offOK || off < 0 || !limOK || lim < 1 || lim > 50 {
		return nil, &appValueError{"Invalid session page"}
	}
	candidates, err := foreignCandidates(source)
	if err != nil {
		return nil, err
	}
	rows, unreadable := []any{}, 0
	start := min(off, len(candidates))
	end := min(start+lim, len(candidates))
	for _, c := range candidates[start:end] {
		parsed, err := parseForeignCandidate(c)
		if err != nil {
			unreadable++
			continue
		}
		rows = append(rows, map[string]any{"id": c.handle, "source": c.source, "label": foreignSourceLabels[c.source],
			"title": foreignDisplayTitle(parsed, c.source), "cwd": optionalText(parsed.cwd), "mtime": c.mtime,
			"turn_count": len(parsed.turns), "excerpt": runePrefix(parsed.turns[0].content, 200)})
	}
	var nextOffset any
	if off < len(candidates) && off+lim < len(candidates) {
		nextOffset = off + lim
	}
	host, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	return map[string]any{"sessions": rows, "next_offset": nextOffset, "host": host, "unreadable": unreadable}, nil
}

// Nil for an absent string, the text otherwise
func optionalText(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// Provenance an imported foreign session carries
func foreignOrigin(c foreignCandidate, parsed foreignParsed) *pyjson.Object {
	return pyjson.ObjectOf("tool", foreignSourceDBNames[c.source], "path", c.path, "foreign_session_id", optionalText(parsed.sessionID))
}

// Last forty turns of a foreign session cut for the renderer, with whether it was imported already
func PreviewForeignSession(ctx context.Context, handle any, db *state.DB) (map[string]any, error) {
	c, parsed, err := resolveForeignSession(handle)
	if err != nil {
		return nil, err
	}
	existing, err := db.FindForeignImport(ctx, foreignOrigin(c, parsed))
	if err != nil {
		return nil, err
	}
	tail := parsed.turns[max(0, len(parsed.turns)-40):]
	messages, cut := make([]any, 0, len(tail)), false
	for _, turn := range tail {
		if len([]rune(turn.content)) > 8000 {
			cut = true
		}
		messages = append(messages, map[string]any{"role": turn.role, "content": runePrefix(turn.content, 8000)})
	}
	var already any
	if existing != "" {
		already = existing
	}
	return map[string]any{"messages": messages, "total": len(parsed.turns), "truncated": len(parsed.turns) > 40 || cut,
		"already_imported": already, "cwd": optionalText(parsed.cwd)}, nil
}

// Imports a browsed foreign session into the store under its display title and existing working directory
func ImportBrowserSession(ctx context.Context, handle any, db *state.DB, profile string) (*pyjson.Object, error) {
	c, parsed, err := resolveForeignSession(handle)
	if err != nil {
		return nil, err
	}
	turns := make([]any, 0, len(parsed.turns))
	for _, turn := range parsed.turns {
		turns = append(turns, pyjson.ObjectOf("role", turn.role, "content", turn.content))
	}
	var cwd any
	if parsed.cwd != nil && *parsed.cwd != "" {
		if st, err := os.Stat(*parsed.cwd); err == nil && st.IsDir() {
			cwd = *parsed.cwd
		}
	}
	return db.ImportForeignHistory(ctx, foreignOrigin(c, parsed), turns, foreignDisplayTitle(parsed, c.source), cwd, profile)
}
