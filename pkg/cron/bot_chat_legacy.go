package cron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var botChatLegacyWarned = struct {
	sync.Mutex
	paths map[string]bool
}{paths: map[string]bool{}}

// Marks a pending record path warned, reporting whether it already was
func botChatLegacyNoteWarned(path string) bool {
	botChatLegacyWarned.Lock()
	defer botChatLegacyWarned.Unlock()
	was := botChatLegacyWarned.paths[path]
	botChatLegacyWarned.paths[path] = true
	return was
}

func (s *Store) botChatPendingRoot() (string, error) {
	resolved, err := pypath.Resolve(s.home)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, "cron", "bot_chat_pending"), nil
}

// Parks a Bot Chat output whose target owner is not running, once per delivery key
func (s *Store) botChatLegacyDefer(key string, job *Job, content, target string, forFailure bool) error {
	root, err := s.botChatPendingRoot()
	if err != nil {
		return err
	}
	if _, err := home.MakeUserDir(root, 0o700, true, true); err != nil {
		return err
	}
	path := filepath.Join(root, key+".json")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	jobPart := pyjson.NewObject()
	if job.ID != "" || jobHasKey(job, "id") {
		jobPart.Set("id", job.ID)
	}
	if job.Name != "" || jobHasKey(job, "name") {
		jobPart.Set("name", job.Name)
	}
	record := pyjson.NewObject()
	record.Set("id", key)
	record.Set("status", "queued")
	record.Set("job", jobPart)
	record.Set("content", content)
	record.Set("home", target)
	record.Set("for_failure", forFailure)
	record.Set("sequence", time.Now().UnixNano())
	return config.AtomicJSONWriteMode(path, record, 0o600)
}

type legacyRecord struct {
	path   string
	record *pyjson.Object
}

// Still queued pending records ordered by sequence then file name, unreadable ones logged once
func (s *Store) botChatLegacyQueued(root string) []legacyRecord {
	paths, _ := filepath.Glob(filepath.Join(root, "*.json"))
	var found []legacyRecord
	for _, path := range paths {
		record, err := readLegacyRecord(path)
		if err != nil {
			if !botChatLegacyNoteWarned(path) {
				s.log.Error(fmt.Sprintf("Unreadable legacy Bot Chat pending record %s kept as evidence: %v", path, err))
			}
			continue
		}
		if st, _ := record.Get("status"); st == "queued" {
			found = append(found, legacyRecord{path, record})
		}
	}
	sort.SliceStable(found, func(i, j int) bool {
		si, sj := recordSequence(found[i].record), recordSequence(found[j].record)
		if si != sj {
			return si < sj
		}
		return filepath.Base(found[i].path) < filepath.Base(found[j].path)
	})
	return found
}

func readLegacyRecord(path string) (*pyjson.Object, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := string(trimBOM(raw))
	v, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	obj, ok := v.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("expected a JSON object, got %s", pyTypeName(v))
	}
	return obj, nil
}

func recordSequence(r *pyjson.Object) float64 {
	v, ok := r.Get("sequence")
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case json.Number:
		f, _ := n.Float64()
		return f
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}

// Reason a record cannot be delivered, empty when well formed
func legacyRecordMalformed(r *pyjson.Object) string {
	var missing []string
	for _, key := range []string{"id", "home", "content"} {
		v, _ := r.Get(key)
		if str, ok := v.(string); !ok || str == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("malformed legacy bot-chat record (missing %s); not delivered", strings.Join(missing, ", "))
	}
	if jobVal, _ := r.Get("job"); pyTruthy(jobVal) {
		if _, ok := jobVal.(*pyjson.Object); !ok {
			return "malformed legacy bot-chat record (job is not an object); not delivered"
		}
	}
	return ""
}

// Hands one record to the target live owner, false when it stays queued for a later drain
func (s *Store) botChatLegacyDispose(ctx context.Context, path string, r *pyjson.Object) (bool, error) {
	if reason := legacyRecordMalformed(r); reason != "" {
		r.Set("status", "ambiguous")
		r.Set("error", reason)
		return true, nil
	}
	homeVal, _ := r.Get("home")
	target, err := pypath.Resolve(homeVal.(string))
	if err != nil {
		return false, err
	}
	forFailureVal, _ := r.Get("for_failure")
	forFailure := pyTruthy(forFailureVal)
	if st, err := os.Stat(filepath.Join(target, "state.db")); err != nil || !st.Mode().IsRegular() {
		r.Set("status", "ambiguous")
		r.Set("error", "bot-chat delivery target no longer exists: "+target)
		return true, nil
	}
	if forFailure {
		userCfg, err := config.LoadUserEffective(filepath.Join(target, "config.yaml"), nil)
		if err != nil {
			return false, err
		}
		if !gateway.WarningNotificationsEnabled(ctx, BotChatPolicyPlatform, userCfg) {
			r.Set("status", "suppressed")
			r.Set("error", nil)
			return true, nil
		}
	}
	owner, err := findCanonicalLiveOwner(ctx, target)
	var pyErr *gateway.PyError
	if errors.As(err, &pyErr) && pyErr.Kind == "ValueError" {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if owner == nil {
		return false, nil
	}
	name := "?"
	if jobVal, _ := r.Get("job"); jobVal != nil {
		if jobObj, ok := jobVal.(*pyjson.Object); ok {
			if v, ok := jobObj.Get("id"); ok {
				name = pyrepr.Str(v)
			}
			if v, ok := jobObj.Get("name"); ok {
				name = pyrepr.Str(v)
			}
		}
	}
	content, _ := r.Get("content")
	deliveryID, _ := r.Get("id")
	category := "result"
	if forFailure {
		category = "diagnostic"
	}
	receipt, err := deliverToLiveOwner(ctx, target, owner, s.botChatMessageNamed(name, content.(string)), deliveryID.(string), category)
	if err != nil {
		msg := fmt.Sprintf("Legacy Bot Chat pending record %s not handed off yet: %v", path, err)
		if botChatLegacyNoteWarned(path) {
			s.log.Debug(msg)
		} else {
			s.log.Warn(msg)
		}
		return false, nil
	}
	var receiptStatus any
	if receipt != nil {
		receiptStatus = receipt["status"]
	}
	r.Set("status", "transferred")
	r.Set("error", nil)
	r.Set("receipt_status", receiptStatus)
	return true, nil
}

// Hands every still queued legacy Bot Chat record to its target live owner, records kept as evidence
func (s *Store) drainLegacyPendingRecords(ctx context.Context) error {
	root, err := s.botChatPendingRoot()
	if err != nil {
		return err
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil
	}
	for _, item := range s.botChatLegacyQueued(root) {
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%v", r)
				}
			}()
			done, err := s.botChatLegacyDispose(ctx, item.path, item.record)
			if err != nil || !done {
				if err == nil {
					err = errNotQueued
				}
				return err
			}
			return config.AtomicJSONWriteMode(item.path, item.record, 0o600)
		}()
		if errors.Is(err, errNotQueued) {
			continue
		}
		if err != nil {
			msg := fmt.Sprintf("Legacy Bot Chat pending record %s left queued: %v", item.path, err)
			if botChatLegacyNoteWarned(item.path) {
				s.log.Debug(msg)
			} else {
				s.log.Warn(msg)
			}
			continue
		}
		st, _ := item.record.Get("status")
		s.log.Info(fmt.Sprintf("Legacy Bot Chat pending record %s -> %s", filepath.Base(item.path), pyrepr.Str(st)))
	}
	return nil
}

// Legacy pending drain the tick runs, the seam tests replace the way pytest patches the module attribute
var drainLegacyPending = func(ctx context.Context, s *Store) error { return s.drainLegacyPendingRecords(ctx) }
