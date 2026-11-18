package googlechat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Classified service account load failure, the kind selecting the message
type saCredentialError struct {
	kind   string
	detail error
}

func (e *saCredentialError) Error() string { return e.kind }

// Connect time messages of each service account failure kind
var saErrorMessages = map[string]string{
	"inline_invalid": "Inline SA JSON is not valid JSON: %v",
	"not_found":      "Service Account JSON file not found at configured path.",
	"file_invalid":   "Service Account JSON file is not valid JSON: %v",
	"adc_foreign": "Google Chat ADC skipped for this profile: service-account credentials are set in the process " +
		"environment but not in this profile's secret scope. Set GOOGLE_CHAT_SERVICE_ACCOUNT_JSON in this profile's .env.",
	"adc_failed": "No Service Account credentials configured and Application Default Credentials are unavailable. Set " +
		"GOOGLE_CHAT_SERVICE_ACCOUNT_JSON or run ``gcloud auth application-default login``. ADC error: %v",
}

// Standalone send details of each service account failure kind
var standaloneSAErrors = map[string]string{
	"inline_invalid": "inline SA JSON is invalid: %v",
	"not_found":      "SA JSON file not found at %s",
	"file_invalid":   "SA JSON file is invalid: %v",
	"adc_foreign":    "ADC skipped for this profile: service-account credentials are set in the process environment but not in this profile's secret scope",
	"adc_failed":     "no SA credentials configured and Application Default Credentials are unavailable: %v",
}

// Message of a classified failure for the given table, the detail or path filled in
func saErrorText(table map[string]string, err *saCredentialError, path string) string {
	format := table[err.kind]
	switch {
	case err.kind == "not_found" && strings.Contains(format, "%s"):
		return fmt.Sprintf(format, path)
	case strings.Contains(format, "%v"):
		return fmt.Sprintf(format, err.detail)
	}
	return format
}

// Whether ADC would read another profile's service account from the process env under multiplexing
func adcWouldBorrowForeignCredentials(ctx context.Context) bool {
	return provider.IsMultiplexActive(ctx) &&
		(os.Getenv("GOOGLE_CHAT_SERVICE_ACCOUNT_JSON") != "" || os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "")
}

// Service account credential from a path or inline JSON, application default credentials when unset
func loadSACredentialsFrom(ctx context.Context, saValue string) (*provider.GoogleCredentials, error) {
	if saValue != "" {
		var info map[string]any
		if strings.HasPrefix(strings.TrimLeft(saValue, " \t\r\n\f\v"), "{") {
			if err := json.Unmarshal([]byte(saValue), &info); err != nil {
				return nil, &saCredentialError{"inline_invalid", err}
			}
		} else if _, err := os.Stat(saValue); err != nil {
			return nil, &saCredentialError{"not_found", nil}
		} else {
			raw, err := os.ReadFile(saValue)
			if err != nil {
				return nil, err
			}
			text, err := pystr.DecodeUTF8Sig(raw)
			if err != nil {
				return nil, &saCredentialError{"file_invalid", err}
			}
			if err := json.Unmarshal([]byte(text), &info); err != nil {
				return nil, &saCredentialError{"file_invalid", err}
			}
		}
		return provider.GoogleServiceAccountCredentials(info, chatScopes)
	}
	if adcWouldBorrowForeignCredentials(ctx) {
		return nil, &saCredentialError{"adc_foreign", nil}
	}
	creds, _, err := provider.GoogleDefaultCredentials(ctx, chatScopes)
	if err != nil {
		return nil, &saCredentialError{"adc_failed", err}
	}
	return creds, nil
}

// Persisted inbound counter per space and thread driving the DM main flow versus side thread heuristic
type threadCountStore struct {
	mu     sync.Mutex
	path   string
	counts map[string]map[string]int
}

// Store at the thread count file of the home
func newThreadCountStore(ctx context.Context) *threadCountStore {
	return &threadCountStore{path: filepath.Join(home.Dir(ctx), "google_chat_thread_counts.json"), counts: map[string]map[string]int{}}
}

// Loads counts, an absent file reading empty and a corrupt one empty with a warning
func (s *threadCountStore) load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts = map[string]map[string]int{}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("[GoogleChat] could not read thread-count store at %s: %v", s.path, err))
		return
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("[GoogleChat] thread-count store at %s is corrupt; starting fresh: %v", s.path, err))
		return
	}
	if strings.TrimSpace(text) == "" {
		return
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		var anyData any
		if json.Unmarshal([]byte(text), &anyData) == nil {
			return
		}
		pkgLog().Warn(fmt.Sprintf("[GoogleChat] thread-count store at %s is corrupt; starting fresh: %v", s.path, err))
		return
	}
	for chatID, threadsRaw := range data {
		threads, ok := threadsRaw.(map[string]any)
		if !ok {
			continue
		}
		clean := map[string]int{}
		for thread, c := range threads {
			if f, ok := c.(float64); ok && f == float64(int(f)) {
				clean[thread] = int(f)
			}
		}
		if len(clean) > 0 {
			s.counts[chatID] = clean
		}
	}
}

// Increments and writes through, answering the count before the increment
func (s *threadCountStore) incr(chatID, thread string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	chat := s.counts[chatID]
	if chat == nil {
		chat = map[string]int{}
		s.counts[chatID] = chat
	}
	prev := chat[thread]
	chat[thread] = prev + 1
	s.saveLocked()
	return prev
}

// Undoes one increment for an inbound that was not admitted, never below zero
func (s *threadCountStore) decr(chatID, thread string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if chat := s.counts[chatID]; chat != nil && chat[thread] > 0 {
		chat[thread]--
		s.saveLocked()
	}
}

// Atomic compact write, a failure only logged
func (s *threadCountStore) saveLocked() {
	data, err := json.Marshal(s.counts)
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(s.path), 0o755); err == nil {
			tmp := s.path + ".tmp"
			if err = os.WriteFile(tmp, data, 0o644); err == nil {
				err = os.Rename(tmp, s.path)
			}
		}
	}
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("[GoogleChat] could not persist thread-count store to %s: %v", s.path, err))
	}
}
