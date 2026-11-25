package tui

import (
	_ "embed"
	"fmt"
	"regexp"
	"strconv"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/ui/lang"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

const defaultLocale = "en"

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
		word := "more sessions"
		if n, isNumber := argNumber(arg(args, 0)); isNumber && n == 1 {
			word = "more session"
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

// Active TUI locale with the pack entries English knows
type tuiCatalog struct {
	mu   sync.RWMutex
	lang string
	pack map[string]string
}

var catalog = &tuiCatalog{lang: defaultLocale}

// Language switches the active locale, keeping only the pack keys English has, and names it to the kit for its casing rules
func Language(id string, messages map[string]string) {
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

// Text of a key in the active locale, then English, its {key:action.id} placeholders naming the keys bound now. Every key is an entry of the English
// catalog, so there is always a text
func tr(key txt.Key, args ...any) string { return lang.Keys(catalogText(key, args...)) }

// Text of a key in the active locale, then English, as the catalog words it
func catalogText(key txt.Key, args ...any) string {
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
