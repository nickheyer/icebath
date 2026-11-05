package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Error when voice submit_mode is present and neither direct nor draft
func ValidateVoice(cfg map[string]any, issues *[]ConfigIssue) {
	voice := AsMap(cfg["voice"])
	if voice == nil {
		return
	}
	submit, ok := voice["submit_mode"]
	if !ok {
		return
	}
	if s, isStr := submit.(string); isStr {
		if n := strings.ToLower(strings.TrimSpace(s)); n == "direct" || n == "draft" {
			return
		}
	}
	AddIssue(issues, "error", fmt.Sprintf("voice.submit_mode must be 'direct' or 'draft', got %s", PyRepr(submit)),
		"Set voice.submit_mode to direct (submit immediately) or draft (edit before sending)")
}

const timezoneHint = "Use an IANA zone name such as America/New_York or Asia/Tokyo (see " +
	"`timedatectl list-timezones`). With an invalid value the agent clock and cron " +
	"schedules silently fall back to server-local time. ICEBATH_TIMEZONE overrides " +
	"this key when set."

// Error when timezone is not a string or names a zone the tz database cannot load
func ValidateTimezone(cfg map[string]any, issues *[]ConfigIssue) {
	tz, present := cfg["timezone"]
	if !present {
		return
	}
	s, isStr := tz.(string)
	if tz != nil && !isStr {
		AddIssue(issues, "error", fmt.Sprintf("timezone must be an IANA zone name string, got %s", PyRepr(tz)), timezoneHint)
		return
	}
	name := strings.TrimSpace(s)
	if name == "" {
		return
	}
	if _, err := loadZone("Etc/UTC"); err != nil {
		return
	}
	if !loadableZone(name) {
		AddIssue(issues, "error", fmt.Sprintf("timezone %s is not a valid IANA zone name", PyRepr(name)), timezoneHint)
	}
}

// Zone loader the timezone check consults, swapped by tests to model a host without a tz database
var loadZone = time.LoadLocation

// Reports whether the tz database holds a zone file for name
func loadableZone(name string) bool {
	if name == "Local" {
		return false
	}
	_, err := loadZone(name)
	return err == nil
}

// Warning for each web backend selection naming a backend removed from the tree
func ValidateWebBackends(cfg map[string]any, issues *[]ConfigIssue, removedNote func(section, name string) string) {
	web := AsMap(cfg["web"])
	if web == nil {
		return
	}
	seen := map[string]bool{}
	for _, key := range []string{"backend", "search_backend", "extract_backend"} {
		raw := web[key]
		val := ""
		if PyTruthy(raw) {
			val = strings.ToLower(strings.TrimSpace(PyStr(raw)))
		}
		if val == "" || seen[val] {
			continue
		}
		seen[val] = true
		if note := removedNote("web", val); note != "" {
			AddIssue(issues, "warning",
				fmt.Sprintf("web.%s is set to '%s', but %s — web_search/web_extract will fail until it is changed", key, val, note),
				"Run 'icebath tools' and pick a different Web Search & Extract provider")
		}
	}
}

// Container roots absent from the defaults whose readers fix their shape
var KnownContainerTypes = map[string]string{
	"custom_providers":                 "list",
	"providers":                        "mapping",
	"model.aliases":                    "mapping",
	"model_aliases":                    "mapping",
	"plugins.enabled":                  "list",
	"plugins.disabled":                 "list",
	"model_catalog.excluded_providers": "list",
}

// List slots whose readers accept a bare name as a one item list
var ScalarAsOneItemListKeys = map[string]bool{"agent.disabled_toolsets": true, "skills.disabled": true}

// Dotted key to list or mapping for every container slot the defaults and known container table fix
func ContainerSlots() map[string]string {
	slots := map[string]string{}
	var walk func(node map[string]any, prefix string)
	walk = func(node map[string]any, prefix string) {
		for key, value := range node {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			if m := AsMap(value); m != nil {
				slots[path] = "mapping"
				walk(m, path)
			} else if value != nil && reflect.ValueOf(value).Kind() == reflect.Slice {
				slots[path] = "list"
			}
		}
	}
	walk(Defaults(), "")
	for k, v := range KnownContainerTypes {
		slots[k] = v
	}
	return slots
}

// Value at a nested key path through mappings only, false on any miss
func CfgGet(cfg map[string]any, keys ...string) (any, bool) {
	var node any = cfg
	for _, key := range keys {
		m := AsMap(node)
		if m == nil {
			return nil, false
		}
		v, ok := m[key]
		if !ok {
			return nil, false
		}
		node = v
	}
	return node, true
}

// Reports whether a string plausibly encodes a YAML or JSON list or mapping
func LooksStructuredValue(value string) bool {
	stripped := strings.TrimLeft(value, " \t\n\r\x0b\x0c")
	if strings.HasPrefix(stripped, "[") || strings.HasPrefix(stripped, "{") {
		return true
	}
	if !strings.Contains(value, "\n") {
		return false
	}
	for _, line := range pySplitLines(value) {
		item := strings.TrimSpace(line)
		if item == "-" || strings.HasPrefix(item, "- ") {
			return true
		}
		if head, _, ok := strings.Cut(item, ": "); ok && head != "" && !strings.Contains(head, " ") && !strings.HasPrefix(head, "#") {
			return true
		}
		if strings.HasSuffix(item, ":") && len(item) > 1 && !strings.Contains(item[:len(item)-1], " ") {
			return true
		}
	}
	return false
}

// Lines split at every Python line boundary without the terminators
func pySplitLines(s string) []string {
	var out []string
	start := 0
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '\r':
			out = append(out, string(runes[start:i]))
			if i+1 < len(runes) && runes[i+1] == '\n' {
				i++
			}
			start = i + 1
		case '\n', '\x0b', '\x0c', '\x1c', '\x1d', '\x1e', '\x85', ' ', ' ':
			out = append(out, string(runes[start:i]))
			start = i + 1
		}
	}
	if start < len(runes) {
		out = append(out, string(runes[start:]))
	}
	return out
}

// Warning for each container slot holding one quoted string that parses as a list or mapping
func ValidateQuotedContainers(cfg map[string]any, issues *[]ConfigIssue) {
	slots := ContainerSlots()
	keys := make([]string, 0, len(slots))
	for k := range slots {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		kind := slots[key]
		if ScalarAsOneItemListKeys[key] {
			continue
		}
		raw, _ := CfgGet(cfg, strings.Split(key, ".")...)
		value, ok := raw.(string)
		if !ok || !LooksStructuredValue(value) {
			continue
		}
		var parsed any
		if err := yaml.Unmarshal([]byte(value), &parsed); err != nil {
			continue
		}
		if AsMap(parsed) == nil {
			if _, isList := parsed.([]any); !isList {
				continue
			}
		}
		AddIssue(issues, "warning",
			fmt.Sprintf("%s is the quoted string %s — Icebath expects a YAML %s here and every reader ignores the string", key, PyRepr(value), kind),
			fmt.Sprintf("Run: icebath config set %s %s  (stores a real %s), or remove the quotes in config.yaml", key, ShlexQuote(value), kind))
	}
}
