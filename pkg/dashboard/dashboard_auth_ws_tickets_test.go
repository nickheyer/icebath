package dashboard

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func resetTicketsForTest(t *testing.T) {
	resetWSTickets()
	t.Cleanup(resetWSTickets)
}

func TestMintAndConsumeRoundTrip(t *testing.T) {
	resetTicketsForTest(t)
	info, err := ConsumeWSTicket(MintWSTicket("u1", "nous", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	if info.Vals["user_id"] != "u1" || info.Vals["provider"] != "nous" {
		t.Fatalf("info %v", info.Vals)
	}
	if _, ok := info.Vals["minted_at"]; !ok {
		t.Fatal("missing minted_at")
	}
}

func TestMintAndConsumeTicketHasMinimumLength(t *testing.T) {
	resetTicketsForTest(t)
	if ticket := MintWSTicket("u1", "nous", "", nil); len(ticket) < 32 {
		t.Fatalf("ticket %q too short", ticket)
	}
}

func TestSingleUseSecondConsumeRaises(t *testing.T) {
	resetTicketsForTest(t)
	ticket := MintWSTicket("u1", "plain", "", nil)
	if _, err := ConsumeWSTicket(ticket); err != nil {
		t.Fatal(err)
	}
	_, err := ConsumeWSTicket(ticket)
	var invalid *TicketInvalid
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("second consume: %v", err)
	}
}

func TestSingleUseUnknownTicketRejected(t *testing.T) {
	resetTicketsForTest(t)
	if _, err := ConsumeWSTicket("nope-never-minted"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown ticket: %v", err)
	}
}

func TestTTLExpiredTicketRejected(t *testing.T) {
	resetTicketsForTest(t)
	ticket := MintWSTicket("u1", "plain", "", nil)
	wsTicketsMu.Lock()
	entry := wsTickets[ticket]
	entry.expiresAt -= WSTicketTTLSeconds + 1
	wsTickets[ticket] = entry
	wsTicketsMu.Unlock()
	if _, err := ConsumeWSTicket(ticket); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired ticket: %v", err)
	}
}

func TestErrorMessagesUnknownTicketErrorTruncatesValue(t *testing.T) {
	resetTicketsForTest(t)
	long := strings.Repeat("a", 100)
	_, err := ConsumeWSTicket(long)
	if err == nil || strings.Contains(err.Error(), long) || !strings.Contains(err.Error(), long[:8]) {
		t.Fatalf("message %v", err)
	}
}

func TestConcurrencyMintAndConsumeConcurrent(t *testing.T) {
	resetTicketsForTest(t)
	var mu sync.Mutex
	seen := map[string]bool{}
	var errs []error
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			info, err := ConsumeWSTicket(MintWSTicket(fmt.Sprintf("u%d", i), "plain", "", nil))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			seen[info.Vals["user_id"].(string)] = true
		}(i)
	}
	wg.Wait()
	if len(errs) != 0 || len(seen) != 20 {
		t.Fatalf("errors %v, users %d", errs, len(seen))
	}
}

func TestInternalCredentialResetClearsAndRemints(t *testing.T) {
	resetTicketsForTest(t)
	first := InternalWSCredential()
	resetWSTickets()
	if _, err := ConsumeInternalWSCredential(first); err == nil {
		t.Fatal("a reset must invalidate the old credential")
	}
	second := InternalWSCredential()
	if second == first {
		t.Fatal("reset must mint a new credential")
	}
	info, err := ConsumeInternalWSCredential(second)
	if err != nil || info.Vals["user_id"] != InternalWSUserID {
		t.Fatalf("info %v err %v", info, err)
	}
}

func TestInternalCredentialIndependentOfTicketStore(t *testing.T) {
	resetTicketsForTest(t)
	cred := InternalWSCredential()
	ticket := MintWSTicket("u1", "nous", "", nil)
	if _, err := ConsumeInternalWSCredential(cred); err != nil {
		t.Fatal(err)
	}
	info, err := ConsumeWSTicket(ticket)
	if err != nil || info.Vals["user_id"] != "u1" {
		t.Fatalf("info %v err %v", info, err)
	}
}
