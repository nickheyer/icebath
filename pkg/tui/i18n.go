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
	"github.com/nickheyer/icebath/pkg/ui/lang"
	"github.com/nickheyer/icebath/pkg/ui/txt"
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
var enLogic = map[txt.Key]func(args []any) string{
	txt.StatusSessionCount: func(args []any) string {
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

// Text of the catalog entry with a name made at run time, the fallback when the catalog holds no such entry
func trOr(name, fallback string, args ...any) string { return lang.Or(name, fallback, args...) }

// Text of a count, one key for exactly one and the other key for any other count, the count as argument 0
func trCount(one, other txt.Key, n int, args ...any) string {
	key := other
	if n == 1 {
		key = one
	}
	return tr(key, append([]any{n}, args...)...)
}

// Text of a count, nothing for none
func trSome(one, other txt.Key, n int) string {
	if n == 0 {
		return ""
	}
	return trCount(one, other, n)
}

// Line that words a failure in the active language
func errorLine(text string) string { return tr(txt.CommonError, text) }

// Line that words a warning in the active language
func warningLine(text string) string { return tr(txt.CommonWarning, text) }

// Reports whether a transcript line reads as an error, in English or in the active language
func isErrorLine(text string) bool {
	return strings.HasPrefix(text, "error:") || strings.HasPrefix(text, strings.TrimRight(tr(txt.CommonError, ""), " "))
}

// Language id of the pack in force
func activeLocale() string {
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	return catalog.lang
}

// Switches the active locale, keeping only the pack keys English has, and names it to the kit for its casing rules
func applyLocale(id string, messages map[string]string) {
	if id == "" {
		id = defaultLocale
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
	catalog.lang, catalog.pack = id, kept
	catalog.mu.Unlock()
	lang.Use(id)
}

func init() { lang.Install(tr) }

// Text of a key in the active locale, then English. Every key is an entry of the English catalog, so there is always a text
func tr(key txt.Key, args ...any) string {
	name := key.Name()
	catalog.mu.RLock()
	text, packed := catalog.pack[name]
	catalog.mu.RUnlock()
	if english, isString := enCatalog.Strings[name]; isString {
		if packed {
			return text
		}
		return english
	}
	if packed {
		return formatPositional(text, args)
	}
	if logic, coded := enLogic[key]; coded {
		return logic(args)
	}
	return formatPositional(enCatalog.Formats[name], args)
}

// English text of a key, whatever the active locale
func english(key txt.Key) string {
	if text, isString := enCatalog.Strings[key.Name()]; isString {
		return text
	}
	return enCatalog.Formats[key.Name()]
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
