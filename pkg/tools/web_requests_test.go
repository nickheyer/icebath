package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrequests"
)

func TestKeenableTransportFailuresUseRequestsText(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	port := addr[strings.LastIndex(addr, ":")+1:]
	setVar(t, &keenableAPIURL, "http://"+addr)
	env := httpxProviderEnv(t, "KEENABLE_API_KEY=kn\n", nil)
	resp, _ := keenableP().Search(context.Background(), env, "q", 5)
	want := fmt.Sprintf(`Keenable search failed: HTTPConnectionPool(host='127.0.0.1', port=%s): Max retries exceeded with url: /v1/search (Caused by NewConnectionError("HTTPConnection(host='127.0.0.1', port=%s): Failed to establish a new connection: [Errno 111] Connection refused"))`, port, port)
	if resp.Error != want {
		t.Fatalf("search %q", resp.Error)
	}
	pages, _ := keenableP().Extract(context.Background(), env, []string{"https://a.example/x"}, "")
	if len(pages) != 1 || !strings.HasPrefix(pages[0].Error, "Keenable extract failed: HTTPConnectionPool(host='127.0.0.1', port="+port+"): Max retries exceeded with url: /v1/fetch?url=https%3A%2F%2Fa.example%2Fx (Caused by NewConnectionError(") {
		t.Fatalf("extract %+v", pages)
	}
}

func TestExaTypedTimeouts(t *testing.T) {
	tm := pyrequests.Seconds(600.0)
	refused := &pyrequests.Error{Class: "ConnectionError", Text: "pool: refused", Reason: "NewConnectionError"}
	var re *pyrequests.Error
	if err := exaTypedTimeouts(refused, tm, time.Second); !errors.As(err, &re) || re != refused {
		t.Fatalf("quick refusal stays a ConnectionError %v", err)
	}
	err := exaTypedTimeouts(refused, tm, 600*time.Second)
	if !errors.As(err, &re) || re.Class != "ExaTimeoutError" || re.Text != "Connect timed out. (connect timeout=600.0) The last address tried failed with: pool: refused" {
		t.Fatalf("failover %+v", re)
	}
	dns := &pyrequests.Error{Class: "ConnectionError", Text: "pool: dns", Reason: "NameResolutionError", DNS: true}
	if err := exaTypedTimeouts(dns, tm, 700*time.Second); !errors.As(err, &re) || re != dns {
		t.Fatalf("dns stays %v", err)
	}
	for _, e := range []*pyrequests.Error{{Class: "ReadTimeout", Text: "r"}, {Class: "ConnectionError", Reason: "ReadTimeoutError", Text: "b"}, {Class: "ConnectTimeout", Text: "c"}} {
		if err := exaTypedTimeouts(e, tm, 0); !errors.As(err, &re) || re.Class != "ExaTimeoutError" || re.Text != e.Text {
			t.Fatalf("typed %+v", re)
		}
	}
	plain := errors.New("x")
	if exaTypedTimeouts(plain, tm, 0) != plain {
		t.Fatal("non requests errors pass through")
	}
}
