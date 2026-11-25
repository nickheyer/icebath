package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

// Usage record keys whose first parseable value dates a skill
var usageTimestampKeys = []string{"last_activity_at", "last_used_at", "last_viewed_at", "last_patched_at", "created_at"}

// One learned or bundled skill as the learning graph sees it
type SkillNode struct {
	Name     string
	Category string
	Source   string
	// Epoch seconds as int64, or an integral float64 beyond the int64 range, nil when unknown
	Timestamp any
	UseCount  int
	State     string
	CreatedBy any
	Pinned    bool
	Related   []string
}

// Skill tree the graph walks, an embedded tree when FS is set and a directory on disk otherwise
type SkillRoot struct {
	Source string
	Dir    string
	FS     fs.FS
}

// Top level frontmatter key or the icebath metadata copy, the older metadata spelling accepted
func fmField(fm map[string]any, key string) any {
	if anyutil.IsTrue(fm[key]) {
		return fm[key]
	}
	meta, ok := fm["metadata"].(map[string]any)
	if !ok {
		return nil
	}
	for _, metaKey := range skills.MetadataKeys {
		if own, ok := meta[metaKey].(map[string]any); ok {
			return own[key]
		}
	}
	return nil
}

// Related skill names declared in frontmatter, a bracketed comma string split like a list
func relatedSkills(fm map[string]any) []string {
	raw := fmField(fm, "related_skills")
	var items []any
	switch v := raw.(type) {
	case string:
		for _, part := range strings.Split(strings.Trim(v, "[]"), ",") {
			items = append(items, part)
		}
	case []any:
		items = v
	default:
		return []string{}
	}
	out := []string{}
	for _, item := range items {
		if s := strings.TrimSpace(anyutil.String(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Whether a rune is a Unicode decimal digit that int and float accept
func isUnicodeDecimal(r rune) bool { return unicode.Is(unicode.Nd, r) }

// Digit value of a Unicode decimal digit, its offset from the start of its contiguous digit run
func unicodeDecimalValue(r rune) rune {
	zero := r
	for unicode.Is(unicode.Nd, zero-1) {
		zero--
	}
	return (r - zero) % 10
}

// Python float literal shape after Unicode digits are folded to ASCII
var floatLiteralRe = regexp.MustCompile(`^[+-]?(?:(?:[0-9](?:_?[0-9])*)(?:\.(?:[0-9](?:_?[0-9])*)?)?|\.[0-9](?:_?[0-9])*)(?:[eE][+-]?[0-9](?:_?[0-9])*)?$`)

// Float value of text the way float parses it, false where float raises
func floatString(text string) (float64, bool) {
	var b strings.Builder
	for _, r := range strings.TrimSpace(text) {
		if r > 0x7f && isUnicodeDecimal(r) {
			b.WriteRune('0' + unicodeDecimalValue(r))
			continue
		}
		b.WriteRune(r)
	}
	s := b.String()
	switch strings.ToLower(strings.TrimLeft(s, "+-")) {
	case "inf", "infinity", "nan":
		if len(s)-len(strings.TrimLeft(s, "+-")) > 1 {
			return 0, false
		}
		lower := strings.ToLower(s)
		switch {
		case strings.HasSuffix(lower, "nan"):
			return math.NaN(), true
		case strings.HasPrefix(s, "-"):
			return math.Inf(-1), true
		default:
			return math.Inf(1), true
		}
	}
	if !floatLiteralRe.MatchString(s) {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

// Integer part of a float as int64, an integral float64 past the int64 range, nil for nan or infinity
func truncTimestamp(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	t := math.Trunc(f)
	if t >= -9.223372036854775808e18 && t < 9.223372036854775808e18 {
		return int64(t)
	}
	return t
}

// Epoch seconds from a number, numeric string, or ISO timestamp, nil otherwise
func toIntTimestamp(value any) any {
	if value == nil {
		return nil
	}
	s := strings.TrimSpace(anyutil.String(value))
	if s == "" {
		return nil
	}
	switch v := value.(type) {
	case bool:
		if v {
			return int64(1)
		}
		return int64(0)
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return truncTimestamp(v)
	case json.Number:
		if n, err := strconv.ParseInt(string(v), 10, 64); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(string(v), 64); err == nil {
			return truncTimestamp(f)
		}
		return nil
	}
	if f, ok := floatString(s); ok {
		if math.IsInf(f, 0) {
			return nil
		}
		if !math.IsNaN(f) {
			return truncTimestamp(f)
		}
	}
	t, err := parseISOTimestamp(s)
	if err != nil {
		return nil
	}
	return isoEpochSeconds(t)
}

// Whole seconds since the epoch of an instant, truncated toward zero like numeric timestamps
func isoEpochSeconds(t time.Time) int64 {
	secs := t.Unix()
	if secs < 0 && t.Nanosecond() > 0 {
		secs++
	}
	return secs
}

// Instant of an ISO 8601 timestamp as pkg/skills reads the usage records it writes, text without a UTC offset read as UTC
func parseISOTimestamp(s string) (time.Time, error) {
	t, err := timeutil.ParseISO(s, time.UTC)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// Python round to n decimals on the exact binary value, halves to even
func roundHalfEven(x float64, n int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	r, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', n, 64), 64)
	return r
}

// Skill names that root lists in walk order with their relative path parts and absolute parts
type skillFile struct {
	rel      []string
	abs      []string
	path     string
	readFile func() ([]byte, error)
	mtime    func() (time.Time, error)
}

// Every SKILL.md under a root, a disk root in the order Path.rglob yields, symlinked directories never descended
func skillFiles(root SkillRoot) ([]skillFile, error) {
	var out []skillFile
	if root.FS != nil {
		_ = fs.WalkDir(root.FS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() != "SKILL.md" {
				return nil
			}
			rel := strings.Split(p, "/")
			out = append(out, skillFile{
				rel:      rel,
				abs:      append([]string{"skills"}, rel...),
				path:     p,
				readFile: func() ([]byte, error) { return fs.ReadFile(root.FS, p) },
				mtime: func() (time.Time, error) {
					st, err := fs.Stat(root.FS, p)
					if err != nil {
						return time.Time{}, err
					}
					return st.ModTime(), nil
				},
			})
			return nil
		})
		return out, nil
	}
	if _, err := os.Stat(root.Dir); err != nil {
		return nil, nil
	}
	base, err := filepath.Abs(root.Dir)
	if err != nil {
		base = root.Dir
	}
	found, err := pathutil.Glob(root.Dir, "**/SKILL.md")
	if err != nil {
		return nil, err
	}
	for _, p := range found {
		rel, err := filepath.Rel(root.Dir, p)
		if err != nil {
			return nil, err
		}
		relParts := strings.Split(filepath.ToSlash(rel), "/")
		abs := strings.Split(filepath.ToSlash(filepath.Join(base, rel)), "/")
		path := p
		out = append(out, skillFile{
			rel:      relParts,
			abs:      abs,
			path:     path,
			readFile: func() ([]byte, error) { return os.ReadFile(path) },
			mtime: func() (time.Time, error) {
				st, err := os.Stat(path)
				if err != nil {
					return time.Time{}, err
				}
				return st.ModTime(), nil
			},
		})
	}
	return out, nil
}

// Text decoded as utf-8-sig, the UnicodeDecodeError read_text raises for invalid bytes
func decodeUTF8Sig(data []byte) (string, error) {
	data = []byte(strings.TrimPrefix(string(data), "\xef\xbb\xbf"))
	if err := utf8DecodeError(data); err != nil {
		return "", err
	}
	return string(data), nil
}

func isContinuationByte(b byte) bool { return b >= 0x80 && b <= 0xBF }

// First UTF-8 decode failure described the way the CPython strict decoder words it, nil for valid input
func utf8DecodeError(data []byte) error {
	n := len(data)
	for i := 0; i < n; {
		ch := data[i]
		if ch < 0x80 {
			i++
			continue
		}
		reason, span := "", 0
		rem := n - i
		switch {
		case ch < 0xC2:
			reason, span = "invalid start byte", 1
		case ch < 0xE0:
			switch {
			case rem < 2:
				reason, span = "unexpected end of data", rem
			case !isContinuationByte(data[i+1]):
				reason, span = "invalid continuation byte", 1
			default:
				i += 2
				continue
			}
		case ch < 0xF0:
			bad1 := func(c2 byte) bool {
				return !isContinuationByte(c2) || (ch == 0xE0 && c2 < 0xA0) || (ch == 0xED && c2 >= 0xA0)
			}
			switch {
			case rem < 2:
				reason, span = "unexpected end of data", rem
			case bad1(data[i+1]):
				reason, span = "invalid continuation byte", 1
			case rem < 3:
				reason, span = "unexpected end of data", rem
			case !isContinuationByte(data[i+2]):
				reason, span = "invalid continuation byte", 2
			default:
				i += 3
				continue
			}
		case ch < 0xF5:
			bad1 := func(c2 byte) bool {
				return !isContinuationByte(c2) || (ch == 0xF0 && c2 < 0x90) || (ch == 0xF4 && c2 >= 0x90)
			}
			switch {
			case rem < 2:
				reason, span = "unexpected end of data", rem
			case bad1(data[i+1]):
				reason, span = "invalid continuation byte", 1
			case rem < 3:
				reason, span = "unexpected end of data", rem
			case !isContinuationByte(data[i+2]):
				reason, span = "invalid continuation byte", 2
			case rem < 4:
				reason, span = "unexpected end of data", rem
			case !isContinuationByte(data[i+3]):
				reason, span = "invalid continuation byte", 3
			default:
				i += 4
				continue
			}
		default:
			reason, span = "invalid start byte", 1
		}
		if span == 1 {
			verb := map[string]string{"invalid start byte": "cannot start a character", "invalid continuation byte": "begins a character that the next byte breaks",
				"unexpected end of data": "begins a character the text ends inside"}[reason]
			return &kindError{class: "UnicodeDecodeError", msg: fmt.Sprintf("not valid UTF-8: byte 0x%02x at position %d %s", ch, i, verb)}
		}
		verb := map[string]string{"invalid continuation byte": "begin a character that the next byte breaks", "unexpected end of data": "begin a character the text ends inside"}[reason]
		return &kindError{class: "UnicodeDecodeError", msg: fmt.Sprintf("not valid UTF-8: bytes at positions %d-%d %s", i, i+span-1, verb)}
	}
	return nil
}

// Modification time as fractional epoch seconds like st_mtime
func mtimeSeconds(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

// Skill nodes from each root in order, the first root to name a skill keeping it
func BuildSkillNodes(roots []SkillRoot, usage map[string]map[string]any, isExternal func(path string) bool) ([]*SkillNode, error) {
	var nodes []*SkillNode
	seen := map[string]bool{}
	for _, root := range roots {
		files, err := skillFiles(root)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			excluded := false
			for _, part := range f.rel {
				if skills.ExcludedSkillDirs[part] {
					excluded = true
					break
				}
			}
			if excluded {
				continue
			}
			data, err := f.readFile()
			if err != nil {
				continue
			}
			text, err := decodeUTF8Sig(data)
			if err != nil {
				return nil, err
			}
			fm, _ := skills.ParseFrontmatter(runePrefix(text, 4000))
			if fm == nil {
				fm = map[string]any{}
			}
			name := anyutil.StringOr(fm["name"], "")
			if !anyutil.IsTrue(fm["name"]) {
				name = f.abs[len(f.abs)-2]
			}
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			rec := usage[name]
			if rec == nil {
				rec = map[string]any{}
			}
			var usageTS any
			for _, key := range usageTimestampKeys {
				if ts := toIntTimestamp(rec[key]); ts != nil {
					usageTS = ts
					break
				}
			}
			source := root.Source
			if root.FS == nil && isExternal != nil && isExternal(f.path) {
				source = "external"
			}
			category := "general"
			if cat := fmField(fm, "category"); anyutil.IsTrue(cat) {
				category = anyutil.String(cat)
			} else if len(f.abs) >= 3 {
				category = f.abs[len(f.abs)-3]
			}
			timestamp := usageTS
			if !anyutil.IsTrue(usageTS) {
				mt, err := f.mtime()
				if err != nil {
					return nil, &kindError{class: "OSError", msg: osErrorText(err)}
				}
				timestamp = toIntTimestamp(mtimeSeconds(mt))
			}
			useCount := 0
			if raw, ok := rec["use_count"]; ok && anyutil.IsTrue(raw) {
				if useCount, err = intOf(raw); err != nil {
					return nil, err
				}
			}
			state := "active"
			if raw, ok := rec["state"]; ok && anyutil.IsTrue(raw) {
				state = anyutil.String(raw)
			}
			seen[name] = true
			nodes = append(nodes, &SkillNode{
				Name: name, Category: category, Source: source, Timestamp: timestamp, UseCount: useCount,
				State: state, CreatedBy: rec["created_by"], Pinned: anyutil.IsTrue(rec["pinned"]), Related: relatedSkills(fm),
			})
		}
	}
	return nodes, nil
}

// Undirected related skill edges whose endpoints both exist, deduped in first seen order
func BuildEdges(nodes []*SkillNode) [][2]string {
	names := map[string]bool{}
	for _, n := range nodes {
		names[n.Name] = true
	}
	var edges [][2]string
	seen := map[[2]string]bool{}
	for _, n := range nodes {
		for _, target := range n.Related {
			if !names[target] || target == n.Name {
				continue
			}
			edge := [2]string{min(n.Name, target), max(n.Name, target)}
			if !seen[edge] {
				seen[edge] = true
				edges = append(edges, edge)
			}
		}
	}
	return edges
}

// Insertion ordered tally like collections Counter
type orderedCounter struct {
	keys   []string
	counts map[string]int
}

func newOrderedCounter() *orderedCounter { return &orderedCounter{counts: map[string]int{}} }

func (c *orderedCounter) add(key string, n int) {
	if _, ok := c.counts[key]; !ok {
		c.keys = append(c.keys, key)
	}
	c.counts[key] += n
}

func (c *orderedCounter) set(key string, n int) {
	if _, ok := c.counts[key]; !ok {
		c.keys = append(c.keys, key)
	}
	c.counts[key] = n
}

// Entries sorted by count descending, ties kept in insertion order
func (c *orderedCounter) mostCommon() []categoryCount {
	out := make([]categoryCount, len(c.keys))
	for i, k := range c.keys {
		out[i] = categoryCount{k, c.counts[k]}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].count > out[j].count })
	return out
}

type categoryCount struct {
	category string
	count    int
}

// Graph density figures over skill nodes and their related edges
func DensityStats(nodes []*SkillNode, edges [][2]string) map[string]any {
	linked := map[string]bool{}
	for _, e := range edges {
		linked[e[0]], linked[e[1]] = true, true
	}
	cats := newOrderedCounter()
	agentCreated, used := 0, 0
	for _, n := range nodes {
		cats.add(n.Category, 1)
		if s, ok := n.CreatedBy.(string); ok && s == "agent" {
			agentCreated++
		}
		if n.UseCount > 0 {
			used++
		}
	}
	n := len(nodes)
	if n == 0 {
		n = 1
	}
	top := [][]any{}
	for i, cc := range cats.mostCommon() {
		if i == 8 {
			break
		}
		top = append(top, []any{cc.category, cc.count})
	}
	return map[string]any{
		"nodes": len(nodes), "related_edges": len(edges), "edges_per_node": roundHalfEven(float64(len(edges))/float64(n), 3),
		"linked_nodes": len(linked), "isolated_pct": roundHalfEven(100*float64(n-len(linked))/float64(n), 1), "categories": len(cats.keys),
		"agent_created": agentCreated, "used": used, "top_categories": top,
	}
}

// Short stable digest of a memory entry text carried in its node id
func MemoryFingerprint(entry string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(entry)))
	return hex.EncodeToString(sum[:])[:12]
}

// Node id naming the memory source, the occurrence index, and the text fingerprint
func MemoryNodeID(card map[string]any, index int) string {
	return fmt.Sprintf("memory:%s:%d:%s", anyutil.String(card["source"]), index, anyutil.String(card["fingerprint"]))
}

// Python str.splitlines boundaries
func splitLines(s string) []string {
	var lines []string
	start := 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', '\u2028', '\u2029':
			lines = append(lines, string(rs[start:i]))
			start = i + 1
		case '\r':
			lines = append(lines, string(rs[start:i]))
			if i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(rs) {
		lines = append(lines, string(rs[start:]))
	}
	return lines
}

// MEMORY.md then USER.md entries of a home as cards, one per entry the store parses
func MemoryCards(h string) ([]map[string]any, error) {
	base := home.MemoriesDir(h)
	cards := []map[string]any{}
	for _, f := range []struct{ name, source string }{{"MEMORY.md", "memory"}, {"USER.md", "profile"}} {
		path := filepath.Join(base, f.name)
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		fileTS := toIntTimestamp(mtimeSeconds(st.ModTime()))
		for idx, chunk := range ReadEntriesFile(path) {
			lines := splitLines(chunk)
			if len(lines) == 0 {
				return nil, &kindError{class: "IndexError", msg: fmt.Sprintf("memory entry %d of %s is empty", idx, path)}
			}
			first := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(lines[0]), "# "))
			title := first
			if utf8.RuneCountInString(first) > 80 {
				title = runePrefix(first, 80) + "\u2026"
			}
			var ts any
			switch v := fileTS.(type) {
			case int64:
				ts = v + int64(idx)
			case float64:
				ts = v + float64(idx)
			}
			cards = append(cards, map[string]any{
				"source": f.source, "timestamp": ts, "title": title, "body": runePrefix(chunk, 1200),
				"fingerprint": MemoryFingerprint(chunk),
			})
		}
	}
	return cards, nil
}

// Lowercase ASCII word tokens of three or more characters
func tokenize(text string) map[string]bool {
	out := map[string]bool{}
	for _, t := range regexp.MustCompile(`[^a-z0-9]+`).Split(strings.ToLower(text), -1) {
		if len(t) >= 3 {
			out[t] = true
		}
	}
	return out
}

// Top four lexically overlapping skills per memory card, a name hit weighing six
func memorySkillEdges(cards []map[string]any, nodes []*SkillNode) [][2]string {
	type meta struct {
		name, lower string
		tokens      map[string]bool
	}
	metas := make([]meta, len(nodes))
	for i, s := range nodes {
		metas[i] = meta{s.Name, strings.ToLower(s.Name), tokenize(s.Name)}
	}
	var edges [][2]string
	for idx, card := range cards {
		text := strings.ToLower(anyutil.StringOr(card["title"], "") + "\n" + anyutil.StringOr(card["body"], ""))
		textTokens := tokenize(text)
		type scored struct {
			score int
			name  string
		}
		var hits []scored
		for _, m := range metas {
			score := 0
			if strings.Contains(text, m.lower) {
				score = 6
			}
			for t := range m.tokens {
				if textTokens[t] {
					score++
				}
			}
			if score > 0 {
				hits = append(hits, scored{score, m.name})
			}
		}
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].score != hits[j].score {
				return hits[i].score > hits[j].score
			}
			return hits[i].name < hits[j].name
		})
		for i, h := range hits {
			if i == 4 {
				break
			}
			edges = append(edges, [2]string{MemoryNodeID(card, idx), h.name})
		}
	}
	return edges
}

// Graph worthy skills, agent created, taught through learn, or used, never an external mount
func hasLearningSignal(n *SkillNode) bool {
	if n.Source == "external" {
		return false
	}
	if s, ok := n.CreatedBy.(string); ok && (s == "agent" || s == "learn") {
		return true
	}
	return n.UseCount > 0
}

// Home and skills scope the learning views read, the context home override first
func learningScope(ctx context.Context, env *tools.Env) (string, *skills.Scope, error) {
	h := registryHome(ctx, env)
	cfg, err := registryConfig(ctx, env)
	if err != nil {
		return "", nil, err
	}
	scope := skills.ScopeFromEnv(env)
	scope.Home, scope.Config, scope.Secrets = h, cfg, config.NewSecrets(h)
	return h, scope, nil
}

// Learning panel payload of non bundled skills with learning signal plus memory entries as nodes
func BuildLearningGraph(ctx context.Context, env *tools.Env) (map[string]any, error) {
	h, scope, err := learningScope(ctx, env)
	if err != nil {
		return nil, err
	}
	roots := []SkillRoot{{Source: "base", FS: skills.BundledFS()}, {Source: "profile", Dir: home.SkillsDir(h)}}
	all, err := BuildSkillNodes(roots, LoadSkillUsage(h), scope.IsExternalSkillPath)
	if err != nil {
		return nil, err
	}
	var learned []*SkillNode
	for _, n := range all {
		if n.Source != "base" && hasLearningSignal(n) {
			learned = append(learned, n)
		}
	}
	skillEdges := BuildEdges(learned)
	cards, err := MemoryCards(h)
	if err != nil {
		return nil, err
	}
	memoryEdges := memorySkillEdges(cards, learned)
	clusters := newOrderedCounter()
	for _, n := range learned {
		clusters.add(n.Category, 1)
	}
	if len(cards) > 0 {
		clusters.set("memory", len(cards))
	}
	graphNodes := []map[string]any{}
	for _, n := range learned {
		graphNodes = append(graphNodes, map[string]any{
			"id": n.Name, "label": n.Name, "kind": "skill", "timestamp": n.Timestamp, "category": n.Category,
			"useCount": n.UseCount, "state": n.State, "createdBy": n.CreatedBy, "pinned": n.Pinned,
		})
	}
	for i, card := range cards {
		graphNodes = append(graphNodes, map[string]any{
			"id": MemoryNodeID(card, i), "label": card["title"], "kind": "memory", "memorySource": card["source"],
			"timestamp": card["timestamp"], "category": "memory", "useCount": 0, "state": "active",
			"createdBy": "memory", "pinned": false,
		})
	}
	edges := []map[string]any{}
	for _, e := range append(append([][2]string{}, skillEdges...), memoryEdges...) {
		edges = append(edges, map[string]any{"source": e[0], "target": e[1]})
	}
	clusterRows := []map[string]any{}
	for _, cc := range clusters.mostCommon() {
		clusterRows = append(clusterRows, map[string]any{"category": cc.category, "count": cc.count})
	}
	stats := DensityStats(learned, skillEdges)
	stats["memory_nodes"] = len(cards)
	stats["memory_skill_edges"] = len(memoryEdges)
	stats["learned_skills"] = len(learned)
	return map[string]any{"nodes": graphNodes, "edges": edges, "clusters": clusterRows, "memory": cards, "stats": stats}, nil
}
