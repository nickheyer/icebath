package config

import (
	"context"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Top level keys whose children are names the user chooses, so any path below them is accepted
func openDictTopLevelKeys() []string {
	return []string{
		"providers", "credential_pool_strategies", "mcp_servers", "hooks", "quick_commands",
		"personalities", "command_allowlist", "model_catalog", "channel_prompts", "server_actions",
		"secrets", "goals", "loops",
	}
}

// Top level keys whose sections take fields the defaults do not enumerate, platform sections among them
func schemaDefinedDictKeys() []string {
	return []string{
		"discord", "telegram", "slack", "whatsapp", "signal", "mattermost",
		"matrix", "feishu", "wecom", "weixin", "bluebubbles", "qqbot", "yuanbao",
		"email", "sms", "dingtalk", "sessions", "checkpoints", "plugins",
	}
}

// Container whose direct children are platform names, at the top level and under gateway
const platformContainerKey = "platforms"

// Top level keys whose sub keys are accepted without walking the defaults
func openSubkeyTopLevelKeys() []string {
	return slices.Concat(openDictTopLevelKeys(), []string{"custom_providers"}, schemaDefinedDictKeys())
}

// Terminal keys of backends that left the core, each with the variable its plugin falls back to
func leftCoreTerminalKeys() map[string]string {
	return map[string]string{"terminal.singularity_image": "TERMINAL_SINGULARITY_IMAGE"}
}

// Core keys read at runtime that the defaults leave unseeded, each covering every path below it
func coreRuntimeKeys() []string {
	return []string{"computer_use.backend", "dashboard.kanban"}
}

// Preset fields accepted under any preset name of moa.presets
func moaPresetFieldNames() []string {
	return []string{
		"reference_models", "aggregator", "reference_temperature", "aggregator_temperature",
		"reference_timeout", "degraded_reference_policy", "fanout", "enabled",
	}
}

// Reports a split moa.presets.<name>.<field> path whose field a preset or its aggregator slot defines
func isMoAPresetFieldPath(parts []string) bool {
	if (len(parts) != 4 && len(parts) != 5) || parts[0] != "moa" || parts[1] != "presets" || !slices.Contains(moaPresetFieldNames(), parts[3]) {
		return false
	}
	if parts[2] == "" || parts[2] != trimEnvSpace(parts[2]) {
		return false
	}
	return len(parts) == 4 || (parts[3] == "aggregator" && slices.Contains([]string{"provider", "model", "reasoning_effort"}, parts[4]))
}

// Every top level key the validator knows, the defaults, the optional roots and the open sections
func knownTopLevelKeys(defaults *yaml.Node) []string {
	out := mappingKeys(defaults)
	for _, k := range slices.Concat(extraKnownRootKeys(), openSubkeyTopLevelKeys()) {
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// Positions of every element of b, elements above one percent popularity dropped once b holds 200 or more
func runePositions(b []rune) map[rune][]int {
	at := map[rune][]int{}
	for i, r := range b {
		at[r] = append(at[r], i)
	}
	if n := len(b); n >= 200 {
		limit := n/100 + 1
		for r, idxs := range at {
			if len(idxs) > limit {
				delete(at, r)
			}
		}
	}
	return at
}

// Longest run a and b share inside the given ranges, the earliest in a then in b among equals
func longestSharedRun(a, b []rune, at map[rune][]int, alo, ahi, blo, bhi int) (int, int, int) {
	besti, bestj, size := alo, blo, 0
	runs := map[int]int{}
	for i := alo; i < ahi; i++ {
		next := map[int]int{}
		for _, j := range at[a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := runs[j-1] + 1
			next[j] = k
			if k > size {
				besti, bestj, size = i-k+1, j-k+1, k
			}
		}
		runs = next
	}
	for besti > alo && bestj > blo && a[besti-1] == b[bestj-1] {
		besti, bestj, size = besti-1, bestj-1, size+1
	}
	for besti+size < ahi && bestj+size < bhi && a[besti+size] == b[bestj+size] {
		size++
	}
	return besti, bestj, size
}

// Share of two texts their matching runs cover, twice the matched runes over the combined length
func similarity(a, b []rune) float64 {
	total := len(a) + len(b)
	if total == 0 {
		return 1
	}
	at := runePositions(b)
	matched := 0
	stack := [][4]int{{0, len(a), 0, len(b)}}
	for len(stack) > 0 {
		q := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		i, j, k := longestSharedRun(a, b, at, q[0], q[1], q[2], q[3])
		if k == 0 {
			continue
		}
		matched += k
		if q[0] < i && q[2] < j {
			stack = append(stack, [4]int{q[0], i, q[2], j})
		}
		if i+k < q[1] && j+k < q[3] {
			stack = append(stack, [4]int{i + k, q[1], j + k, q[3]})
		}
	}
	return 2 * float64(matched) / float64(total)
}

// Candidate most similar to a mistyped key at or above cutoff, the greatest text among equally similar ones
func closestKey(key string, candidates []string, cutoff float64) (string, bool) {
	word := []rune(key)
	best, bestScore, found := "", 0.0, false
	for _, c := range candidates {
		score := similarity([]rune(c), word)
		if score < cutoff {
			continue
		}
		if !found || score > bestScore || (score == bestScore && c > best) {
			best, bestScore, found = c, score, true
		}
	}
	return best, found
}

// Similarity a suggestion needs before it is offered for a mistyped key
const suggestionCutoff = 0.6

// Reports a key equal to one of paths or lying below one
func coveredBy(key string, paths []string) bool {
	return slices.ContainsFunc(paths, func(p string) bool { return key == p || strings.HasPrefix(key, p+".") })
}

// Reports a dotted path the defaults define, every segment a key of the mapping above it
func inDefaults(defaults *yaml.Node, path string) bool {
	node := defaults
	for _, seg := range strings.Split(path, ".") {
		at := mappingIndex(node, seg)
		if at < 0 {
			return false
		}
		node = node.Content[at]
	}
	return true
}

// Reports a schema refused key the runtime still reads, a preset field, selection key or plugin declared path
func (s *Store) isRuntimeReadKey(ctx context.Context, defaults *yaml.Node, parts []string) (bool, error) {
	if isMoAPresetFieldPath(parts) {
		return true, nil
	}
	dotted := strings.Join(parts, ".")
	if s.know.SelectionKeys == nil {
		return false, &SeamError{Seam: "tool selection keys"}
	}
	if slices.Contains(s.know.SelectionKeys(), dotted) || coveredBy(dotted, coreRuntimeKeys()) {
		return true, nil
	}
	if s.know.PluginConfigKeys == nil {
		return false, &SeamError{Seam: "plugin config keys"}
	}
	declared, err := s.know.PluginConfigKeys(ctx)
	if err != nil {
		return false, err
	}
	declared = slices.DeleteFunc(slices.Clone(declared), func(p string) bool { return inDefaults(defaults, p) })
	return coveredBy(dotted, declared), nil
}

// Verdict on a config key in command line syntax, with the path to suggest when it looks mistyped
type KeyVerdict struct {
	Known      bool
	Suggestion string
}

// Verdict of the schema walk alone on a key in command line syntax
func (s *Store) validateSchemaKey(ctx context.Context, defaults *yaml.Node, key string) (KeyVerdict, error) {
	if key == "" {
		return KeyVerdict{}, nil
	}
	segments := SplitKeyPath(key)
	top := segments[0]
	if _, left := leftCoreTerminalKeys()[key]; strings.HasPrefix(top, "_") || top == platformContainerKey || left {
		return KeyVerdict{Known: true}, nil
	}
	known := knownTopLevelKeys(defaults)
	if !slices.Contains(known, top) {
		suggestion, ok := closestKey(top, known, suggestionCutoff)
		if !ok {
			return KeyVerdict{}, nil
		}
		if rest := strings.Join(segments[1:], "."); rest != "" {
			suggestion += "." + rest
		}
		return KeyVerdict{Suggestion: suggestion}, nil
	}
	if slices.Contains(openSubkeyTopLevelKeys(), top) {
		return KeyVerdict{Known: true}, nil
	}
	node := childOf(defaults, top)
	consumed := []string{top}
	for _, seg := range segments[1:] {
		if seg == platformContainerKey || !isMapping(node) || len(node.Content) == 0 {
			return KeyVerdict{Known: true}, nil
		}
		at := mappingIndex(node, seg)
		if at < 0 {
			rest := strings.Join(segments[len(consumed):], ".")
			head := SplitKeyPath(rest)[0]
			if mappingIndex(defaults, head) >= 0 || slices.Contains(openSubkeyTopLevelKeys(), head) {
				inner, err := s.ValidateKey(ctx, rest)
				if err != nil {
					return KeyVerdict{}, err
				}
				if inner.Known {
					return KeyVerdict{Suggestion: rest}, nil
				}
			}
			if sibling, ok := closestKey(seg, mappingKeys(node), suggestionCutoff); ok {
				return KeyVerdict{Suggestion: strings.Join(append(slices.Clone(consumed), sibling), ".")}, nil
			}
			return KeyVerdict{}, nil
		}
		consumed = append(consumed, seg)
		node = node.Content[at]
	}
	return KeyVerdict{Known: true}, nil
}

// Verdict on a dotted config key, the schema walk first and the runtime read keys for a refused one
func (s *Store) ValidateKey(ctx context.Context, key string) (KeyVerdict, error) {
	defaults, err := s.defaults()
	if err != nil {
		return KeyVerdict{}, err
	}
	verdict, err := s.validateSchemaKey(ctx, defaults, key)
	if err != nil || verdict.Known || key == "" {
		return verdict, err
	}
	runtime, err := s.isRuntimeReadKey(ctx, defaults, SplitKeyPath(key))
	if err != nil {
		return KeyVerdict{}, err
	}
	if runtime {
		return KeyVerdict{Known: true}, nil
	}
	return verdict, nil
}

// Reports a suggestion proving the key only carries an extra prefix before a known path
func (s *Store) isWrongPrefixSuggestion(ctx context.Context, key, suggestion string) (bool, error) {
	if suggestion == "" {
		return false, nil
	}
	keySegs, sugSegs := SplitKeyPath(key), SplitKeyPath(suggestion)
	if len(sugSegs) >= len(keySegs) || !slices.Equal(keySegs[len(keySegs)-len(sugSegs):], sugSegs) {
		return false, nil
	}
	verdict, err := s.ValidateKey(ctx, suggestion)
	return verdict.Known, err
}

// Canonical per platform key path and its note, gateway.platforms folded into platforms, display keys into display.platforms
func redirectPlatformDisplayKey(key string) (string, string) {
	segs := SplitKeyPath(key)
	note := ""
	if len(segs) >= 3 && segs[0] == "gateway" && segs[1] == platformContainerKey {
		segs = segs[1:]
		key = strings.Join(segs, ".")
		note = "  (note: the top-level platforms." + segs[1] + " block outranks gateway.platforms — saved as " + key + ")"
	}
	if len(segs) != 3 || segs[0] != platformContainerKey || !slices.Contains(DisplayOverrideKeys(), segs[2]) {
		return key, note
	}
	canonical := "display.platforms." + segs[1] + "." + segs[2]
	return canonical, "  (note: per-platform display setting — saved as " + canonical + ")"
}

// Spelling under gateway.platforms the person typed, empty when the key names no such path
func legacyGatewayPlatformsKey(requested string) string {
	segs := SplitKeyPath(requested)
	if len(segs) >= 3 && segs[0] == "gateway" && segs[1] == platformContainerKey {
		return strings.Join(segs, ".")
	}
	return ""
}
