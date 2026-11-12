package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

const DeliveryDirName = "bot_live_delivery"

const deliverySequenceFile = ".sequence"

var deliveryOwnerKeys = []string{"profile_home", "session_id", "lease_id", "live_session_id"}

var deliveryTerminal = map[string]bool{"settled": true, "failed": true, "cancelled": true, "ambiguous": true}

var deliveryIDRe = regexp.MustCompile(`\A[0-9a-f]{32,64}\z`)

// Poll interval of a receipt waiter, a variable so tests can shorten it
var deliveryPollInterval = 500 * time.Millisecond

// Mailbox dirs and tickets already reported by this process, warned once and debug logged after
var (
	deliveryWarnMu         sync.Mutex
	warnedUnhardened       = map[string]bool{}
	warnedUnusableMailbox  = map[string]bool{}
	warnedUnreadableTicket = map[string]bool{}
)

// Logs at warning the first time a key is seen in a set and at debug after
func deliveryWarn(set map[string]bool, key, msg string) {
	deliveryWarnMu.Lock()
	seen := set[key]
	set[key] = true
	deliveryWarnMu.Unlock()
	if seen {
		slog.Debug(msg)
	} else {
		slog.Warn(msg)
	}
}

func deliveryForget(set map[string]bool, key string) {
	deliveryWarnMu.Lock()
	delete(set, key)
	deliveryWarnMu.Unlock()
}

// Current Bot Chat tip id, empty when the profile has no store or no Bot Chat yet
func botChatTip(ctx context.Context, profileHome string) (string, error) {
	if st, err := os.Stat(filepath.Join(profileHome, "state.db")); err != nil || !st.Mode().IsRegular() {
		return "", nil
	}
	db, err := state.NewSessionDB(ctx, filepath.Join(profileHome, "state.db"), true)
	if err != nil {
		return "", err
	}
	defer db.Close()
	row, err := db.GetSessionByTitle(ctx, "Bot Chat")
	if err != nil || row == nil {
		return "", err
	}
	id, _ := row.Vals["id"].(string)
	return db.GetCompressionTip(ctx, id)
}

// Profile authority and its exact Bot Chat without a lease, nil when the profile has no store
func FindCanonicalLiveOwner(ctx context.Context, profileHome string) (map[string]any, error) {
	resolved, err := filepath.Abs(profileHome)
	if err != nil {
		return nil, err
	}
	if r, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = r
	}
	discovery := DiscoverGatewayEndpoint(ctx, resolved, 5*time.Second)
	if discovery.State != "ready" || discovery.Endpoint == nil {
		return nil, errors.New("profile authority is not ready")
	}
	if st, err := os.Stat(filepath.Join(resolved, "state.db")); err != nil || !st.Mode().IsRegular() {
		return nil, nil
	}
	tip, err := botChatTip(ctx, resolved)
	if err != nil {
		return nil, err
	}
	return map[string]any{"profile_home": resolved, "session_id": tip, "canonical": true,
		"lease_id": discovery.Endpoint.InstanceID, "live_session_id": tip}, nil
}

// Authority call that died before its RPC reached the socket, a definite no send safe to retry
type PreDialError struct{ Msg string }

func (e *PreDialError) Error() string { return e.Msg }

// Calls this home's already running authority deliver door, never starting a fallback
func AuthorityDelivery(ctx context.Context, home string, params map[string]any) (map[string]any, error) {
	return authorityRPC(ctx, home, "bot_relay.deliver", params)
}

// Settles a delivery id as never sent through the same authority, or answers its existing receipt
func AuthoritySettle(ctx context.Context, home string, params map[string]any) (map[string]any, error) {
	return authorityRPC(ctx, home, "bot_relay.settle", params)
}

// Absolute, symlink resolved form of a profile home like Path.resolve
func deliveryResolveHome(profileHome string) string {
	abs, err := filepath.Abs(profileHome)
	if err != nil {
		return profileHome
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	return abs
}

// One RPC to the ready authority of a home, failures before the request went out wrapped as PreDialError
func authorityRPC(ctx context.Context, profileHome, method string, params map[string]any) (map[string]any, error) {
	h := deliveryResolveHome(profileHome)
	rpcStarted := false
	result, err := func() (map[string]any, error) {
		discovery := DiscoverGatewayEndpoint(ctx, h, 5*time.Second)
		if discovery.State != "ready" || discovery.Endpoint == nil {
			return nil, errors.New("profile authority is not ready")
		}
		ticket, err := sessionTicket(h, discovery.Endpoint, "", "", "", false)
		if err != nil {
			return nil, err
		}
		target, protocols := GatewayWSTarget(discovery.Endpoint, ticket)
		dialer := websocket.Dialer{Proxy: nil, HandshakeTimeout: 10 * time.Second, Subprotocols: protocols}
		conn, resp, err := dialer.DialContext(ctx, target, http.Header{})
		if err != nil {
			return nil, HandshakeError(resp, err)
		}
		conn.SetReadLimit(1 << 20)
		sock := &wsSocket{conn: conn}
		if sock.Subprotocol() != CanonicalGatewayProtocol {
			sock.Close()
			return nil, errors.New("authority protocol mismatch")
		}
		client := NewGatewayClient(sock).Start()
		defer client.Close()
		rpcStarted = true
		return client.RPC(ctx, method, params)
	}()
	if err != nil && !rpcStarted {
		return nil, &PreDialError{Msg: err.Error()}
	}
	return result, err
}

// Owner pin of the four identity keys, refused when incomplete or from another profile home
func liveOwnerPin(profileHome string, owner map[string]any) (map[string]string, error) {
	pinned := map[string]string{}
	for _, key := range deliveryOwnerKeys {
		value, ok := owner[key].(string)
		if !ok || (value == "" && key != "session_id" && key != "live_session_id") {
			return nil, errors.New("owner requires profile_home, session_id, lease_id and live_session_id")
		}
		pinned[key] = value
	}
	if pinned["profile_home"] != deliveryResolveHome(profileHome) {
		return nil, errors.New("owner belongs to a different profile home")
	}
	return pinned, nil
}

func checkDeliveryID(value string) (string, error) {
	if !deliveryIDRe.MatchString(value) {
		return "", errors.New("delivery id must be 32 to 64 lowercase hex characters")
	}
	return value, nil
}

func deliveryRoot(profileHome string) string {
	return filepath.Join(deliveryResolveHome(profileHome), "runtime", DeliveryDirName)
}

// Whether any delivery was ever admitted for this profile, the mailbox existing only after one
func HasMailbox(profileHome string) bool {
	st, err := os.Stat(deliveryRoot(profileHome))
	return err == nil && st.IsDir()
}

// Mailbox directory this token cannot list, create or lock inside, a permission error
type MailboxUnusable struct {
	Errno    syscall.Errno
	Strerror string
	Filename string
}

func (e *MailboxUnusable) Error() string {
	return fmt.Sprintf("[Errno %d] %s: %s", int(e.Errno), e.Strerror, pyrepr.Repr(e.Filename))
}

func (e *MailboxUnusable) Unwrap() error { return fs.ErrPermission }

// Mailbox refusal logged once per dir with its owner and the repair remedy
func mailboxUnusable(root string, cause error) *MailboxUnusable {
	remedy := home.UnusableDirRemedy(root)
	deliveryWarn(warnedUnusableMailbox, root, fmt.Sprintf("bot_live_delivery: mailbox directory %s (owner %s) is not usable by this process (%s); "+
		"Bot Chat deliveries are paused until it is repaired, other clients are unaffected. To repair, %s",
		root, dirOwner(root), deliveryOSErrorText(cause), remedy))
	var errno syscall.Errno
	errors.As(cause, &errno)
	return &MailboxUnusable{Errno: errno, Strerror: "bot mailbox directory is not usable: " + deliveryOSErrorText(cause), Filename: root}
}

// Python str of an OSError with errno and filename, the plain text otherwise
func deliveryOSErrorText(err error) string {
	var pathErr *fs.PathError
	var errno syscall.Errno
	if errors.As(err, &pathErr) && errors.As(err, &errno) {
		return fmt.Sprintf("[Errno %d] %s: %s", int(errno), deliveryStrerror(errno), pyrepr.Repr(pathErr.Path))
	}
	return err.Error()
}

// Capitalised strerror text as Python prints it
func deliveryStrerror(errno syscall.Errno) string {
	text := errno.Error()
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

func deliveryPermission(err error) bool { return errors.Is(err, fs.ErrPermission) }

// Creates and hardens the mailbox dir and its lock file, answering the lock path
func prepareMailbox(ctx context.Context, root string) (string, error) {
	st, statErr := os.Stat(root)
	created := statErr != nil || !st.IsDir()
	if _, err := home.MakeUserDir(ctx, filepath.Dir(root), 0o700, true, true); err != nil {
		return "", err
	}
	if _, err := home.MakeUserDir(ctx, root, 0o700, false, true); err != nil {
		return "", err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		if chmodRefusesMailbox(root) {
			return "", err
		}
		deliveryWarn(warnedUnhardened, root, fmt.Sprintf("bot_live_delivery: could not harden mailbox directory %s (%s)", root, deliveryOSErrorText(err)))
	}
	if created {
		pyfs.FsyncDirectory(filepath.Dir(root))
		pyfs.FsyncDirectory(filepath.Dir(filepath.Dir(root)))
	}
	lock := filepath.Join(root, ".lock")
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	f.Close()
	return lock, nil
}

// Runs body holding the mailbox dir lock, an unusable dir refused as MailboxUnusable
func lockedMailbox(ctx context.Context, profileHome string, body func(root string) error) error {
	root := deliveryRoot(profileHome)
	lock, err := prepareMailbox(ctx, root)
	var release func()
	if err == nil {
		release, err = acquireFileLock(lock)
	}
	if err != nil {
		if deliveryPermission(err) {
			return mailboxUnusable(root, err)
		}
		return err
	}
	defer release()
	deliveryForget(warnedUnusableMailbox, root)
	return body(root)
}

// Exact id ticket read, nil when absent and an error when unreadable or not a JSON object
func readTicket(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	decoded, err := pyjson.LoadsBytes(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	if err != nil {
		return nil, err
	}
	obj, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("ticket %s is not a JSON object (%s)", filepath.Base(path), deliveryTypeName(decoded))
	}
	return pyjson.Plain(obj).(map[string]any), nil
}

// Python type name of a decoded JSON value
func deliveryTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int64, *big.Int, int:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	}
	return "dict"
}

func deliveryIsInt(v any) bool {
	switch v.(type) {
	case int64, int, *big.Int:
		return true
	}
	return false
}

// Why a parsed ticket is unusable by the scans, empty when it is well formed
func ticketShapeError(path string, record map[string]any) string {
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	createdAt := record["created_at"]
	sequence, has := record["sequence"]
	if !has {
		sequence = createdAt
	}
	if record["delivery_id"] != stem || record["id"] != stem {
		return "id does not match filename"
	}
	status, ok := record["status"].(string)
	if !ok || !(status == "queued" || status == "claimed" || deliveryTerminal[status]) {
		return "unknown status " + pyrepr.Repr(record["status"])
	}
	if !deliveryIsInt(createdAt) || !deliveryIsInt(sequence) {
		return "created_at/sequence are not integers"
	}
	owner, ok := record["owner"].(map[string]any)
	if !ok {
		return "owner pin is incomplete"
	}
	for _, key := range deliveryOwnerKeys {
		if v, ok := owner[key].(string); !ok || v == "" {
			return "owner pin is incomplete"
		}
	}
	return ""
}

// Bulk scan read where one unreadable or malformed ticket is skipped with a warning
func scanReadTicket(path string) map[string]any {
	record, err := readTicket(path)
	problem := ""
	if err != nil {
		record, problem = nil, err.Error()
	} else if record != nil {
		problem = ticketShapeError(path, record)
	}
	if problem != "" {
		deliveryWarn(warnedUnreadableTicket, path, fmt.Sprintf("bot_live_delivery: skipping unreadable ticket %s (%s)", filepath.Base(path), problem))
		return nil
	}
	deliveryForget(warnedUnreadableTicket, path)
	return record
}

func deliveryInt(v any) *big.Int {
	switch x := v.(type) {
	case int64:
		return big.NewInt(x)
	case int:
		return big.NewInt(int64(x))
	case *big.Int:
		return x
	}
	return big.NewInt(0)
}

// Next admission sequence under the dir lock, persisted in a counter file beside the tickets
func nextDeliverySequence(ctx context.Context, root string) (*big.Int, error) {
	counter := filepath.Join(root, deliverySequenceFile)
	persisted := big.NewInt(0)
	if data, err := os.ReadFile(counter); err == nil {
		if n, ok := new(big.Int).SetString(strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff")), 10); ok {
			persisted = n
		}
	}
	scanned := big.NewInt(0)
	candidates, _ := filepath.Glob(filepath.Join(root, "*.json"))
	for _, candidate := range candidates {
		record := scanReadTicket(candidate)
		if record == nil {
			continue
		}
		seq, has := record["sequence"]
		if !has {
			seq = record["created_at"]
		}
		if v := deliveryInt(seq); v.Cmp(scanned) > 0 {
			scanned = v
		}
	}
	sequence := persisted
	if scanned.Cmp(sequence) > 0 {
		sequence = scanned
	}
	sequence = new(big.Int).Add(sequence, big.NewInt(1))
	if err := pyfs.AtomicWriteTextMode(ctx, counter, sequence.String(), ".tmp_", 0o600); err != nil {
		return nil, err
	}
	return sequence, nil
}

func writeTicket(ctx context.Context, path string, record map[string]any) error {
	return config.AtomicJSONWriteMode(ctx, path, record, 0o600, pyjson.SortKeys(true))
}

// Durable admission answered at once without waiting for the owner
func DeliverToLiveOwner(ctx context.Context, profileHome string, owner map[string]any, message, deliveryID, notificationCategory string) (map[string]any, error) {
	return DeliverToLiveOwnerAuthored(ctx, profileHome, owner, message, deliveryID, notificationCategory, nil)
}

// Durable admission carrying the author of the delivered message
func DeliverToLiveOwnerAuthored(ctx context.Context, profileHome string, owner map[string]any, message, deliveryID, notificationCategory string, author map[string]any) (map[string]any, error) {
	return deliveryDoor(ctx, AuthorityDelivery, profileHome, owner, message, deliveryID, author, notificationCategory)
}

// Finalizes an unadmitted refusal under the same identity, or answers the receipt that admitted it meanwhile
func SettleHeldDelivery(ctx context.Context, profileHome string, owner map[string]any, message, deliveryID string, author map[string]any) (map[string]any, error) {
	return deliveryDoor(ctx, AuthoritySettle, profileHome, owner, message, deliveryID, author, "result")
}

// Builds the door envelope for one authority call and echoes a diagnostic category on the receipt
func deliveryDoor(ctx context.Context, call func(context.Context, string, map[string]any) (map[string]any, error), profileHome string,
	owner map[string]any, message, deliveryID string, author map[string]any, notificationCategory string) (map[string]any, error) {
	pinned, err := liveOwnerPin(profileHome, owner)
	if err != nil {
		return nil, err
	}
	if notificationCategory == "" {
		notificationCategory = "result"
	}
	if notificationCategory != "result" && notificationCategory != "diagnostic" {
		return nil, errors.New("invalid notification category")
	}
	h := deliveryResolveHome(profileHome)
	if deliveryID == "" {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		raw[6] = raw[6]&0x0f | 0x40
		raw[8] = raw[8]&0x3f | 0x80
		deliveryID = hex.EncodeToString(raw)
	}
	id, err := checkDeliveryID(deliveryID)
	if err != nil {
		return nil, err
	}
	profile := "default"
	if filepath.Base(filepath.Dir(h)) == "profiles" {
		profile = filepath.Base(h)
	}
	params := map[string]any{"id": id, "profile": profile, "message": message}
	if pinned["session_id"] != "" {
		params["session_id"] = pinned["session_id"]
	}
	if len(author) > 0 {
		copied := map[string]any{}
		for k, v := range author {
			copied[k] = v
		}
		params["author"] = copied
	}
	if notificationCategory == "diagnostic" {
		params["notification_category"] = "diagnostic"
	}
	answer, err := call(ctx, h, params)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for k, v := range answer {
		out[k] = v
	}
	if notificationCategory == "diagnostic" {
		out["notification_category"] = "diagnostic"
	}
	return out, nil
}

// Whether the profile authority still holds a record, failing while the authority is unreachable
func OwnerHoldsDelivery(ctx context.Context, profileHome string, record map[string]any) (bool, error) {
	owner, err := FindCanonicalLiveOwner(ctx, profileHome)
	if err != nil {
		return false, err
	}
	return owner != nil || deliveryTruthy(record["admission_id"]), nil
}

// Python truth value of a decoded JSON value
func deliveryTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// Retired UI poller claim, only the session bot may migrate and admit queued records
func ClaimPendingDelivery(profileHome string, owner map[string]any) map[string]any { return nil }

// Persists an immutable terminal receipt where an identical duplicate completion is safe
func CompleteDelivery(ctx context.Context, profileHome, deliveryID, status, reply, errText, reason string) (map[string]any, error) {
	key, err := checkDeliveryID(deliveryID)
	if err != nil {
		return nil, err
	}
	if !deliveryTerminal[status] {
		return nil, errors.New("invalid terminal delivery status")
	}
	outcome := map[string]any{"status": status, "reply": reply, "error": errText, "reason": reason}
	var out map[string]any
	err = lockedMailbox(ctx, profileHome, func(root string) error {
		path := filepath.Join(root, key+".json")
		record, err := readTicket(path)
		if err != nil {
			return err
		}
		if record == nil {
			return fmt.Errorf("delivery not found: %s: %w", key, fs.ErrNotExist)
		}
		current, has := record["status"]
		if !has {
			return fmt.Errorf("KeyError: 'status'")
		}
		if s, _ := current.(string); deliveryTerminal[s] {
			for k, v := range outcome {
				if !pyjson.Equal(record[k], v) {
					return errors.New("delivery already has a different terminal receipt")
				}
			}
			out = record
			return nil
		}
		if current != "claimed" {
			return errors.New("delivery must be claimed before completion")
		}
		for k, v := range outcome {
			record[k] = v
		}
		record["completed_at"] = time.Now().UnixNano()
		if err := writeTicket(ctx, path, record); err != nil {
			return err
		}
		out = record
		return nil
	})
	return out, err
}

// Cancels an ownerless queued receipt, a claim or result that won the lock answered unchanged
func CancelQueuedDelivery(ctx context.Context, profileHome, deliveryID, errText, reason string) (map[string]any, error) {
	key, err := checkDeliveryID(deliveryID)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = lockedMailbox(ctx, profileHome, func(root string) error {
		path := filepath.Join(root, key+".json")
		record, err := readTicket(path)
		if err != nil {
			return err
		}
		if record == nil {
			return nil
		}
		current, has := record["status"]
		if !has {
			return fmt.Errorf("KeyError: 'status'")
		}
		if current != "queued" || deliveryTruthy(record["admission_id"]) {
			out = record
			return nil
		}
		record["status"], record["reply"], record["error"], record["reason"] = "cancelled", "", errText, reason
		record["completed_at"] = time.Now().UnixNano()
		if err := writeTicket(ctx, path, record); err != nil {
			return err
		}
		out = record
		return nil
	})
	return out, err
}

// Admission, claim or terminal state of a delivery without waiting or deleting its receipt
func ReadDeliveryResult(ctx context.Context, profileHome, deliveryID string) (map[string]any, error) {
	key, err := checkDeliveryID(deliveryID)
	if err != nil {
		return nil, err
	}
	if !HasMailbox(profileHome) {
		return nil, nil
	}
	var record map[string]any
	err = lockedMailbox(ctx, profileHome, func(root string) error {
		var err error
		record, err = readTicket(filepath.Join(root, key+".json"))
		return err
	})
	if err != nil {
		return nil, err
	}
	if record != nil && deliveryTruthy(record["admission_id"]) && record["status"] == "canonical" {
		return deliveryWithStatus(record, "queued"), nil
	}
	return record, nil
}

// Copy of a record with its status replaced
func deliveryWithStatus(record map[string]any, status string) map[string]any {
	out := make(map[string]any, len(record))
	for k, v := range record {
		out[k] = v
	}
	out["status"] = status
	return out
}

// Waiter read of the receipt file, a transitional canonical status still pending
func pollReceipt(profileHome, deliveryID string) (map[string]any, error) {
	key, err := checkDeliveryID(deliveryID)
	if err != nil {
		return nil, err
	}
	record, err := readTicket(filepath.Join(deliveryRoot(profileHome), key+".json"))
	if err != nil {
		return nil, err
	}
	if record != nil && record["status"] == "canonical" {
		return deliveryWithStatus(record, "queued"), nil
	}
	return record, nil
}

// Polls a receipt until the owner settles it, the timeout lapses, shouldStop says so or the context ends
func AwaitDelivery(ctx context.Context, profileHome, deliveryID string, timeout *time.Duration, shouldStop func() bool) (map[string]any, error) {
	var deadline time.Time
	if timeout != nil {
		deadline = time.Now().Add(*timeout)
	}
	for {
		record, err := pollReceipt(profileHome, deliveryID)
		if err != nil {
			return nil, err
		}
		if record == nil {
			return nil, nil
		}
		status, has := record["status"]
		if !has {
			return nil, fmt.Errorf("KeyError: 'status'")
		}
		if status != "queued" && status != "claimed" {
			return record, nil
		}
		if shouldStop != nil && shouldStop() {
			return record, nil
		}
		wait := deliveryPollInterval
		if timeout != nil {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return record, nil
			}
			wait = min(wait, remaining)
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return record, ctx.Err()
		}
	}
}
