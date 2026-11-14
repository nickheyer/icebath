package dashboard

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Full application on the private local Webapp surface with a fixed session token, the local_webapp fixture
func localWebapp(t *testing.T) (*FastAPI, *testClient) {
	t.Helper()
	app := dashboardTestApp(t)
	previous := SessionToken()
	t.Cleanup(func() { setSessionToken(previous) })
	setSessionToken(strings.Repeat("a", 43))
	app.State.Set("ui_surface", "webapp")
	app.State.Set("auth_required", false)
	app.State.Set("bound_host", "127.0.0.1")
	app.State.Set("webapp_window_tickets", map[string]webappWindowTicket{})
	return app, newTestClientAt(t, app, "http", "127.0.0.1", 80)
}

// Ticket of a successful window ticket mint
func issueWindowTicket(t *testing.T, c *testClient) string {
	t.Helper()
	r := c.request("POST", "/api/webapp/window-ticket", nil, sessionHeaderName, SessionToken())
	if r.Status != 200 {
		t.Fatalf("window ticket %d %s", r.Status, r.Body)
	}
	ticket, _ := appObject(t, r).GetOr("ticket", nil).(string)
	return ticket
}

// Window session exchange of a ticket from the same origin
func spendWindowTicket(c *testClient, ticket string) *testResponse {
	return c.request("POST", "/webapp/window-session", nil, "Origin", "http://127.0.0.1", "X-Icebath-Window-Ticket", ticket)
}

func TestChildHandoffIsBoundToLocalWebappStart(t *testing.T) {
	for _, invalidate := range []string{"expiry", "restart", "gated", "dashboard"} {
		t.Run(invalidate, func(t *testing.T) {
			app, c := localWebapp(t)
			ticket := issueWindowTicket(t, c)
			switch invalidate {
			case "expiry":
				tickets := webappWindowTickets(app.State)
				entry := tickets[ticket]
				entry.expires = monotonic() - 1
				tickets[ticket] = entry
			case "restart":
				setSessionToken(strings.Repeat("b", 43))
			case "gated":
				app.State.Set("auth_required", true)
			default:
				app.State.Set("ui_surface", "dashboard")
			}
			denied := spendWindowTicket(c, ticket)
			switch denied.Status {
			case 302, 401, 403, 404:
			default:
				t.Fatalf("status %d %s", denied.Status, denied.Body)
			}
			if strings.Contains(string(denied.Body), SessionToken()) {
				t.Fatal("denied response carries the session token")
			}
		})
	}
}

func TestPendingHandoffsKeepTheCredentialsGenerationNotTheCredential(t *testing.T) {
	app, c := localWebapp(t)
	token := SessionToken()
	window := issueWindowTicket(t, c)
	if _, err := mintLaunchTicket(app.State); err != nil {
		t.Fatal(err)
	}
	if _, err := mintPrintedLaunchTicket(app.State); err != nil {
		t.Fatal(err)
	}
	stored := webappWindowTickets(app.State)
	if len(stored) != 3 {
		t.Fatalf("stored %d tickets", len(stored))
	}
	for key, value := range stored {
		if key == token || value.generation == token {
			t.Fatal("a pending ticket stores the credential")
		}
	}
	spent := spendWindowTicket(c, window)
	if got := appObject(t, spent).GetOr("token", nil); got != token {
		t.Fatalf("token %v", got)
	}
}

func TestOnLinuxThePeerCheckNamesTheOwnerOfARealLoopbackConnection(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	browser, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	served, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer served.Close()
	host, portText, _ := net.SplitHostPort(browser.LocalAddr().String())
	port, _ := strconv.Atoi(portText)
	lhost, lportText, _ := net.SplitHostPort(listener.Addr().String())
	lport, _ := strconv.Atoi(lportText)
	own := &Request{HTTPConnection: HTTPConnection{Scope: &Scope{Client: &Address{host, port}, Server: &Address{lhost, lport}}}}
	if !peerIsServerUser(own, webappSysPlatform()) {
		t.Fatal("own loopback connection failed the peer check")
	}
	unknown := &Request{HTTPConnection: HTTPConnection{Scope: &Scope{Client: &Address{host, port}, Server: &Address{host, 1}}}}
	if peerIsServerUser(unknown, webappSysPlatform()) {
		t.Fatal("a connection nobody holds passed the peer check")
	}
}

// Request through the client whose scope also carries extra raw headers, repeated names kept
func requestWithRawHeaders(c *testClient, method, target string, extra []Header, headers ...string) *testResponse {
	c.t.Helper()
	scope := testScope("http", method, target, headers)
	scope.Headers = append(scope.Headers, extra...)
	if c.server != nil {
		scope.Scheme, scope.Server = c.scheme, c.server
		for j := range scope.Headers {
			if scope.Headers[j][0] == "host" && scope.Headers[j][1] == "testserver" {
				scope.Headers[j][1] = c.server.Host
			}
		}
	}
	resp := &testResponse{}
	sent := false
	receive := func(ctx context.Context) (Message, error) {
		if !sent {
			sent = true
			return Message{Type: "http.request"}, nil
		}
		<-ctx.Done()
		return Message{}, ctx.Err()
	}
	send := func(_ context.Context, m Message) error {
		switch m.Type {
		case "http.response.start":
			resp.Status, resp.Raw = m.Status, append([]Header(nil), m.Headers...)
		case "http.response.body":
			resp.Body = append(resp.Body, m.Body...)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	resp.AppErr = c.app.ASGI(ctx, scope, receive, send)
	cancel()
	resp.Headers = headersOfRaw(&resp.Raw)
	return resp
}

func TestChildHandoffIsPrivateSingleUseAndAuthorizesRealApi(t *testing.T) {
	for _, publicURL := range []string{"", "http://localhost", "http://127.0.0.1:8080"} {
		t.Run(publicURL, func(t *testing.T) {
			_, c := localWebapp(t)
			t.Setenv("ICEBATH_DASHBOARD_PUBLIC_URL", publicURL)
			token := SessionToken()
			if r := c.request("POST", "/api/webapp/window-ticket", nil); r.Status != 401 {
				t.Fatalf("unauthenticated ticket %d", r.Status)
			}
			page := c.get("/webapp/window")
			if page.Status != 200 || strings.Contains(string(page.Body), token) || strings.Contains(fmt.Sprint(page.Raw), token) {
				t.Fatalf("page %d leaks the token or failed", page.Status)
			}
			if page.Headers.Get("cache-control") != "no-store" {
				t.Fatalf("page cache-control %q", page.Headers.Get("cache-control"))
			}
			issued := c.request("POST", "/api/webapp/window-ticket", nil, sessionHeaderName, token)
			if issued.Status != 200 || issued.Headers.Get("cache-control") != "no-store" {
				t.Fatalf("issued %d %q", issued.Status, issued.Headers.Get("cache-control"))
			}
			ticket, _ := appObject(t, issued).GetOr("ticket", nil).(string)
			if ticket == token {
				t.Fatal("the window ticket is the session token")
			}
			proof := filepath.Join(t.TempDir(), "handoff.txt")
			if err := os.WriteFile(proof, []byte("child-window-proof"), 0o644); err != nil {
				t.Fatal(err)
			}
			readTarget := "/api/fs/read-text?path=" + url.QueryEscape(proof)
			if r := c.get(readTarget, sessionHeaderName, ticket); r.Status != 401 {
				t.Fatalf("ticket opened the API %d", r.Status)
			}
			for _, origin := range []string{"", "null", "http://127.0.0.1:9999", "https://127.0.0.1", "http://evil.example"} {
				denied := c.request("POST", "/webapp/window-session", nil, "Origin", origin, "X-Icebath-Window-Ticket", ticket)
				if denied.Status != 403 || strings.Contains(string(denied.Body), token) {
					t.Fatalf("origin %q got %d", origin, denied.Status)
				}
			}
			rebinding := c.request("POST", "/webapp/window-session", nil, "Host", "evil.example", "Origin", "http://evil.example",
				"X-Icebath-Window-Ticket", ticket)
			if rebinding.Status != 400 {
				t.Fatalf("rebinding %d", rebinding.Status)
			}
			doubled := requestWithRawHeaders(c, "POST", "/webapp/window-session",
				[]Header{{"origin", "http://127.0.0.1"}, {"origin", "http://127.0.0.1"}}, "X-Icebath-Window-Ticket", ticket)
			if doubled.Status != 403 {
				t.Fatalf("doubled origin %d", doubled.Status)
			}
			responses := make([]*testResponse, 2)
			var wg sync.WaitGroup
			for i := range responses {
				wg.Add(1)
				go func() {
					defer wg.Done()
					responses[i] = spendWindowTicket(c, ticket)
				}()
			}
			wg.Wait()
			statuses := []int{responses[0].Status, responses[1].Status}
			sort.Ints(statuses)
			if statuses[0] != 200 || statuses[1] != 403 {
				t.Fatalf("concurrent exchanges %v", statuses)
			}
			granted := responses[0]
			if granted.Status != 200 {
				granted = responses[1]
			}
			if granted.Headers.Get("cache-control") != "no-store" {
				t.Fatalf("granted cache-control %q", granted.Headers.Get("cache-control"))
			}
			if got := appObject(t, granted).GetOr("token", nil); got != token {
				t.Fatalf("granted token %v", got)
			}
			read := c.get(readTarget, sessionHeaderName, token)
			if read.Status != 200 || !strings.Contains(string(read.Body), "child-window-proof") {
				t.Fatalf("read %d %s", read.Status, read.Body)
			}
			if r := spendWindowTicket(c, ticket); r.Status != 403 {
				t.Fatalf("replay %d", r.Status)
			}
			if r := c.get("/webapp/window-session?ticket=" + url.QueryEscape(ticket)); strings.Contains(string(r.Body), token) {
				t.Fatal("a GET recovered the token")
			}
		})
	}
}
