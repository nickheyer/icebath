package files

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// PluginDataFolder is the handle of a plugin's own data folder
const PluginDataFolder = "data"

var grantIDRE = regexp.MustCompile(`^[a-f0-9]{16}$`)

// PluginFolderGrant is one folder the user granted a plugin
type PluginFolderGrant struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	GrantedAt string `json:"grantedAt"`
}

// PluginFilesDeps are the app facts the plugin file door needs
type PluginFilesDeps struct {
	UserDataDir    func() string
	ProtectedRoots func() []string
	TrashItem      func(target string) error
	Platform       string
}

// grantStore keeps the grants record with its key order
type grantStore struct {
	keys []string
	rows map[string][]any
}

func (g *grantStore) clone() *grantStore {
	c := &grantStore{keys: append([]string{}, g.keys...), rows: map[string][]any{}}
	for k, v := range g.rows {
		copied, _ := jsonParseOrdered(jsonStringify(v, "", ""))
		c.rows[k], _ = copied.([]any)
	}
	return c
}

func (g *grantStore) set(key string, rows []any) {
	if _, ok := g.rows[key]; !ok {
		g.keys = append(g.keys, key)
	}
	g.rows[key] = rows
}

func (g *grantStore) remove(key string) {
	if _, ok := g.rows[key]; !ok {
		return
	}
	delete(g.rows, key)
	for i, k := range g.keys {
		if k == key {
			g.keys = append(g.keys[:i], g.keys[i+1:]...)
			return
		}
	}
}

// Keys in JS property order, array index keys ascending first then insertion order
func (g *grantStore) orderedKeys() []string {
	var index, named []string
	for _, k := range g.keys {
		if n, err := strconv.ParseUint(k, 10, 32); err == nil && strconv.FormatUint(n, 10) == k && n < 1<<32-1 {
			index = append(index, k)
		} else {
			named = append(named, k)
		}
	}
	for i := 1; i < len(index); i++ {
		for j := i; j > 0; j-- {
			a, _ := strconv.ParseUint(index[j-1], 10, 32)
			b, _ := strconv.ParseUint(index[j], 10, 32)
			if a <= b {
				break
			}
			index[j-1], index[j] = index[j], index[j-1]
		}
	}
	return append(index, named...)
}

func (g *grantStore) toJS() *OrderedObject {
	o := NewOrderedObject()
	for _, k := range g.orderedKeys() {
		o.Set(k, g.rows[k])
	}
	return o
}

// Grants record parsed with its key order, rows kept as written, an unparsable record as no grants
func parseGrantStore(raw string) *grantStore {
	g := &grantStore{rows: map[string][]any{}}
	parsed, err := jsonParseOrdered(raw)
	if err != nil {
		return g
	}
	top, ok := parsed.(*OrderedObject)
	if !ok {
		return g
	}
	grants, ok := top.Get("grants").(*OrderedObject)
	if !ok {
		return g
	}
	for _, k := range grants.keys {
		if rows, ok := grants.vals[k].([]any); ok {
			g.set(k, rows)
		}
	}
	return g
}

// PluginFiles is the folder scoped file door behind the plugin SDK ctx.files
type PluginFiles struct {
	deps    PluginFilesDeps
	tools   pathTools
	storeMu sync.Mutex
	store   *grantStore
	writeMu sync.Mutex
}

// Door over the app's grants record and data folders
func NewPluginFiles(deps PluginFilesDeps) *PluginFiles {
	if deps.Platform == "" {
		deps.Platform = electronPlatformName()
	}
	return &PluginFiles{deps: deps, tools: newPathTools(deps.Platform)}
}

func (pf *PluginFiles) baseDir() string   { return filepath.Join(pf.deps.UserDataDir(), "plugin-files") }
func (pf *PluginFiles) storeFile() string { return filepath.Join(pf.baseDir(), "grants.json") }

// Reads the record, a missing or unparsable one as no grants, any other read error raised
func (pf *PluginFiles) readStore() (*grantStore, error) {
	raw, err := pfs.readFile(pf.storeFile())
	if err != nil {
		if ErrorCode(err) == "ENOENT" {
			return &grantStore{rows: map[string][]any{}}, nil
		}
		return nil, nodeFsError(err, "open", pf.storeFile())
	}
	return parseGrantStore(string(raw)), nil
}

// The shared store, read once
func (pf *PluginFiles) load() (*grantStore, error) {
	pf.storeMu.Lock()
	defer pf.storeMu.Unlock()
	if pf.store != nil {
		return pf.store, nil
	}
	read, err := pf.readStore()
	if err != nil {
		return nil, err
	}
	pf.store = read
	return pf.store, nil
}

// Serialized read modify write on a copy, saved through a temp file and published only once saved
func (pf *PluginFiles) mutate(change func(*grantStore) any) (any, error) {
	pf.writeMu.Lock()
	defer pf.writeMu.Unlock()
	current, err := pf.load()
	if err != nil {
		return nil, err
	}
	pf.storeMu.Lock()
	grants := current.clone()
	pf.storeMu.Unlock()
	result := change(grants)
	if err := os.MkdirAll(pf.baseDir(), 0o777); err != nil {
		return nil, nodeFsError(err, "mkdir", pf.baseDir())
	}
	tmp := fmt.Sprintf("%s.%d.tmp", pf.storeFile(), os.Getpid())
	body := jsonStringify(NewOrderedObject("version", 1, "grants", grants.toJS()), "  ", "")
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return nil, nodeFsError(err, "open", tmp)
	}
	if err := os.Rename(tmp, pf.storeFile()); err != nil {
		return nil, nodeFsError(err, "rename", tmp, pf.storeFile())
	}
	pf.storeMu.Lock()
	pf.store = grants
	pf.storeMu.Unlock()
	return result, nil
}

func (pf *PluginFiles) protectedRoots() ([]string, error) {
	roots := append(append(append([]string{}, pf.deps.ProtectedRoots()...), pf.deps.UserDataDir()), systemRoots(pf.deps.Platform)...)
	return realRoots(roots)
}

// A second spelling of this machine's files no realpath comparison matches
func (pf *PluginFiles) secondPath(real string) bool {
	return pf.deps.Platform == "win32" && (WslProtectedPath(real) || LoopbackShare(real))
}

func (pf *PluginFiles) refusePseudo(real string) error {
	if pf.deps.Platform == "linux" && onPseudoFilesystem(real) {
		return refuse("protected", "That is a kernel filesystem and cannot be used by a plugin.")
	}
	return nil
}

func (pf *PluginFiles) overlapsProtected(real string, roots []string) bool {
	for _, root := range roots {
		if pf.tools.insideLoose(real, root) || pf.tools.insideLoose(root, real) {
			return true
		}
	}
	return pf.secondPath(real)
}

func rowField(row any, key string) any {
	switch m := row.(type) {
	case map[string]any:
		return m[key]
	case *OrderedObject:
		return m.Get(key)
	}
	return nil
}

// Folder a handle names as a realpath and the protected roots an op inside it must avoid
func (pf *PluginFiles) openFolder(pluginID string, folder any) (string, []string, error) {
	roots, err := realRoots(pf.deps.ProtectedRoots())
	if err != nil {
		return "", nil, err
	}
	if folder == PluginDataFolder {
		dir := filepath.Join(pf.baseDir(), "data", dataDirName(pluginID))
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return "", nil, nodeFsError(err, "mkdir", dir)
		}
		real, err := pfs.realpath(dir)
		if err != nil {
			return "", nil, nodeFsError(err, "realpath", dir)
		}
		return real, roots, nil
	}
	var grant any
	if s, ok := folder.(string); ok && grantIDRE.MatchString(s) {
		store, err := pf.load()
		if err != nil {
			return "", nil, err
		}
		pf.storeMu.Lock()
		for _, row := range store.rows[pluginID] {
			if rowField(row, "id") == s {
				grant = row
				break
			}
		}
		pf.storeMu.Unlock()
	}
	if grant == nil {
		return "", nil, refuse("not-granted", "This plugin has no access to that folder.")
	}
	grantPath := jsString(rowField(grant, "path"))
	real, err := realpathOrEmpty(grantPath)
	if err != nil {
		return "", nil, err
	}
	if real == "" {
		return "", nil, refuse("folder-missing", "The granted folder no longer exists.")
	}
	if !pf.tools.same(real, grantPath) {
		return "", nil, refuse("folder-changed", "The granted folder was moved or replaced; grant it again.")
	}
	avoid, err := pf.protectedRoots()
	if err != nil {
		return "", nil, err
	}
	if pf.overlapsProtected(real, avoid) {
		return "", nil, refuse("protected", "That folder overlaps Icebath data and cannot be used by a plugin.")
	}
	if err := pf.refusePseudo(real); err != nil {
		return "", nil, err
	}
	return real, avoid, nil
}

// Resolves a folder handle and relative path to a checked target inside the folder
func (pf *PluginFiles) resolve(pluginID string, folder, rel any) (Resolved, error) {
	parts, err := pf.tools.segments(rel)
	if err != nil {
		return Resolved{}, err
	}
	root, avoid, err := pf.openFolder(pluginID, folder)
	if err != nil {
		return Resolved{}, err
	}
	guard := func(real string) error {
		if !pf.tools.inside(real, root) {
			return refuse("outside-folder", "That path leaves the folder.")
		}
		for _, p := range avoid {
			if pf.tools.insideLoose(real, p) {
				return refuse("protected", "That path is Icebath data and cannot be used by a plugin.")
			}
		}
		if pf.secondPath(real) {
			return refuse("protected", "That path is Icebath data and cannot be used by a plugin.")
		}
		return nil
	}
	refuseMounted := func(real string) error {
		if protectedIdentityWithin(real, root, avoid) {
			return refuse("protected", "That path is Icebath data mounted inside the folder and cannot be used by a plugin.")
		}
		return nil
	}
	if len(parts) == 0 {
		st, err := pfs.stat(root)
		if err != nil {
			return Resolved{}, nodeFsError(err, "stat", root)
		}
		return Resolved{Root: root, Parts: parts, Entry: root, Target: root, Stat: st}, nil
	}
	parent, err := realpathOrEmpty(filepath.Join(append([]string{root}, parts[:len(parts)-1]...)...))
	if err != nil {
		return Resolved{}, err
	}
	if parent == "" {
		return Resolved{}, refuse("not-found", "The parent folder does not exist.")
	}
	if err := guard(parent); err != nil {
		return Resolved{}, err
	}
	entry := filepath.Join(parent, parts[len(parts)-1])
	lst, err := lstatOrNil(entry)
	if err != nil {
		return Resolved{}, err
	}
	if lst == nil {
		if err := refuseMounted(parent); err != nil {
			return Resolved{}, err
		}
		return Resolved{Root: root, Parts: parts, Parent: parent, Entry: entry, Target: entry}, nil
	}
	target, err := realpathOrEmpty(entry)
	if err != nil {
		return Resolved{}, err
	}
	if target == "" {
		return Resolved{}, refuse("outside-folder", "That link points nowhere inside the folder.")
	}
	if err := guard(target); err != nil {
		return Resolved{}, err
	}
	if err := pf.refusePseudo(target); err != nil {
		return Resolved{}, err
	}
	if err := refuseMounted(target); err != nil {
		return Resolved{}, err
	}
	st, err := pfs.lstat(target)
	if err != nil {
		return Resolved{}, nodeFsError(err, "lstat", target)
	}
	if isLinkMode(st.Mode()) {
		return Resolved{}, refuse("outside-folder", "The file changed while it was being checked.")
	}
	return Resolved{Root: root, Parts: parts, Parent: parent, Entry: entry, Target: target, Stat: st}, nil
}

// Turns a refusal or a file system failure into the answer the plugin reads
func answer(result map[string]any, err error) map[string]any {
	if err == nil {
		return result
	}
	var r *Refusal
	if errors.As(err, &r) {
		return fail(r.Code, r.Message)
	}
	switch ErrorCode(err) {
	case "ENOENT", "ENOTDIR":
		return fail("not-found", "No such file or folder.")
	case "EEXIST":
		return fail("exists", "A file with that name already exists.")
	}
	return fail("io-error", err.Error())
}

// Copies of the grants of one plugin
func (pf *PluginFiles) Grants(pluginID any) ([]any, error) {
	key, _ := pluginID.(string)
	store, err := pf.load()
	if err != nil {
		return nil, err
	}
	pf.storeMu.Lock()
	defer pf.storeMu.Unlock()
	return append([]any{}, store.rows[key]...), nil
}

// Grants a user picked folder unless it is a system folder, overlaps Icebath data or holds keys
func (pf *PluginFiles) AddGrant(pluginID any, picked string) map[string]any {
	return answer(func() (map[string]any, error) {
		key, err := pluginKey(pluginID)
		if err != nil {
			return nil, err
		}
		real, err := realpathOrEmpty(picked)
		if err != nil {
			return nil, err
		}
		if real == "" {
			return fail("not-a-directory", "Pick a folder."), nil
		}
		st, err := pfs.stat(real)
		if err != nil {
			return nil, nodeFsError(err, "stat", real)
		}
		if !st.IsDir() {
			return fail("not-a-directory", "Pick a folder."), nil
		}
		sys, err := realRoots(systemRoots(pf.deps.Platform))
		if err != nil {
			return nil, err
		}
		for _, root := range sys {
			if pf.tools.insideLoose(real, root) {
				return fail("protected", "That is a system folder and cannot be shared with a plugin."), nil
			}
		}
		roots, err := pf.protectedRoots()
		if err != nil {
			return nil, err
		}
		if pf.overlapsProtected(real, roots) || OverlapsByIdentity(real, roots) {
			return fail("protected", "That folder holds Icebath data and cannot be shared with a plugin."), nil
		}
		if err := pf.refusePseudo(real); err != nil {
			return nil, err
		}
		if sensitiveReason(filepath.Join(real, "entry")) != "" || sensitiveReason(real) != "" {
			return fail("protected", "That folder holds keys or credentials and cannot be shared with a plugin."), nil
		}
		result, err := pf.mutate(func(g *grantStore) any {
			rows := g.rows[key]
			for _, row := range rows {
				if p, ok := rowField(row, "path").(string); ok && pf.tools.same(p, real) {
					return map[string]any{"ok": true, "folder": copyRow(row)}
				}
			}
			id := make([]byte, 8)
			rand.Read(id)
			name := filepath.Base(real)
			if name == "" || name == string(filepath.Separator) || name == "." {
				name = real
			}
			folder := map[string]any{"id": hex.EncodeToString(id), "name": name, "path": real,
				"grantedAt": time.Now().UTC().Format("2006-01-02T15:04:05.000Z")}
			g.set(key, append(rows, NewOrderedObject("id", folder["id"], "name", name, "path", real, "grantedAt", folder["grantedAt"])))
			return map[string]any{"ok": true, "folder": folder}
		})
		if err != nil {
			return nil, err
		}
		return result.(map[string]any), nil
	}())
}

// Plain copy of a stored grant row
func copyRow(row any) any {
	copied, _ := jsonParseOrdered(jsonStringify(row, "", ""))
	return copied
}

// Removes one grant, or every grant of the plugin when all is set, answering how many went
func (pf *PluginFiles) Revoke(pluginID any, folderID any, all bool) (map[string]any, error) {
	result, err := pf.mutate(func(g *grantStore) any {
		key, _ := pluginID.(string)
		rows := g.rows[key]
		var kept []any
		if !all {
			for _, row := range rows {
				if rowField(row, "id") != folderID {
					kept = append(kept, row)
				}
			}
		}
		if len(kept) > 0 {
			g.set(key, kept)
		} else {
			g.remove(key)
		}
		return map[string]any{"ok": true, "removed": len(rows) - len(kept)}
	})
	if err != nil {
		return nil, err
	}
	return result.(map[string]any), nil
}

// Runs one file operation for a plugin, every failure answered as a result
func (pf *PluginFiles) Run(pluginID any, request any) map[string]any {
	return answer(func() (map[string]any, error) {
		key, err := pluginKey(pluginID)
		if err != nil {
			return nil, err
		}
		req, _ := request.(map[string]any)
		op, ok := req["op"].(string)
		fn, known := pluginOps[op]
		if !ok || !known {
			return fail("invalid-op", "Unknown file operation."), nil
		}
		return fn(pf, key, req)
	}())
}
