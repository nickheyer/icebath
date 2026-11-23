package config

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pysys"
)

// Streams the facade prompts read from and its plain output is printed to
var (
	FacadeStdout io.Writer = pysys.Stdout
	FacadeStdin  io.Reader = os.Stdin
)

// Env variables every install must set, none since the setup wizard picks the provider
var RequiredEnvVars = map[string]EnvVar{}

// One env variable the install lacks, its catalog entry and whether it is required
type MissingEnvVar struct {
	Name string
	EnvVar
	IsRequired bool
}

// Sets a mapping key keeping the position of an existing key and appending a new one
func setOrdered(m map[string]any, key string, value any) {
	if _, ok := m[key]; !ok {
		AppendMappingKey(m, key)
	}
	m[key] = value
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

const skillConfigPrefix = "skills.config"

// Env variables with no value in the scoped process env or the home .env, required ones first
func GetMissingEnvVars(ctx context.Context, h string, requiredOnly bool) ([]MissingEnvVar, error) {
	type group struct {
		table    map[string]EnvVar
		names    []string
		required bool
	}
	groups := []group{{RequiredEnvVars, SortedEnvNames(RequiredEnvVars), true}}
	if !requiredOnly {
		groups = append(groups, group{OptionalEnvVars.Copy(), OptionalEnvVars.Names(), false})
	}
	var out []MissingEnvVar
	for _, g := range groups {
		for _, name := range g.names {
			v, _, err := GetEnvValue(ctx, h, name)
			if err != nil {
				return nil, err
			}
			if v == "" {
				out = append(out, MissingEnvVar{Name: name, EnvVar: g.table[name], IsRequired: g.required})
			}
		}
	}
	return out, nil
}

// Names of an env catalog in sorted order
func SortedEnvNames(table map[string]EnvVar) []string {
	out := make([]string, 0, len(table))
	for name := range table {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Skill declared config variables, installed by the skills owner
var skillConfigVarsSource func() ([]SkillConfigVar, error)

// Installs the reader of the skill declared config variables
func RegisterSkillConfigVars(fn func() ([]SkillConfigVar, error)) { skillConfigVarsSource = fn }

// Skill declared settings under skills.config that are missing or blank in the loaded config
func GetMissingSkillConfigVars(ctx context.Context, h string) ([]SkillConfigVar, error) {
	discover := skillConfigVarsSource
	if discover == nil {
		return nil, nil
	}
	all, err := discover()
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("discover_all_skill_config_vars failed: %v", err))
		return nil, nil
	}
	if len(all) == 0 {
		return nil, nil
	}
	cfg, err := LoadContext(ctx, h, EnvRefLookupContext(ctx))
	if err != nil {
		return nil, err
	}
	var out []SkillConfigVar
	for _, v := range all {
		value, _ := CfgGet(cfg, strings.Split(skillConfigPrefix+"."+v.Key, ".")...)
		if s, isStr := value.(string); value == nil || (isStr && strings.TrimSpace(s) == "") {
			out = append(out, v)
		}
	}
	return out, nil
}

// Decodes bytes as utf-8-sig with invalid sequences replaced and universal newlines, keeping line ends
func readEnvTextLines(raw []byte) []string {
	raw = trimUTF8BOM(raw)
	var b strings.Builder
	for len(raw) > 0 {
		r, size := utf8.DecodeRune(raw)
		if r == utf8.RuneError && size <= 1 {
			b.WriteRune(utf8.RuneError)
			raw = raw[pyInvalidUTF8Len(raw):]
			continue
		}
		b.WriteRune(r)
		raw = raw[size:]
	}
	text := strings.ReplaceAll(strings.ReplaceAll(b.String(), "\r\n", "\n"), "\r", "\n")
	return strings.SplitAfter(text, "\n")
}

func trimUTF8BOM(raw []byte) []byte {
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		return raw[3:]
	}
	return raw
}

// Bytes Python's replace handler consumes for one invalid sequence, the maximal valid prefix of a multibyte form
func pyInvalidUTF8Len(raw []byte) int {
	lead := raw[0]
	var need int
	lo, hi := byte(0x80), byte(0xBF)
	switch {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 1
	case lead == 0xE0:
		need, lo = 2, 0xA0
	case lead >= 0xE1 && lead <= 0xEC, lead == 0xEE, lead == 0xEF:
		need = 2
	case lead == 0xED:
		need, hi = 2, 0x9F
	case lead == 0xF0:
		need, lo = 3, 0x90
	case lead >= 0xF1 && lead <= 0xF3:
		need = 3
	case lead == 0xF4:
		need, hi = 3, 0x8F
	default:
		return 1
	}
	n := 1
	for i := 1; i <= need && i < len(raw); i++ {
		c := raw[i]
		if i == 1 && (c < lo || c > hi) || i > 1 && (c < 0x80 || c > 0xBF) {
			break
		}
		n++
	}
	return n
}

var (
	envWriteLockHoldersGuard sync.Mutex
	envWriteLockHolders      = map[string]*pyfs.LockHolder{}
)

// Reentrancy tracker of one .env path so multiplexed profiles each get their own
func envWriteLockHolder(envPath string) *pyfs.LockHolder {
	envWriteLockHoldersGuard.Lock()
	defer envWriteLockHoldersGuard.Unlock()
	holder, ok := envWriteLockHolders[envPath]
	if !ok {
		holder = &pyfs.LockHolder{}
		envWriteLockHolders[envPath] = holder
	}
	return holder
}

// Serializes a whole .env read modify write cycle across goroutines and processes, reentrant per goroutine
func envWriteLock(envPath string) (func(), error) {
	return pyfs.HolderFileLock(envPath+".lock", envWriteLockHolder(envPath), 15*time.Second,
		fmt.Sprintf("Timed out waiting for the environment file lock (%s)", envPath))
}

// Holds the .env write lock for the duration of fn
func withEnvWriteLock(path string, fn func() error) error {
	unlock, err := envWriteLock(path)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// Rewrites the home .env with normalized line formatting and returns how many lines changed
func SanitizeEnvFile(ctx context.Context, h string) (int, error) {
	path := GetEnvPath(h)
	if _, err := os.Stat(path); err != nil {
		return 0, nil
	}
	fixes := 0
	err := withEnvWriteLock(path, func() error {
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		original := readEnvTextLines(raw)
		if n := len(original); n > 0 && original[n-1] == "" {
			original = original[:n-1]
		}
		sanitized := SanitizeEnvLines(original)
		if slices.Equal(sanitized, original) {
			return nil
		}
		if d := len(sanitized) - len(original); d != 0 {
			fixes = max(d, -d)
		} else {
			for i := range original {
				if original[i] != sanitized[i] {
					fixes++
				}
			}
		}
		return writeEnvLinesMode(ctx, h, path, sanitized, false)
	})
	if err == nil && fixes > 0 {
		InvalidateEnvCache()
	}
	return fixes, err
}
