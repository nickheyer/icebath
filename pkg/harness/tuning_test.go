package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

// What the fake endpoint saw of the last turn, its headers, raw body and system message
type seen struct {
	mu     sync.Mutex
	header http.Header
	body   map[string]json.RawMessage
	system *string
}

func (s *seen) last() (http.Header, map[string]json.RawMessage, *string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.header, s.body, s.system
}

// App on an endpoint whose profile asks for ghost and bare through its own headers and body field
func tunedApp(t *testing.T) (*App, *seen) {
	t.Helper()
	got := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)
		var msgs []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		}
		json.Unmarshal(body["messages"], &msgs)
		got.mu.Lock()
		got.header, got.body, got.system = r.Header.Clone(), body, nil
		for _, m := range msgs {
			if text, ok := m.Content.(string); ok && m.Role == "system" {
				got.system = &text
			}
		}
		got.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, filepath.Join(dir, "cfg"))
	list := fmt.Sprintf(`[{"name":"gw","vendor":"openai","protocol":"openai-completions","base_url":%q,"models":["m"],"ghost":{"header":{"X-Ghost":["1"]},"extra":{"store":false}},"bare":{"header":{"X-Bare":["1"]}}}]`, srv.URL)
	path := filepath.Join(dir, "profiles.json")
	os.WriteFile(path, []byte(list), 0o644)
	app, err := Build(context.Background(), Config{Profile: "gw", ProfileFiles: []string{path}, Permissions: rulesAllowAll(), Web: WebConfig{Disabled: true}}, newSink().send, BuildOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close(context.Background()) })
	return app, got
}

// Runs one turn on the session and waits for it to end with every save written
func turn(t *testing.T, app *App, l *Live, text string) {
	t.Helper()
	r, err := app.RunOn(context.Background(), l, "", []protocol.Part{protocol.Text{Text: text}})
	if err != nil {
		t.Fatal(err)
	}
	done, _ := app.Control.Done(r.ID)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run never ended")
	}
	app.Saver.Wait()
}

func stored(t *testing.T, app *App, id string) bool {
	t.Helper()
	ok, err := app.Store.Exists(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestDefaultTurnsSendTheBaseAndEnvironmentAndAskNothing(t *testing.T) {
	app, got := tunedApp(t)
	l, _ := app.Create()
	turn(t, app, l, "hi")
	header, body, system := got.last()
	if system == nil || !strings.HasPrefix(*system, "You are icebath") || !strings.Contains(*system, "# Environment\n- Working directory: ") {
		t.Fatalf("default system prompt %v", system)
	}
	if header.Get("X-Ghost") != "" || header.Get("X-Bare") != "" || body["store"] != nil {
		t.Fatalf("an untuned turn asks for nothing %v %s", header, body["store"])
	}
	if !stored(t, app, l.Session.ID()) {
		t.Fatal("an untuned session is written")
	}
}

func TestTuningShapesThePromptAndAsksTheEndpoint(t *testing.T) {
	app, got := tunedApp(t)
	l, _ := app.Create()
	child := session.New()
	child.Label(parentLabel, l.Session.ID())
	child.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "scout"}}})
	app.Store.Save(context.Background(), child)
	turn(t, app, l, "hi")
	if err := app.Tune(context.Background(), l, Tuning{System: "be terse", Custom: true, NoContext: true, Bare: true, Ghost: true}); err != nil {
		t.Fatal(err)
	}
	if stored(t, app, l.Session.ID()) || stored(t, app, child.ID()) || !app.Ghosted(l.Session) || !app.Ghosted(child) {
		t.Fatal("turning ghost removes the stored copies of the session and its sub-agents")
	}
	turn(t, app, l, "again")
	header, body, system := got.last()
	if system == nil || *system != "be terse" {
		t.Fatalf("a custom base without context is the whole prompt %v", system)
	}
	if header.Get("X-Ghost") != "1" || header.Get("X-Bare") != "1" || string(body["store"]) != "false" {
		t.Fatalf("the profile hears both directives %v %s", header, body["store"])
	}
	if stored(t, app, l.Session.ID()) {
		t.Fatal("a ghost session stays off disk after its turns")
	}
	app.Tune(context.Background(), l, Tuning{Custom: true, NoContext: true, Bare: true})
	turn(t, app, l, "bare")
	if _, _, system := got.last(); system == nil || *system != "" {
		t.Fatalf("an empty prompt is sent as an empty slot while bare %v", system)
	}
	if !stored(t, app, l.Session.ID()) || TuningOf(l.Session) != (Tuning{Custom: true, NoContext: true, Bare: true}) {
		t.Fatal("leaving ghost writes the session again with its tuning")
	}
	app.Tune(context.Background(), l, Tuning{Custom: true, NoContext: true})
	turn(t, app, l, "plain")
	if header, _, system := got.last(); system != nil || header.Get("X-Bare") != "" {
		t.Fatalf("an empty prompt is left out once the session is not bare %v", system)
	}
	app.Tune(context.Background(), l, Tuning{})
	if _, ok := l.Session.Labels()[tuningLabel]; ok || app.PromptFor(context.Background(), l.Session, "m") != Compose(app.DefaultBase(), app.EnvironmentFor(context.Background(), "m")) {
		t.Fatal("default tuning drops the label and sends the default prompt")
	}
}

func TestDirectedContextReportsTheLiveTuning(t *testing.T) {
	app, _ := tunedApp(t)
	l, _ := app.Create()
	ctx := app.Directed(context.Background(), l.Session)
	if protocol.DirectivesFrom(ctx) != (protocol.Directives{}) || protocol.DirectivesFrom(context.Background()) != (protocol.Directives{}) {
		t.Fatal("nothing is asked before tuning")
	}
	app.Tune(context.Background(), l, Tuning{Bare: true})
	if protocol.DirectivesFrom(ctx) != (protocol.Directives{Bare: true}) {
		t.Fatal("a context made earlier follows the tuning as it changes")
	}
}
