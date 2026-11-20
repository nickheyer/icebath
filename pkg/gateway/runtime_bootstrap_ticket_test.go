package gateway

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Whether err is the PermissionError of a refused bootstrap ticket
func isPermissionError(err error) bool {
	var py *PyError
	return errors.As(err, &py) && py.Kind == "PermissionError"
}

func TestTicketAtomicSingleUseProfilePurposeExpiryAndCapacity(t *testing.T) {
	var clockMu sync.Mutex
	clock := 0.0
	previous := ticketMonotonic
	ticketMonotonic = func() float64 { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	t.Cleanup(func() { ticketMonotonic = previous })
	store, err := NewTicketStore("boot", map[string]bool{"a": true, "b": true})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := store.Mint("a", "uid:1", "interactive", "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"b", "interactive"}, {"a", "exposure"}} {
		profile := pair[0]
		if _, err := store.Redeem(ticket, &profile, pair[1]); !isPermissionError(err) {
			t.Fatalf("%v: err = %v", pair, err)
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			profile := "a"
			if grant, err := store.Redeem(ticket, &profile, "interactive"); err == nil && grant != nil {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != 1 {
		t.Fatalf("granted = %d, want exactly one", granted)
	}
	expired, _ := store.Mint("a", "uid:1", "interactive", "", false)
	native, _ := store.Mint("a", "uid:1", "native-http", "", false)
	clockMu.Lock()
	clock = 30
	clockMu.Unlock()
	profile := "a"
	if _, err := store.Redeem(expired, &profile, "interactive"); !isPermissionError(err) {
		t.Fatalf("expired redeem err = %v", err)
	}
	if _, err := store.Redeem(native, nil, "native-http"); !isPermissionError(err) || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("native redeem err = %v", err)
	}
	exposure, _ := store.Mint("a", "uid:1", "exposure", "", false)
	grant, err := store.Redeem(exposure, &profile, "exposure")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(grant["capabilities"], map[string]bool{"transport:delegate": true}) {
		t.Fatalf("capabilities = %v", grant["capabilities"])
	}
	for range ticketMaxEntries {
		if _, err := store.Mint("a", "uid:1", "interactive", "", false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Mint("a", "uid:1", "interactive", "", false); !isPermissionError(err) {
		t.Fatalf("over capacity err = %v", err)
	}
}
