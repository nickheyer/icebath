package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyast"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Whether a statement is a registry.register(...) call expression
func isRegistryRegisterCall(n *pyast.Node) bool {
	call := n.Child("value")
	if !n.Is("Expr") || !call.Is("Call") {
		return false
	}
	fn := call.Child("func")
	attr, _ := fn.Str("attr")
	id, _ := fn.Child("value").Str("id")
	return fn.Is("Attribute") && attr == "register" && fn.Child("value").Is("Name") && id == "registry"
}

// Whether the module body or a module level for loop calls registry.register, false on read or syntax errors
func moduleRegistersTools(modulePath string) (bool, error) {
	data, err := os.ReadFile(modulePath)
	if err != nil {
		return false, nil
	}
	source, err := pystr.DecodeUTF8Sig(data)
	if err != nil {
		return false, err
	}
	if !strings.Contains(source, "registry") || !strings.Contains(source, "register") {
		return false, nil
	}
	tree, err := pyast.Parse(source)
	if err != nil {
		var se *pyast.SyntaxError
		if errors.As(err, &se) {
			return false, nil
		}
		return false, err
	}
	for _, stmt := range tree.Children("body") {
		if isRegistryRegisterCall(stmt) {
			return true, nil
		}
		if stmt.Is("For") {
			for _, s := range stmt.Children("body") {
				if isRegistryRegisterCall(s) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// Flat tools/*.py modules plus package entry points tools/<pkg>/tool.py in one sorted list
func toolModuleCandidates(toolsPath string) []string {
	flat, _ := filepath.Glob(filepath.Join(toolsPath, "*.py"))
	nested, _ := filepath.Glob(filepath.Join(toolsPath, "*", "tool.py"))
	out := append(flat, nested...)
	sort.Slice(out, func(i, j int) bool { return pathPartsLess(out[i], out[j]) })
	return out
}

// Ordering of two paths by their parts as sorting pathlib paths compares them
func pathPartsLess(a, b string) bool {
	pa, pb := strings.Split(filepath.ToSlash(a), "/"), strings.Split(filepath.ToSlash(b), "/")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}

func discoveryCachePath() string {
	return filepath.Join(home.Root(), "cache", "tool_discovery_cache.json")
}

// Discovery cache entries, empty on any read or decode error
func loadDiscoveryCache() *pyjson.Object {
	data, err := os.ReadFile(discoveryCachePath())
	if err != nil {
		return pyjson.NewObject()
	}
	text, err := pystr.DecodeUTF8Sig(data)
	if err != nil {
		return pyjson.NewObject()
	}
	v, err := pyjson.Loads(text)
	if obj, ok := v.(*pyjson.Object); err == nil && ok {
		return obj
	}
	return pyjson.NewObject()
}

// Atomic write of the discovery cache that logs and swallows every failure
func saveDiscoveryCache(ctx context.Context, cache *pyjson.Object) {
	path := discoveryCachePath()
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		slog.Debug(fmt.Sprintf("Could not write tool discovery cache %s: %s", path, err))
		return
	}
	if err := config.AtomicJSONWriteWith(ctx, path, cache, pyjson.Indent(0)); err != nil {
		slog.Debug(fmt.Sprintf("Could not write tool discovery cache %s: %s", path, err))
	}
}

// Python equality of a decoded JSON number with an int
func jsonNumberEquals(v any, n int64) bool {
	switch x := v.(type) {
	case bool:
		return (n == 1 && x) || (n == 0 && !x)
	case int:
		return int64(x) == n
	case int64:
		return x == n
	case *big.Int:
		return x.IsInt64() && x.Int64() == n
	case float64:
		return x == float64(n) && float64(int64(x)) == x
	}
	return false
}

// Dotted names of the self registering tool modules under a tools directory with the stat keyed verdict cache
func discoverToolModules(ctx context.Context, toolsDir string) ([]string, error) {
	toolsPath, err := filepath.Abs(toolsDir)
	if err != nil {
		return nil, err
	}
	if r, err := filepath.EvalSymlinks(toolsPath); err == nil {
		toolsPath = r
	}
	cache := loadDiscoveryCache()
	fresh := pyjson.NewObject()
	dirty := false
	var names []string
	for _, path := range toolModuleCandidates(toolsPath) {
		switch filepath.Base(path) {
		case "__init__.py", "registry.py", "mcp_tool.py":
			continue
		}
		rel, _ := filepath.Rel(toolsPath, strings.TrimSuffix(path, ".py"))
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) > 1 {
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), "__init__.py")); err != nil {
				slog.Warn(fmt.Sprintf("Skipping %s: package %s has no __init__.py", path, filepath.Base(filepath.Dir(path))))
				continue
			}
		}
		absPath, _ := filepath.Abs(path)
		if r, err := filepath.EvalSymlinks(absPath); err == nil {
			absPath = r
		}
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		mtime, size := st.ModTime().UnixNano(), st.Size()
		var registers bool
		cached, _ := cache.Get(absPath)
		if l, ok := cached.([]any); ok && len(l) == 3 && jsonNumberEquals(l[0], mtime) && jsonNumberEquals(l[1], size) {
			registers = pyrepr.Truthy(l[2])
		} else {
			if registers, err = moduleRegistersTools(path); err != nil {
				return nil, err
			}
			dirty = true
		}
		fresh.Set(absPath, []any{mtime, size, registers})
		if registers {
			names = append(names, strings.Join(append([]string{"tools"}, parts...), "."))
		}
	}
	sameKeys := fresh.Len() == cache.Len()
	for _, k := range fresh.Keys {
		if _, ok := cache.Get(k); !ok {
			sameKeys = false
		}
	}
	if dirty || !sameKeys {
		saveDiscoveryCache(ctx, fresh)
	}
	return names, nil
}
