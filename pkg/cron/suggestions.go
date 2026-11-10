package cron

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Installs the job creator and suggestion registrar the skills blueprint bridge calls
func init() {
	skills.RegisterCronBlueprintHooks(
		func(ctx context.Context, jobSpec *pyjson.Object) (map[string]any, error) {
			job, err := openCronStore(home.Dir(ctx), time.Now).createJobWithSchedulerRegistration(ctx, jobSpec)
			if err != nil {
				return nil, err
			}
			return jobAsMap(job)
		},
		func(ctx context.Context, title, description, source string, jobSpec *pyjson.Object, dedupKey string) (map[string]any, error) {
			rec, err := openCronStore(home.Dir(ctx), time.Now).AddSuggestion(title, description, source, jobSpec, dedupKey)
			if err != nil || rec == nil {
				return nil, err
			}
			return objectAsMap(rec), nil
		})
}

// Top level keys of an ordered object as a map
func objectAsMap(obj *pyjson.Object) map[string]any {
	out := make(map[string]any, obj.Len())
	for _, k := range obj.Keys {
		out[k] = obj.Vals[k]
	}
	return out
}

// Stored record of a job as a map of Python values
func jobAsMap(job *Job) (map[string]any, error) {
	raw, err := job.MarshalJSON()
	if err != nil {
		return nil, err
	}
	decoded, err := pyjson.LoadsBytes(raw)
	if err != nil {
		return nil, err
	}
	return objectAsMap(decoded.(*pyjson.Object)), nil
}

// MaxPendingSuggestions caps the pending list so new suggestions are dropped once it is full
const MaxPendingSuggestions = 5

var validSuggestionSources = map[string]bool{"catalog": true, "blueprint": true, "usage": true, "integration": true}

const (
	suggestionPending   = "pending"
	suggestionAccepted  = "accepted"
	suggestionDismissed = "dismissed"
)

// Explicit suggestions file a test sets, empty resolves the store home at call time
var suggestionsFileOverride string

// Serialises load modify save cycles across the review fork and the main agent
var suggestionsMu sync.Mutex

// Suggestions file of the explicit override or the profile cron directory
func (s *Store) suggestionsFile() string {
	if suggestionsFileOverride != "" {
		return suggestionsFileOverride
	}
	return filepath.Join(s.home, "cron", "suggestions.json")
}

// Field of a stored record, an AttributeError for a record that is not an object
func suggestionField(rec any, key string) (any, error) {
	obj, ok := rec.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(rec))
	}
	return obj.Vals[key], nil
}

// Stored records of the suggestions file, empty for a missing, unreadable or malformed file
func (s *Store) loadSuggestionsRaw() ([]any, error) {
	path := s.suggestionsFile()
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
			return []any{}, nil
		}
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		slog.Warn(fmt.Sprintf("suggestions.json unreadable (%s); starting empty", pypath.OSErrorText(err)))
		return []any{}, nil
	}
	text := strings.TrimPrefix(string(raw), "\ufeff")
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("UnicodeDecodeError: 'utf-8' codec can't decode %s", path)
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		slog.Warn(fmt.Sprintf("suggestions.json unreadable (%s); starting empty", err))
		return []any{}, nil
	}
	if obj, ok := data.(*pyjson.Object); ok {
		if list, ok := obj.Vals["suggestions"].([]any); ok {
			return list, nil
		}
	}
	if list, ok := data.([]any); ok {
		return list, nil
	}
	slog.Warn("suggestions.json malformed; starting empty")
	return []any{}, nil
}

// Writes the records with a fresh updated_at as a private atomic file
func (s *Store) saveSuggestionsRaw(suggestions []any) error {
	path := s.suggestionsFile()
	if err := ensureCronDir(filepath.Dir(path)); err != nil {
		return err
	}
	payload := pyjson.NewObject()
	payload.Set("suggestions", suggestions)
	payload.Set("updated_at", FormatISO(s.clk.now()))
	text, err := pyjson.Dumps(payload, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	if !utf8.ValidString(text) {
		if text, err = pyjson.Dumps(payload, pyjson.Indent(2), pyjson.EnsureASCII(true)); err != nil {
			return err
		}
	}
	return config.AtomicWrite(path, []byte(text), 0o600)
}

// Every suggestion record of any status
func (s *Store) LoadSuggestions() ([]any, error) { return s.loadSuggestionsRaw() }

// Records whose status is pending, in creation order
func pendingSuggestions(suggestions []any) ([]any, error) {
	out := []any{}
	for _, rec := range suggestions {
		status, err := suggestionField(rec, "status")
		if err != nil {
			return nil, err
		}
		if status == suggestionPending {
			out = append(out, rec)
		}
	}
	return out, nil
}

// Pending suggestions oldest first
func (s *Store) ListPendingSuggestions() ([]any, error) {
	all, err := s.loadSuggestionsRaw()
	if err != nil {
		return nil, err
	}
	return pendingSuggestions(all)
}

// Registers a pending suggestion, nil when its dedup key was seen or the pending list is full
func (s *Store) AddSuggestion(title, description, source string, jobSpec any, dedupKey string) (*pyjson.Object, error) {
	if !validSuggestionSources[source] {
		return nil, fmt.Errorf("unknown suggestion source: %s", pyrepr.Repr(source))
	}
	if pystr.Strip(title) == "" || pystr.Strip(dedupKey) == "" {
		return nil, errors.New("title and dedup_key are required")
	}
	suggestionsMu.Lock()
	defer suggestionsMu.Unlock()
	suggestions, err := s.loadSuggestionsRaw()
	if err != nil {
		return nil, err
	}
	for _, existing := range suggestions {
		key, err := suggestionField(existing, "dedup_key")
		if err != nil {
			return nil, err
		}
		if key != dedupKey {
			continue
		}
		status, _ := suggestionField(existing, "status")
		if status == suggestionDismissed || status == suggestionAccepted || status == suggestionPending {
			return nil, nil
		}
	}
	pending, err := pendingSuggestions(suggestions)
	if err != nil {
		return nil, err
	}
	if len(pending) >= MaxPendingSuggestions {
		slog.Info(fmt.Sprintf("Suggestion backlog full (%d); dropping %s", MaxPendingSuggestions, pyrepr.Repr(title)))
		return nil, nil
	}
	record := pyjson.NewObject()
	record.Set("id", uuid4Hex()[:12])
	record.Set("title", pystr.Strip(title))
	record.Set("description", pystr.Strip(description))
	record.Set("source", source)
	record.Set("job_spec", jobSpec)
	record.Set("dedup_key", pystr.Strip(dedupKey))
	record.Set("status", suggestionPending)
	record.Set("created_at", FormatISO(s.clk.now()))
	if err := s.saveSuggestionsRaw(append(suggestions, record)); err != nil {
		return nil, err
	}
	return record, nil
}

// True when every rune is a digit like str.isdigit
func isDigitString(ref string) bool {
	if ref == "" {
		return false
	}
	for _, r := range ref {
		if !pystr.IsDigit(r) {
			return false
		}
	}
	return true
}

// Suggestion by id, one based pending index, or case insensitive exact title
func (s *Store) GetSuggestion(ref string) (*pyjson.Object, error) {
	suggestions, err := s.loadSuggestionsRaw()
	if err != nil {
		return nil, err
	}
	for _, rec := range suggestions {
		id, err := suggestionField(rec, "id")
		if err != nil {
			return nil, err
		}
		if id == ref {
			return rec.(*pyjson.Object), nil
		}
	}
	if isDigitString(ref) {
		pending, err := pendingSuggestions(suggestions)
		if err != nil {
			return nil, err
		}
		n, ok := pyint.Parse(ref)
		if !ok {
			return nil, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(ref))
		}
		if n.IsInt64() && n.Int64() >= 1 && n.Int64() <= int64(len(pending)) {
			return pending[n.Int64()-1].(*pyjson.Object), nil
		}
	}
	for _, rec := range suggestions {
		obj := rec.(*pyjson.Object)
		raw, present := obj.Vals["title"]
		if !present {
			raw = ""
		}
		title, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'lower'", pyrepr.TypeName(raw))
		}
		if pystr.Lower(title) == pystr.Lower(ref) {
			return obj, nil
		}
	}
	return nil, nil
}

// Stamps status and resolved_at on the record with the id, false when no record matches
func (s *Store) setSuggestionStatus(id any, status string) (bool, error) {
	suggestionsMu.Lock()
	defer suggestionsMu.Unlock()
	suggestions, err := s.loadSuggestionsRaw()
	if err != nil {
		return false, err
	}
	for _, rec := range suggestions {
		got, err := suggestionField(rec, "id")
		if err != nil {
			return false, err
		}
		if pyEqual(got, id) {
			obj := rec.(*pyjson.Object)
			obj.Set("status", status)
			obj.Set("resolved_at", FormatISO(s.clk.now()))
			return true, s.saveSuggestionsRaw(suggestions)
		}
	}
	return false, nil
}

// Python equality of two decoded JSON scalars
func pyEqual(a, b any) bool {
	if pyrepr.Repr(a) == pyrepr.Repr(b) {
		return pyrepr.TypeName(a) == pyrepr.TypeName(b) || (isPyNumber(a) && isPyNumber(b))
	}
	return false
}

// True for decoded JSON ints and floats
func isPyNumber(v any) bool {
	switch v.(type) {
	case int, int64, float64:
		return true
	}
	return pyrepr.TypeName(v) == "int"
}

// Dismisses a suggestion so its dedup key is never offered again
func (s *Store) DismissSuggestion(ref string) (bool, error) {
	rec, err := s.GetSuggestion(ref)
	if err != nil || rec == nil || rec.Len() == 0 {
		return false, err
	}
	return s.setSuggestionStatus(rec.Vals["id"], suggestionDismissed)
}

// Job creator of an accepted suggestion, the scheduler registered create by default
var acceptCreateJob = func(ctx context.Context, s *Store, spec *pyjson.Object) (*Job, error) {
	return s.createJobWithSchedulerRegistration(home.WithOverride(ctx, s.home), spec)
}

// Creates the job of a pending suggestion, merging origin, and marks the suggestion accepted
func (s *Store) AcceptSuggestion(ctx context.Context, ref string, origin map[string]any) (*Job, error) {
	rec, err := s.GetSuggestion(ref)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.Len() == 0 || rec.Vals["status"] != suggestionPending {
		return nil, nil
	}
	spec := pyjson.NewObject()
	switch js := rec.Vals["job_spec"].(type) {
	case *pyjson.Object:
		for _, k := range js.Keys {
			spec.Set(k, js.Vals[k])
		}
	case []any:
		for i, item := range js {
			pair, ok := item.([]any)
			if !ok || len(pair) != 2 {
				return nil, fmt.Errorf("ValueError: dictionary update sequence element #%d has wrong length", i)
			}
			key, ok := pair[0].(string)
			if !ok {
				return nil, errors.New("TypeError: keywords must be strings")
			}
			spec.Set(key, pair[1])
		}
	default:
		if pyrepr.Truthy(js) {
			return nil, fmt.Errorf("TypeError: '%s' object is not iterable", pyrepr.TypeName(js))
		}
	}
	if origin != nil {
		if _, present := spec.Vals["origin"]; !present {
			spec.Set("origin", origin)
		}
	}
	job, err := acceptCreateJob(home.WithOverride(ctx, s.home), s, spec)
	if err != nil {
		var regErr *CronSchedulerRegistrationError
		if errors.As(err, &regErr) {
			if _, serr := s.setSuggestionStatus(rec.Vals["id"], suggestionAccepted); serr != nil {
				return nil, serr
			}
		}
		return nil, err
	}
	if _, err := s.setSuggestionStatus(rec.Vals["id"], suggestionAccepted); err != nil {
		return nil, err
	}
	return job, nil
}

// Drops accepted records and keeps dismissed ones for their dedup key, returning the count removed
func (s *Store) ClearResolvedSuggestions() (int, error) {
	suggestionsMu.Lock()
	defer suggestionsMu.Unlock()
	suggestions, err := s.loadSuggestionsRaw()
	if err != nil {
		return 0, err
	}
	kept := []any{}
	for _, rec := range suggestions {
		status, err := suggestionField(rec, "status")
		if err != nil {
			return 0, err
		}
		if status != suggestionAccepted {
			kept = append(kept, rec)
		}
	}
	removed := len(suggestions) - len(kept)
	if removed > 0 {
		if err := s.saveSuggestionsRaw(kept); err != nil {
			return 0, err
		}
	}
	return removed, nil
}
