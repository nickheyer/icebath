package wm

// Reports whether the workspace holds no card that is not on its way out
func (w *Workspace) Empty() bool {
	for _, e := range w.entries() {
		if !e.closing {
			return false
		}
	}
	return true
}

// Reports whether the card could be tiled in the workspace with every tile at or above its least size
func (m *Model) Fits(ws *Workspace, c Card) bool {
	if ws == nil || c == nil {
		return false
	}
	if m.width == 0 || m.height == 0 || floats(c) {
		return true
	}
	if have := ws.find(keyOf(c)); have != nil {
		return true
	}
	live := ws.live()
	probe := &entry{card: c, weight: 1}
	boxes := map[*entry]Box{}
	st := m.stage()
	if ws.mode == Dwindle && ws.tree != nil {
		tree := ws.tree.clone(nil)
		at := tree.find(ws.focus)
		if at == nil {
			if leaves := tree.leaves(nil); len(leaves) > 0 {
				at = tree.find(leaves[len(leaves)-1])
			}
		}
		tree = cut(tree, at, probe)
		tree.layout(st, m.gap)
		for _, e := range tree.leaves(nil) {
			boxes[e] = e.box
		}
	} else {
		all := append(append([]*entry(nil), live...), probe)
		weights := make([]float64, 0, len(all))
		for _, e := range all {
			weights = append(weights, e.weight)
		}
		for i, b := range arrange(st.W, st.H, len(all), ws.ratio, weights, m.gap, ws.MainCount()) {
			boxes[all[i]] = b
		}
	}
	for e, b := range boxes {
		w, h := minSize(e.card)
		content := m.contentRect(b)
		if content.W < w || content.H < h {
			return false
		}
	}
	return true
}

// Copy of the subtree under a parent, its leaves pointing at the same tiles with their boxes left untouched
func (n *node) clone(parent *node) *node {
	if n == nil {
		return nil
	}
	out := &node{leaf: n.leaf, parent: parent, vertical: n.vertical, ratio: n.ratio, box: n.box}
	if n.leaf != nil {
		return out
	}
	out.a, out.b = n.a.clone(out), n.b.clone(out)
	return out
}

// Tree with the target leaf cut in two to hold the probe, the probe alone for an empty tree
func cut(tree, target *node, probe *entry) *node {
	nl := &node{leaf: probe, ratio: 0.5}
	if tree == nil || target == nil {
		return nl
	}
	split := &node{a: target, b: nl, parent: target.parent, vertical: cutOf(target.box), ratio: 0.5, box: target.box}
	switch {
	case target.parent == nil:
		tree = split
	case target.parent.a == target:
		target.parent.a = split
	default:
		target.parent.b = split
	}
	target.parent, nl.parent = split, split
	return tree
}
