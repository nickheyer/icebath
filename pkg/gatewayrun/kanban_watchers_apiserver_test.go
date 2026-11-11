package gatewayrun

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/kanban"
)

// One wake self-post the loopback API server received
type selfPost struct {
	text      string
	sessionID string
}

// Loopback chat completions endpoint recording each self-post, failing the first fail posts with HTTP 400
type selfPostServer struct {
	mu    sync.Mutex
	posts []selfPost
	fail  int
	host  string
	port  int
}

// Started recording endpoint closed at test end
func newSelfPostServer(t *testing.T, fail int) *selfPostServer {
	t.Helper()
	s := &selfPostServer{fail: fail}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		s.mu.Lock()
		text := ""
		if len(body.Messages) > 0 {
			text = body.Messages[0].Content
		}
		s.posts = append(s.posts, selfPost{text, req.Header.Get("X-Hermes-Session-Id")})
		failing := len(s.posts) <= s.fail
		s.mu.Unlock()
		if failing {
			http.Error(w, "simulated wake failure", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	s.host = host
	s.port, _ = strconv.Atoi(port)
	return s
}

// Session ids of the recorded posts
func (s *selfPostServer) sessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, p := range s.posts {
		out = append(out, p.sessionID)
	}
	return out
}

// API server shaped adapter with no push channel whose send reports failure
type apiServerLikeAdapter struct {
	*gateway.BasePlatformAdapter
	server      *selfPostServer
	handleCalls int
	sendCalls   int
	mu          sync.Mutex
}

func (a *apiServerLikeAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *apiServerLikeAdapter) Disconnect(context.Context) error            { return nil }
func (a *apiServerLikeAdapter) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	a.mu.Lock()
	a.sendCalls++
	a.mu.Unlock()
	return gateway.SendResult{Success: false, Error: "API server uses HTTP request/response, not send()"}, nil
}
func (a *apiServerLikeAdapter) HandleMessage(context.Context, *gateway.MessageEvent) error {
	a.mu.Lock()
	a.handleCalls++
	a.mu.Unlock()
	return nil
}
func (a *apiServerLikeAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}
func (a *apiServerLikeAdapter) SelfPostTarget() (string, int, string, string) {
	return a.server.host, a.server.port, "k", "icebath"
}

// API server adapter posting wakes to server
func newAPIServerLikeAdapter(server *selfPostServer) *apiServerLikeAdapter {
	a := &apiServerLikeAdapter{server: server}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), gateway.PlatformAPIServer, a)
	a.BasePlatformAdapter.Traits.SupportsAsyncDelivery = false
	return a
}

// Runner serving only the API server adapter
func makeAPIServerRunner(t *testing.T, adapter gateway.PlatformAdapter) *GatewayRunner {
	t.Helper()
	r := makeKanbanRunner(t, adapter)
	r.adapters = adapterDict(gateway.PlatformAPIServer, adapter)
	return r
}

// Completed task with one subscription on platform and chat
func createCompletedSubscription(t *testing.T, platform, chatID string, sessionID *string) string {
	t.Helper()
	conn := kanbanTestConn(t)
	tid := kanbanCreate(t, conn, "notify once", "worker", sessionID)
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: platform, ChatID: chatID})
	kanbanComplete(t, conn, tid, "done once")
	return tid
}

// Unseen terminal events of a subscription
func unseenOn(t *testing.T, tid, platform, chatID string) []*kanban.Event {
	t.Helper()
	conn := kanbanTestConn(t)
	_, events, err := kanban.UnseenEventsForSub(context.Background(), conn, tid, platform, chatID, nil,
		[]string{"completed", "blocked", "gave_up", "crashed", "timed_out"})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestApiserverSubWakesSubscriptionDestinationViaSelfPost(t *testing.T) {
	kanbanTestDB(t, "apiserver.db")
	tid := createCompletedSubscription(t, "api_server", "origin-session", kstr("worker-session"))
	server := newSelfPostServer(t, 0)
	adapter := newAPIServerLikeAdapter(server)
	runOneNotifierTick(t, makeAPIServerRunner(t, adapter))
	if adapter.handleCalls != 0 {
		t.Fatal("api_server wake must not go through handle_message (wrong-session bug)")
	}
	if len(server.posts) != 1 || server.posts[0].sessionID != "origin-session" {
		t.Fatalf("posts %+v", server.posts)
	}
	wake := server.posts[0].text
	lower := strings.ToLower(wake)
	if !strings.Contains(wake, tid) || !strings.Contains(wake, "done once") ||
		!strings.Contains(lower, "not a request to decompose") || !strings.Contains(lower, "do not recreate") {
		t.Fatalf("wake %q", wake)
	}
	if len(unseenOn(t, tid, "api_server", "origin-session")) != 0 {
		t.Fatal("cursor not advanced")
	}
}

func TestApiserverSubscriptionsHaveIndependentWakeDestinations(t *testing.T) {
	kanbanTestDB(t, "apiserver-multi.db")
	conn := kanbanTestConn(t)
	tid := kanbanCreate(t, conn, "notify both", "worker", kstr("worker-session"))
	for _, chat := range []string{"origin-a", "origin-b"} {
		kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: "api_server", ChatID: chat})
	}
	kanbanComplete(t, conn, tid, "done once")
	server := newSelfPostServer(t, 0)
	runOneNotifierTick(t, makeAPIServerRunner(t, newAPIServerLikeAdapter(server)))
	got := server.sessions()
	sort.Strings(got)
	if strings.Join(got, ",") != "origin-a,origin-b" {
		t.Fatalf("sessions %v", got)
	}
	if len(unseenOn(t, tid, "api_server", "origin-a")) != 0 || len(unseenOn(t, tid, "api_server", "origin-b")) != 0 {
		t.Fatal("cursors not advanced")
	}
}

func TestApiserverWakeFailureRewindsThenRetriesDestination(t *testing.T) {
	kanbanTestDB(t, "apiserver-retry.db")
	tid := createCompletedSubscription(t, "api_server", "origin-session", kstr("worker-session"))
	server := newSelfPostServer(t, 1)
	r := makeAPIServerRunner(t, newAPIServerLikeAdapter(server))
	runOneNotifierTick(t, r)
	if len(unseenOn(t, tid, "api_server", "origin-session")) == 0 {
		t.Fatal("failed wake must rewind")
	}
	r.running.Store(true)
	runOneNotifierTick(t, r)
	if got := server.sessions(); strings.Join(got, ",") != "origin-session,origin-session" {
		t.Fatalf("attempted %v", got)
	}
	if len(unseenOn(t, tid, "api_server", "origin-session")) != 0 {
		t.Fatal("retry did not advance")
	}
}
