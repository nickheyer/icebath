package files

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

const OutlineName = "Outline"

const outlineSchema = `{"type":"object","properties":{
"path":{"type":"string","description":"File or directory to outline, the working directory when empty"},
"pattern":{"type":"string","description":"Regular expression a definition line must match, such as a symbol name"}},
"additionalProperties":false}`

// Structured outcome of Outline
type OutlineData struct {
	Path    string
	Files   int
	Symbols int
	// Files passed over for having no rule or being binary
	Skipped int
}

// Definition line patterns by file extension
var outlineRules = map[string]*regexp.Regexp{
	".go":       regexp.MustCompile(`^(func|type|var|const)\s`),
	".py":       regexp.MustCompile(`^\s*(async\s+def|def|class)\s`),
	".pyi":      regexp.MustCompile(`^\s*(async\s+def|def|class)\s`),
	".js":       jsRule,
	".jsx":      jsRule,
	".mjs":      jsRule,
	".cjs":      jsRule,
	".ts":       jsRule,
	".tsx":      jsRule,
	".mts":      jsRule,
	".cts":      jsRule,
	".rs":       regexp.MustCompile(`^\s*(pub(\([^)]*\))?\s+)?(unsafe\s+)?(async\s+)?(fn|struct|enum|trait|impl|mod|type|const|static|macro_rules!)\s`),
	".rb":       regexp.MustCompile(`^\s*(def|class|module)\s`),
	".java":     jvmRule,
	".kt":       jvmRule,
	".kts":      jvmRule,
	".scala":    jvmRule,
	".cs":       jvmRule,
	".swift":    jvmRule,
	".c":        cRule,
	".h":        cRule,
	".cc":       cRule,
	".cpp":      cRule,
	".cxx":      cRule,
	".hh":       cRule,
	".hpp":      cRule,
	".m":        cRule,
	".mm":       cRule,
	".sh":       regexp.MustCompile(`^\s*(function\s+\w+|\w+\s*\(\))`),
	".bash":     regexp.MustCompile(`^\s*(function\s+\w+|\w+\s*\(\))`),
	".zsh":      regexp.MustCompile(`^\s*(function\s+\w+|\w+\s*\(\))`),
	".lua":      regexp.MustCompile(`^\s*(local\s+)?function\s`),
	".ex":       regexp.MustCompile(`^\s*(def|defp|defmodule|defmacro|defstruct|defprotocol|defimpl)\s`),
	".exs":      regexp.MustCompile(`^\s*(def|defp|defmodule|defmacro|defstruct|defprotocol|defimpl)\s`),
	".php":      regexp.MustCompile(`^\s*(abstract\s+|final\s+)?(class|interface|trait|function|enum)\s|^\s*(public|private|protected|static|\s)+function\s`),
	".md":       regexp.MustCompile(`^#{1,6}\s`),
	".markdown": regexp.MustCompile(`^#{1,6}\s`),
	".sql":      regexp.MustCompile(`(?i)^\s*create\s+(or\s+replace\s+)?(table|view|function|procedure|index|type|trigger)\s`),
	".proto":    regexp.MustCompile(`^\s*(message|service|enum|rpc)\s`),
	".tf":       regexp.MustCompile(`^(resource|module|variable|output|data|provider|locals)\b`),
	".yaml":     regexp.MustCompile(`^[A-Za-z_][\w-]*:`),
	".yml":      regexp.MustCompile(`^[A-Za-z_][\w-]*:`),
	".toml":     regexp.MustCompile(`^\[`),
}

var (
	jsRule  = regexp.MustCompile(`^\s*(export\s+)?(default\s+)?(declare\s+)?(abstract\s+)?(async\s+)?(function\*?|class|interface|type|enum|namespace|const|let|var)\s+[A-Za-z_$]`)
	jvmRule = regexp.MustCompile(`^\s*(public|private|protected|internal|static|final|abstract|override|open|sealed|data|async|partial|virtual|readonly|\s)*(class|interface|enum|struct|object|trait|fun|func|def|record|extension|protocol)\s+\w|^\s{0,4}(public|private|protected|static|final|abstract|synchronized|override|\s)+[\w<>\[\],\s]+\s+\w+\s*\(`)
	cRule   = regexp.MustCompile(`^[A-Za-z_][\w\s\*&:<>,]*\b\w+\s*\([^;]*$|^\s*(struct|class|enum|union|typedef|namespace|template)\s|^#define\s`)
)

type outliner struct{ Options }

// Tool that lists definition lines of a file or tree with line numbers
func Outline(o Options) tool.Tool { return outliner{o} }

func (o outliner) Definition() protocol.Tool {
	exts := make([]string, 0, len(outlineRules))
	for ext := range outlineRules {
		exts = append(exts, ext)
	}
	slices.Sort(exts)
	return protocol.Tool{
		Name:        OutlineName,
		Description: fmt.Sprintf("Lists the definitions of a file or every file under a directory, such as functions, types, classes and headings, each with its line number, in place of grepping for them by hand. Files are listed in path order. Known file types are %s. Output is cut at %d bytes.", strings.Join(exts, " "), maxOutput),
		Parameters:  json.RawMessage(outlineSchema),
	}
}

func (outliner) Annotations() tool.Annotations {
	return tool.Annotations{ReadOnly: true, Idempotent: true}
}

func (o outliner) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
	}
	if err := decode(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	var filter *regexp.Regexp
	if in.Pattern != "" {
		re, err := regexp.Compile(in.Pattern)
		if err != nil {
			return tool.Errorf("invalid pattern: %v", err), nil
		}
		filter = re
	}
	if in.Path == "" {
		in.Path = "."
	}
	target, err := o.Resolve(in.Path)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	st, err := os.Stat(target)
	if err != nil {
		return tool.Errorf("%s does not exist", target), nil
	}
	data := OutlineData{Path: target}
	var sb strings.Builder
	if st.IsDir() {
		err = o.walk(target, false, func(p string, d fs.DirEntry) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			o.file(p, p, filter, &data, &sb)
			return nil
		})
		if err != nil {
			return tool.Error(err.Error()), nil
		}
	} else {
		if _, ok := outlineRules[strings.ToLower(filepath.Ext(target))]; !ok {
			return tool.Errorf("%s has no outline rules, Grep it instead", target), nil
		}
		o.file(target, "", filter, &data, &sb)
	}
	text := strings.TrimRight(sb.String(), "\n")
	if total := len(text); total > maxOutput {
		text = text[:maxOutput]
		if i := strings.LastIndexByte(text, '\n'); i > 0 {
			text = text[:i]
		}
		text += fmt.Sprintf("\n[output cut at %d of %d bytes, narrow with path or pattern]", maxOutput, total)
	}
	if text == "" {
		text = fmt.Sprintf("no definitions found in %s", target)
	}
	return tool.Text(text).With(data), nil
}

// Appends the definition lines of one file, prefixing each with the label when one is given
func (o outliner) file(p, label string, filter *regexp.Regexp, data *OutlineData, sb *strings.Builder) {
	rule, ok := outlineRules[strings.ToLower(filepath.Ext(p))]
	if !ok {
		data.Skipped++
		return
	}
	f, err := os.Open(p)
	if err != nil {
		data.Skipped++
		return
	}
	defer f.Close()
	head, err := peek(f)
	if err != nil || isBinary(head) {
		data.Skipped++
		return
	}
	n, found := 0, 0
	err = lines(f, func(line string) bool {
		n++
		if !rule.MatchString(line) || (filter != nil && !filter.MatchString(line)) {
			return true
		}
		found++
		if label != "" {
			fmt.Fprintf(sb, "%s:%d: %s\n", label, n, strings.TrimSpace(line))
		} else {
			fmt.Fprintf(sb, "%d: %s\n", n, strings.TrimSpace(line))
		}
		return true
	})
	if err != nil {
		data.Skipped++
		return
	}
	data.Files++
	data.Symbols += found
}
