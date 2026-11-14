package home

import (
	"context"
	"errors"
	"net"
	"net/http"
	"reflect"
	"testing"
)

func TestPreferIPv4ResolvesUnspecifiedFamiliesAsIPv4(t *testing.T) {
	prev := lookupIPv4
	t.Cleanup(func() { lookupIPv4 = prev })
	lookupIPv4 = func(_ context.Context, host string) ([]string, error) {
		switch host {
		case "dual.example":
			return []string{"192.0.2.1", "192.0.2.2"}, nil
		case "v6only.example":
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return nil, errors.New("unexpected lookup " + host)
	}
	var dialed []string
	refused := errors.New("refused")
	dial := PreferIPv4(func(_ context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, network+" "+addr)
		return nil, refused
	})
	ctx := context.Background()
	if _, err := dial(ctx, "tcp", "dual.example:443"); err != refused {
		t.Fatalf("err = %v, want the first attempt's error", err)
	}
	dial(ctx, "tcp", "v6only.example:443")
	dial(ctx, "tcp6", "dual.example:443")
	dial(ctx, "unix", "/run/x.sock")
	want := []string{"tcp4 192.0.2.1:443", "tcp4 192.0.2.2:443", "tcp v6only.example:443", "tcp6 dual.example:443", "unix /run/x.sock"}
	if len(dialed) != len(want) {
		t.Fatalf("dialed %v", dialed)
	}
	for i := range want {
		if dialed[i] != want[i] {
			t.Fatalf("dialed %v, want %v", dialed, want)
		}
	}
}

func TestApplyIPv4PreferencePatchesOnceAndOnlyWhenForced(t *testing.T) {
	tr := http.DefaultTransport.(*http.Transport)
	orig := tr.DialContext
	t.Cleanup(func() { tr.DialContext = orig; ipv4Preferred.Store(false) })
	ApplyIPv4Preference(false)
	if IPv4Preferred() {
		t.Fatal("unforced call patched")
	}
	ApplyIPv4Preference(true)
	patched := tr.DialContext
	ApplyIPv4Preference(true)
	if !IPv4Preferred() || patched == nil {
		t.Fatal("forced call did not patch")
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	conn, err := tr.DialContext(context.Background(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

// Default transport dialing through record before the preference is applied, both restored after the test
func recordingDefaultDial(t *testing.T) (*http.Transport, *[]string) {
	t.Helper()
	tr := http.DefaultTransport.(*http.Transport)
	orig, prevPreferred := tr.DialContext, ipv4Preferred.Load()
	prevLookup := lookupIPv4
	t.Cleanup(func() { tr.DialContext, lookupIPv4 = orig, prevLookup; ipv4Preferred.Store(prevPreferred) })
	ipv4Preferred.Store(false)
	var calls []string
	tr.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
		calls = append(calls, network+" "+addr)
		return nil, errors.New("recorded")
	}
	lookupIPv4 = func(_ context.Context, host string) ([]string, error) { return []string{"93.184.216.34"}, nil }
	return tr, &calls
}

func TestDoublePatchIsSafe(t *testing.T) {
	tr, _ := recordingDefaultDial(t)
	ApplyIPv4Preference(true)
	first := reflect.ValueOf(tr.DialContext).Pointer()
	ApplyIPv4Preference(true)
	if reflect.ValueOf(tr.DialContext).Pointer() != first {
		t.Fatal("second apply wrapped the dialer again")
	}
}

func TestAfUnspecBecomesAfInet(t *testing.T) {
	tr, calls := recordingDefaultDial(t)
	ApplyIPv4Preference(true)
	tr.DialContext(context.Background(), "tcp", "example.com:80")
	if n := len(*calls); n == 0 || (*calls)[n-1] != "tcp4 93.184.216.34:80" {
		t.Fatalf("AF_UNSPEC should be rewritten to AF_INET, dialed %v", *calls)
	}
}

func TestExplicitFamilyPreserved(t *testing.T) {
	tr, calls := recordingDefaultDial(t)
	ApplyIPv4Preference(true)
	tr.DialContext(context.Background(), "tcp6", "example.com:80")
	if n := len(*calls); n == 0 || (*calls)[n-1] != "tcp6 example.com:80" {
		t.Fatalf("Explicit AF_INET6 should pass through, dialed %v", *calls)
	}
}
