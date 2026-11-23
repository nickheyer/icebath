package provider

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
)

// auth.json section naming the login ids removed per pool key, so a pool loaded before a removal never writes them back
const poolTombstonesKey = "credential_pool_removed"

// Removed ids kept per pool key, the newest first
const poolTombstoneCap = 256

// Seconds a removed id stays recorded
const poolTombstoneTTL = 180 * 24 * 3600

// Removal time of every recorded id of one pool key
func poolTombstones(data AuthData, providerID string) map[string]float64 {
	out := map[string]float64{}
	stones, _ := data[poolTombstonesKey].(map[string]any)
	ids, _ := stones[providerID].(map[string]any)
	for id, at := range ids {
		when, _ := at.(float64)
		out[id] = when
	}
	return out
}

// Ids of one pool key removed from this store or the global root store
func (s *Store) tombstonedIDs(data AuthData, providerID string) map[string]bool {
	out := map[string]bool{}
	for id := range poolTombstones(data, providerID) {
		out[id] = true
	}
	if s.globalPath != "" && !samePath(s.globalPath, s.path) {
		for id := range poolTombstones(s.LoadGlobal(), providerID) {
			out[id] = true
		}
	}
	return out
}

// Rows whose id the store recorded as removed dropped, a stale writer's copy of a removed login never written back
func withoutTombstoned(entries []map[string]any, stones map[string]bool) []map[string]any {
	if len(stones) == 0 {
		return entries
	}
	kept := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		if !stones[entryID(e)] {
			kept = append(kept, e)
		}
	}
	return kept
}

// Pool row ids per pool key and the recorded removals of the auth.json a save is about to replace, read without side effects
func storedPoolState(target string) (map[string]map[string]bool, AuthData) {
	raw, err := os.ReadFile(target)
	if err != nil {
		return nil, nil
	}
	var doc map[string]any
	if json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\ufeff")), &doc) != nil || doc == nil {
		return nil, nil
	}
	return poolIDsByProvider(AuthData(doc)), AuthData(doc)
}

// Row ids of every pool key of a document
func poolIDsByProvider(data AuthData) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for provider, rows := range poolMap(data) {
		ids := map[string]bool{}
		for _, row := range poolEntries(rows) {
			if id := entryID(row); id != "" {
				ids[id] = true
			}
		}
		out[provider] = ids
	}
	return out
}

// Records in data every pool id the stored document held and data drops, forgets ids data holds again, and prunes old removals
func recordPoolTombstones(data AuthData, target string) {
	before, stored := storedPoolState(target)
	now := nowEpoch()
	stones := map[string]map[string]float64{}
	merge := func(doc AuthData) {
		section, _ := doc[poolTombstonesKey].(map[string]any)
		for provider := range section {
			for id, at := range poolTombstones(doc, provider) {
				if stones[provider] == nil {
					stones[provider] = map[string]float64{}
				}
				if at > stones[provider][id] {
					stones[provider][id] = at
				}
			}
		}
	}
	merge(stored)
	merge(data)
	after := poolIDsByProvider(data)
	for provider, ids := range before {
		for id := range ids {
			if after[provider][id] {
				continue
			}
			if stones[provider] == nil {
				stones[provider] = map[string]float64{}
			}
			if _, known := stones[provider][id]; !known {
				stones[provider][id] = now
			}
		}
	}
	section := map[string]any{}
	for provider, ids := range stones {
		type stone struct {
			id string
			at float64
		}
		var kept []stone
		for id, at := range ids {
			if after[provider][id] || now-at > poolTombstoneTTL {
				continue
			}
			kept = append(kept, stone{id, at})
		}
		sort.Slice(kept, func(i, j int) bool {
			if kept[i].at != kept[j].at {
				return kept[i].at > kept[j].at
			}
			return kept[i].id < kept[j].id
		})
		if len(kept) > poolTombstoneCap {
			kept = kept[:poolTombstoneCap]
		}
		if len(kept) == 0 {
			continue
		}
		recorded := make(map[string]any, len(kept))
		for _, st := range kept {
			recorded[st.id] = st.at
		}
		section[provider] = recorded
	}
	if len(section) == 0 {
		delete(data, poolTombstonesKey)
		return
	}
	data[poolTombstonesKey] = section
}

// Drops from an in memory pool the rows a write left out because they were removed, so a login removed elsewhere leaves this pool too
func (p *Pool) dropUnwritten(written map[string]map[string]any) {
	missing := false
	for _, e := range p.entries {
		if _, present := written[e.ID]; !present {
			missing = true
			break
		}
	}
	if !missing || p.env.Store == nil {
		return
	}
	data, err := p.env.Store.Load()
	if err != nil {
		return
	}
	stones := p.env.Store.tombstonedIDs(data, p.provider)
	kept := p.entries[:0:0]
	for _, e := range p.entries {
		if _, present := written[e.ID]; present || !stones[e.ID] {
			kept = append(kept, e)
		}
	}
	p.entries = kept
}
