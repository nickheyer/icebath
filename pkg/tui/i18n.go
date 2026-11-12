package tui

import (
	_ "embed"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	defaultLocale = "en"
	tuiSurface    = "tui"
)

//go:embed locales/en.tui.yaml
var enCatalogYAML []byte

// English catalog, literal strings and positional formats keyed by dotted path
type enCatalogFile struct {
	Strings map[string]string `yaml:"strings"`
	Formats map[string]string `yaml:"formats"`
}

var enCatalog = func() enCatalogFile {
	var c enCatalogFile
	if err := yaml.Unmarshal(enCatalogYAML, &c); err != nil {
		panic(err)
	}
	return c
}()

// English entries whose wording depends on an argument, chosen in code
var enLogic = map[string]func(args []any) string{
	"status.sessionCount": func(args []any) string {
		word := "sessions"
		if jsNumberEquals(arg(args, 0), 1) {
			word = "session"
		}
		return jsString(arg(args, 0)) + " " + word
	},
	"pickers.model.plan.limitReached": func(args []any) string {
		name := jsString(arg(args, 0))
		r, size := utf8.DecodeRuneInString(name)
		if size == 0 {
			return " limit reached"
		}
		return strings.ToUpper(string(r)) + name[size:] + " limit reached"
	},
}

var positionalRe = regexp.MustCompile(`\{(\d+)\}`)

// Argument at index, nil past the end
func arg(args []any, i int) any {
	if i < len(args) {
		return args[i]
	}
	return nil
}

// Numeric argument strictly equal to n
func jsNumberEquals(v any, n float64) bool {
	switch t := v.(type) {
	case int:
		return float64(t) == n
	case int64:
		return float64(t) == n
	case float64:
		return t == n
	}
	return false
}

// Text of a value as JavaScript String renders it
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		switch {
		case math.IsNaN(t):
			return "NaN"
		case math.IsInf(t, 1):
			return "Infinity"
		case math.IsInf(t, -1):
			return "-Infinity"
		case t == 0:
			return "0"
		case math.Abs(t) >= 1e21 || math.Abs(t) < 1e-6:
			s := strconv.FormatFloat(t, 'e', -1, 64)
			mant, exp, _ := strings.Cut(s, "e")
			if exp[0] == '+' {
				return mant + "e+" + strings.TrimLeft(exp[1:], "0")
			}
			return mant + "e-" + strings.TrimLeft(exp[1:], "0")
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

// Template with each {n} replaced by argument n, unknown indices left as written
func formatPositional(template string, args []any) string {
	return positionalRe.ReplaceAllStringFunc(template, func(m string) string {
		i, err := strconv.Atoi(m[1 : len(m)-1])
		if err != nil || i >= len(args) {
			return m
		}
		return jsString(args[i])
	})
}

// Active TUI locale with the pack entries English knows, and the loader state
type tuiCatalog struct {
	mu            sync.RWMutex
	lang          string
	pack          map[string]string
	lastRequested string
	inFlight      chan struct{}
}

var catalog = &tuiCatalog{lang: defaultLocale}

// Switches the active locale, keeping only the pack keys English has
func applyLocale(lang string, messages map[string]string) {
	if lang == "" {
		lang = defaultLocale
	}
	kept := map[string]string{}
	for k, v := range messages {
		if _, isString := enCatalog.Strings[k]; isString {
			kept[k] = v
		} else if _, isFormat := enCatalog.Formats[k]; isFormat {
			kept[k] = v
		}
	}
	catalog.mu.Lock()
	catalog.lang, catalog.pack = lang, kept
	catalog.mu.Unlock()
}

// Text of a key in the active locale, then English, then the key itself
func tr(key string, args ...any) string {
	catalog.mu.RLock()
	text, packed := catalog.pack[key]
	catalog.mu.RUnlock()
	if _, isString := enCatalog.Strings[key]; isString {
		if packed {
			return text
		}
		return enCatalog.Strings[key]
	}
	if template, isFormat := enCatalog.Formats[key]; isFormat {
		if packed {
			return formatPositional(text, args)
		}
		if logic, coded := enLogic[key]; coded {
			return logic(args)
		}
		return formatPositional(template, args)
	}
	return key
}

// Language id of display.language the way the backend normalizes it, English for anything empty or not a string
func normalizeLanguageID(raw any) string {
	s, isString := raw.(string)
	if !isString {
		return defaultLocale
	}
	id := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "_", "-")
	if id == "" {
		return defaultLocale
	}
	return id
}

// Gateway request seam of the locale loader
type requestFunc func(method string, params map[string]any) (any, error)

// TUI pack messages of a language from i18n.catalog, false when the backend lacks it or answers without messages
func fetchCatalogPack(request requestFunc, lang string) (map[string]string, bool) {
	raw, err := request("i18n.catalog", map[string]any{"lang": lang, "surface": tuiSurface})
	if err != nil {
		return nil, false
	}
	result, isDict := raw.(map[string]any)
	if !isDict {
		return nil, false
	}
	messages, isDict := result["messages"].(map[string]any)
	if !isDict {
		return nil, false
	}
	out := map[string]string{}
	for k, v := range messages {
		if s, isString := v.(string); isString {
			out[k] = s
		}
	}
	return out, true
}

// Loads and applies the pack of display.language, English needing no request and a repeat joining the one in flight
func syncTUILocale(request requestFunc, rawLanguage any) {
	lang := normalizeLanguageID(rawLanguage)
	catalog.mu.Lock()
	if lang == catalog.lastRequested && (catalog.inFlight != nil || lang == catalog.lang) {
		wait := catalog.inFlight
		catalog.mu.Unlock()
		if wait != nil {
			<-wait
		}
		return
	}
	catalog.lastRequested = lang
	if lang == defaultLocale {
		catalog.inFlight = nil
		catalog.mu.Unlock()
		applyLocale(defaultLocale, nil)
		return
	}
	done := make(chan struct{})
	catalog.inFlight = done
	catalog.mu.Unlock()
	messages, _ := fetchCatalogPack(request, lang)
	catalog.mu.Lock()
	current := catalog.lastRequested == lang
	if catalog.inFlight == done {
		catalog.inFlight = nil
	}
	catalog.mu.Unlock()
	if current {
		applyLocale(lang, messages)
	}
	close(done)
}
