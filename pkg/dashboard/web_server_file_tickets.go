package dashboard

import (
	"crypto/rand"
	"encoding/base64"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Lifetimes and use counts of path bound file tickets
const (
	fileTicketDownloadTTLSeconds     = 60
	fileTicketDownloadMaxRedemptions = 3
	fileTicketStreamTTLSeconds       = 60 * 60
	fileTicketMaxTickets             = 1024
)

// Route path each ticket kind admits
var fileTicketRoutes = map[string]string{"download": "/api/fs/download", "stream": "/api/files/stream"}

// Private resolved identity of a ticketed file, its target, owning profile homes and the SSH workspace reader or nil
type FileAuthority struct {
	Target    string
	Homes     []string
	Workspace any
}

// Not found error once a named profile home the file belongs to is missing or deleted
func (a FileAuthority) CheckLive() error {
	for _, h := range a.Homes {
		if err := home.AssertProfileLive(h); err != nil {
			return err
		}
	}
	return nil
}

// Route path and sorted query pairs a ticket admits
type fileTicketScope struct {
	path  string
	query [][2]string
}

func (s fileTicketScope) equal(o fileTicketScope) bool {
	return s.path == o.path && slices.Equal(s.query, o.query)
}

type fileTicket struct {
	scope      fileTicketScope
	expiresAt  float64
	usesLeft   *int
	authority  FileAuthority
	generation *string
}

var (
	fileTicketsMu    sync.Mutex
	fileTickets      = map[string]fileTicket{}
	fileTicketsOrder []string
)

func init() {
	BrowserCredentialOnRotate("file tickets", revokeReplacedFileTickets)
}

// Scope of a route path and its query pairs sorted
func newFileTicketScope(routePath string, query [][2]string) fileTicketScope {
	sorted := slices.Clone(query)
	slices.SortFunc(sorted, func(a, b [2]string) int {
		if c := strings.Compare(a[0], b[0]); c != 0 {
			return c
		}
		return strings.Compare(a[1], b[1])
	})
	return fileTicketScope{path: routePath, query: sorted}
}

// Browser session token the request was admitted with on the loopback gate, false for any other credential
func sessionTokenOf(req *Request) (string, bool) {
	appState := req.App().State
	if pyrepr.Truthy(appState.Get("auth_required")) || pyrepr.Truthy(req.State().Get("native_http_principal")) ||
		pyrepr.Truthy(req.State().Get("token_authenticated")) {
		return "", false
	}
	if token := presentedSessionHeader(req.Headers()); token != "" {
		return token, true
	}
	if token := strings.TrimPrefix(req.Headers().Get("authorization"), "Bearer "); token != "" {
		return token, true
	}
	return "", false
}

// Removes a ticket key, the caller holding the lock
func dropFileTicketLocked(key string) {
	delete(fileTickets, key)
	if i := slices.Index(fileTicketsOrder, key); i >= 0 {
		fileTicketsOrder = slices.Delete(fileTicketsOrder, i, i+1)
	}
}

// Ticket for GET or HEAD of one route with exactly the query, bound to the minting browser credential when given
func mintFileTicket(route string, query [][2]string, authority FileAuthority, sessionToken *string) string {
	routePath := fileTicketRoutes[route]
	download := route == "download"
	now := pythread.Monotonic()
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	ticket := base64.RawURLEncoding.EncodeToString(raw)
	ttl := float64(fileTicketStreamTTLSeconds)
	var usesLeft *int
	if download {
		ttl = fileTicketDownloadTTLSeconds
		n := fileTicketDownloadMaxRedemptions
		usesLeft = &n
	}
	var generation *string
	if sessionToken != nil {
		g := BrowserCredentialGenerationOf(*sessionToken)
		generation = &g
	}
	entry := fileTicket{scope: newFileTicketScope(routePath, query), expiresAt: now + ttl, usesLeft: usesLeft,
		authority: authority, generation: generation}
	fileTicketsMu.Lock()
	defer fileTicketsMu.Unlock()
	for _, key := range slices.Clone(fileTicketsOrder) {
		if fileTickets[key].expiresAt <= now {
			dropFileTicketLocked(key)
		}
	}
	for len(fileTickets) >= fileTicketMaxTickets {
		dropFileTicketLocked(fileTicketsOrder[0])
	}
	fileTickets[ticket] = entry
	fileTicketsOrder = append(fileTicketsOrder, ticket)
	return ticket
}

// Drops every ticket minted under a replaced browser credential, returning how many were dropped
func revokeReplacedFileTickets() int {
	fileTicketsMu.Lock()
	defer fileTicketsMu.Unlock()
	var replaced []string
	for _, key := range fileTicketsOrder {
		if g := fileTickets[key].generation; g != nil && !BrowserCredentialIsCurrent(g) {
			replaced = append(replaced, key)
		}
	}
	for _, key := range replaced {
		dropFileTicketLocked(key)
	}
	return len(replaced)
}

// Counts one admitted request against a download ticket, dropping it at its last use
func spendFileTicketUseLocked(ticket string, entry fileTicket) {
	if entry.usesLeft == nil {
		return
	}
	if *entry.usesLeft > 1 {
		n := *entry.usesLeft - 1
		entry.usesLeft = &n
		fileTickets[ticket] = entry
		return
	}
	dropFileTicketLocked(ticket)
}

// Whether the ticket query parameter authorizes this exact request, spending one use and carrying its authority
func redeemFileTicket(req *Request) bool {
	params := req.QueryParams()
	ticket := params.Get("ticket")
	if ticket == "" || (req.Method() != "GET" && req.Method() != "HEAD") {
		return false
	}
	var query [][2]string
	for _, kv := range params.MultiItems() {
		if kv[0] != "ticket" {
			query = append(query, kv)
		}
	}
	scope := newFileTicketScope(req.URL().Path(), query)
	fileTicketsMu.Lock()
	entry, ok := fileTickets[ticket]
	if !ok || !entry.scope.equal(scope) || entry.expiresAt <= pythread.Monotonic() {
		fileTicketsMu.Unlock()
		return false
	}
	if entry.generation != nil && !BrowserCredentialIsCurrent(entry.generation) {
		dropFileTicketLocked(ticket)
		fileTicketsMu.Unlock()
		return false
	}
	spendFileTicketUseLocked(ticket, entry)
	fileTicketsMu.Unlock()
	req.State().Set("file_ticket_authority", entry.authority)
	return true
}
