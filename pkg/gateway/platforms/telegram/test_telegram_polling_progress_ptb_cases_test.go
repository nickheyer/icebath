package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
)

// Instrumented getUpdates request against a loopback server answering every call with payload
func envelopeRequest(t *testing.T, payload []byte) (*ptbRequest, *pollingLifecycle, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	p, _ := newTestLifecycle(t, nil)
	request, err := newPTBRequest(context.Background(), requestSettings{connectTimeout: 5, readTimeout: 5, writeTimeout: 5,
		poolTimeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = request.Shutdown(context.Background()) })
	return instrumentPollingRequest(request, p), p, srv.URL + "/bot-token/getUpdates"
}

// Posts one getUpdates request under the generation context
func postUnderGeneration(t *testing.T, request *ptbRequest, url string, generation int) (string, error) {
	t.Helper()
	ctx := withPollingGeneration(context.Background(), &generation)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := request.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), nil
}

// Asserts the counters and degraded flag were left as the test seeded them
func assertNoProgress(t *testing.T, p *pollingLifecycle, progress *pollEvent) {
	t.Helper()
	if progress.isSet() || p.networkErrorCount != 4 || p.conflictCount != 3 || !p.isSendPathDegraded() {
		t.Fatalf("progress %v network %d conflict %d degraded %v", progress.isSet(), p.networkErrorCount, p.conflictCount,
			p.isSendPathDegraded())
	}
}

func TestRealBaseRequestBomRejectedByPtbCannotRecordProgress(t *testing.T) {
	request, p, url := envelopeRequest(t, []byte("\xef\xbb\xbf{\"ok\":true,\"result\":[]}"))
	generation, progress := p.beginPollingGeneration()
	p.networkErrorCount, p.conflictCount = 4, 3
	_, err := postUnderGeneration(t, request, url, generation)
	if err == nil || className(err) != "TelegramError" || !strings.Contains(err.Error(), "Invalid server response") {
		t.Fatalf("want TelegramError Invalid server response, got %v", err)
	}
	assertNoProgress(t, p, progress)
}

func TestRealBaseRequestPtbReplacementDecodeRecordsProgress(t *testing.T) {
	request, p, url := envelopeRequest(t, []byte("{\"ok\":true,\"result\":[],\"note\":\"\xff\"}"))
	generation, progress := p.beginPollingGeneration()
	p.networkErrorCount, p.conflictCount = 4, 3
	if _, err := postUnderGeneration(t, request, url, generation); err != nil {
		t.Fatal(err)
	}
	if !progress.isSet() || p.networkErrorCount != 0 || p.conflictCount != 0 || p.isSendPathDegraded() {
		t.Fatal("replacement decoded envelope must record progress")
	}
}

func TestRealBaseRequestUnsuccessful200EnvelopeCannotRecordProgress(t *testing.T) {
	for _, payload := range []string{`{"ok":false,"result":[]}`, `{"ok":true}`} {
		t.Run(payload, func(t *testing.T) {
			request, p, url := envelopeRequest(t, []byte(payload))
			generation, progress := p.beginPollingGeneration()
			p.networkErrorCount, p.conflictCount = 4, 3
			body, err := postUnderGeneration(t, request, url, generation)
			if err != nil || body != payload {
				t.Fatalf("got %q %v", body, err)
			}
			assertNoProgress(t, p, progress)
		})
	}
}

func TestRealBaseRequestValidSuccessEnvelopeRecordsProgress(t *testing.T) {
	request, p, url := envelopeRequest(t, []byte(`{"ok":true,"result":[]}`))
	generation, progress := p.beginPollingGeneration()
	p.networkErrorCount, p.conflictCount = 4, 3
	if _, err := postUnderGeneration(t, request, url, generation); err != nil {
		t.Fatal(err)
	}
	if !progress.isSet() || p.networkErrorCount != 0 || p.conflictCount != 0 || p.isSendPathDegraded() {
		t.Fatal("valid envelope must record progress")
	}
}

// Loopback Bot API whose getUpdates answers 409 first, then blocks or serves one empty batch once enabled
type fakeBotAPI struct {
	mu                  sync.Mutex
	conflictSent        bool
	replacementEnabled  bool
	replacementSent     bool
	cleanupCalls        int
	block               chan struct{}
	deleteWebhookCalled int
}

func (f *fakeBotAPI) handler(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	switch method {
	case "getMe":
		_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"Test","username":"test_bot"}}`)
	case "getUpdates":
		var params map[string]any
		_ = json.NewDecoder(r.Body).Decode(&params)
		f.mu.Lock()
		if timeout, _ := params["timeout"].(float64); timeout == 0 {
			f.cleanupCalls++
			f.mu.Unlock()
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
			return
		}
		if !f.conflictSent {
			f.conflictSent = true
			f.mu.Unlock()
			w.WriteHeader(409)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":409,"description":"Conflict: another getUpdates request"}`)
			return
		}
		if f.replacementEnabled && !f.replacementSent {
			f.replacementSent = true
			f.mu.Unlock()
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
			return
		}
		block := f.block
		f.mu.Unlock()
		select {
		case <-block:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
	default:
		f.mu.Lock()
		f.deleteWebhookCalled++
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	}
}

func TestRealPtbStopCleanupCannotHealRecoveryGeneration(t *testing.T) {
	api := &fakeBotAPI{block: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()
	defer close(api.block)
	p, _ := newTestLifecycle(t, nil)
	settings := requestSettings{connectTimeout: 5, readTimeout: 5, writeTimeout: 5, poolTimeout: 5}
	general, err := newPTBRequest(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	pollingReq, err := newPTBRequest(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	app, err := newPTBApp("123456:test-token", srv.URL, general, instrumentPollingRequest(pollingReq, p),
		func(context.Context, *models.Update) {})
	if err != nil {
		t.Fatal(err)
	}
	p.app = app
	p.networkErrorCount, p.conflictCount = 4, 3
	callbackCalled := make(chan struct{})
	var recovery atomic.Pointer[chan error]
	var once sync.Once
	schedule := func(err error) {
		if !isPTBError(err, "Conflict") {
			t.Errorf("want Conflict, got %v", err)
		}
		once.Do(func() {
			done := make(chan error, 1)
			recovery.Store(&done)
			go func() { done <- app.Updater().Stop(context.Background()) }()
			close(callbackCalled)
		})
	}
	ctx := context.Background()
	if err := app.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Shutdown(ctx) }()
	if _, _, err := p.startPollingOnce(ctx, app, false, schedule, false, true); err != nil {
		t.Fatal(err)
	}
	generation, progress := p.generation, p.progress
	select {
	case <-callbackCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("conflict never reached the error callback")
	}
	select {
	case err := <-*recovery.Load():
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("recovery stop did not finish")
	}
	api.mu.Lock()
	cleanup := api.cleanupCalls
	api.replacementEnabled = true
	api.mu.Unlock()
	if cleanup != 1 || progress.isSet() || p.networkErrorCount != 4 || p.conflictCount != 3 || !p.isSendPathDegraded() {
		t.Fatalf("cleanup %d progress %v network %d conflict %d", cleanup, progress.isSet(), p.networkErrorCount, p.conflictCount)
	}
	if _, _, err := p.startPollingOnce(ctx, app, false, schedule, false, true); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	replacementGeneration, replacementProgress := p.generation, p.progress
	p.mu.Unlock()
	select {
	case <-replacementProgress.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement generation never progressed")
	}
	if replacementGeneration != generation+1 || p.networkErrorCount != 0 || p.conflictCount != 0 || p.isSendPathDegraded() {
		t.Fatal("replacement generation must heal the counters")
	}
	if app.Updater().Running() {
		_ = app.Updater().Stop(context.Background())
	}
}
