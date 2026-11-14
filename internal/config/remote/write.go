package remote

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
)

var valueRefusals = map[string]int{
	"config_secret_literal": 400, "config_value_invalid": 400, "config_path_invalid": 400,
	"config_path_reserved": 400, "config_level_too_large": 413, "config_body_too_large": 413,
}

func rejected(message, code string) error {
	return &config.RejectedValueError{Message: message, Code: code}
}

func lockedError(path []string, level string) error {
	p := encodePath(path)
	return &config.LockedError{Path: p, LockedBy: level, Message: fmt.Sprintf("%s is locked by the %s level of Remote Config", p, level)}
}

func diffBase(st *profileState) *yaml.Node {
	src := st.base
	if src == nil {
		src = st.server
	}
	base := clone(src)
	if base == nil {
		base = newMap()
	}
	dropChild(base, versionKey)
	return base
}

func (b *Backend) WriteChanges(ctx context.Context, h string, c config.Changes) error {
	st, err := b.state(ctx, h)
	if err != nil {
		return err
	}
	token, err := b.bearer(ctx, st.home)
	if err != nil {
		return &config.WriteError{Message: "Remote Config write failed: " + err.Error(), Code: "config_plane_unreachable"}
	}
	var docWire *yaml.Node
	if c.Document != nil {
		if docWire, err = config.ToWire(c.Document.Node(), nil, encodePath); err != nil {
			return err
		}
		if !isMap(docWire) {
			return rejected("the config document must be a mapping", "config_value_invalid")
		}
	}
	setWires := make([]change, 0, len(c.Set))
	for _, kv := range c.Set {
		path := fromDotted(kv.Path)
		n, err := config.ValueNode(kv.Value)
		if err != nil {
			return err
		}
		w, err := config.ToWire(n, path, encodePath)
		if err != nil {
			return err
		}
		setWires = append(setWires, change{path: path, value: w})
	}
	var notes []note
	var notices []string
	err = b.sendChanges(ctx, st, c, docWire, setWires, token, &notes, &notices)
	logNotes(notes)
	for _, n := range notices {
		b.notify(ctx, n)
	}
	return err
}

func (b *Backend) sendChanges(ctx context.Context, st *profileState, c config.Changes, docWire *yaml.Node, setWires []change,
	token string, notes *[]note, notices *[]string) error {
	var in *intent
	for attempt := 1; attempt <= 2; attempt++ {
		conflicted := false
		for range maxPrepare {
			if err := b.completed(ctx, st); err != nil {
				return err
			}
			st.mu.Lock()
			if !st.postprocessed && privateFor(ctx, st) == nil {
				st.mu.Unlock()
				continue
			}
			seen := st.installed
			if in == nil {
				var err error
				if in, err = intentOf(ctx, st, c, docWire, setWires, notices); err != nil {
					st.mu.Unlock()
					return err
				}
			}
			body, sets, unsets, ok, err := patchBody(st, in)
			if err != nil || !ok {
				st.mu.Unlock()
				return err
			}
			check := migrationCheckInput(st, stampsWriter(st), sets, unsets)
			st.mu.Unlock()
			if check != nil {
				if err := b.checkMigrationKeeps(ctx, st, sets, check); err != nil {
					return err
				}
			}
			st.mu.Lock()
			if st.installed != seen {
				st.mu.Unlock()
				continue
			}
			resp, err := b.request(ctx, http.MethodPatch, st.home, st.profile, "", body, token)
			if err != nil {
				st.mu.Unlock()
				return &config.WriteError{Message: "Remote Config write failed: " + err.Error(), Code: "config_plane_unreachable"}
			}
			if cfg, ok := effective(resp); resp.status == http.StatusOK && ok {
				*notes = append(*notes, install(st, resp, cfg)...)
				remember(ctx, st)
				*notes = append(*notes, inheritedNotes(st, unsets)...)
				st.mu.Unlock()
				return nil
			}
			st.mu.Unlock()
			if resp.status == http.StatusConflict && attempt == 1 {
				if _, failure := b.fetchInto(ctx, st, false, token, notes); failure != nil {
					return &config.WriteError{Message: "Remote Config write conflict and re-read failed: " + failure.detail,
						Code: "config_version_conflict"}
				}
				conflicted = true
				break
			}
			return writeError(resp)
		}
		if !conflicted {
			return &config.WriteError{Message: "Remote Config write not sent: the profile's config kept changing while the write was prepared",
				Code: "config_version_conflict"}
		}
	}
	return &config.WriteError{Message: "Remote Config write not sent: the profile's config kept changing while the write was prepared",
		Code: "config_version_conflict"}
}

func intentOf(ctx context.Context, st *profileState, c config.Changes, docWire *yaml.Node, setWires []change, notices *[]string) (*intent, error) {
	base := diffBase(st)
	var next *yaml.Node
	if docWire != nil {
		if privateFor(ctx, st) == nil {
			if read := readBaseFor(ctx, st); read != nil {
				base = read
			}
		}
		next = clone(docWire)
	} else {
		next = clone(base)
	}
	for _, sw := range setWires {
		setPath(next, sw.path, clone(sw.value))
	}
	for _, key := range c.Unset {
		popPath(next, fromDotted(key))
	}
	dropChild(next, versionKey)
	if docWire != nil {
		var dropped [][]string
		base, next, dropped = stripLocked(base, next, st.locks)
		for _, prefix := range config.RefusedLockedOmissionPrefixes(ctx) {
			whole := fromDotted(prefix)
			for _, p := range dropped {
				if covers(whole, p) || covers(p, whole) {
					level := "upper"
					for _, l := range st.locks {
						if samePath(l.path, p) {
							level = l.level
							break
						}
					}
					return nil, lockedError(p, level)
				}
			}
		}
		if len(dropped) > 0 {
			names := make([]string, len(dropped))
			for i, p := range dropped {
				names[i] = encodePath(p)
			}
			slices.Sort(names)
			*notices = append(*notices, fmt.Sprintf("Note: %d setting(s) locked by Remote Config were not saved: %s",
				len(dropped), strings.Join(names, ", ")))
		}
	}
	sets, unsets := intentDiff(base, next, nil)
	in := &intent{sets: sets, unsets: unsets}
	var named []string
	for _, kv := range c.Set {
		named = append(named, kv.Path)
	}
	if docWire != nil {
		named = append(named, config.PinnedKeys(ctx)...)
	}
	seen := map[string]bool{}
	for _, key := range named {
		path := fromDotted(key)
		found, wire := getPath(next, path)
		if !found {
			continue
		}
		leaves := []change{{path: path, value: wire}}
		if isMap(wire) && len(wire.Content) > 0 {
			s, _ := intentDiff(newMap(), wire, path)
			leaves = s.list
		}
		for _, leaf := range leaves {
			if !in.sets.has(leaf.path) {
				in.sets.put(leaf.path, leaf.value)
			}
			if k := pathKey(leaf.path); !seen[k] {
				seen[k] = true
				in.pinned = append(in.pinned, leaf.path)
			}
		}
	}
	return in, nil
}

func patchBody(st *profileState, in *intent) ([]byte, changeSet, [][]string, bool, error) {
	base := diffBase(st)
	next := clone(base)
	applyIntent(next, in.sets, in.unsets)
	sets, unsets := diffDocs(base, next, nil)
	for _, p := range in.pinned {
		value := in.sets.get(p)
		if isEmptyMap(value) || sets.has(p) {
			continue
		}
		if _, locked := coveringLock(st.locks, p); locked {
			continue
		}
		if slices.ContainsFunc(sets.list, func(ch change) bool { return covers(ch.path, p) }) {
			continue
		}
		sets.put(p, clone(value))
	}
	if l, refused := writeCheck(sets, unsets, st.locks); refused {
		return nil, sets, unsets, false, lockedError(l.path, l.level)
	}
	for _, ch := range sets.list {
		if bad, ok := secretLiteralPath(ch.path, ch.value); ok {
			return nil, sets, unsets, false, rejected(fmt.Sprintf("%s is a secret-shaped key: Remote Config stores only a ${VAR} "+
				"reference there, never the secret itself. Put the value in .env and set a ${VAR} reference.", encodePath(bad)),
				"config_secret_literal")
		}
	}
	if sets.empty() && len(unsets) == 0 {
		return nil, sets, unsets, false, nil
	}
	var body strings.Builder
	body.WriteString(`{"expectedVersion":` + strconv.FormatInt(st.profileVersion, 10))
	if stampsWriter(st) {
		body.WriteString(`,"writerConfigVersion":` + strconv.Itoa(config.Version))
	}
	if !sets.empty() {
		body.WriteString(`,"set":{`)
		for i, ch := range sets.list {
			if i > 0 {
				body.WriteByte(',')
			}
			writeJSONString(&body, encodePath(ch.path))
			body.WriteByte(':')
			writeJSON(&body, ch.value)
		}
		body.WriteByte('}')
	}
	if len(unsets) > 0 {
		body.WriteString(`,"unset":[`)
		for i, p := range unsets {
			if i > 0 {
				body.WriteByte(',')
			}
			writeJSONString(&body, encodePath(p))
		}
		body.WriteByte(']')
	}
	body.WriteByte('}')
	return []byte(body.String()), sets, unsets, true, nil
}

func stampsWriter(st *profileState) bool {
	w := st.profileWriter
	return w.known && (w.null || w.value == int64(config.Version))
}

func migrationCheckInput(st *profileState, stamped bool, sets changeSet, unsets [][]string) *migrationCheck {
	if sets.empty() {
		return nil
	}
	latest := int64(config.Version)
	stamps := slices.Clone(st.otherWriters)
	switch {
	case stamped:
		stamps = append(stamps, latest)
	case st.profileWriter.known && !st.profileWriter.null:
		stamps = append(stamps, st.profileWriter.value)
	}
	after := latest
	if len(stamps) > 0 {
		after = slices.Min(stamps)
	}
	if after < int64(config.SupportFloorVersion) || after >= latest {
		return nil
	}
	stored := clone(st.server)
	dropChild(stored, versionKey)
	for _, ch := range sets.list {
		setPath(stored, ch.path, clone(ch.value))
	}
	for _, p := range unsets {
		popPath(stored, p)
	}
	return &migrationCheck{stored: stored, after: int(after)}
}

func (b *Backend) checkMigrationKeeps(ctx context.Context, st *profileState, sets changeSet, check *migrationCheck) error {
	migrated, err := b.runPrivate(ctx, st, check.stored, check.after)
	if err != nil {
		return err
	}
	for _, ch := range sets.list {
		found, got := getPath(migrated, ch.path)
		if changed := firstChange(ch.path, ch.value, found, got); changed != nil {
			return rejected(fmt.Sprintf("Remote Config: %s cannot be saved with that value. This profile's remote settings are "+
				"stored at config schema v%d, and every read migrates them to v%d in memory, which rewrites that value "+
				"(migrations are never written back, D12), so it would never be read back. Nothing was sent; choose another "+
				"value, or have the profile level re-saved at the current schema on the config plane.",
				encodePath(changed), check.after, config.Version), "config_migration_conflict")
		}
	}
	return nil
}

func firstChange(path []string, sent *yaml.Node, found bool, got *yaml.Node) []string {
	if !found {
		return path
	}
	if isMap(sent) && isMap(got) {
		for i := 0; i+1 < len(sent.Content); i += 2 {
			key := sent.Content[i].Value
			g, ok := child(got, key)
			if changed := firstChange(extend(path, key), sent.Content[i+1], ok, g); changed != nil {
				return changed
			}
		}
		return nil
	}
	wire, err := config.ToWire(got, path, encodePath)
	if err != nil || !jsonEqual(wire, sent) {
		return path
	}
	return nil
}

func writeError(resp *response) error {
	code := resp.code()
	if resp.status == http.StatusForbidden && code == "config_key_locked" {
		lockedBy := resp.field("lockedBy")
		if lockedBy == "" {
			lockedBy = "upper"
		}
		return &config.LockedError{Path: resp.field("path"), LockedBy: lockedBy,
			Message: "Remote Config refused the write: " + resp.message()}
	}
	if status, ok := valueRefusals[code]; ok && status == resp.status {
		detail := resp.message()
		var extras []string
		for _, name := range []string{"path", "reason"} {
			if v, isText := rawString(resp.body[name]); isText && v != "" && !strings.Contains(detail, v) {
				extras = append(extras, name+" "+v)
			}
		}
		if len(extras) > 0 {
			detail += " (" + strings.Join(extras, ", ") + ")"
		}
		return rejected("Remote Config refused the write: "+detail, code)
	}
	return &config.WriteError{Message: fmt.Sprintf("Remote Config refused the write (%d %s): %s", resp.status, code, resp.message()), Code: code}
}

func inheritedNotes(st *profileState, unsets [][]string) []note {
	var notes []note
	for _, p := range unsets {
		if found, _ := getPath(st.server, p); found {
			notes = append(notes, note{text: fmt.Sprintf("Remote Config: %s is still set by an upper level; removing it from this "+
				"profile cannot remove an inherited value (set it to null for 'no value')", encodePath(p))})
		}
	}
	return notes
}

func (b *Backend) ApplyInMemory(ctx context.Context, h string, doc *config.YAMLDocument) error {
	st, err := b.state(ctx, h)
	if err != nil {
		return err
	}
	root := doc.Node()
	if root == nil {
		root = newMap()
	}
	wire, err := config.ToWire(root, nil, encodePath)
	if err != nil {
		return err
	}
	if !isMap(wire) {
		return fmt.Errorf("%w: Remote Config cannot hold an in memory document that is not a mapping", config.ErrShape)
	}
	if m := privateFor(ctx, st); m != nil {
		m.mu.Lock()
		m.doc = wire
		m.gen++
		m.mu.Unlock()
		return nil
	}
	base := clone(wire)
	dropChild(base, versionKey)
	st.mu.Lock()
	publishLocked(st, wire, base)
	st.mu.Unlock()
	return nil
}
