package botmode

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Roster age in seconds past which its absence of a target proves nothing
const RosterFreshSeconds = 600

// Refusal to queue an envelope, nothing written, with a stable machine reason and the human text
type EnvelopeRefusedError struct {
	Reason  string
	Message string
}

func (e *EnvelopeRefusedError) Error() string { return e.Message }

// Python class name of the refusal
func (e *EnvelopeRefusedError) PyTypeName() string { return "EnvelopeRefusedError" }

// Matched row for a bare or connection qualified handle, profile or title slug, ambiguous across connections
func ResolveRemoteTarget(rawTarget string, roster []RemoteRosterRow) (row *RemoteRosterRow, ambiguous bool) {
	want, conn, at := strings.Cut(strings.TrimLeft(pystr.Strip(rawTarget), "@"), "@")
	want, conn = pystr.Strip(want), pystr.Strip(conn)
	if want == "" || (at && conn == "") {
		return nil, false
	}
	want = pystr.Lower(want)
	var rows []RemoteRosterRow
	for _, candidate := range roster {
		if conn == "" || pystr.Lower(candidate.ConnectionID) == pystr.Lower(conn) {
			rows = append(rows, candidate)
		}
	}
	var matches []RemoteRosterRow
	for _, candidate := range rows {
		if targetIDs(candidate)[want] {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		for _, candidate := range rows {
			if targetAliases(candidate)[want] {
				matches = append(matches, candidate)
			}
		}
	}
	switch len(matches) {
	case 0:
		return nil, false
	case 1:
		return &matches[0], false
	}
	return nil, true
}

// Online flag of the target on a fresh roster, false when a fresh roster omits it, nil when unknown
func targetLiveness(root string, target RemoteRosterRow) *bool {
	fi, err := os.Stat(filepath.Join(RelayRoot(root), RosterFile))
	if err != nil {
		return nil
	}
	age := float64(time.Now().UnixNano())/1e9 - float64(fi.ModTime().UnixNano())/1e9
	var roster []RemoteRosterRow
	if age <= RosterFreshSeconds {
		roster = ReadRemoteRoster(root)
	}
	if len(roster) == 0 {
		return nil
	}
	for _, row := range roster {
		if row.ConnectionID == target.ConnectionID && row.Profile == target.Profile {
			return row.Online
		}
	}
	offline := false
	return &offline
}

// Hex of a random version 4 UUID
func uuid4Hex() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}

// First non empty text of the values, else the fallback
func firstText(fallback string, values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return fallback
}

// Queues a cross connection DM for the Desktop relay, refusing runtime_offline unwritten for a target known offline
func EnqueueEnvelope(ctx context.Context, root string, target RemoteRosterRow, message, senderProfile, senderHandle string) (*pyjson.Object, error) {
	if live := targetLiveness(root, target); live != nil && !*live {
		label := fmt.Sprintf("@%s on %s", firstText("?", target.Handle, target.Profile), firstText("?", target.ConnectionLabel, target.ConnectionID))
		return nil, &EnvelopeRefusedError{Reason: "runtime_offline", Message: label + " is offline right now — the message was NOT queued. " +
			"Try again once that machine reconnects to the Desktop."}
	}
	base, err := ensureRelayDirs(ctx, root)
	if err != nil {
		return nil, err
	}
	envelope := pyjson.ObjectOf("id", uuid4Hex(), "created_at", time.Now().Unix(), "canonical_delivery_v1", true,
		"from_profile", senderProfile, "from_handle", senderHandle, "target_connection", target.ConnectionID,
		"target_profile", target.Profile, "target_handle", target.Handle, "message", message)
	id, _ := envelope.GetOr("id", "").(string)
	if err := atomicRelayWrite(ctx, filepath.Join(base, OutboxDir, id+".json"), envelope, false); err != nil {
		return nil, err
	}
	return envelope, nil
}
