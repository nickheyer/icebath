package proc

import (
	"fmt"
	"slices"
	"sort"
)

// One process as the table lists it
type Entry struct {
	PID  int
	PPID int
	// Start stamp matching Identity.Start
	Start  float64
	Status Status
}

// Listed incarnation as a recorded pid and start stamp
func (e Entry) Identity() Identity { return Identity{PID: e.PID, Start: e.Start} }

// Every process listed at one instant, sorted by pid
type Table []Entry

// Every process the operating system lists now
func ReadTable() (Table, error) {
	t, err := readTable()
	if err != nil {
		return nil, err
	}
	slices.SortFunc(t, func(a, b Entry) int { return a.PID - b.PID })
	return t, nil
}

// Entry of pid, false when the table does not list it
func (t Table) Lookup(pid int) (Entry, bool) {
	i := sort.Search(len(t), func(i int) bool { return t[i].PID >= pid })
	if i < len(t) && t[i].PID == pid {
		return t[i], true
	}
	return Entry{}, false
}

// Listed processes keyed by parent pid, each list in ascending pid order
func (t Table) byParent() map[int][]Entry {
	kids := map[int][]Entry{}
	for _, e := range t {
		kids[e.PPID] = append(kids[e.PPID], e)
	}
	return kids
}

// Direct children of root started no earlier than root, a child older than its parent holding a reused pid
func (t Table) Children(root Identity) []Identity {
	var out []Identity
	for _, e := range t.byParent()[root.PID] {
		if e.PID != root.PID && e.Start >= root.Start {
			out = append(out, e.Identity())
		}
	}
	return out
}

// Every process descending from root and started no earlier than it, in depth first discovery order
func (t Table) Descendants(root Identity) []Identity {
	kids := t.byParent()
	seen := map[int]bool{}
	var out []Identity
	stack := []int{root.PID}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		for _, e := range kids[pid] {
			if e.PID == root.PID || e.Start < root.Start {
				continue
			}
			out = append(out, e.Identity())
			stack = append(stack, e.PID)
		}
	}
	return out
}

// Parents of pid nearest first, stopping at the lowest listed pid, a vanished parent or one younger than its child
func (t Table) Ancestors(pid int) ([]Identity, error) {
	cur, ok := t.Lookup(pid)
	if !ok {
		return nil, fmt.Errorf("pid %d: %w", pid, ErrNotFound)
	}
	lowest := t[0].PID
	seen := map[int]bool{pid: true}
	var out []Identity
	for cur.PID != lowest {
		parent, ok := t.Lookup(cur.PPID)
		if !ok || seen[parent.PID] || parent.Start > cur.Start {
			break
		}
		seen[parent.PID] = true
		out = append(out, parent.Identity())
		cur = parent
	}
	return out, nil
}
