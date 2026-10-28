package wm

import (
	"encoding/json"
	"slices"

	tea "charm.land/bubbletea/v2"
)

// What the manager keeps between runs, the defaults new workspaces take and every open workspace as it stands
type Layout struct {
	Theme  string   `json:"theme,omitempty"`
	Motion *float64 `json:"motion,omitempty"`
	Ratio  float64  `json:"ratio,omitempty"`
	Mode   string   `json:"mode,omitempty"`
	Main   int      `json:"main,omitempty"`
	// Index of the workspace on the stage
	Active int `json:"active,omitempty"`
	// Command ids run lately, newest first
	Recent     []string          `json:"recent,omitempty"`
	Workspaces []WorkspaceLayout `json:"workspaces,omitempty"`
}

// One workspace as it stands, its tiles in stage order
type WorkspaceLayout struct {
	ID     string        `json:"id"`
	Mode   string        `json:"mode,omitempty"`
	Ratio  float64       `json:"ratio,omitempty"`
	Main   int           `json:"main,omitempty"`
	Tiles  []TileLayout  `json:"tiles,omitempty"`
	Floats []FloatLayout `json:"floats,omitempty"`
	Tree   *SplitLayout  `json:"tree,omitempty"`
	Focus  string        `json:"focus,omitempty"`
	Zoom   string        `json:"zoom,omitempty"`
	// State the cards keep between runs, by key
	Cards map[string]json.RawMessage `json:"cards,omitempty"`
}

// Tile by key with its share of the stack it sits in
type TileLayout struct {
	Key    string  `json:"key"`
	Weight float64 `json:"weight,omitempty"`
}

// Float by key with the box it rests in
type FloatLayout struct {
	Key string `json:"key"`
	Box Box    `json:"box"`
}

// Node of the dwindle tree, a tile key at a leaf or a cut between two subtrees
type SplitLayout struct {
	Key      string       `json:"key,omitempty"`
	Vertical bool         `json:"vertical,omitempty"`
	Ratio    float64      `json:"ratio,omitempty"`
	A        *SplitLayout `json:"a,omitempty"`
	B        *SplitLayout `json:"b,omitempty"`
}

// Layout as it stands, the defaults following the workspace on the stage
func (m *Model) Layout() Layout {
	motion := m.motion
	l := Layout{Theme: m.theme.Name, Motion: &motion, Ratio: m.Ratio(), Mode: m.mode.String(), Main: m.main, Active: m.active, Recent: slices.Clone(m.recent)}
	if ws := m.Active(); ws != nil {
		l.Mode = ws.mode.String()
		l.Main = ws.MainCount()
	}
	for _, ws := range m.spaces {
		l.Workspaces = append(l.Workspaces, m.layoutOf(ws))
	}
	return l
}

// Workspace as it stands, cards on their way out left off
func (m *Model) layoutOf(ws *Workspace) WorkspaceLayout {
	l := WorkspaceLayout{ID: ws.ID, Mode: ws.mode.String(), Ratio: ws.ratio, Main: ws.MainCount()}
	for _, e := range ws.tiles {
		if !e.closing {
			l.Tiles = append(l.Tiles, TileLayout{Key: e.key(), Weight: e.weight})
		}
	}
	for _, e := range ws.floats {
		if !e.closing {
			l.Floats = append(l.Floats, FloatLayout{Key: e.key(), Box: e.fbox})
		}
	}
	if ws.mode == Dwindle {
		l.Tree = splitOf(ws.tree)
	}
	if ws.focus != nil && !ws.focus.closing {
		l.Focus = ws.focus.key()
	}
	if ws.zoom != nil && !ws.zoom.closing {
		l.Zoom = ws.zoom.key()
	}
	for _, e := range ws.entries() {
		s, ok := e.card.(Stateful)
		if !ok || e.closing {
			continue
		}
		raw, err := json.Marshal(s.State())
		if err != nil {
			continue
		}
		if l.Cards == nil {
			l.Cards = map[string]json.RawMessage{}
		}
		l.Cards[e.key()] = raw
	}
	return l
}

// Workspace with every card key passed through rename, keys it returns empty for left out along with their tree leaves,
// focus, zoom and saved state
func (l WorkspaceLayout) Rekeyed(id string, rename func(key string) string) WorkspaceLayout {
	out := WorkspaceLayout{ID: id, Mode: l.Mode, Ratio: l.Ratio, Main: l.Main}
	seen := map[string]bool{}
	for _, t := range l.Tiles {
		if key := rename(t.Key); key != "" && !seen[key] {
			seen[key] = true
			out.Tiles = append(out.Tiles, TileLayout{Key: key, Weight: t.Weight})
		}
	}
	for _, f := range l.Floats {
		if key := rename(f.Key); key != "" && !seen[key] {
			seen[key] = true
			out.Floats = append(out.Floats, FloatLayout{Key: key, Box: f.Box})
		}
	}
	out.Tree = l.Tree.rekeyed(rename)
	if l.Focus != "" {
		out.Focus = rename(l.Focus)
	}
	if l.Zoom != "" {
		out.Zoom = rename(l.Zoom)
	}
	for key, raw := range l.Cards {
		renamed := rename(key)
		if renamed == "" {
			continue
		}
		if out.Cards == nil {
			out.Cards = map[string]json.RawMessage{}
		}
		out.Cards[renamed] = raw
	}
	return out
}

// Tree with every leaf key passed through rename, leaves it returns empty for dropped and their cuts collapsed
func (n *SplitLayout) rekeyed(rename func(key string) string) *SplitLayout {
	if n == nil {
		return nil
	}
	if n.A == nil && n.B == nil {
		key := rename(n.Key)
		if key == "" {
			return nil
		}
		return &SplitLayout{Key: key}
	}
	a, b := n.A.rekeyed(rename), n.B.rekeyed(rename)
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return &SplitLayout{Vertical: n.Vertical, Ratio: n.Ratio, A: a, B: b}
}

// Saved form of a split tree, nil for an empty one
func splitOf(n *node) *SplitLayout {
	if n == nil {
		return nil
	}
	if n.leaf != nil {
		return &SplitLayout{Key: n.leaf.key()}
	}
	return &SplitLayout{Vertical: n.vertical, Ratio: n.ratio, A: splitOf(n.a), B: splitOf(n.b)}
}

// Tree over the tiles by key, a cut missing one side collapsing to the other, repeats kept once
func treeOf(l *SplitLayout, tiles map[string]*entry, seen map[*entry]bool) *node {
	if l == nil {
		return nil
	}
	if l.A == nil && l.B == nil {
		e := tiles[l.Key]
		if e == nil || seen[e] {
			return nil
		}
		seen[e] = true
		return &node{leaf: e, ratio: 0.5}
	}
	a, b := treeOf(l.A, tiles, seen), treeOf(l.B, tiles, seen)
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	n := &node{a: a, b: b, vertical: l.Vertical, ratio: clampSplit(l.Ratio)}
	a.parent, b.parent = n, n
	return n
}

// Applies the defaults, motion and recent commands of a saved layout where options did not fix them
func (m *Model) Restore(l Layout) {
	if l.Ratio > 0 && !m.ratioSet {
		m.ratio = clampRatio(l.Ratio)
		for _, ws := range m.spaces {
			ws.ratio = m.ratio
		}
	}
	if mode, ok := ParseMode(l.Mode); ok && !m.modeSet {
		m.mode = mode
		for _, ws := range m.spaces {
			ws.mode = mode
		}
	}
	if l.Main > 0 {
		m.main = l.Main
		for _, ws := range m.spaces {
			ws.main = l.Main
		}
	}
	for _, ws := range m.spaces {
		if ws.mode == Dwindle && ws.tree == nil {
			ws.rebuildTree(m.stage(), m.gap)
		}
		m.retarget(ws)
	}
	if l.Motion != nil && !m.motionSet {
		m.SetMotion(*l.Motion)
	}
	m.recent = slices.Clone(l.Recent)
	if len(m.recent) > maxRecent {
		m.recent = m.recent[:maxRecent]
	}
}

// Opens the cards of a saved workspace through resolve and lays them out as saved, skipping unresolved keys
func (m *Model) RestoreWorkspace(ws *Workspace, l WorkspaceLayout, resolve func(key string) Card) tea.Cmd {
	if ws == nil || !slices.Contains(m.spaces, ws) {
		return nil
	}
	if mode, ok := ParseMode(l.Mode); ok && !m.modeSet {
		ws.mode = mode
	}
	if l.Ratio > 0 && !m.ratioSet {
		ws.ratio = clampRatio(l.Ratio)
	}
	if l.Main > 0 {
		ws.main = l.Main
	}
	var cmds []tea.Cmd
	have := map[string]*entry{}
	for _, e := range ws.entries() {
		have[e.key()] = e
	}
	place := func(key string) *entry {
		if e, ok := have[key]; ok {
			return e
		}
		if key == "" || m.Has(key) {
			return nil
		}
		c := resolve(key)
		if c == nil || keyOf(c) != key {
			return nil
		}
		e := m.newEntry(c, ws)
		have[key] = e
		cmds = append(cmds, c.Init())
		return e
	}
	var tiles, floats []*entry
	for _, t := range l.Tiles {
		e := place(t.Key)
		if e == nil {
			continue
		}
		if t.Weight > 0 {
			e.weight = t.Weight
		}
		e.floating = false
		if !slices.Contains(tiles, e) {
			tiles = append(tiles, e)
		}
	}
	for _, f := range l.Floats {
		e := place(f.Key)
		if e == nil || slices.Contains(floats, e) {
			continue
		}
		if i := slices.Index(tiles, e); i >= 0 {
			tiles = slices.Delete(tiles, i, i+1)
		}
		e.floating = true
		e.fbox = f.Box
		floats = append(floats, e)
	}
	for _, e := range ws.entries() {
		if !slices.Contains(tiles, e) && !slices.Contains(floats, e) {
			e.floating = false
			tiles = append([]*entry{e}, tiles...)
		}
	}
	ws.tiles, ws.floats, ws.tree, ws.zoom = tiles, floats, nil, nil
	if ws.mode == Dwindle {
		byKey := map[string]*entry{}
		for _, e := range tiles {
			byKey[e.key()] = e
		}
		ws.tree = treeOf(l.Tree, byKey, map[*entry]bool{})
		for _, e := range tiles {
			if ws.tree.find(e) == nil {
				ws.insertLeaf(e, nil)
			}
		}
		ws.syncTiles()
	}
	if e, ok := have[l.Zoom]; ok && !e.floating {
		ws.zoom = e
	}
	for key, raw := range l.Cards {
		e, ok := have[key]
		if !ok {
			continue
		}
		if s, ok := e.card.(Stateful); ok {
			if err := s.SetState(raw); err != nil {
				cmds = append(cmds, m.Toast(key+": "+err.Error(), Warn, 0))
			}
		}
	}
	next := have[l.Focus]
	if next == nil {
		next = ws.focus
	}
	if next == nil && len(ws.tiles) > 0 {
		next = ws.tiles[0]
	}
	if next != ws.focus {
		if ws == m.Active() && ws.focus != nil {
			cmds = append(cmds, m.send(ws.focus, FocusMsg{}))
		}
		ws.focus = next
		if ws == m.Active() && next != nil {
			cmds = append(cmds, m.send(next, FocusMsg{Focused: true}))
		}
	}
	if next != nil && next.floating {
		ws.raise(next)
	}
	m.retarget(ws)
	return tea.Batch(append(cmds, m.wake())...)
}
