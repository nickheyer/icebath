package tui

import (
	_ "embed"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/tuigateway"
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
		if n, isNumber := argNumber(arg(args, 0)); isNumber && n == 1 {
			word = "session"
		}
		return argText(arg(args, 0)) + " " + word
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

// Value of a numeric catalog argument, false for anything else
func argNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	}
	return 0, false
}

// Text of a catalog argument, numbers in their shortest decimal form
func argText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// Template with each {n} replaced by argument n, unknown indices left as written
func formatPositional(template string, args []any) string {
	return positionalRe.ReplaceAllStringFunc(template, func(m string) string {
		i, err := strconv.Atoi(m[1 : len(m)-1])
		if err != nil || i >= len(args) {
			return m
		}
		return argText(args[i])
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

// Language id of display.language the way the backend normalizes it, English for an empty one
func normalizeLanguageID(language string) string {
	id := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(language)), "_", "-")
	if id == "" {
		return defaultLocale
	}
	return id
}

// Gateway request seam of the locale loader
type requestFunc func(method string, params map[string]any) (any, error)

// TUI pack messages of a language from i18n.catalog, false when the backend lacks it or answers without messages
func fetchCatalogPack(request requestFunc, lang string) (map[string]string, bool) {
	params := tuigateway.I18nCatalogParams{Lang: lang, Surface: tuigateway.LocaleSurface(tuiSurface)}
	raw, err := request(tuigateway.I18nCatalog.Name(), wireParams(params))
	if err != nil {
		return nil, false
	}
	result, err := tuigateway.I18nCatalog.Result(raw)
	if err != nil || result.Messages == nil {
		return nil, false
	}
	return result.Messages, true
}

// Loads and applies the pack of display.language, English needing no request and a repeat joining the one in flight
func syncTUILocale(request requestFunc, language string) {
	lang := normalizeLanguageID(language)
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
