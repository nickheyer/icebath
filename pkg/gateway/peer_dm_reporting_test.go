package gateway

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

const peerReportingSession = "20260916_bot_chat"

// Runs one dm with the Bot Chat resolution and the chat request replaced the way the hermes helper patches them
func peerReportingRun(t *testing.T, base string, raiseOnPost, raiseOnSession error) (int, string) {
	t.Helper()
	swapSeam(t, &peerEnsureBotChat, func(context.Context, string, string) (string, error) {
		if raiseOnSession != nil {
			return "", raiseOnSession
		}
		return peerReportingSession, nil
	})
	if raiseOnPost != nil {
		swapSeam(t, &peerRequest, func(context.Context, string, string, peerRequestOptions) (*pyjson.Object, error) {
			return nil, raiseOnPost
		})
	}
	var out, errOut bytes.Buffer
	c := &peerCLI{ctx: context.Background(), home: t.TempDir(), out: &out, errOut: &errOut,
		stdin: strings.NewReader(""), stdinIsatty: func() bool { return true }}
	code, err := peerDM(c, map[string]any{"json": false}, "hello", "mini", "", base, "key")
	if err != nil {
		t.Fatalf("peerDM raised %v", err)
	}
	return code, errOut.String()
}

func TestOnlyATimeoutOnAnAcceptedTurnIsReportedAsDelivered(t *testing.T) {
	timedOut := func() error { return &peerOSError{class: "TimeoutError", text: "timed out"} }
	cases := []struct {
		id             string
		raiseOnPost    error
		raiseOnSession error
		expected       string
		forbidden      string
	}{
		{"response-timeout", timedOut(), nil, "accepted the message but its turn is still running", "Could not reach"},
		{"connect-timeout", &peerURLError{timedOut()}, nil, "Could not reach peer", "still running"},
		{"connection-refused", &peerURLError{&peerOSError{class: "ConnectionRefusedError", text: "[Errno 61] Connection refused"}}, nil,
			"Could not reach peer", "still running"},
		{"network-unreachable", &peerOSError{class: "OSError", text: "[Errno 51] Network is unreachable"}, nil, "Could not reach peer", "still running"},
		{"timeout-before-the-session-is-known", timedOut(), timedOut(), "Could not reach peer", "still running"},
		{"http-rejection", &peerHTTPError{code: 503, msg: "Service Unavailable"}, nil, "rejected the request (HTTP 503)", "still running"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			code, stderr := peerReportingRun(t, "http://192.168.2.55:8642", tc.raiseOnPost, tc.raiseOnSession)
			if code != 1 || !strings.Contains(stderr, tc.expected) || strings.Contains(stderr, tc.forbidden) {
				t.Fatalf("code %d stderr %q", code, stderr)
			}
			if strings.Contains(tc.expected, "still running") && (!strings.Contains(stderr, peerReportingSession) || !strings.Contains(stderr, "Do NOT resend")) {
				t.Fatalf("stderr %q", stderr)
			}
		})
	}
}

// Dialer that fails every connect with a socket timeout, the patched socket.create_connection
func peerDialTimesOut(context.Context, string, string) (net.Conn, error) {
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
}

func TestTheRealUrllibStackRaisesTheSignaturesTheBranchReliesOn(t *testing.T) {
	for _, phase := range []string{"response", "connect"} {
		t.Run(phase, func(t *testing.T) {
			for _, v := range []string{"http_proxy", "HTTP_PROXY", "all_proxy", "ALL_PROXY"} {
				t.Setenv(v, "")
				os.Unsetenv(v)
			}
			swapSeam(t, &peerDMTimeout, 500*time.Millisecond)
			server, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var held []net.Conn
			done := make(chan struct{})
			accepted := make(chan struct{})
			go func() {
				defer close(accepted)
				conn, err := server.Accept()
				if err != nil {
					return
				}
				mu.Lock()
				held = append(held, conn)
				mu.Unlock()
				buf := make([]byte, 65536)
				conn.Read(buf)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
				}
			}()
			if phase == "connect" {
				provider.InstallOpener(&provider.Opener{Transport: &http.Transport{DialContext: peerDialTimesOut}})
				t.Cleanup(func() { provider.InstallOpener(nil) })
			}
			code, stderr := func() (int, string) {
				defer func() {
					close(done)
					server.Close()
					<-accepted
					mu.Lock()
					for _, conn := range held {
						conn.Close()
					}
					mu.Unlock()
				}()
				return peerReportingRun(t, "http://"+server.Addr().String(), nil, nil)
			}()
			if code != 1 {
				t.Fatalf("code %d stderr %q", code, stderr)
			}
			if phase == "response" {
				if !strings.Contains(stderr, "accepted the message but its turn is still running") || !strings.Contains(stderr, "Do NOT resend") {
					t.Fatalf("stderr %q", stderr)
				}
				return
			}
			if !strings.Contains(stderr, "Could not reach peer") || strings.Contains(stderr, "still running") {
				t.Fatalf("stderr %q", stderr)
			}
		})
	}
}
