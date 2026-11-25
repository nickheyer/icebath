package googlemeet

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// File backed registry of approved remote nodes, name to url and token
type nodeRegistry struct{ path string }

// Registry at path, the profile meetings nodes.json when empty
func newNodeRegistry(ctx context.Context, path string) *nodeRegistry {
	if path == "" {
		path = filepath.Join(home.Dir(ctx), "workspace", "meetings", "nodes.json")
	}
	return &nodeRegistry{path: path}
}

// The nodes map, empty when the file is missing or malformed
func (r *nodeRegistry) load() *jsonutil.Object {
	if data, ok := readJSON(r.path).(*jsonutil.Object); ok {
		if nodes, ok := data.Value("nodes").(*jsonutil.Object); ok {
			return nodes
		}
	}
	return jsonutil.NewObject()
}

func (r *nodeRegistry) save(ctx context.Context, nodes *jsonutil.Object) error {
	return config.AtomicJSONWrite(ctx, r.path, jsonutil.ObjectOf("nodes", nodes))
}

// Entry with its name prepended, the TypeError of a non mapping entry returned
func withName(name string, entry any) (*jsonutil.Object, error) {
	e, ok := entry.(*jsonutil.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object is not a mapping", typeName(entry))
	}
	out := jsonutil.ObjectOf("name", name)
	out.Merge(e)
	return out, nil
}

// Entry of a named node, nil when unknown
func (r *nodeRegistry) get(name string) (*jsonutil.Object, error) {
	entry, ok := r.load().Get(name)
	if !ok || entry == nil {
		return nil, nil
	}
	return withName(name, entry)
}

// Registers or replaces a node, refusing empty fields
func (r *nodeRegistry) add(ctx context.Context, name, url, token string) error {
	for _, f := range [][2]string{{"node name", name}, {"url", url}, {"token", token}} {
		if f[1] == "" {
			return fmt.Errorf("%s must be a non-empty string", f[0])
		}
	}
	nodes := r.load()
	nodes.Set(name, jsonutil.ObjectOf("url", url, "token", token, "added_at", nowSeconds()))
	return r.save(ctx, nodes)
}

// Forgets a node, false when it was not registered
func (r *nodeRegistry) remove(ctx context.Context, name string) (bool, error) {
	nodes := r.load()
	if !nodes.Delete(name) {
		return false, nil
	}
	return true, r.save(ctx, nodes)
}

// Every node sorted by name
func (r *nodeRegistry) listAll() ([]*jsonutil.Object, error) {
	nodes := r.load()
	names := append([]string(nil), nodes.Keys()...)
	sort.Strings(names)
	out := make([]*jsonutil.Object, 0, len(names))
	for _, n := range names {
		e, err := withName(n, nodes.Value(n))
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// Named node, or the sole registered node when name is empty, nil when unknown or ambiguous
func (r *nodeRegistry) resolve(chromeNode string) (*jsonutil.Object, error) {
	if chromeNode != "" {
		return r.get(chromeNode)
	}
	nodes, err := r.listAll()
	if err != nil || len(nodes) != 1 {
		return nil, err
	}
	return nodes[0], nil
}
