package mcp

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Serves the waiter's handler on a fresh port, sends paths back to back, then reads the outcome
func driveCallbackServer(t *testing.T, paths []string) ([]int, *AuthorizationCodeResult) {
	t.Helper()
	resetOAuthState(t)
	port, err := reserveCallbackPort()
	if err != nil {
		t.Fatal(err)
	}
	handler, result := makeCallbackHandler()
	server, err := startCallbackServer(port, handler)
	if err != nil {
		t.Fatal(err)
	}
	defer server.shutdown()
	client := &http.Client{Timeout: 5 * time.Second}
	var statuses []int
	for _, p := range paths {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, p))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		statuses = append(statuses, resp.StatusCode)
	}
	select {
	case <-result.done:
	case <-time.After(5 * time.Second):
		t.Fatal("no terminal callback latched")
	}
	res, err := callbackOutcome(result, "")
	if err != nil {
		t.Fatal(err)
	}
	return statuses, res
}

func TestOAuthCallbackLatchFaviconAfterCallbackDoesNotClobberTheCode(t *testing.T) {
	statuses, res := driveCallbackServer(t, []string{"/callback?code=synthetic&state=s1&iss=https://as.example", "/favicon.ico"})
	if fmt.Sprint(statuses) != "[200 404]" || res.Code != "synthetic" || res.State != "s1" || res.Iss != "https://as.example" {
		t.Fatalf("statuses %v result %+v", statuses, res)
	}
}

func TestOAuthCallbackLatchFirstTerminalCallbackWins(t *testing.T) {
	statuses, res := driveCallbackServer(t, []string{"/favicon.ico", "/callback?code=first&state=s1",
		"/callback?code=second&state=s2", "/callback?error=access_denied&state=s1"})
	if fmt.Sprint(statuses) != "[404 200 200 200]" || res.Code != "first" || res.State != "s1" {
		t.Fatalf("statuses %v result %+v", statuses, res)
	}
}

func TestOAuthStdinConsoleDevnullIsNotAConsole(t *testing.T) {
	if os.Getenv("ICEBATH_TEST_STDIN_CONSOLE_CHILD") == "1" {
		fmt.Printf("console=%v\n", stdinIsConsole())
		os.Exit(0)
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestOAuthStdinConsoleDevnullIsNotAConsole$")
	cmd.Env = append(os.Environ(), "ICEBATH_TEST_STDIN_CONSOLE_CHILD=1")
	cmd.Stdin = devnull
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("child failed %v %s", err, out)
	}
	if !strings.Contains(string(out), "console=false") {
		t.Fatalf("child output %q", out)
	}
}
