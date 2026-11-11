package botmode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Relay subdirectories of queued, drained and answered envelopes
const (
	OutboxDir  = "outbox"
	ClaimedDir = "claimed"
	RepliesDir = "replies"
)

// Relay timing budgets in seconds, mirrored by the Desktop relay budget
const (
	TurnWaitSecondsFallback               = 120
	DefaultEnvelopeTTLSeconds             = 900
	TurnAttemptTimeoutSeconds             = 600
	TurnMaxAttempts                       = 2
	DesktopDeliverSettlementMarginSeconds = 180
	DesktopDeliverTimeoutSeconds          = TurnWaitSecondsFallback + TurnAttemptTimeoutSeconds*TurnMaxAttempts + DesktopDeliverSettlementMarginSeconds
	ReofferAfterSeconds                   = DesktopDeliverTimeoutSeconds + 60
	ReplyWaitSeconds                      = ReofferAfterSeconds + DesktopDeliverTimeoutSeconds + 60
	StaleAfterSeconds                     = 6 * 3600
)

// Envelope ids a reply may be written under
var envelopeIDRe = regexp.MustCompile(`^[0-9a-f]{32}\z`)

// ValueError a reply raises for an envelope id that is not 32 lowercase hex characters
type InvalidEnvelopeError struct{ Msg string }

func (e *InvalidEnvelopeError) Error() string { return e.Msg }

// ValueError of an envelope file that is not UTF-8 JSON holding one object
type envelopeDecodeError struct{ msg string }

func (e *envelopeDecodeError) Error() string { return e.msg }

// Creates the relay outbox, claimed and replies directories under a live home and returns the relay root
func ensureRelayDirs(ctx context.Context, root string) (string, error) {
	base := RelayRoot(root)
	for _, sub := range []string{OutboxDir, ClaimedDir, RepliesDir} {
		if _, err := home.Mkdir(ctx, filepath.Join(base, sub)); err != nil {
			return "", err
		}
	}
	return base, nil
}

// Atomic owner only JSON write of one relay file
func atomicRelayWrite(ctx context.Context, target string, payload any, sortKeys bool) error {
	return config.AtomicJSONWriteMode(ctx, target, payload, 0o600, pyjson.SortKeys(sortKeys))
}

// One bot_mode config key read from the readonly config, nil when absent or unreadable
func botModeCfg(ctx context.Context, key string) (value any) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("bot_mode.%s config read failed", key), "error", r)
			value = nil
		}
	}()
	h := home.Dir(ctx)
	cfg, err := config.LoadReadonly(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		slog.Debug(fmt.Sprintf("bot_mode.%s config read failed", key), "error", err)
		return nil
	}
	section := cfg["bot_mode"]
	if !pyrepr.Truthy(section) {
		return nil
	}
	dict, ok := section.(map[string]any)
	if !ok {
		slog.Debug(fmt.Sprintf("bot_mode.%s config read failed", key), "error", "bot_mode is not a mapping")
		return nil
	}
	return dict[key]
}

// Configured drain TTL in seconds, the default for a missing or unusable value, zero or less disabling expiry
func envelopeTTLSeconds(ctx context.Context) *big.Int {
	value := botModeCfg(ctx, "envelope_ttl_seconds")
	if value == nil {
		return big.NewInt(DefaultEnvelopeTTLSeconds)
	}
	ttl, err := pyint.Of(value)
	if err != nil {
		slog.Debug(fmt.Sprintf("Invalid bot_mode.envelope_ttl_seconds %s; using fallback", pyrepr.Repr(value)))
		return big.NewInt(DefaultEnvelopeTTLSeconds)
	}
	return ttl
}

// Deletes relay files older than the stale horizon from every relay directory and returns the count
func sweepStaleRelay(base string) int {
	total := 0
	for _, sub := range []string{ClaimedDir, RepliesDir, OutboxDir} {
		total += pyfs.UnlinkFilesOlderThan(filepath.Join(base, sub), "*.json", StaleAfterSeconds, nil)
	}
	return total
}

// Claim order of one relay file, its mtime then its name, zero mtime when it cannot be read
type queuedOrder struct {
	mtime float64
	name  string
}

// Claim order key of a relay file
func queuedAt(path string) queuedOrder {
	info, err := os.Stat(path)
	if err != nil {
		return queuedOrder{0, filepath.Base(path)}
	}
	return queuedOrder{float64(info.ModTime().UnixNano()) / 1e9, filepath.Base(path)}
}

// Relay JSON files of a directory in claim order
func relayFilesInOrder(dir string) []string {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.SliceStable(paths, func(i, j int) bool {
		a, b := queuedAt(paths[i]), queuedAt(paths[j])
		if a.mtime != b.mtime {
			return a.mtime < b.mtime
		}
		return a.name < b.name
	})
	return paths
}

// Envelope object of a relay file read BOM tolerant, an OS error or a decode error on failure
func readEnvelope(path string) (*pyjson.Object, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(raw) {
		return nil, &envelopeDecodeError{"'utf-8' codec can't decode the envelope"}
	}
	text := strings.TrimPrefix(string(raw), "\ufeff")
	value, err := pyjson.Loads(text)
	if err != nil {
		return nil, &envelopeDecodeError{err.Error()}
	}
	envelope, ok := value.(*pyjson.Object)
	if !ok {
		return nil, &envelopeDecodeError{"expected a JSON object, got " + pyrepr.TypeName(value)}
	}
	return envelope, nil
}

// Whether an error is one the Python suppresses as OSError or ValueError
func relaySuppressed(err error) bool {
	var decode *envelopeDecodeError
	var invalid *InvalidEnvelopeError
	var conversion *pyint.Error
	var pathErr *os.PathError
	var linkErr *os.LinkError
	switch {
	case errors.As(err, &decode), errors.As(err, &invalid), errors.As(err, &pathErr), errors.As(err, &linkErr):
		return true
	case errors.As(err, &conversion):
		return conversion.Kind == "ValueError"
	}
	return false
}

// Creation time of an envelope, its created_at field else the file mtime
func envelopeCreated(envelope *pyjson.Object, path string) (float64, error) {
	raw := envelope.GetOr("created_at", nil)
	if !pyrepr.Truthy(raw) {
		info, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		return float64(info.ModTime().UnixNano()) / 1e9, nil
	}
	return pyint.Float(raw)
}

// Target label of an envelope as its sender reads it
func envelopeLabel(envelope *pyjson.Object) string {
	handle, connection := strOr(envelope.GetOr("target_handle", nil)), strOr(envelope.GetOr("target_connection", nil))
	if handle == "" {
		handle = "?"
	}
	if connection == "" {
		connection = "?"
	}
	return "@" + handle + " on " + connection
}

// True for an outbox envelope older than the TTL, its queued_expired reply written so the sender's waiter resolves
func expireIfStale(ctx context.Context, root, path string, ttl *big.Int, now float64) (bool, error) {
	envelope, err := readEnvelope(path)
	if err != nil {
		return false, nil
	}
	created, err := envelopeCreated(envelope, path)
	if err != nil {
		if relaySuppressed(err) {
			return false, nil
		}
		return false, err
	}
	limit, _ := new(big.Float).SetInt(ttl).Float64()
	if now-created <= limit {
		return false, nil
	}
	message := fmt.Sprintf("queued message to %s expired after %ss waiting for the Desktop to drain it \u2014 it was NOT delivered. "+
		"Resend once the Desktop reconnects.", envelopeLabel(envelope), ttl.String())
	if _, err := WriteReply(ctx, root, strOr(envelope.GetOr("id", nil)), "", message, "queued_expired"); err != nil && !relaySuppressed(err) {
		return false, err
	}
	return true, nil
}

// One drained envelope with its claim order
type queuedEnvelope struct {
	order    queuedOrder
	envelope *pyjson.Object
}

// Whether a value Python could not place in a set
func unhashable(value any) bool {
	switch value.(type) {
	case *pyjson.Object, []any, map[string]any:
		return true
	}
	return false
}

// Python set membership of an envelope id among the drained ids
func seenID(seen []any, id any) (bool, error) {
	if unhashable(id) {
		return false, &pyint.Error{Kind: "TypeError", Msg: "unhashable type: '" + pyrepr.TypeName(id) + "'"}
	}
	for _, s := range seen {
		if pyrepr.Equal(s, id) {
			return true, nil
		}
	}
	return false, nil
}

// Settles a claim from before canonical delivery as unknown once the old re-offer point passed
func settlePreUpgradeClaim(ctx context.Context, root, path, envID, label string, now float64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if now-float64(info.ModTime().UnixNano())/1e9 < ReofferAfterSeconds {
		return nil
	}
	_, err = WriteReply(ctx, root, envID, "", fmt.Sprintf("the message to %s was picked up before this gateway was upgraded and its delivery was "+
		"never reported \u2014 it may or may not have been delivered. It will not be retried; check with "+
		"the recipient before resending.", label), tools.ReasonUnknown)
	return err
}

// Claimed canonical envelopes still unanswered, replayed on every drain until a reply lands or the waiter is gone
func replayUnanswered(ctx context.Context, root, base string, seen []any, now float64) ([]queuedEnvelope, error) {
	var out []queuedEnvelope
	for _, path := range relayFilesInOrder(filepath.Join(base, ClaimedDir)) {
		if _, err := os.Stat(filepath.Join(base, RepliesDir, filepath.Base(path))); err == nil {
			continue
		}
		envelope, err := readEnvelope(path)
		if err != nil {
			continue
		}
		id := envelope.GetOr("id", nil)
		known, err := seenID(seen, id)
		if err != nil {
			return nil, err
		}
		if known {
			continue
		}
		envID, label := strOr(id), envelopeLabel(envelope)
		if envelope.GetOr("canonical_delivery_v1", nil) != true {
			if err := settlePreUpgradeClaim(ctx, root, path, envID, label, now); err != nil && !relaySuppressed(err) {
				return nil, err
			}
			continue
		}
		created, err := envelopeCreated(envelope, path)
		if err != nil {
			if relaySuppressed(err) {
				continue
			}
			return nil, err
		}
		if now-created > ReplyWaitSeconds {
			_, err := WriteReply(ctx, root, envID, "", fmt.Sprintf("no reply from %s within %ds of sending \u2014 the Desktop picked "+
				"the message up but never reported a delivery. It will not be retried; resend if it matters.", label, ReplyWaitSeconds), "delivery_timeout")
			if err != nil && !relaySuppressed(err) {
				return nil, err
			}
			continue
		}
		out = append(out, queuedEnvelope{queuedAt(path), envelope})
	}
	return out, nil
}

// Drains the outbox into claimed oldest first, expired envelopes answered and removed, unanswered claims replayed
func ClaimPendingEnvelopes(ctx context.Context, root string) ([]*pyjson.Object, error) {
	base, err := ensureRelayDirs(ctx, root)
	if err != nil {
		return nil, err
	}
	sweepStaleRelay(base)
	ttl := envelopeTTLSeconds(ctx)
	now := float64(time.Now().UnixNano()) / 1e9
	var out []queuedEnvelope
	for _, path := range relayFilesInOrder(filepath.Join(base, OutboxDir)) {
		if ttl.Sign() > 0 {
			expired, err := expireIfStale(ctx, root, path, ttl, now)
			if err != nil {
				return nil, err
			}
			if expired {
				_ = os.Remove(path)
				continue
			}
		}
		claimed := filepath.Join(base, ClaimedDir, filepath.Base(path))
		order := queuedAt(path)
		if err := os.Rename(path, claimed); err != nil {
			continue
		}
		envelope, err := readEnvelope(claimed)
		if err != nil {
			continue
		}
		out = append(out, queuedEnvelope{order, envelope})
	}
	seen := make([]any, 0, len(out))
	for _, row := range out {
		id, ok := row.envelope.Get("id")
		if !ok {
			return nil, &pyint.Error{Kind: "KeyError", Msg: "'id'"}
		}
		if unhashable(id) {
			return nil, &pyint.Error{Kind: "TypeError", Msg: "unhashable type: '" + pyrepr.TypeName(id) + "'"}
		}
		seen = append(seen, id)
	}
	replayed, err := replayUnanswered(ctx, root, base, seen, now)
	if err != nil {
		return nil, err
	}
	out = append(out, replayed...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].order, out[j].order
		if a.mtime != b.mtime {
			return a.mtime < b.mtime
		}
		return a.name < b.name
	})
	envelopes := make([]*pyjson.Object, len(out))
	for i, row := range out {
		envelopes[i] = row.envelope
	}
	return envelopes, nil
}

// Persists the relayed reply or delivery error under its envelope id, the first settled reply standing
func WriteReply(ctx context.Context, root string, envelopeID, reply, errText, reason any) (string, error) {
	base, err := ensureRelayDirs(ctx, root)
	if err != nil {
		return "", err
	}
	safe := pystr.Strip(strOr(envelopeID))
	if !envelopeIDRe.MatchString(safe) {
		return "", &InvalidEnvelopeError{"invalid envelope id: " + pyrepr.Repr(envelopeID)}
	}
	path := filepath.Join(base, RepliesDir, safe+".json")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	message, code := strOr(errText), strOr(reason)
	if code == "" && message != "" {
		code = tools.ClassifyAgentError(message)
	}
	payload := pyjson.ObjectOf("id", safe, "at", time.Now().Unix(), "reply", strOr(reply), "error", message, "reason", code)
	return path, atomicRelayWrite(ctx, path, payload, false)
}

// Atomically persists the Desktop pushed remote roster, deduplicated by connection and profile, and returns its size
func WriteRemoteRoster(ctx context.Context, root string, rows any) (int, error) {
	base, err := ensureRelayDirs(ctx, root)
	if err != nil {
		return 0, err
	}
	byKey := map[[2]string]RemoteRosterRow{}
	list, _ := rows.([]any)
	for _, row := range list {
		norm, ok := normalizeRosterRow(row)
		if !ok {
			continue
		}
		key := [2]string{norm.ConnectionID, norm.Profile}
		if _, held := byKey[key]; !held {
			byKey[key] = norm
		}
	}
	keys := make([][2]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	cleaned := make([]any, len(keys))
	for i, key := range keys {
		row := byKey[key]
		out := pyjson.ObjectOf("profile", row.Profile, "handle", row.Handle, "connection_id", row.ConnectionID,
			"connection_label", row.ConnectionLabel, "title", row.Title, "description", row.Description)
		if row.Online != nil {
			out.Set("online", *row.Online)
		}
		cleaned[i] = out
	}
	payload := pyjson.ObjectOf("updated_at", time.Now().Unix(), "agents", cleaned)
	if err := atomicRelayWrite(ctx, filepath.Join(base, RosterFile), payload, true); err != nil {
		return 0, err
	}
	return len(cleaned), nil
}
