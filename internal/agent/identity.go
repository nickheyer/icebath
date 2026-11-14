package agent

import (
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/state"
)

type Identity struct {
	UID          string
	AbsorbedUIDs []string
	ToolCallUIDs state.ToolCallUIDs
	ToolCallUID  string
}

func cloneToolCallUIDs(uids state.ToolCallUIDs) state.ToolCallUIDs {
	if uids == nil {
		return nil
	}
	out := make(state.ToolCallUIDs, len(uids))
	for id, uid := range uids {
		out[id] = state.ToolCallUID{One: uid.One, Many: slices.Clone(uid.Many)}
	}
	return out
}

func (i Identity) Apply(row *state.Message) {
	if i.UID != "" {
		row.UID = i.UID
	}
	if len(i.AbsorbedUIDs) > 0 {
		row.AbsorbedUIDs = slices.Clone(i.AbsorbedUIDs)
	}
	if len(i.ToolCallUIDs) > 0 {
		row.ToolCallUIDs = cloneToolCallUIDs(i.ToolCallUIDs)
	}
	if i.ToolCallUID != "" {
		row.ToolCallUID = i.ToolCallUID
	}
}

func identityOf(row *state.Message) Identity {
	return Identity{
		UID:          row.UID,
		AbsorbedUIDs: slices.Clone(row.AbsorbedUIDs),
		ToolCallUIDs: cloneToolCallUIDs(row.ToolCallUIDs),
		ToolCallUID:  row.ToolCallUID,
	}
}

func CopyIdentity(src, dst *state.Message) {
	identityOf(src).Apply(dst)
}

func (m *Message) Identity(withToolUIDs bool) Identity {
	m.StampUID()
	id := identityOf(&m.Row)
	if !withToolUIDs {
		id.ToolCallUIDs, id.ToolCallUID = nil, ""
	}
	return id
}

func RecordAbsorbed(survivor, dropped *Message, droppedLeads bool) {
	survivorUID, droppedUID := survivor.Row.UID, dropped.Row.UID
	own := state.UIDList(survivor.Row.AbsorbedUIDs)
	theirs := state.UIDList(dropped.Row.AbsorbedUIDs)
	var ordered []string
	if droppedLeads && droppedUID != "" {
		survivor.Row.UID = droppedUID
		ordered = append(ordered, theirs...)
		if survivorUID != "" {
			ordered = append(ordered, survivorUID)
		}
		ordered = append(ordered, own...)
	} else {
		ordered = append(ordered, own...)
		if droppedUID != "" {
			ordered = append(ordered, droppedUID)
		}
		ordered = append(ordered, theirs...)
	}
	var absorbed []string
	for _, uid := range state.UIDList(ordered) {
		if uid != survivor.Row.UID {
			absorbed = append(absorbed, uid)
		}
	}
	if len(absorbed) > 0 {
		survivor.Row.AbsorbedUIDs = absorbed
	}
}

func uidOccurrences(u state.ToolCallUID) []string {
	if u.Many != nil {
		return slices.Clone(u.Many)
	}
	return []string{u.One}
}

func MergeToolCallUIDs(into, extra state.ToolCallUIDs) state.ToolCallUIDs {
	merged := make(state.ToolCallUIDs, len(into)+len(extra))
	maps.Copy(merged, cloneToolCallUIDs(into))
	for _, id := range slices.Sorted(maps.Keys(extra)) {
		uid := extra[id]
		if have, ok := merged[id]; ok {
			merged[id] = state.ToolCallUID{Many: append(uidOccurrences(have), uidOccurrences(uid)...)}
			continue
		}
		merged[id] = state.ToolCallUID{One: uid.One, Many: slices.Clone(uid.Many)}
	}
	return merged
}

func PerOccurrenceToolCallUIDs(uids state.ToolCallUIDs, calls []wire.ToolCall) state.ToolCallUIDs {
	counts := map[string]int{}
	for i := range calls {
		if id := calls[i].EffectiveID(); id != "" {
			counts[id]++
		}
	}
	expanded := cloneToolCallUIDs(uids)
	if expanded == nil {
		expanded = state.ToolCallUIDs{}
	}
	for id, count := range counts {
		uid, ok := uids[id]
		if count > 1 && ok && uid.Many == nil {
			many := make([]string, count)
			for i := range many {
				many[i] = uid.One
			}
			expanded[id] = state.ToolCallUID{Many: many}
		}
	}
	return expanded
}

func IndexToolCallUIDs(index map[string]string, assistant *Message) map[string]bool {
	variants := make([]map[string]bool, len(assistant.ToolCalls))
	named := map[string]bool{}
	for i := range assistant.ToolCalls {
		variants[i] = assistant.ToolCalls[i].IDVariants()
		for v := range variants[i] {
			named[v] = true
		}
	}
	for v := range named {
		delete(index, v)
	}
	uids := assistant.Row.ToolCallUIDs
	if len(uids) == 0 {
		return named
	}
	occurrence := map[string]int{}
	for i := range assistant.ToolCalls {
		id := assistant.ToolCalls[i].EffectiveID()
		entry, ok := uids[id]
		if !ok {
			continue
		}
		uid := entry.One
		if entry.Many != nil {
			nth := occurrence[id]
			occurrence[id] = nth + 1
			uid = ""
			if nth < len(entry.Many) {
				uid = entry.Many[nth]
			}
		}
		if uid == "" {
			continue
		}
		for v := range variants[i] {
			index[v] = uid
		}
	}
	return named
}

func ResolveToolCallUID(index map[string]string, toolCallID string) (string, bool) {
	if len(index) == 0 || toolCallID == "" {
		return "", false
	}
	if uid := index[toolCallID]; uid != "" {
		return uid, true
	}
	for _, variant := range slices.Sorted(maps.Keys(wire.ResultIDVariants(toolCallID))) {
		if uid := index[variant]; uid != "" {
			return uid, true
		}
	}
	return "", false
}

type toolCallOwner struct {
	named map[string]bool
	index map[string]string
}

type ToolCallOwners map[*Message]toolCallOwner

func ToolCallUIDFromHistory(msgs []*Message, toolIndex int, owners ToolCallOwners) (string, bool) {
	if toolIndex < 0 || toolIndex >= len(msgs) || msgs[toolIndex] == nil {
		return "", false
	}
	toolCallID := msgs[toolIndex].ToolCallID
	if toolCallID == "" {
		return "", false
	}
	resultVariants := wire.ResultIDVariants(toolCallID)
	for i := toolIndex - 1; i >= 0; i-- {
		prior := msgs[i]
		if prior == nil {
			continue
		}
		if prior.Role == wire.RoleUser {
			return "", false
		}
		if prior.Role != wire.RoleAssistant {
			continue
		}
		entry, ok := owners[prior]
		if !ok {
			index := map[string]string{}
			entry = toolCallOwner{named: IndexToolCallUIDs(index, prior), index: index}
			owners[prior] = entry
		}
		shared := false
		for v := range resultVariants {
			if entry.named[v] {
				shared = true
				break
			}
		}
		if !shared {
			continue
		}
		return ResolveToolCallUID(entry.index, toolCallID)
	}
	return "", false
}

func DataUseRequirement(msgs []*Message) []string {
	seen := map[string]bool{}
	for _, m := range msgs {
		if m == nil {
			continue
		}
		for _, r := range m.DataUse {
			seen[r] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func (m *Message) StampDataUse(requires []string) {
	if len(requires) == 0 {
		return
	}
	m.DataUse = slices.Clone(requires)
	m.Row.DataUse = slices.Clone(requires)
}

func (m *Message) WithheldPrompt() (string, bool) {
	if m == nil || m.Withheld == "" {
		return "", false
	}
	return m.Withheld, true
}

func bareStringPart(p *wire.Part) bool {
	raw := p.JSON()
	return len(raw) > 0 && raw[0] == '"'
}

func (m *Message) TypedContent(view wire.Message) wire.Message {
	typed, ok := m.WithheldPrompt()
	if !ok {
		return view
	}
	out := view.Clone()
	if out.Parts == nil {
		out.Text = strings.ReplaceAll(out.Text, WithheldPromptNotice, typed)
		return out
	}
	for i := range out.Parts {
		if out.Parts[i].Kind != wire.PartText || bareStringPart(&out.Parts[i]) {
			continue
		}
		out.Parts[i].Text = strings.ReplaceAll(out.Parts[i].Text, WithheldPromptNotice, typed)
	}
	return out
}

func WithoutWithheld(msgs []*Message) []*Message {
	if !slices.ContainsFunc(msgs, func(m *Message) bool { return m != nil && m.Withheld != "" }) {
		return msgs
	}
	out := make([]*Message, len(msgs))
	for i, m := range msgs {
		if m == nil || m.Withheld == "" {
			out[i] = m
			continue
		}
		c := m.Clone()
		c.Withheld = ""
		c.Row.DisplayMetadata = metadataWithout(c.Row.DisplayMetadata, withheldPromptKey)
		out[i] = c
	}
	return out
}

func (m *Message) MarkInflightRestatement() {
	m.InflightRestatement = true
	m.Row.DisplayMetadata = metadataWith(m.Row.DisplayMetadata, inflightRestatementKey, wire.Bool(true))
}

func (m *Message) CompactionScaffolding() bool {
	return m != nil && (m.Row.CompressedSummary || m.InflightRestatement)
}

func (m *Message) CarryCompactionProvenance(dst *Message) {
	if m.Row.CompressedSummary {
		dst.Row.CompressedSummary = true
	}
	if m.InflightRestatement {
		dst.MarkInflightRestatement()
	}
}
