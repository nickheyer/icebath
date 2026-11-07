package kanban

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Reports whether a directory carries board.json, the identity marker a db-only board directory lacks
func dirHoldsBoard(d string) bool {
	_, err := os.Stat(pypath.PathJoin(d, "board.json"))
	return err == nil
}

// board.json path holding display metadata, the directory slug being the identity
func BoardMetadataPath(board *string) (string, error) {
	d, err := BoardDir(board)
	if err != nil {
		return "", err
	}
	return pypath.PathJoin(d, "board.json"), nil
}

// Title cased words of a slug, the slug itself when it has none
func defaultBoardDisplayName(slug string) string {
	var parts []string
	for _, part := range strings.Split(strings.ReplaceAll(slug, "_", "-"), "-") {
		if part != "" {
			parts = append(parts, pystr.Capitalize(part))
		}
	}
	if len(parts) == 0 {
		return slug
	}
	return strings.Join(parts, " ")
}

// board.json merged over defaults plus slug and db_path, a missing or malformed file yielding the synthesized entry
func ReadBoardMetadata(board *string) (*pyjson.Object, error) {
	slug, err := slugOrDefault(board)
	if err != nil {
		return nil, err
	}
	meta := pyjson.NewObject()
	meta.Set("slug", slug)
	meta.Set("name", defaultBoardDisplayName(slug))
	meta.Set("description", "")
	meta.Set("icon", "")
	meta.Set("color", "")
	meta.Set("default_workdir", nil)
	meta.Set("project_id", nil)
	meta.Set("created_at", nil)
	meta.Set("archived", false)
	p, _ := BoardMetadataPath(&slug)
	if data, err := os.ReadFile(p); err == nil {
		if text, err := pystr.DecodeUTF8Sig(data); err == nil {
			if raw, err := pyjson.Loads(text); err == nil {
				if obj, ok := raw.(*pyjson.Object); ok {
					obj.Set("slug", slug)
					for _, k := range obj.Keys {
						meta.Set(k, obj.Vals[k])
					}
				}
			}
		}
	}
	dbPath, err := KanbanDBPath(context.Background(), &slug)
	if err != nil {
		return nil, err
	}
	meta.Set("db_path", dbPath)
	return meta, nil
}

// Optional board.json fields a write updates, nil leaving a field unchanged
type BoardMetadataUpdate struct {
	Name           *string
	Description    *string
	Icon           *string
	Color          *string
	Archived       *bool
	DefaultWorkdir *string
	ProjectID      *string
}

// Creates or updates board.json preserving unmentioned fields, created_at set on first write
func WriteBoardMetadata(ctx context.Context, board *string, u BoardMetadataUpdate) (*pyjson.Object, error) {
	if err := assertNotDelegatedChildMutation(ctx, ""); err != nil {
		return nil, err
	}
	slug, err := slugOrDefault(board)
	if err != nil {
		return nil, err
	}
	meta, err := ReadBoardMetadata(&slug)
	if err != nil {
		return nil, err
	}
	meta.Delete("db_path")
	if u.Name != nil {
		name := pystr.Strip(*u.Name)
		if name == "" {
			name = defaultBoardDisplayName(slug)
		}
		meta.Set("name", name)
	}
	for _, kv := range []struct {
		key   string
		value *string
	}{{"description", u.Description}, {"icon", u.Icon}, {"color", u.Color}} {
		if kv.value != nil {
			meta.Set(kv.key, *kv.value)
		}
	}
	if u.Archived != nil {
		meta.Set("archived", *u.Archived)
	}
	for _, kv := range []struct {
		key   string
		value *string
	}{{"default_workdir", u.DefaultWorkdir}, {"project_id", u.ProjectID}} {
		if kv.value != nil {
			if *kv.value != "" {
				meta.Set(kv.key, *kv.value)
			} else {
				meta.Set(kv.key, nil)
			}
		}
	}
	if !pyrepr.Truthy(objGet(meta, "created_at")) {
		meta.Set("created_at", time.Now().Unix())
	}
	path, err := BoardMetadataPath(&slug)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(pypath.PathParent(path), 0o777); err != nil {
		return nil, err
	}
	text, err := pyjson.Dumps(meta, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(text+"\n"), 0o666); err != nil {
		return nil, err
	}
	dbPath, err := KanbanDBPath(context.Background(), &slug)
	if err != nil {
		return nil, err
	}
	meta.Set("db_path", dbPath)
	return meta, nil
}

// Board fields a new board starts with
type BoardCreateOptions struct {
	Name           *string
	Description    *string
	Icon           *string
	Color          *string
	DefaultWorkdir *string
	ProjectID      *string
}

// Creates board dir, database and metadata, an existing board returning its metadata and losing any archive tombstone
func CreateBoard(ctx context.Context, slug string, o BoardCreateOptions) (*pyjson.Object, error) {
	normed, err := requireSlug(slug)
	if err != nil {
		return nil, err
	}
	archived := false
	meta, err := WriteBoardMetadata(ctx, &normed, BoardMetadataUpdate{
		Name: o.Name, Description: o.Description, Icon: o.Icon, Color: o.Color,
		DefaultWorkdir: o.DefaultWorkdir, ProjectID: o.ProjectID, Archived: &archived,
	})
	if err != nil {
		return nil, err
	}
	if _, err := InitDB(ctx, "", &normed); err != nil {
		return nil, err
	}
	return meta, nil
}

// Metadata for every board, default first then board dirs holding board.json sorted case insensitively
func ListBoards(includeArchived bool) ([]*pyjson.Object, error) {
	def := DefaultBoard
	first, err := ReadBoardMetadata(&def)
	if err != nil {
		return nil, err
	}
	entries := []*pyjson.Object{first}
	seen := map[string]bool{DefaultBoard: true}
	root := BoardsRoot()
	children, err := os.ReadDir(root)
	if err != nil {
		return entries, nil
	}
	slices.SortStableFunc(children, func(a, b os.DirEntry) int {
		return strings.Compare(pystr.Lower(a.Name()), pystr.Lower(b.Name()))
	})
	for _, child := range children {
		full := pypath.PathJoin(root, child.Name())
		if st, err := os.Stat(full); err != nil || !st.IsDir() {
			continue
		}
		name := child.Name()
		normed, err := normalizeBoardSlug(&name)
		if err != nil || normed == "" || seen[normed] || !dirHoldsBoard(full) {
			continue
		}
		meta, err := ReadBoardMetadata(&normed)
		if err != nil {
			return nil, err
		}
		if pyrepr.Truthy(objGet(meta, "archived")) && !includeArchived {
			continue
		}
		entries = append(entries, meta)
		seen[normed] = true
	}
	return entries, nil
}

// Archives a board under boards/_archived with a tombstone at its slug, or deletes it, default never removable
func RemoveBoard(ctx context.Context, slug string, archive bool) (*pyjson.Object, error) {
	if err := assertNotDelegatedChildMutation(ctx, ""); err != nil {
		return nil, err
	}
	normed, err := requireSlug(slug)
	if err != nil {
		return nil, err
	}
	if normed == DefaultBoard {
		return nil, &ValueError{"the 'default' board cannot be removed"}
	}
	d, err := BoardDir(&normed)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(d); err != nil {
		return nil, &ValueError{fmt.Sprintf("board %s does not exist", pyrepr.StrRepr(normed))}
	}
	if GetCurrentBoard(ctx) == normed {
		if err := ClearCurrentBoard(ctx); err != nil {
			return nil, err
		}
	}
	if resolved, err := pypath.Resolve(pypath.PathJoin(d, "kanban.db")); err == nil {
		initMu.Lock()
		delete(initializedPaths, resolved)
		initMu.Unlock()
	}
	result := pyjson.NewObject()
	result.Set("slug", normed)
	if !archive {
		if err := pyfs.RmTree(d, nil); err != nil {
			return nil, err
		}
		result.Set("action", "deleted")
		result.Set("new_path", "")
		return result, nil
	}
	prior, err := ReadBoardMetadata(&normed)
	if err != nil {
		return nil, err
	}
	archiveRoot := pypath.PathJoin(BoardsRoot(), "_archived")
	if err := os.MkdirAll(archiveRoot, 0o777); err != nil {
		return nil, err
	}
	ts := time.Now().Unix()
	target := pypath.PathJoin(archiveRoot, fmt.Sprintf("%s-%d", normed, ts))
	for suffix := 1; pathExists(target); suffix++ {
		target = pypath.PathJoin(archiveRoot, fmt.Sprintf("%s-%d-%d", normed, ts, suffix))
	}
	if err := os.Rename(d, target); err != nil {
		return nil, err
	}
	archived := true
	if _, err := WriteBoardMetadata(ctx, &normed, BoardMetadataUpdate{
		Name: metaStr(prior, "name"), Description: metaStr(prior, "description"), Icon: metaStr(prior, "icon"),
		Color: metaStr(prior, "color"), DefaultWorkdir: metaClearableStr(prior, "default_workdir"),
		ProjectID: metaClearableStr(prior, "project_id"), Archived: &archived,
	}); err != nil {
		return nil, err
	}
	result.Set("action", "archived")
	result.Set("new_path", target)
	return result, nil
}

// Metadata value as the str() a write applies, nil when absent or null
func metaStr(meta *pyjson.Object, key string) *string {
	v := objGet(meta, key)
	if v == nil {
		return nil
	}
	s := pyrepr.Str(v)
	return &s
}

// Value of a key in a decoded object, nil when absent
func objGet(o *pyjson.Object, key string) any {
	v, _ := o.Get(key)
	return v
}

// Metadata value for a clearable field, empty for a falsy value so the write stores null
func metaClearableStr(meta *pyjson.Object, key string) *string {
	v := objGet(meta, key)
	if v == nil {
		return nil
	}
	s := ""
	if pyrepr.Truthy(v) {
		s = pyrepr.Str(v)
	}
	return &s
}

// Reports whether a path exists the way Path.exists does
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
