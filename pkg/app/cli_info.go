package app

import (
	"fmt"
	"sort"

	templateutil "github.com/nickheyer/icebath/pkg/utils/template"
)

// Python str.format of a constant spec, a malformed spec raising as in Python
func pyFmt(spec string, kw map[string]any) string {
	out, err := templateutil.Format(spec, kw)
	if err != nil {
		panic(err)
	}
	return out
}

// Keys of a string keyed map in sorted order
func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Runs fn and turns a raised panic into its error, as a Python except Exception clause
func pyExcept(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s", panicText(r))
		}
	}()
	return fn()
}
