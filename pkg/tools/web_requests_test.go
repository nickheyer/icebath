package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	httputil "github.com/nickheyer/icebath/pkg/utils/http"
)

func TestKeenableTransportFailuresUseRequestsText(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	setVar(t, &keenableAPIURL, "http://"+addr)
	env := httpxProviderEnv(t, "KEENABLE_API_KEY=kn\n", nil)
	resp, _ := keenableP().Search(context.Background(), env, "q", 5)
	want := fmt.Sprintf(`Keenable search failed: Post "http://%s/v1/search": dial tcp %s: connect: connection refused`, addr, addr)
	if resp.Error != want {
		t.Fatalf("search %q", resp.Error)
	}
	pages, _ := keenableP().Extract(context.Background(), env, []string{"https://a.example/x"}, "")
	if len(pages) != 1 || !strings.HasPrefix(pages[0].Error, `Keenable extract failed: Get "http://`+addr+`/v1/fetch?url=https%3A%2F%2Fa.example%2Fx": dial tcp `+addr+": ") {
		t.Fatalf("extract %+v", pages)
	}
}

func TestExaTypedTimeouts(t *testing.T) {
	tm := httputil.EachPhase(600 * time.Second)
	target := exaBaseURL + "/search"
	refused := &url.Error{Op: "Post", URL: target, Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}
	if err := exaTypedTimeouts(refused, tm, time.Second); err != error(refused) {
		t.Fatalf("quick refusal stays the dial error %v", err)
	}
	err := exaTypedTimeouts(refused, tm, 600*time.Second)
	var xe *exaTimeoutError
	if !errors.As(err, &xe) || !errors.Is(err, syscall.ECONNREFUSED) || err.Error() != "connect timed out after 10m0s, the last address tried failed with: "+refused.Error() {
		t.Fatalf("failover %v", err)
	}
	dns := &url.Error{Op: "Post", URL: target, Err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "api.exa.ai", IsNotFound: true}}}
	if err := exaTypedTimeouts(dns, tm, 700*time.Second); err != error(dns) {
		t.Fatalf("dns stays %v", err)
	}
	for _, phase := range []string{httputil.PhaseRead, httputil.PhaseConnect} {
		timedOut := &url.Error{Op: "Post", URL: target, Err: &httputil.TimeoutError{Phase: phase, Limit: 600 * time.Second}}
		if err := exaTypedTimeouts(timedOut, tm, 0); !errors.As(err, &xe) || err.Error() != timedOut.Error() {
			t.Fatalf("typed %s %v", phase, err)
		}
	}
	plain := errors.New("x")
	if exaTypedTimeouts(plain, tm, 0) != plain {
		t.Fatal("non transport errors pass through")
	}
}
