package observability

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type ingestScript struct {
	status  int
	payload map[string]any
	extra   map[string]string
}

type ingestRecord struct {
	headers    http.Header
	body       map[string]any
	rawLen     int
	decodedLen int
}

// ingest is a real HTTP ingest answering from a script, 202 once it runs dry
type ingest struct {
	mu       sync.Mutex
	received []ingestRecord
	script   []ingestScript
	srv      *httptest.Server
}

func newIngest(t *testing.T) *ingest {
	in := &ingest{}
	in.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := raw
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(bytes.NewReader(raw))
			if err == nil {
				body, _ = io.ReadAll(zr)
			}
		}
		var decoded map[string]any
		json.Unmarshal(body, &decoded)
		in.mu.Lock()
		in.received = append(in.received, ingestRecord{headers: r.Header.Clone(), body: decoded, rawLen: len(raw), decodedLen: len(body)})
		step := ingestScript{status: 202, payload: map[string]any{}}
		if len(in.script) > 0 {
			step, in.script = in.script[0], in.script[1:]
		}
		in.mu.Unlock()
		encoded, _ := json.Marshal(step.payload)
		w.Header().Set("Content-Type", "application/json")
		for k, v := range step.extra {
			w.Header().Set(k, v)
		}
		w.WriteHeader(step.status)
		w.Write(encoded)
	}))
	t.Cleanup(in.srv.Close)
	return in
}

func (in *ingest) endpoint() string { return in.srv.URL + "/v1/telemetry" }

func (in *ingest) records() []ingestRecord {
	in.mu.Lock()
	defer in.mu.Unlock()
	return append([]ingestRecord(nil), in.received...)
}

func addE2E(t *testing.T, s *SharedMetricsStore, packageID string, metrics int) map[string]any {
	t.Helper()
	day := "2026-08-26"
	list := []any{}
	for i := 0; i < metrics; i++ {
		list = append(list, map[string]any{"name": fmt.Sprintf("icebath.metric.%d", i), "type": "counter",
			"dimensions": map[string]any{"outcome": "ok"}, "value": i})
	}
	payload := map[string]any{
		"schema_version": "icebath.shared_metrics.v2", "package_id": packageID, "install_id": senderInstallID,
		"generated_at": day + "T01:00:00Z", "period_start": day + "T00:00:00Z", "period_end": day + "T23:59:59Z",
		"resource": map[string]any{"icebath_version": "0.20.5", "os_family": "macos", "architecture": "arm64", "install_method": "git"},
		"metrics":  list,
	}
	text, _ := json.Marshal(payload)
	storeExec(t, s, `INSERT INTO package_outbox(package_id, period_start, period_end, payload_json, created_at, exported_at)
		VALUES (?, ?, ?, ?, ?, ?)`, packageID, day+"T00:00:00Z", day+"T23:59:59Z", string(text), day+"T01:00:00Z", day+"T01:00:01Z")
	return payload
}

func e2eSend(s *SharedMetricsStore, endpoint string) SendOutcome {
	return NewSharedMetricsSender(s, endpoint, WithSleep(func(float64) {}), WithNow(func() time.Time { return senderNow })).
		SendPending(context.Background())
}

func TestSenderRealTransport(t *testing.T) {
	t.Run("a_package_is_delivered_and_marked_sent", func(t *testing.T) {
		s, in := senderStore(t), newIngest(t)
		addE2E(t, s, "pkg-1", 1)
		out := e2eSend(s, in.endpoint())
		recs := in.records()
		if out.Sent != 1 || len(recs) != 1 || recs[0].body["package_id"] != "pkg-1" || deref(rowOf(t, s, "pkg-1").SendState) != "sent" {
			t.Fatalf("%+v %d", out, len(recs))
		}
	})
	t.Run("the_stable_install_id_crosses_the_wire_as_is", func(t *testing.T) {
		s, in := senderStore(t), newIngest(t)
		addE2E(t, s, "pkg-1", 40)
		e2eSend(s, in.endpoint())
		if in.records()[0].body["install_id"] != senderInstallID {
			t.Fatal(in.records()[0].body["install_id"])
		}
	})
	t.Run("content_type_is_json", func(t *testing.T) {
		s, in := senderStore(t), newIngest(t)
		addE2E(t, s, "pkg-1", 1)
		e2eSend(s, in.endpoint())
		if ct := in.records()[0].headers.Get("Content-Type"); ct != "application/json" {
			t.Fatal(ct)
		}
	})
	t.Run("a_realistic_package_is_gzipped_and_parses", func(t *testing.T) {
		s, in := senderStore(t), newIngest(t)
		original := addE2E(t, s, "pkg-1", 120)
		e2eSend(s, in.endpoint())
		rec := in.records()[0]
		if rec.headers.Get("Content-Encoding") != "gzip" || rec.rawLen >= rec.decodedLen {
			t.Fatal("not gzipped")
		}
		if !jsonEqual(rec.body["metrics"], original["metrics"]) || !jsonEqual(rec.body["resource"], original["resource"]) {
			t.Fatal("server could not parse what was sent")
		}
	})
	t.Run("400_is_permanent", func(t *testing.T) {
		s, in := senderStore(t), newIngest(t)
		addE2E(t, s, "pkg-1", 1)
		in.script = []ingestScript{{400, map[string]any{"error": "invalid_envelope"}, nil}}
		if out := e2eSend(s, in.endpoint()); out.Rejected != 1 || len(in.records()) != 1 {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("429_is_honoured", func(t *testing.T) {
		s, in := senderStore(t), newIngest(t)
		addE2E(t, s, "pkg-1", 1)
		in.script = []ingestScript{{429, map[string]any{"error": "rate_limited"}, map[string]string{"Retry-After": "90"}}}
		if out := e2eSend(s, in.endpoint()); out.Deferred != 1 || deref(rowOf(t, s, "pkg-1").NextAttemptAt) != "2026-08-26T12:01:30Z" {
			t.Fatalf("%+v %s", out, deref(rowOf(t, s, "pkg-1").NextAttemptAt))
		}
	})
	t.Run("5xx_retries_then_succeeds", func(t *testing.T) {
		s, in := senderStore(t), newIngest(t)
		addE2E(t, s, "pkg-1", 1)
		in.script = []ingestScript{{503, map[string]any{"error": "storage_unavailable"}, nil}, {202, map[string]any{"package_id": "pkg-1"}, nil}}
		if out := e2eSend(s, in.endpoint()); out.Sent != 1 || len(in.records()) != 2 {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("several_packages_in_one_pass", func(t *testing.T) {
		s, in := senderStore(t), newIngest(t)
		for i := 0; i < 5; i++ {
			addE2E(t, s, fmt.Sprintf("pkg-%d", i), 1)
		}
		if out := e2eSend(s, in.endpoint()); out.Sent != 5 || len(in.records()) != 5 {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("a_dead_server_defers_without_raising", func(t *testing.T) {
		s, in := senderStore(t), newIngest(t)
		addE2E(t, s, "pkg-1", 1)
		endpoint := in.endpoint()
		in.srv.Close()
		if out := e2eSend(s, endpoint); out.Deferred != 1 {
			t.Fatalf("%+v", out)
		}
		r := rowOf(t, s, "pkg-1")
		if deref(r.SendState) != "pending" || r.LastError == nil || *r.LastError == "" {
			t.Fatalf("%+v", r)
		}
	})
}
