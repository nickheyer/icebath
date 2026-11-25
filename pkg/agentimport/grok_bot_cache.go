package agentimport

import (
	"encoding/base32"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Grok Bot app user data folder per OS
var grokUserDataDirs = []string{"AppData/Roaming/Grok Bot", "Library/Application Support/Grok Bot", ".config/Grok Bot"}

const (
	grokCacheDir   = "sand-client-persistence"
	grokSlicePrefx = "sand.client.slice."
)

// GrokBotCache holds Bot names in roster order and per Bot the routine names its cached chat created
type GrokBotCache struct {
	Bots        []string
	RoutineBots []string
	Routines    map[string][]string
}

// Slice id a cache file name encodes as unpadded lowercase base32, empty when it decodes to nothing valid
func sliceID(path string) string {
	name := filepath.Base(path)
	stem := strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name)))
	if pad := len(stem) % 8; pad != 0 {
		stem += strings.Repeat("=", 8-pad)
	}
	raw, err := base32.StdEncoding.DecodeString(stem)
	if err != nil || !utf8.Valid(raw) {
		return ""
	}
	return string(raw)
}

// Value of a cache slice envelope, nil when unreadable or mid rewrite
func sliceValue(path string) any {
	raw, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(raw) {
		return nil
	}
	data, err := jsonutil.Decode(string(raw))
	if err != nil {
		return nil
	}
	return get(data, "value")
}

// Percent decoding like urllib unquote, bad escapes kept and invalid utf-8 replaced
func unquote(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	hex := func(c byte) (byte, bool) {
		switch {
		case c >= '0' && c <= '9':
			return c - '0', true
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10, true
		case c >= 'A' && c <= 'F':
			return c - 'A' + 10, true
		}
		return 0, false
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := hex(s[i+1])
			lo, ok2 := hex(s[i+2])
			if ok1 && ok2 {
				out = append(out, hi<<4|lo)
				i += 2
				continue
			}
		}
		out = append(out, s[i])
	}
	return strings.ToValidUTF8(string(out), "\uFFFD")
}

// ReadGrokBotCache reads the cached roster and routine names under the user home, empty when the app is not installed
func ReadGrokBotCache() GrokBotCache {
	cache := GrokBotCache{Routines: map[string][]string{}}
	homeDir := userHome()
	folder := ""
	for _, d := range grokUserDataDirs {
		if p := filepath.Join(homeDir, d, grokCacheDir); isDir(p) {
			folder = p
			break
		}
	}
	if folder == "" {
		return cache
	}
	blobs, _ := filepath.Glob(filepath.Join(globEscape(folder), "*.blob"))
	sortPaths(blobs)
	slices := jsonutil.NewObject()
	for _, p := range blobs {
		sid := sliceID(p)
		if sid == "" || !strings.HasPrefix(sid, grokSlicePrefx+"account.") {
			continue
		}
		parts := strings.SplitN(sid[len(grokSlicePrefx):], ".", 3)
		if len(parts) == 3 {
			slices.Set(unquote(parts[2]), p)
		}
	}
	var roster any
	if p, ok := slices.Value("roster.last-roster").(string); ok {
		roster = sliceValue(p)
	}
	names := jsonutil.NewObject()
	if rows, ok := get(roster, "rows").([]any); ok {
		for i, row := range rows {
			o, ok := asObject(row)
			if !ok || truthy(o.Value("isGroup")) {
				continue
			}
			name := ""
			if truthy(o.Value("name")) {
				name = strings.TrimSpace(anyutil.String(o.Value("name")))
			}
			if name == "" {
				continue
			}
			key := anyutil.String(o.Value("id"))
			if o.Value("id") == nil {
				key = "\x00row " + strconv.Itoa(i)
			}
			names.Set(key, name)
		}
	}
	for _, id := range names.Keys() {
		cache.Bots = append(cache.Bots, names.Value(id).(string))
	}
	for _, kind := range slices.Keys() {
		id := kind[strings.LastIndex(kind, ".")+1:]
		if _, known := names.Get(id); !strings.HasPrefix(kind, "transcript.replicas.") || !known {
			continue
		}
		seen := jsonutil.NewObject()
		entries, _ := get(sliceValue(slices.Value(kind).(string)), "entries").([]any)
		for _, entry := range entries {
			event, ok := asObject(get(entry, "event"))
			if evType, _ := event.Value("type").(string); !ok || evType != "automation-changed" {
				continue
			}
			key := ""
			switch {
			case truthy(event.Value("automationId")):
				key = anyutil.String(event.Value("automationId"))
			case truthy(event.Value("automationName")):
				key = anyutil.String(event.Value("automationName"))
			}
			if action, _ := event.Value("action").(string); action == "deleted" {
				seen.Delete(key)
			} else if key != "" {
				label := key
				if truthy(event.Value("automationName")) {
					label = anyutil.String(event.Value("automationName"))
				}
				seen.Set(key, label)
			}
		}
		if seen.Len() > 0 {
			bot := names.Value(id).(string)
			if _, had := cache.Routines[bot]; !had {
				cache.RoutineBots = append(cache.RoutineBots, bot)
			}
			var routines []string
			for _, k := range seen.Keys() {
				routines = append(routines, seen.Value(k).(string))
			}
			cache.Routines[bot] = routines
		}
	}
	return cache
}
