package kanban

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Reports whether a directory carries board.json, the identity marker a db-only board directory lacks
func dirHoldsBoard(d string) bool {
	_, err := os.Stat(pathutil.Join(d, "board.json"))
	return err == nil
}

// board.json path holding display metadata, the directory slug being the identity
func BoardMetadataPath(board *string) (string, error) {
	d, err := BoardDir(board)
	if err != nil {
		return "", err
	}
	return pathutil.Join(d, "board.json"), nil
}

// Title cased words of a slug, the slug itself when it has none
func defaultBoardDisplayName(slug string) string {
	var parts []string
	for _, part := range strings.Split(strings.ReplaceAll(slug, "_", "-"), "-") {
		if part != "" {
			parts = append(parts, textutil.UpperFirst(strings.ToLower(part)))
		}
	}
	if len(parts) == 0 {
		return slug
	}
	return strings.Join(parts, " ")
}

// board.json merged over defaults plus slug and db_path, a missing or malformed file yielding the synthesized entry
func ReadBoardMetadata(board *string) (*jsonutil.Object, error) {
	slug, err := slugOrDefault(board)
	if err != nil {
		return nil, err
	}
	meta := jsonutil.NewObject()
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
		if text, err := textutil.DecodeUTF8(textutil.TrimBOM(data)); err == nil {
			if raw, err := jsonutil.Decode(text); err == nil {
				if obj, ok := raw.(*jsonutil.Object); ok {
					obj.Set("slug", slug)
					for _, k := range obj.Keys() {
						meta.Set(k, obj.Value(k))
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
func WriteBoardMetadata(ctx context.Context, board *string, u BoardMetadataUpdate) (*jsonutil.Object, error) {
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
		name := strings.TrimSpace(*u.Name)
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
	if !anyutil.IsTrue(objGet(meta, "created_at")) {
		meta.Set("created_at", time.Now().Unix())
	}
	path, err := BoardMetadataPath(&slug)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Clean(path)), 0o777); err != nil {
		return nil, err
	}
	text, err := jsonutil.Encode(meta, jsonutil.Indent(2))
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
func CreateBoard(ctx context.Context, slug string, o BoardCreateOptions) (*jsonutil.Object, error) {
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
func ListBoards(includeArchived bool) ([]*jsonutil.Object, error) {
	def := DefaultBoard
	first, err := ReadBoardMetadata(&def)
	if err != nil {
		return nil, err
	}
	entries := []*jsonutil.Object{first}
	seen := map[string]bool{DefaultBoard: true}
	root := BoardsRoot()
	children, err := os.ReadDir(root)
	if err != nil {
		return entries, nil
	}
	slices.SortStableFunc(children, func(a, b os.DirEntry) int {
		return strings.Compare(strings.ToLower(a.Name()), strings.ToLower(b.Name()))
	})
	for _, child := range children {
		full := pathutil.Join(root, child.Name())
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
		if anyutil.IsTrue(objGet(meta, "archived")) && !includeArchived {
			continue
		}
		entries = append(entries, meta)
		seen[normed] = true
	}
	return entries, nil
}

// Archives a board under boards/_archived with a tombstone at its slug, or deletes it, default never removable
func RemoveBoard(ctx context.Context, slug string, archive bool) (*jsonutil.Object, error) {
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
		return nil, &ValueError{fmt.Sprintf("board %s does not exist", errchain.Show(normed))}
	}
	if GetCurrentBoard(ctx) == normed {
		if err := ClearCurrentBoard(ctx); err != nil {
			return nil, err
		}
	}
	if resolved, err := pathutil.Resolve(pathutil.Join(d, "kanban.db")); err == nil {
		initMu.Lock()
		delete(initializedPaths, resolved)
		initMu.Unlock()
	}
	result := jsonutil.NewObject()
	result.Set("slug", normed)
	if !archive {
		if err := os.RemoveAll(d); err != nil {
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
	archiveRoot := pathutil.Join(BoardsRoot(), "_archived")
	if err := os.MkdirAll(archiveRoot, 0o777); err != nil {
		return nil, err
	}
	ts := time.Now().Unix()
	target := pathutil.Join(archiveRoot, fmt.Sprintf("%s-%d", normed, ts))
	for suffix := 1; pathExists(target); suffix++ {
		target = pathutil.Join(archiveRoot, fmt.Sprintf("%s-%d-%d", normed, ts, suffix))
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
func metaStr(meta *jsonutil.Object, key string) *string {
	v := objGet(meta, key)
	if v == nil {
		return nil
	}
	s := anyutil.String(v)
	return &s
}

// Value of a key in a decoded object, nil when absent
func objGet(o *jsonutil.Object, key string) any {
	v, _ := o.Get(key)
	return v
}

// Metadata value for a clearable field, empty for a falsy value so the write stores null
func metaClearableStr(meta *jsonutil.Object, key string) *string {
	v := objGet(meta, key)
	if v == nil {
		return nil
	}
	s := ""
	if anyutil.IsTrue(v) {
		s = anyutil.String(v)
	}
	return &s
}

// Reports whether a path exists the way Path.exists does
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
