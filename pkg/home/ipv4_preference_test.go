package home

import (
	"context"
	"errors"
	"net"
	"net/http"
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
