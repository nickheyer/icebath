package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// One value at a path inside a configuration file
type Field struct {
	Path  []string
	Value any
	// Removes the value from every file holding it instead of writing one
	Unset bool
}

// Applies a setting and saves it so the next start under dir reads the same value
func (c *Config) SaveSetting(dir, key, value string) error {
	f, err := c.settingField(key, value)
	if err != nil {
		return err
	}
	return Save(dir, c.Sources, f)
}

// Writes each value into every file setting it, else the user file, and removes unset ones from every file
func Save(dir string, sources []string, fields ...Field) error {
	layers, err := configLayers(dir)
	if err != nil {
		return err
	}
	var files []string
	docs := map[string]*object{}
	for _, path := range append(layers, sources...) {
		if _, ok := docs[path]; ok {
			continue
		}
		doc, err := readObject(path)
		if err != nil {
			return err
		}
		docs[path] = doc
		files = append(files, path)
	}
	changed := map[string]bool{}
	for _, f := range fields {
		var targets []string
		for _, path := range files {
			if docs[path].holds(f.Path) {
				targets = append(targets, path)
			}
		}
		if f.Unset {
			for _, path := range targets {
				if err := docs[path].remove(f.Path); err != nil {
					return fmt.Errorf("%s: %w", path, err)
				}
				changed[path] = true
			}
			continue
		}
		if len(targets) == 0 {
			targets = layers[:1]
		}
		for _, path := range targets {
			if err := docs[path].place(f.Path, f.Value); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			changed[path] = true
		}
	}
	for _, path := range files {
		if !changed[path] {
			continue
		}
		if err := writeObject(path, docs[path]); err != nil {
			return err
		}
	}
	return nil
}

// JSON map that keeps its keys in file order and its values as written
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func newObject() *object { return &object{vals: map[string]json.RawMessage{}} }

// Reads the top level keys of JSON in order, leaving every value raw
func decodeObject(data []byte) (*object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if t != json.Delim('{') {
		return nil, errors.New("not an object")
	}
	o := newObject()
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := t.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		o.set(key, raw)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

// Replaces a value in place or appends it as the last key
func (o *object) set(key string, raw json.RawMessage) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = raw
}

// Writes the keys in their kept order
func (o *object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := encode(k)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(o.vals[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Reports whether the object holds a value at the field path
func (o *object) holds(path []string) bool {
	raw, ok := o.vals[path[0]]
	if !ok {
		return false
	}
	if len(path) == 1 {
		return true
	}
	inner, err := decodeObject(raw)
	return err == nil && inner.holds(path[1:])
}

// Sets the value at the field path, creating the objects along it
func (o *object) place(path []string, value any) error {
	if len(path) == 1 {
		raw, err := encode(value)
		if err != nil {
			return err
		}
		o.set(path[0], raw)
		return nil
	}
	inner := newObject()
	if raw, ok := o.vals[path[0]]; ok && string(bytes.TrimSpace(raw)) != "null" {
		var err error
		if inner, err = decodeObject(raw); err != nil {
			return fmt.Errorf("%s is not an object", path[0])
		}
	}
	if err := inner.place(path[1:], value); err != nil {
		return err
	}
	raw, err := encode(inner)
	if err != nil {
		return err
	}
	o.set(path[0], raw)
	return nil
}

// Drops the value at the field path, and any object along it left empty
func (o *object) remove(path []string) error {
	raw, ok := o.vals[path[0]]
	if !ok {
		return nil
	}
	if len(path) == 1 {
		o.drop(path[0])
		return nil
	}
	inner, err := decodeObject(raw)
	if err != nil {
		return fmt.Errorf("%s is not an object", path[0])
	}
	if err := inner.remove(path[1:]); err != nil {
		return err
	}
	if len(inner.keys) == 0 {
		o.drop(path[0])
		return nil
	}
	enc, err := encode(inner)
	if err != nil {
		return err
	}
	o.set(path[0], enc)
	return nil
}

// Deletes a key and its place in the order
func (o *object) drop(key string) {
	delete(o.vals, key)
	if i := slices.Index(o.keys, key); i >= 0 {
		o.keys = slices.Delete(o.keys, i, i+1)
	}
}

// Encodes a value as compact JSON with HTML characters left as they are
func encode(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Loads a configuration file in key order, empty when the file does not exist
func readObject(path string) (*object, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return newObject(), nil
	}
	if err != nil {
		return nil, err
	}
	o, err := decodeObject(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return o, nil
}

// Saves as indented JSON, creating the directory it lives in
func writeObject(path string, o *object) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(o); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
