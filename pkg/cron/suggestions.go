package cron

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/skills"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Installs the suggestion registrar the skills blueprint bridge calls
func init() {
	skills.RegisterCronBlueprintHooks(
		func(ctx context.Context, title, description, source string, jobSpec *jsonutil.Object, dedupKey string) (map[string]any, error) {
			rec, err := openCronStore(home.Dir(ctx), time.Now).AddSuggestion(title, description, source, jobSpec, dedupKey)
			if err != nil || rec == nil {
				return nil, err
			}
			return objectAsMap(rec), nil
		})
}

// Top level keys of an ordered object as a map
func objectAsMap(obj *jsonutil.Object) map[string]any {
	out := make(map[string]any, obj.Len())
	for _, k := range obj.Keys() {
		out[k] = obj.Value(k)
	}
	return out
}

// Stored record of a job as a map of Python values
func jobAsMap(job *Job) (map[string]any, error) {
	raw, err := job.MarshalJSON()
	if err != nil {
		return nil, err
	}
	decoded, err := jsonutil.Decode(raw)
	if err != nil {
		return nil, err
	}
	return objectAsMap(decoded.(*jsonutil.Object)), nil
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
	obj, ok := rec.(*jsonutil.Object)
	if !ok {
		return nil, fmt.Errorf("a cron suggestion is %s, expected a mapping", errchain.Kind(rec))
	}
	return obj.Value(key), nil
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
		pkgLog().Warn(fmt.Sprintf("suggestions.json unreadable (%s); starting empty", errchain.OSText(err)))
		return []any{}, nil
	}
	text := strings.TrimPrefix(string(raw), "\ufeff")
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("%s is not valid UTF-8", path)
	}
	data, err := jsonutil.Decode(text)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("suggestions.json unreadable (%s); starting empty", err))
		return []any{}, nil
	}
	if obj, ok := data.(*jsonutil.Object); ok {
		if list, ok := obj.Value("suggestions").([]any); ok {
			return list, nil
		}
	}
	if list, ok := data.([]any); ok {
		return list, nil
	}
	pkgLog().Warn("suggestions.json malformed; starting empty")
	return []any{}, nil
}

// Writes the records with a fresh updated_at as a private atomic file
func (s *Store) saveSuggestionsRaw(suggestions []any) error {
	path := s.suggestionsFile()
	if err := s.ensureStoreDir(filepath.Dir(path)); err != nil {
		return err
	}
	payload := jsonutil.NewObject()
	payload.Set("suggestions", suggestions)
	payload.Set("updated_at", FormatISO(s.clk.now()))
	text, err := jsonutil.Encode(payload, jsonutil.Indent(2))
	if err != nil {
		return err
	}
	if !utf8.ValidString(text) {
		if text, err = jsonutil.Encode(payload, jsonutil.Indent(2), jsonutil.ASCII()); err != nil {
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
func (s *Store) AddSuggestion(title, description, source string, jobSpec any, dedupKey string) (*jsonutil.Object, error) {
	if !validSuggestionSources[source] {
		return nil, fmt.Errorf("unknown suggestion source: %s", anyutil.Quote(source))
	}
	dedupKey = strings.TrimSpace(dedupKey)
	if strings.TrimSpace(title) == "" || dedupKey == "" {
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
		pkgLog().Info(fmt.Sprintf("Suggestion backlog full (%d); dropping %s", MaxPendingSuggestions, anyutil.Quote(title)))
		return nil, nil
	}
	record := jsonutil.NewObject()
	record.Set("id", uuid4Hex()[:12])
	record.Set("title", strings.TrimSpace(title))
	record.Set("description", strings.TrimSpace(description))
	record.Set("source", source)
	record.Set("job_spec", jobSpec)
	record.Set("dedup_key", dedupKey)
	record.Set("status", suggestionPending)
	record.Set("created_at", FormatISO(s.clk.now()))
	if err := s.saveSuggestionsRaw(append(suggestions, record)); err != nil {
		return nil, err
	}
	return record, nil
}

// True when ref is a non-empty run of ASCII digits, the form of a one based pending index
func isDigitString(ref string) bool {
	if ref == "" {
		return false
	}
	for i := 0; i < len(ref); i++ {
		if ref[i] < '0' || ref[i] > '9' {
			return false
		}
	}
	return true
}

// Suggestion by id, one based pending index, or case insensitive exact title
func (s *Store) GetSuggestion(ref string) (*jsonutil.Object, error) {
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
			return rec.(*jsonutil.Object), nil
		}
	}
	if isDigitString(ref) {
		pending, err := pendingSuggestions(suggestions)
		if err != nil {
			return nil, err
		}
		if n, err := strconv.Atoi(ref); err == nil && n >= 1 && n <= len(pending) {
			return pending[n-1].(*jsonutil.Object), nil
		}
	}
	for _, rec := range suggestions {
		obj := rec.(*jsonutil.Object)
		raw, present := obj.Get("title")
		if !present {
			raw = ""
		}
		title, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("a cron suggestion title is %s, expected text", errchain.Kind(raw))
		}
		if strings.ToLower(title) == strings.ToLower(ref) {
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
		if anyutil.Equal(got, id) {
			obj := rec.(*jsonutil.Object)
			obj.Set("status", status)
			obj.Set("resolved_at", FormatISO(s.clk.now()))
			return true, s.saveSuggestionsRaw(suggestions)
		}
	}
	return false, nil
}

// True for decoded JSON ints and floats
func isPyNumber(v any) bool {
	switch v.(type) {
	case int, int64, float64:
		return true
	}
	return anyutil.Kind(v) == "int"
}

// Dismisses a suggestion so its dedup key is never offered again
func (s *Store) DismissSuggestion(ref string) (bool, error) {
	rec, err := s.GetSuggestion(ref)
	if err != nil || rec == nil || rec.Len() == 0 {
		return false, err
	}
	return s.setSuggestionStatus(rec.Value("id"), suggestionDismissed)
}

// Job creator of an accepted suggestion, the scheduler registered create by default
var acceptCreateJob = func(ctx context.Context, s *Store, spec *jsonutil.Object) (*Job, error) {
	return s.createJobWithSchedulerRegistration(home.WithOverride(ctx, s.home), spec)
}

// Creates the job of a pending suggestion, merging origin, and marks the suggestion accepted
func (s *Store) AcceptSuggestion(ctx context.Context, ref string, origin map[string]any) (*Job, error) {
	rec, err := s.GetSuggestion(ref)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.Len() == 0 || rec.Value("status") != suggestionPending {
		return nil, nil
	}
	spec := jsonutil.NewObject()
	switch js := rec.Value("job_spec").(type) {
	case *jsonutil.Object:
		for _, k := range js.Keys() {
			spec.Set(k, js.Value(k))
		}
	case []any:
		for i, item := range js {
			pair, ok := item.([]any)
			if !ok || len(pair) != 2 {
				return nil, fmt.Errorf("job spec entry %d of the suggestion is not a key and value pair", i)
			}
			key, ok := pair[0].(string)
			if !ok {
				return nil, errors.New("the suggestion job spec keys must be text")
			}
			spec.Set(key, pair[1])
		}
	default:
		if anyutil.IsTrue(js) {
			return nil, fmt.Errorf("the suggestion job spec is %s, expected a mapping", errchain.Kind(js))
		}
	}
	if origin != nil {
		if _, present := spec.Get("origin"); !present {
			spec.Set("origin", origin)
		}
	}
	job, err := acceptCreateJob(home.WithOverride(ctx, s.home), s, spec)
	if err != nil {
		var regErr *CronSchedulerRegistrationError
		if errors.As(err, &regErr) {
			if _, serr := s.setSuggestionStatus(rec.Value("id"), suggestionAccepted); serr != nil {
				return nil, serr
			}
		}
		return nil, err
	}
	if _, err := s.setSuggestionStatus(rec.Value("id"), suggestionAccepted); err != nil {
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
