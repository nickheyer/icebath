// Package txt names the texts of the terminal UI catalog, one key per entry of pkg/tui/locales/en.tui.yaml, so code can only ask for a text the catalog holds
package txt

//go:generate go run ./internal/keygen

// Key of a catalog text. The generated keys are the only ones, no other package can make one
type Key struct{ name string }

// Every key by its dotted name
var byName = func() map[string]Key {
	out := make(map[string]Key, len(all))
	for _, k := range all {
		out[k.name] = k
	}
	return out
}()

// Name is the dotted name of the key in the catalog, empty for the zero key
func (k Key) Name() string { return k.name }

// Named is the key with a dotted name made at run time, false when the catalog holds no such text
func Named(name string) (Key, bool) {
	k, ok := byName[name]
	return k, ok
}

// All is every key in the order of their names
func All() []Key { return append([]Key(nil), all...) }
