package dashboard

import (
	"crypto/subtle"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	WSTicketTTLSeconds = 30
	InternalWSUserID   = "server-internal"
	InternalWSProvider = "server-internal"
)

// Expiry and identity of one browser ticket
type wsTicketEntry struct {
	expiresAt int64
	info      *pyjson.Object
}

var (
	wsTicketsMu        sync.Mutex
	wsTickets          = map[string]wsTicketEntry{}
	internalCredential *string
)

// Ticket missing, expired or already consumed
type TicketInvalid struct{ Msg string }

func (e *TicketInvalid) Error() string      { return e.Msg }
func (e *TicketInvalid) PyTypeName() string { return "TicketInvalid" }

// One shot ticket bound to the identity, the extra context riding along to the consumer
func MintWSTicket(userID, provider, issuer string, extra *pyjson.Object) string {
	ticket := tokenURLSafe(32)
	now := time.Now().Unix()
	info := pyjson.ObjectOf("user_id", userID, "provider", provider, "minted_at", now)
	if extra != nil {
		for _, k := range extra.Keys {
			info.Set(k, extra.Vals[k])
		}
	}
	if issuer != "" {
		info.Set("issuer", issuer)
	}
	wsTicketsMu.Lock()
	defer wsTicketsMu.Unlock()
	wsTickets[ticket] = wsTicketEntry{expiresAt: now + WSTicketTTLSeconds, info: info}
	gcExpiredTicketsLocked()
	return ticket
}

// Validates and consumes a single use ticket, TicketInvalid when missing, expired or used
func ConsumeWSTicket(ticket string) (*pyjson.Object, error) {
	now := time.Now().Unix()
	wsTicketsMu.Lock()
	defer wsTicketsMu.Unlock()
	entry, ok := wsTickets[ticket]
	if !ok {
		truncated := "<empty>"
		if ticket != "" {
			truncated = string([]rune(ticket)[:min(8, len([]rune(ticket)))]) + "…"
		}
		return nil, &TicketInvalid{Msg: "unknown ticket: " + truncated}
	}
	delete(wsTickets, ticket)
	if entry.expiresAt < now {
		return nil, &TicketInvalid{Msg: "expired"}
	}
	return entry.info, nil
}

// Drops expired tickets with the lock held
func gcExpiredTicketsLocked() {
	now := time.Now().Unix()
	for t, e := range wsTickets {
		if e.expiresAt < now {
			delete(wsTickets, t)
		}
	}
}

// Process lifetime credential for server spawned WebSocket clients, minted once
func InternalWSCredential() string {
	wsTicketsMu.Lock()
	defer wsTicketsMu.Unlock()
	if internalCredential == nil {
		c := tokenURLSafe(32)
		internalCredential = &c
	}
	return *internalCredential
}

// Validates the reusable internal credential, the fixed server identity on a match
func ConsumeInternalWSCredential(value string) (*pyjson.Object, error) {
	wsTicketsMu.Lock()
	expected := internalCredential
	wsTicketsMu.Unlock()
	if value == "" || expected == nil {
		return nil, &TicketInvalid{Msg: "no internal credential"}
	}
	if subtle.ConstantTimeCompare([]byte(value), []byte(*expected)) != 1 {
		return nil, &TicketInvalid{Msg: "internal credential mismatch"}
	}
	return pyjson.ObjectOf("user_id", InternalWSUserID, "provider", InternalWSProvider), nil
}

// Drops every ticket and the internal credential
func resetWSTickets() {
	wsTicketsMu.Lock()
	defer wsTicketsMu.Unlock()
	clear(wsTickets)
	internalCredential = nil
}
