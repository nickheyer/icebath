package keys

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Name of the keybind file inside the configuration directory
const FileName = "keybinds.json"

// Map read from the file at path, written there with the defaults when none exists, every rejected entry reported
func Open(path string) (*Map, error) {
	m := Default()
	m.path = path
	if path == "" {
		return m, nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return m, m.Save()
	}
	return m, m.Reload()
}

// Reads the file again over the default keys, keeping the keys in force when the file does not parse
func (m *Map) Reload() error {
	if m.path == "" {
		return nil
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		return err
	}
	m.mark()
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("%s: keys: %w", m.path, err)
	}
	m.defaults()
	err = m.Read(data)
	if outdated(err) && !bytes.Equal(data, m.Text()) {
		if serr := m.Save(); serr != nil {
			return fmt.Errorf("%s: %w", m.path, errors.Join(err, serr))
		}
		if err != nil {
			err = fmt.Errorf("%w, the file was written again without them", err)
		}
	}
	if err != nil {
		return fmt.Errorf("%s: %w", m.path, err)
	}
	return nil
}

// Entry of the file naming a context or action the registry no longer has
type unknownError string

func (e unknownError) Error() string { return string(e) }

// Reports whether the file only lacks new actions or names gone ones, so writing it again from the registry loses no bind
func outdated(err error) bool {
	if err == nil {
		return true
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return false
	}
	for _, e := range joined.Unwrap() {
		if _, ok := e.(unknownError); !ok {
			return false
		}
	}
	return true
}

// Reports whether the file changed on disk since it was last read or written
func (m *Map) Stale() bool {
	if m.path == "" {
		return false
	}
	st, err := os.Stat(m.path)
	return err == nil && !st.ModTime().Equal(m.stamp)
}

// Remembers when the file was last touched so only outside edits read as stale
func (m *Map) mark() {
	if st, err := os.Stat(m.path); err == nil {
		m.stamp = st.ModTime()
	}
}

// File the map is written to
func (m *Map) Path() string { return m.path }

// Applies the file text as one swap so actions that trade keys both bind
func (m *Map) Read(data []byte) error {
	var contexts map[string]json.RawMessage
	if err := json.Unmarshal(data, &contexts); err != nil {
		return fmt.Errorf("keys: %w", err)
	}
	var errs []error
	wanted := map[string][]string{}
	for _, ctx := range slices.Sorted(maps.Keys(contexts)) {
		if ContextDocs[ctx] == "" {
			errs = append(errs, unknownError(fmt.Sprintf("keys: unknown context %q", ctx)))
			continue
		}
		var actions map[string]json.RawMessage
		if err := json.Unmarshal(contexts[ctx], &actions); err != nil {
			errs = append(errs, fmt.Errorf("keys: context %q: %w", ctx, err))
			continue
		}
		for _, id := range slices.Sorted(maps.Keys(actions)) {
			a, ok := m.Action(id)
			switch {
			case !ok:
				errs = append(errs, unknownError(fmt.Sprintf("keys: unknown action %q in %s", id, ctx)))
				continue
			case a.Context != ctx:
				errs = append(errs, fmt.Errorf("keys: %s belongs to %s, not %s", id, a.Context, ctx))
				continue
			}
			chords, err := chordList(actions[id])
			if err == nil {
				chords, err = parseAll(chords)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("keys: %s: %w", id, err))
				continue
			}
			wanted[id] = chords
		}
	}
	previous := map[string][]string{}
	for id := range wanted {
		previous[id] = m.binds[id]
		m.binds[id] = nil
	}
	for _, a := range m.actions {
		chords, ok := wanted[a.ID]
		if !ok {
			continue
		}
		if err := m.Set(a.ID, chords); err != nil {
			errs = append(errs, fmt.Errorf("keys: %s: %w", a.ID, err))
			if m.Set(a.ID, previous[a.ID]) != nil {
				m.binds[a.ID] = nil
			}
		}
	}
	m.byChord = nil
	return errors.Join(errs...)
}

// Canonical forms of chords written by a person, failing on the first that does not parse
func parseAll(chords []string) ([]string, error) {
	out := make([]string, 0, len(chords))
	for _, c := range chords {
		p, err := Parse(c)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Chords of one file value, a string, a list of strings or null for none
func chordList(raw json.RawMessage) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var one string
	if json.Unmarshal(trimmed, &one) == nil {
		if strings.TrimSpace(one) == "" {
			return nil, nil
		}
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(trimmed, &many); err != nil {
		return nil, fmt.Errorf("expected a chord or a list of chords, got %s", trimmed)
	}
	return many, nil
}

// File text, contexts in registry order each holding its actions and their chords
func (m *Map) Text() []byte {
	var sb bytes.Buffer
	sb.WriteString("{\n")
	for ci, ctx := range m.contexts {
		fmt.Fprintf(&sb, "  %s: {\n", quote(ctx))
		var ids []string
		for _, a := range m.actions {
			if a.Context == ctx {
				ids = append(ids, a.ID)
			}
		}
		for i, id := range ids {
			chords := m.binds[id]
			parts := make([]string, 0, len(chords))
			for _, c := range chords {
				parts = append(parts, quote(c))
			}
			sep := ","
			if i == len(ids)-1 {
				sep = ""
			}
			fmt.Fprintf(&sb, "    %s: [%s]%s\n", quote(id), strings.Join(parts, ", "), sep)
		}
		sep := ","
		if ci == len(m.contexts)-1 {
			sep = ""
		}
		fmt.Fprintf(&sb, "  }%s\n", sep)
	}
	sb.WriteString("}\n")
	return sb.Bytes()
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Writes the file
func (m *Map) Save() error {
	if m.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(m.path, m.Text(), 0o644); err != nil {
		return err
	}
	m.mark()
	return nil
}
