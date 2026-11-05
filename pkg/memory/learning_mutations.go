package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Journey memory sources and the file each one names
var journeyMemoryFiles = map[string]string{"memory": "MEMORY.md", "profile": "USER.md"}

// Journey memory source to the store target it edits
var journeyStoreTargets = map[string]string{"memory": "memory", "profile": "user"}

// Stale id refusal shown when the entry a node names is gone
const staleMemoryNode = "memory node id is stale \u2014 refresh the graph"

// Memory for memory node ids, skill for everything else
func ParseNodeKind(nodeID string) string {
	if strings.HasPrefix(nodeID, "memory:") {
		return "memory"
	}
	return "skill"
}

func valueError(msg string) error { return &pyError{class: "ValueError", msg: msg} }

// Source, global index, and fingerprint of a memory node id, the fingerprint empty for legacy ids
func parseMemoryID(nodeID string) (string, int, string, error) {
	parts := strings.Split(nodeID, ":")
	bad := valueError(fmt.Sprintf("bad memory node id: %s", pyRepr(nodeID)))
	if (len(parts) != 3 && len(parts) != 4) || parts[0] != "memory" {
		return "", 0, "", bad
	}
	if _, ok := journeyMemoryFiles[parts[1]]; !ok {
		return "", 0, "", bad
	}
	idx, ok := pyIntText(parts[2])
	if !ok {
		return "", 0, "", bad
	}
	fingerprint := ""
	if len(parts) == 4 {
		fingerprint = parts[3]
	}
	return parts[1], idx, fingerprint, nil
}

// Integer of text the way int parses it, Unicode decimal digits folded to ASCII
func pyIntText(s string) (int, bool) {
	var b strings.Builder
	for _, r := range s {
		if r > 0x7f && isUnicodeDecimal(r) {
			b.WriteRune('0' + unicodeDecimalValue(r))
			continue
		}
		b.WriteRune(r)
	}
	return pyInt(b.String())
}

// Card reader legacy ids resolve through, a seam the race tests hold open
var memoryCardsReader = MemoryCards

// Index of the first entry carrying the fingerprint, negative when none does
func resolveFingerprint(chunks []string, fingerprint string) int {
	for i, chunk := range chunks {
		if MemoryFingerprint(chunk) == fingerprint {
			return i
		}
	}
	return -1
}

// Environment rebound to the home the context names so stores, config, and skills follow a profile override
func homeBoundEnv(ctx context.Context, env *tools.Env) *tools.Env {
	h := registryHome(ctx, env)
	out := &tools.Env{}
	if env != nil {
		*out = *env
	}
	if out.Home != h {
		out.Home, out.Config, out.Secrets = h, nil, config.NewSecrets(h)
	}
	return out
}

// File, entries, and local index a memory node id resolves to, by fingerprint or by legacy position
func locateMemory(ctx context.Context, env *tools.Env, nodeID string) (string, []string, int, error) {
	source, gidx, fingerprint, err := parseMemoryID(nodeID)
	if err != nil {
		return "", nil, 0, err
	}
	h := registryHome(ctx, env)
	path := filepath.Join(home.MemoriesDir(h), journeyMemoryFiles[source])
	if _, err := os.Stat(path); err != nil {
		return "", nil, 0, valueError(filepath.Base(path) + " not found")
	}
	chunks := ReadEntriesFile(path)
	if fingerprint != "" {
		local := resolveFingerprint(chunks, fingerprint)
		if local < 0 {
			return "", nil, 0, valueError(staleMemoryNode)
		}
		return path, chunks, local, nil
	}
	cards, err := memoryCardsReader(h)
	if err != nil {
		return "", nil, 0, err
	}
	if gidx < 0 || gidx >= len(cards) {
		return "", nil, 0, &pyError{class: "IndexError", msg: fmt.Sprintf("memory index %d out of range", gidx)}
	}
	if cards[gidx]["source"] != source {
		return "", nil, 0, valueError(staleMemoryNode)
	}
	local := gidx
	if source != "memory" {
		for _, c := range cards {
			if c["source"] == "memory" {
				local--
			}
		}
	}
	if local < 0 || local >= len(chunks) {
		return "", nil, 0, valueError(staleMemoryNode)
	}
	return path, chunks, local, nil
}

// Replaces or with nil removes the entry a node names through the store lock and drift guard
func mutateMemory(ctx context.Context, env *tools.Env, nodeID string, replacement *string) (map[string]any, error) {
	source, _, _, err := parseMemoryID(nodeID)
	if err != nil {
		return nil, err
	}
	name := journeyMemoryFiles[source]
	message := "deleted memory from " + name
	if replacement != nil {
		message = "updated memory in " + name
	}
	store, err := LoadOnDiskStore(homeBoundEnv(ctx, env))
	if err != nil {
		return nil, err
	}
	var applyErr error
	apply := func(entries []string, limit int) Mutation {
		_, chunks, local, err := locateMemory(ctx, env, nodeID)
		if err != nil {
			applyErr = err
			r := NewFailure(err.Error(), "")
			return Mutation{Result: &r}
		}
		text := pyStrip(chunks[local])
		idx := -1
		for i, e := range entries {
			if e == text {
				idx = i
				break
			}
		}
		if idx < 0 {
			r := Result{Data: map[string]any{"success": false, "error": staleMemoryNode}}
			return Mutation{Result: &r}
		}
		next := append([]string{}, entries[:idx]...)
		if replacement != nil {
			next = append(next, *replacement)
		}
		next = append(next, entries[idx+1:]...)
		if replacement != nil {
			if total := runeLen(strings.Join(next, EntryDelimiter)); total > limit {
				r := NewFailure(fmt.Sprintf("Replacement would put memory at %s/%s chars. Shorten the new content.", thousands(total), thousands(limit)), "over_budget")
				return Mutation{Result: &r}
			}
		}
		return Mutation{Entries: next, Message: message}
	}
	result, err := store.Mutate(journeyStoreTargets[source], apply, false)
	if err != nil {
		return nil, err
	}
	if applyErr != nil {
		return nil, applyErr
	}
	if !result.Success() {
		msg, ok := result.Data["error"]
		if !ok {
			msg = name + " write failed"
		}
		return map[string]any{"ok": false, "message": msg}, nil
	}
	return map[string]any{"ok": true, "message": message}, nil
}

// Drops the in process skills prompt cache and its disk snapshot after a skill changed
func clearSkillCache(ctx context.Context, env *tools.Env) {
	skills.ClearSkillsSystemPromptCache(registryHome(ctx, env), true)
}

// Runs the memory or skill handler, value and index errors becoming a failed result
func dispatchNode(nodeID string, memoryFn, skillFn func(string) (map[string]any, error)) (map[string]any, error) {
	fn := skillFn
	if ParseNodeKind(nodeID) == "memory" {
		fn = memoryFn
	}
	out, err := fn(nodeID)
	if err != nil {
		var pe *pyError
		if errors.As(err, &pe) {
			switch pe.class {
			case "ValueError", "IndexError", "UnicodeError", "UnicodeDecodeError", "UnicodeEncodeError":
				return map[string]any{"ok": false, "message": pe.msg}, nil
			}
		}
		return nil, err
	}
	return out, nil
}

// Current content of a node for an edit prefill, the full SKILL.md or the raw memory entry
func NodeDetail(ctx context.Context, env *tools.Env, nodeID string) (map[string]any, error) {
	return dispatchNode(nodeID, func(id string) (map[string]any, error) {
		_, chunks, local, err := locateMemory(ctx, env, id)
		if err != nil {
			return nil, err
		}
		body := pyStrip(chunks[local])
		lines := pystr.Splitlines(body, false)
		if len(lines) == 0 {
			return nil, &pyError{class: "IndexError", msg: "list index out of range"}
		}
		return map[string]any{"ok": true, "kind": "memory", "id": id, "label": runePrefix(lines[0], 80), "content": body}, nil
	}, func(id string) (map[string]any, error) {
		dir, found := skills.ScopeFromEnv(homeBoundEnv(ctx, env)).FindSkill(id)
		if !found {
			return map[string]any{"ok": false, "message": fmt.Sprintf("skill '%s' not found", id)}, nil
		}
		skillMD := filepath.Join(dir, "SKILL.md")
		if _, err := os.Stat(skillMD); err != nil {
			return map[string]any{"ok": false, "message": fmt.Sprintf("SKILL.md missing for '%s'", id)}, nil
		}
		data, err := os.ReadFile(skillMD)
		if err != nil {
			return nil, &pyError{class: "OSError", msg: osErrorText(err)}
		}
		content, err := decodeUTF8Sig(data)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "kind": "skill", "id": id, "label": id, "content": content}, nil
	})
}

// Archives a skill node or removes a memory entry, a pinned skill refused
func DeleteNode(ctx context.Context, env *tools.Env, nodeID string) (map[string]any, error) {
	return dispatchNode(nodeID, func(id string) (map[string]any, error) {
		return mutateMemory(ctx, env, id, nil)
	}, func(name string) (map[string]any, error) {
		bound := homeBoundEnv(ctx, env)
		if pyTruthy(GetSkillRecord(bound.Home, name)["pinned"]) {
			return map[string]any{"ok": false, "message": fmt.Sprintf("'%s' is pinned \u2014 unpin it first (icebath curator unpin %s)", name, name)}, nil
		}
		ok, message := ArchiveSkill(ctx, bound, name)
		if ok {
			clearSkillCache(ctx, env)
			message = fmt.Sprintf("archived '%s' \u2014 restore with: icebath curator restore %s", name, name)
		}
		return map[string]any{"ok": ok, "message": message}, nil
	})
}

// Rewrites a skill SKILL.md or replaces a memory entry, an empty memory body refused
func EditNode(ctx context.Context, env *tools.Env, nodeID, content string) (map[string]any, error) {
	return dispatchNode(nodeID, func(id string) (map[string]any, error) {
		if _, _, _, err := parseMemoryID(id); err != nil {
			return nil, err
		}
		body := pyStrip(content)
		if body == "" {
			return map[string]any{"ok": false, "message": "empty memory \u2014 use delete to remove it"}, nil
		}
		return mutateMemory(ctx, env, id, &body)
	}, func(name string) (map[string]any, error) {
		result := skills.EditSkill(ctx, homeBoundEnv(ctx, env), name, content)
		if pyTruthy(result["success"]) {
			clearSkillCache(ctx, env)
			return map[string]any{"ok": true, "message": fmt.Sprintf("updated '%s'", name)}, nil
		}
		msg, ok := result["error"]
		if !ok {
			msg = "edit failed"
		}
		return map[string]any{"ok": false, "message": msg}, nil
	})
}

// Learning mutations register nothing at import, journey and desktop callers invoke them
func registerLearningMutations() {}
