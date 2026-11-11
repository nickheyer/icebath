package teamspipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const defaultStoreFilename = "teams_pipeline_store.json"

var storeBuckets = []string{"subscriptions", "notification_receipts", "event_timestamps", "jobs", "sink_records"}

var nowUTC = func() time.Time { return time.Now().UTC() }

// Current UTC time as Python datetime isoformat
func utcNowISO() string { return pydatetime.ISOFormatUTC(nowUTC()) }

// Store path from the explicit value, MSGRAPH_WEBHOOK_STORE_PATH or the home default
func resolveTeamsPipelineStorePath(ctx context.Context, path string) string {
	explicit := pystr.Strip(path)
	env := pystr.Strip(os.Getenv("MSGRAPH_WEBHOOK_STORE_PATH"))
	if explicit == "" {
		explicit = env
	}
	if explicit != "" {
		return pypath.PurePathStr(explicit)
	}
	return filepath.Join(home.Dir(ctx), defaultStoreFilename)
}

// JSON backed durable store of Teams pipeline state written by atomic temp file replace
type TeamsPipelineStore struct {
	Path  string
	mu    sync.Mutex
	state map[string]*pyjson.Object
}

// Store at path loaded from disk when the file exists
func NewTeamsPipelineStore(path string) (*TeamsPipelineStore, error) {
	s := &TeamsPipelineStore{Path: pypath.PurePathStr(path), state: emptyBuckets()}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Fresh empty bucket map
func emptyBuckets() map[string]*pyjson.Object {
	out := map[string]*pyjson.Object{}
	for _, b := range storeBuckets {
		out[b] = pyjson.NewObject()
	}
	return out
}

// Loads every bucket from the store file, keeping empty buckets for a missing file or non dict document
func (s *TeamsPipelineStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.ContainsRune(s.Path, 0) {
		return nil
	}
	if _, err := os.Stat(s.Path); err != nil {
		if pypath.PathlibIgnoredError(err) {
			return nil
		}
		return err
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return err
	}
	text, err := pycodecs.Decode(b, "utf-8-sig")
	if err != nil {
		return err
	}
	if text == "" {
		text = "{}"
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		return err
	}
	doc, ok := data.(*pyjson.Object)
	if !ok {
		return nil
	}
	loaded := map[string]*pyjson.Object{}
	for _, bucket := range storeBuckets {
		v, _ := doc.Get(bucket)
		d, err := dictField(v)
		if err != nil {
			return err
		}
		loaded[bucket] = d
	}
	s.state = loaded
	return nil
}

// Writes every bucket as sorted indented JSON through a sibling temp file replace, called with mu held
func (s *TeamsPipelineStore) persist() error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	doc := pyjson.NewObject()
	for _, b := range storeBuckets {
		doc.Set(b, s.state[b])
	}
	text, err := pyjson.Dumps(doc, pyjson.Indent(2), pyjson.SortKeys(true))
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}

// Deep copy of JSON values
func deepCopy(v any) any {
	switch t := v.(type) {
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range t.Keys {
			out.Set(k, deepCopy(t.Vals[k]))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = deepCopy(x)
		}
		return out
	}
	return v
}

// Deep copy of a bucket
func (s *TeamsPipelineStore) list(bucket string) *pyjson.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	return deepCopy(s.state[bucket]).(*pyjson.Object)
}

// Deep copy of one dict record, nil when absent or not a dict
func (s *TeamsPipelineStore) get(bucket, key string) *pyjson.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := get(s.state[bucket], key).(*pyjson.Object); ok {
		return deepCopy(rec).(*pyjson.Object)
	}
	return nil
}

// Merges payload over the existing record, stamping the id field and created and updated timestamps
func (s *TeamsPipelineStore) upsert(bucket, idField, key string, payload *pyjson.Object) (*pyjson.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existingAny, present := s.state[bucket].Get(key)
	existing := pyjson.NewObject()
	if present {
		e, ok := existingAny.(*pyjson.Object)
		if !ok {
			return nil, fmt.Errorf("'%s' object is not a mapping", pyrepr.TypeName(existingAny))
		}
		existing = e
	}
	if payload == nil {
		return nil, pyError("TypeError", "'NoneType' object is not a mapping")
	}
	merged := existing.Copy()
	for _, k := range payload.Keys {
		merged.Set(k, deepCopy(payload.Vals[k]))
	}
	merged.Set(idField, key)
	if _, ok := merged.Get("created_at"); !ok {
		if c := get(existing, "created_at"); pyrepr.Truthy(c) {
			merged.Set("created_at", c)
		} else {
			merged.Set("created_at", utcNowISO())
		}
	}
	merged.Set("updated_at", utcNowISO())
	s.state[bucket].Set(key, merged)
	if err := s.persist(); err != nil {
		return nil, err
	}
	return deepCopy(merged).(*pyjson.Object), nil
}

// Every subscription record keyed by id
func (s *TeamsPipelineStore) ListSubscriptions() *pyjson.Object { return s.list("subscriptions") }

// One subscription record
func (s *TeamsPipelineStore) GetSubscription(id string) *pyjson.Object {
	return s.get("subscriptions", id)
}

// Merges a subscription record
func (s *TeamsPipelineStore) UpsertSubscription(id string, payload *pyjson.Object) (*pyjson.Object, error) {
	return s.upsert("subscriptions", "subscription_id", id, payload)
}

// Every job record keyed by id
func (s *TeamsPipelineStore) ListJobs() *pyjson.Object { return s.list("jobs") }

// One job record
func (s *TeamsPipelineStore) GetJob(id string) *pyjson.Object { return s.get("jobs", id) }

// Merges a job record
func (s *TeamsPipelineStore) UpsertJob(id string, payload *pyjson.Object) (*pyjson.Object, error) {
	return s.upsert("jobs", "job_id", id, payload)
}

// One sink record
func (s *TeamsPipelineStore) GetSinkRecord(key string) *pyjson.Object {
	return s.get("sink_records", key)
}

// Merges a sink record
func (s *TeamsPipelineStore) UpsertSinkRecord(key string, payload *pyjson.Object) (*pyjson.Object, error) {
	return s.upsert("sink_records", "sink_key", key, payload)
}

// Removes a subscription record, false when it was absent
func (s *TeamsPipelineStore) DeleteSubscription(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.state["subscriptions"].Get(id)
	if !ok {
		return false, nil
	}
	s.state["subscriptions"].Delete(id)
	if v == nil {
		return false, nil
	}
	return true, s.persist()
}

// Receipt key from the notification id, else the sha256 of its canonical JSON
func BuildNotificationReceiptKey(notification *pyjson.Object) (string, error) {
	if id := get(notification, "id"); pyrepr.Truthy(id) {
		return "id:" + pyrepr.Str(id), nil
	}
	canonical, err := pyjson.Dumps(notification, pyjson.SortKeys(true), pyjson.Separators(",", ":"))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Records a receipt once, false when the key was already seen
func (s *TeamsPipelineStore) RecordNotificationReceipt(key string, payload any, receivedAt string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state["notification_receipts"].Get(key); ok {
		return false, nil
	}
	if receivedAt == "" {
		receivedAt = utcNowISO()
	}
	if p, ok := payload.(*pyjson.Object); ok {
		payload = deepCopy(p)
	}
	s.state["notification_receipts"].Set(key, obj("received_at", receivedAt, "payload", payload))
	return true, s.persist()
}

// Record count of every bucket
func (s *TeamsPipelineStore) Stats() *pyjson.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := pyjson.NewObject()
	for _, b := range storeBuckets {
		out.Set(b, int64(s.state[b].Len()))
	}
	return out
}

// Ordered dict built from alternating keys and values
func obj(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// Value under key or nil
func get(o *pyjson.Object, key string) any {
	if o == nil {
		return nil
	}
	v, _ := o.Get(key)
	return v
}
