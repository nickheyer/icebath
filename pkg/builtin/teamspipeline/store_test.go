package teamspipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func TestStorePersistsSubscriptionEventAndJobState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "teams-store.json")
	store, err := NewTeamsPipelineStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertSubscription("sub-1", obj("client_state", "abc", "resource", "communications/onlineMeetings")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertJob("job-1", obj("status", "received", "event_id", "evt-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertSinkRecord("notion:meeting-1", obj("page_id", "page-1")); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewTeamsPipelineStore(path)
	if err != nil {
		t.Fatal(err)
	}
	sub, job, sink := reloaded.GetSubscription("sub-1"), reloaded.GetJob("job-1"), reloaded.GetSinkRecord("notion:meeting-1")
	if sub == nil || get(sub, "subscription_id") != "sub-1" || get(sub, "client_state") != "abc" {
		t.Fatal(sub)
	}
	if job == nil || get(job, "status") != "received" || sink == nil || get(sink, "page_id") != "page-1" || get(sink, "sink_key") != "notion:meeting-1" {
		t.Fatal(job, sink)
	}
	if get(sub, "created_at") == nil || get(sub, "updated_at") == nil {
		t.Fatal(sub.Keys)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), "{\n  \"event_timestamps\": {},\n  \"jobs\": {") {
		t.Fatal(string(b))
	}
	st := reloaded.Stats()
	if strings.Join(st.Keys, ",") != "subscriptions,notification_receipts,event_timestamps,jobs,sink_records" || get(st, "jobs") != int64(1) {
		t.Fatal(st.Keys, st.Vals)
	}
}

func TestStoreUpsertMergesAndKeepsCreatedAt(t *testing.T) {
	store, _ := NewTeamsPipelineStore(filepath.Join(t.TempDir(), "s.json"))
	first, _ := store.UpsertJob("j", obj("status", "received", "retry", int64(0)))
	second, _ := store.UpsertJob("j", obj("status", "completed"))
	if get(second, "created_at") != get(first, "created_at") || get(second, "retry") != int64(0) || get(second, "status") != "completed" {
		t.Fatal(second.Vals)
	}
	third, _ := store.UpsertJob("k", obj("created_at", nil))
	if v, ok := third.Get("created_at"); !ok || v != nil {
		t.Fatal(third.Vals)
	}
	third.Set("status", "mutated")
	if get(store.GetJob("k"), "status") != nil {
		t.Fatal("store returned an alias")
	}
	if ok, _ := store.DeleteSubscription("missing"); ok {
		t.Fatal("deleted a missing subscription")
	}
	store.UpsertSubscription("s", obj("resource", "r"))
	if ok, err := store.DeleteSubscription("s"); !ok || err != nil || store.GetSubscription("s") != nil {
		t.Fatal(ok, err)
	}
}

func TestNotificationReceiptsDedupe(t *testing.T) {
	store, _ := NewTeamsPipelineStore(filepath.Join(t.TempDir(), "s.json"))
	key, _ := BuildNotificationReceiptKey(obj("id", "n1", "x", int64(1)))
	if key != "id:n1" {
		t.Fatal(key)
	}
	hashed, _ := BuildNotificationReceiptKey(obj("resource", "r", "changeType", "created"))
	if hashed != "sha256:"+sha256Hex(`{"changeType":"created","resource":"r"}`) {
		t.Fatal(hashed)
	}
	if ok, _ := store.RecordNotificationReceipt(hashed, obj("a", int64(1)), "t0"); !ok {
		t.Fatal("first receipt refused")
	}
	if ok, _ := store.RecordNotificationReceipt(hashed, nil, ""); ok {
		t.Fatal("duplicate accepted")
	}
	rec := get(store.list("notification_receipts"), hashed).(*pyjson.Object)
	if get(rec, "received_at") != "t0" || get(rec, "payload").(*pyjson.Object).Len() != 1 {
		t.Fatal(rec.Vals)
	}
}

func TestStoreLoadToleratesBOMEmptyAndNonDictDocuments(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"bom.json": "\xef\xbb\xbf{\"jobs\": {\"j\": {\"status\": \"x\"}}}", "empty.json": "", "list.json": "[1]"} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		s, err := NewTeamsPipelineStore(p)
		if err != nil {
			t.Fatal(name, err)
		}
		if name == "bom.json" && get(s.GetJob("j"), "status") != "x" {
			t.Fatal(s.ListJobs())
		}
	}
	p := filepath.Join(dir, "bad.json")
	os.WriteFile(p, []byte("{"), 0o644)
	if _, err := NewTeamsPipelineStore(p); err == nil {
		t.Fatal("malformed store loaded")
	}
}

func TestResolveStorePath(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("MSGRAPH_WEBHOOK_STORE_PATH", "")
	ctx := context.Background()
	if got := resolveTeamsPipelineStorePath(ctx, ""); got != filepath.Join(h, "teams_pipeline_store.json") {
		t.Fatal(got)
	}
	t.Setenv("MSGRAPH_WEBHOOK_STORE_PATH", " /tmp/x//store.json ")
	if got := resolveTeamsPipelineStorePath(ctx, ""); got != "/tmp/x/store.json" {
		t.Fatal(got)
	}
	if got := resolveTeamsPipelineStorePath(ctx, "/a/b.json"); got != "/a/b.json" {
		t.Fatal(got)
	}
}

func TestModelsRoundTripSnakeAndCamelPayloads(t *testing.T) {
	sub, err := GraphSubscriptionFromDict(obj("id", " sub-1 ", "resource", "communications/onlineMeetings", "changeType", "created",
		"notificationUrl", "https://x/hook", "expirationDateTime", "2026-01-01T10:00:00.5+02:00", "clientState", "abc"))
	if err != nil {
		t.Fatal(err)
	}
	d := sub.ToDict()
	if strings.Join(d.Keys, ",") != "subscription_id,resource,change_type,notification_url,expiration_datetime,client_state" ||
		get(d, "subscription_id") != "sub-1" || get(d, "expiration_datetime") != "2026-01-01T08:00:00.500000Z" {
		t.Fatal(d.Keys, d.Vals)
	}
	if _, err := GraphSubscriptionFromDict(obj("id", "s", "resource", "r", "changeType", "c", "notificationUrl", "u")); err == nil || err.Error() != "GraphSubscription.expiration_datetime is required." {
		t.Fatal(err)
	}
	if _, err := GraphSubscriptionFromDict(obj("id", "  ")); err == nil || err.Error() != "GraphSubscription.subscription_id is required." {
		t.Fatal(err)
	}
	if _, err := MeetingArtifactFromDict(obj("artifact_type", "video", "id", "a")); err == nil || err.Error() != "MeetingArtifact.artifact_type must be transcript, recording, or call_record." {
		t.Fatal(err)
	}
	art, err := MeetingArtifactFromDict(obj("artifactType", "transcript", "id", "t1", "name", "T", "size", "12", "createdDateTime", "2026-01-01T00:00:00"))
	if err != nil || *art.SizeBytes != 12 || get(art.ToDict(), "created_at") != "2026-01-01T00:00:00Z" || get(art.ToDict(), "display_name") != "T" {
		t.Fatal(err, art.ToDict().Vals)
	}
	if _, ok := art.ToDict().Get("metadata"); ok {
		t.Fatal("empty metadata kept")
	}
	if _, err := TeamsMeetingSummaryPayloadFromDict(obj("title", "x")); err == nil {
		t.Fatal("summary without meeting_ref built")
	}
	sp, err := TeamsMeetingSummaryPayloadFromDict(obj("meeting_ref", obj("id", "m1", "joinWebUrl", "https://teams/x"), "keyDecisions", []any{"a"},
		"source_artifacts", []any{obj("artifact_type", "recording", "artifact_id", "r1")}, "startTime", "2026-01-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	sd := sp.ToDict()
	if strings.Join(sd.Keys, ",") != "meeting_ref,start_time,key_decisions,source_artifacts" || get(get(sd, "meeting_ref").(*pyjson.Object), "join_web_url") != "https://teams/x" {
		t.Fatal(sd.Keys)
	}
	job, err := TeamsMeetingPipelineJobFromDict(obj("jobId", "j", "eventId", "e", "sourceEventType", "created", "dedupeKey", "d", "status", "received",
		"retryCount", nil, "meeting_ref", obj("meeting_id", "m1"), "summary_payload", sd))
	if err != nil || job.RetryCount != 0 || job.MeetingRef.MeetingID != "m1" || job.SummaryPayload.MeetingRef.MeetingID != "m1" {
		t.Fatal(err, job)
	}
	if jd := job.ToDict(); strings.Join(jd.Keys, ",") != "job_id,event_id,source_event_type,dedupe_key,status,retry_count,meeting_ref,summary_payload" {
		t.Fatal(jd.Keys)
	}
	if _, err := TeamsMeetingPipelineJobFromDict(obj("job_id", "j")); err == nil || err.Error() != "TeamsMeetingPipelineJob.event_id is required." {
		t.Fatal(err)
	}
	if _, err := parseDatetime("not a date"); err == nil || err.Error() != "Invalid isoformat string: 'not a date'" {
		t.Fatal(err)
	}
	if camel("organizer_user_id") != "organizerUserId" {
		t.Fatal(camel("organizer_user_id"))
	}
}

type fakeGraph struct {
	subs    []any
	patched []string
}

func (f *fakeGraph) GetJSON(context.Context, string, map[string]any, map[string]string) (any, error) {
	return nil, nil
}
func (f *fakeGraph) PostJSON(context.Context, string, any, map[string]string) (any, error) {
	return nil, nil
}
func (f *fakeGraph) Delete(context.Context, string, map[string]string) (map[string]any, error) {
	return nil, nil
}
func (f *fakeGraph) DownloadToFile(context.Context, string, string, map[string]string) (map[string]any, error) {
	return nil, nil
}
func (f *fakeGraph) CollectPaginated(_ context.Context, path string, _ map[string]any, _ map[string]string) ([]any, error) {
	return f.subs, nil
}
func (f *fakeGraph) PatchJSON(_ context.Context, path string, body any, _ map[string]string) (any, error) {
	f.patched = append(f.patched, path)
	return obj("id", "ignored", "expirationDateTime", get(body.(*pyjson.Object), "expirationDateTime"), "status", "patched"), nil
}

func TestMaintainGraphSubscriptionsRenewsDueManagedSubscriptions(t *testing.T) {
	fixed := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	prev := nowUTC
	nowUTC = func() time.Time { return fixed }
	t.Cleanup(func() { nowUTC = prev })
	store, _ := NewTeamsPipelineStore(filepath.Join(t.TempDir(), "s.json"))
	store.UpsertSubscription("gone", obj("resource", "r"))
	sub := func(id, exp, state string) *pyjson.Object {
		return obj("id", id, "resource", "communications/onlineMeetings", "changeType", "created", "notificationUrl", "https://h", "expirationDateTime", exp, "clientState", state)
	}
	g := &fakeGraph{subs: []any{
		sub("due", "2026-03-01T20:00:00Z", "secret"),
		sub("later", "2026-03-04T12:00:00Z", "secret"),
		sub("expired", "2026-03-01T11:00:00Z", "secret"),
		sub("foreign", "2026-03-01T13:00:00Z", "other"),
		"not a dict",
		obj("id", ""),
	}}
	cs := "secret"
	out, err := maintainGraphSubscriptions(context.Background(), g, store, 24, 48, false, &cs)
	if err != nil {
		t.Fatal(err)
	}
	if get(out, "remote_subscription_count") != int64(6) || get(out, "synced_subscription_count") != int64(3) || get(out, "renewed_count") != int64(1) || get(out, "candidate_count") != int64(1) {
		t.Fatal(out.Vals)
	}
	cand := get(out, "candidates").([]any)[0].(*pyjson.Object)
	if get(cand, "new_expiration") != "2026-03-03T20:00:00Z" || get(cand, "current_expiration") != "2026-03-01T20:00:00Z" || len(g.patched) != 1 || g.patched[0] != "/subscriptions/due" {
		t.Fatal(cand.Vals, g.patched)
	}
	reasons := []string{}
	for _, s := range get(out, "skipped").([]any) {
		reasons = append(reasons, get(s.(*pyjson.Object), "subscription_id").(string)+"="+get(s.(*pyjson.Object), "reason").(string))
	}
	if strings.Join(reasons, ",") != "later=not_due,expired=already_expired,foreign=not_managed_by_teams_pipeline" {
		t.Fatal(reasons)
	}
	due := store.GetSubscription("due")
	if get(due, "status") != "active" || get(due, "latest_renewal_at") != "2026-03-01T12:00:00Z" || get(due, "expiration_datetime") != "2026-03-03T20:00:00Z" {
		t.Fatal(due.Vals)
	}
	if get(store.GetSubscription("expired"), "status") != "expired" || get(store.GetSubscription("gone"), "status") != "missing_remote" {
		t.Fatal(store.ListSubscriptions())
	}
	dry, _ := maintainGraphSubscriptions(context.Background(), g, store, 0, 0, true, &cs)
	if get(dry, "threshold_hours") != int64(1) || get(dry, "dry_run") != true || len(g.patched) != 1 {
		t.Fatal(dry.Vals)
	}
	if expectedClientState(nil) != nil {
		t.Fatal("client state from an unset env")
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestStoreLoadTreatsUnreachablePathAsAbsentAndRejectsInvalidUTF8(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewTeamsPipelineStore(filepath.Join(file, "store.json"))
	if err != nil || s.ListJobs().Len() != 0 {
		t.Fatalf("store under a file %v err %v", s, err)
	}
	bad := filepath.Join(dir, "bad-utf8.json")
	if err := os.WriteFile(bad, []byte("{\"jobs\": \"\xff\"}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTeamsPipelineStore(bad); err == nil || !strings.Contains(err.Error(), "'utf-8' codec can't decode byte 0xff in position 10: invalid start byte") {
		t.Fatalf("invalid utf-8 error %v", err)
	}
}

func TestStoreUpsertRejectsNonePayload(t *testing.T) {
	s, err := NewTeamsPipelineStore(filepath.Join(t.TempDir(), "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertSinkRecord("notion:m", nil); err == nil || pyrepr.ExceptionName(err) != "TypeError" || pyrepr.ExceptionMessage(err) != "'NoneType' object is not a mapping" {
		t.Fatalf("err %v", err)
	}
	if s.GetSinkRecord("notion:m") != nil {
		t.Fatal("record stored")
	}
}
