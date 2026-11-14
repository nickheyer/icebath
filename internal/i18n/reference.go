package i18n

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"path/filepath"
	"strconv"
	"strings"
)

func ReferenceKeys(surface Surface) (map[string]struct{}, bool) {
	fsys, dir := bundledSource()
	at := func(name string) string {
		if dir == "" {
			return name
		}
		return filepath.Join(dir, name)
	}
	keys := map[string]struct{}{}
	switch surface {
	case Core:
		data, err := fs.ReadFile(fsys, "en.yaml")
		if err == nil {
			var loc Locale
			if loc, err = parseLocale(at("en.yaml"), data); err == nil {
				for k := range loc.Messages {
					keys[k] = struct{}{}
				}
				return keys, true
			}
		}
		slog.Debug(fmt.Sprintf("i18n reference keys for %s unavailable: %v", surface, err))
		return nil, false
	case TUI, Desktop:
	default:
		slog.Debug(fmt.Sprintf("i18n reference keys for %s unavailable: unknown surface", surface))
		return nil, false
	}
	name := "_keys." + string(surface) + ".json"
	fi, err := fs.Stat(fsys, name)
	if err != nil && !missing(err) {
		slog.Debug(fmt.Sprintf("i18n reference keys for %s unavailable: %v", surface, err))
		return nil, false
	}
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		slog.Debug(fmt.Sprintf("i18n reference keys for %s unavailable: %v", surface, err))
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		slog.Debug(fmt.Sprintf("i18n reference keys for %s unavailable: %v", surface, err))
		return nil, false
	}
	if dec.More() {
		slog.Debug(fmt.Sprintf("i18n reference keys for %s unavailable: extra data after the JSON document in %s", surface, at(name)))
		return nil, false
	}
	list := doc
	if obj, ok := doc.(map[string]any); ok {
		list = obj["keys"]
	}
	entries, ok := list.([]any)
	if !ok {
		return nil, false
	}
	for _, e := range entries {
		text, ok := jsonScalarText(e)
		if !ok {
			slog.Debug(fmt.Sprintf("i18n reference keys for %s unavailable: non-scalar key entry in %s", surface, at(name)))
			return nil, false
		}
		keys[text] = struct{}{}
	}
	return keys, true
}

func jsonScalarText(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case nil:
		return "None", true
	case bool:
		if t {
			return "True", true
		}
		return "False", true
	case json.Number:
		s := t.String()
		if !strings.ContainsAny(s, ".eE") {
			n, ok := new(big.Int).SetString(s, 10)
			if ok {
				return n.String(), true
			}
		}
		f, _ := strconv.ParseFloat(s, 64)
		return floatRepr(f), true
	}
	return "", false
}
