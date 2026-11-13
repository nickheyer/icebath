package dmdelivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/botmode"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Live owner wait windows, one await budget and the ceiling past which a runner reports pending and exits
const (
	LiveWaitSeconds    = 300
	LiveWaitMaxSeconds = 30 * 60
)

// Hermes key order of a canonical live owner pin as the intent file stores it
var liveOwnerKeyOrder = []string{"profile_home", "session_id", "canonical", "lease_id", "live_session_id"}

// Definite no send refusal of a live target with its typed reason and when the sender may resend
type LiveTargetError interface {
	error
	Reason() string
	Resend() string
}

// Canonical Bot Chat owner unreachable with nothing admitted, a definite no send the sender may retry
type LiveTargetUnavailableError struct{ Msg string }

func (e *LiveTargetUnavailableError) Error() string  { return e.Msg }
func (e *LiveTargetUnavailableError) Reason() string { return "runtime_unavailable" }
func (e *LiveTargetUnavailableError) Resend() string { return "once the target is up" }

// Owner refusal under another session writer that settled this delivery id as never sent
type LiveTargetHeldError struct{ LiveTargetUnavailableError }

func (e *LiveTargetHeldError) Reason() string { return tools.HeldNotSent }
func (e *LiveTargetHeldError) Resend() string { return "after that session closes" }

// Python exception of a named class whose str is its message
type pyError struct{ class, msg string }

func (e *pyError) Error() string      { return e.msg }
func (e *pyError) PyTypeName() string { return e.class }

// Python str of an exception, an OS error spelled with its errno and quoted path
func excText(err error) string {
	switch err.(type) {
	case *fs.PathError, *os.LinkError:
		return pyfs.OSErrorText(err)
	}
	return pyrepr.ExceptionMessage(err)
}

// Text a delivery answer quotes for a failure, an OS error with its errno and quoted path
func ExcText(err error) string { return excText(err) }

// Sweeps stale payloads and stores a message in a fresh private file, never inline shell text
func WriteDMFile(content string) (string, error) {
	botmode.CleanupBotDMCache(botmode.DMStaleHours, nil)
	dir, err := botmode.DMDir()
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "dm-*.txt")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if runtime.GOOS == "windows" {
		content = strings.ReplaceAll(content, "\n", "\r\n")
	}
	_, err = f.WriteString(content)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		UnlinkDMFile(path)
		return "", err
	}
	return path, nil
}

// Pinned live delivery intent beside a DM file, the name the cache sweep globs
func LiveIntentFile(dmFile string) string {
	return dmFile + ".live.json"
}

// One delivery id per DM file, the sha256 of its resolved path shared by ack, intent and every retry
func DMDeliveryID(dmFile string) (string, error) {
	resolved, err := pypath.Resolve(dmFile)
	if err != nil {
		return "", err
	}
	if err := pystr.StrictUTF8EncodeError(pystr.SurrogateEscapeDecode([]byte(resolved))); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(resolved))
	return hex.EncodeToString(sum[:]), nil
}

// Removes a DM file or intent, every OS error swallowed
func UnlinkDMFile(path string) {
	_ = pypath.Unlink(path)
}

// Runner and tool answer to a definite no send, reason and delivery id with the safe to resend text
func UnavailablePayload(dmFile string, exc LiveTargetError) (string, error) {
	id, err := DMDeliveryID(dmFile)
	if err != nil {
		return "", err
	}
	return pyjson.Dumps(pyjson.ObjectOf("reason", exc.Reason(), "delivery_id", id,
		"error", exc.Error()+" — the message was NOT sent; it is safe to resend "+exc.Resend()+"."))
}

// Drops the intent and payload of a definite no send, the unknown outcome when either file stays
func SettledNoSend(dmFile string, exc LiveTargetError) (string, error) {
	var left []string
	for _, path := range []string{LiveIntentFile(dmFile), dmFile} {
		UnlinkDMFile(path)
		if _, err := os.Lstat(path); err == nil {
			left = append(left, path)
		}
	}
	if len(left) > 0 {
		return LiveOutcomeUnknown(dmFile, "could not remove "+left[0])
	}
	return UnavailablePayload(dmFile, exc)
}

// Runner answer when a live admission may have happened but its outcome is unknown
func LiveOutcomeUnknown(dmFile, cause string) (string, error) {
	id, err := DMDeliveryID(dmFile)
	if err != nil {
		return "", err
	}
	return pyjson.Dumps(pyjson.ObjectOf("status", "ambiguous", "delivery_id", id,
		"error", "Live admission outcome unknown: "+cause+". Do not resend.", "evidence_file", dmFile))
}

// Owner's terminal not sent receipt for a delivery refused while another writer held the chat
func heldDisposition(record map[string]any) bool {
	refused, _ := record["refused"].(bool)
	reason, _ := record["reason"].(string)
	return len(record) > 0 && refused && reason == tools.HeldNotSent
}

// Text of a utf-8-sig file read in text mode, BOM dropped and newlines translated
func readText(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text, err := pystr.DecodeUTF8SigFile(raw)
	if err != nil {
		return "", err
	}
	return pystr.TranslateNewlines(text), nil
}

// Decoded JSON of a utf-8-sig file
func readJSON(path string) (any, error) {
	text, err := readText(path)
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(text)
}

// Value of a str subscript, the KeyError or TypeError Python raises for a missing key or non dict
func subscript(v any, key string) (any, error) {
	if d, ok := pyjson.Dict(v); ok {
		if val, has := d[key]; has {
			return val, nil
		}
		return nil, &pyError{"KeyError", pyrepr.StrRepr(key)}
	}
	switch v.(type) {
	case string:
		return nil, &pyError{"TypeError", "string indices must be integers, not 'str'"}
	case []any:
		return nil, &pyError{"TypeError", "list indices must be integers or slices, not str"}
	}
	return nil, &pyError{"TypeError", fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(v))}
}

// Path argument of a decoded value, the TypeError pathlib raises for a non str
func pathArg(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	return "", &pyError{"TypeError", fmt.Sprintf("argument should be a str or an os.PathLike object where __fspath__ returns a str, not '%s'", pyrepr.TypeName(v))}
}

// Owner pin as an ordered object in hermes key order, any further key after them sorted
func ownerObject(owner map[string]any) *pyjson.Object {
	out := pyjson.NewObject()
	for _, k := range liveOwnerKeyOrder {
		if v, ok := owner[k]; ok {
			out.Set(k, v)
		}
	}
	sorted, _ := pyjson.AsObject(owner)
	for _, k := range sorted.Keys {
		if _, ok := out.Get(k); !ok {
			out.Set(k, owner[k])
		}
	}
	return out
}

// Python dict of a decoded value, a copy of a dict or the pairs of a sequence with the errors dict raises
func pyDict(v any) (map[string]any, error) {
	if d, ok := pyjson.Dict(v); ok {
		out := make(map[string]any, len(d))
		for k, val := range d {
			out[k] = val
		}
		return out, nil
	}
	var items []any
	switch t := v.(type) {
	case string:
		for _, r := range pystr.Runes(t) {
			items = append(items, pystr.FromRunes([]rune{r}))
		}
	case []any:
		items = t
	default:
		return nil, &pyError{"TypeError", fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(v))}
	}
	out := map[string]any{}
	for i, item := range items {
		var pair []any
		switch t := item.(type) {
		case []any:
			pair = t
		case string:
			for _, r := range pystr.Runes(t) {
				pair = append(pair, pystr.FromRunes([]rune{r}))
			}
		default:
			if d, ok := pyjson.AsObject(item); ok {
				for _, k := range d.Keys {
					pair = append(pair, k)
				}
				break
			}
			return nil, &pyError{"TypeError", fmt.Sprintf("cannot convert dictionary update sequence element #%d to a sequence", i)}
		}
		if len(pair) != 2 {
			return nil, &pyError{"ValueError", fmt.Sprintf("dictionary update sequence element #%d has length %d; 2 is required", i, len(pair))}
		}
		key, err := jsonKey(pair[0])
		if err != nil {
			return nil, err
		}
		out[key] = pair[1]
	}
	return out, nil
}

// JSON object key json.dumps writes for a dict key, the TypeError for an unhashable one
func jsonKey(k any) (string, error) {
	switch t := k.(type) {
	case string:
		return t, nil
	case nil:
		return "null", nil
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case []any:
		return "", &pyError{"TypeError", "unhashable type: 'list'"}
	}
	if _, ok := pyjson.Dict(k); ok {
		return "", &pyError{"TypeError", "unhashable type: 'dict'"}
	}
	return pyjson.Dumps(k)
}

// Pins the intent beside a DM file before admission, a retry inspecting the pinned owner and message only
func AdmitLiveDM(ctx context.Context, profileHome, dmFile string, author *pyjson.Object) (map[string]any, error) {
	intentPath := LiveIntentFile(dmFile)
	pinnedNow := false
	var intent any
	info, err := pypath.PathlibStat(intentPath)
	if err != nil {
		return nil, err
	}
	if info != nil {
		if intent, err = readJSON(intentPath); err != nil {
			return nil, err
		}
	} else {
		if profileHome == "" {
			return nil, &pyError{"AssertionError", ""}
		}
		owner, err := gateway.FindCanonicalLiveOwner(ctx, profileHome)
		if err != nil {
			return nil, &LiveTargetUnavailableError{Msg: fmt.Sprintf("canonical Bot Chat target is unavailable (%s)", excText(err))}
		}
		if owner == nil {
			return nil, &LiveTargetUnavailableError{Msg: "canonical Bot Chat target is unavailable"}
		}
		message, err := readText(dmFile)
		if err != nil {
			return nil, err
		}
		id, err := DMDeliveryID(dmFile)
		if err != nil {
			return nil, err
		}
		fresh := pyjson.ObjectOf("owner", ownerObject(owner), "message", message, "delivery_id", id)
		if author != nil && author.Len() > 0 {
			fresh.Set("author", author)
		}
		f, err := os.OpenFile(intentPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		switch {
		case errors.Is(err, fs.ErrExist):
			if intent, err = readJSON(intentPath); err != nil {
				return nil, err
			}
		case err != nil:
			return nil, err
		default:
			if err := writeIntent(f, fresh); err != nil {
				return nil, err
			}
			pyfs.FsyncDirectory(pypath.PathParent(intentPath))
			intent, pinnedNow = fresh, true
		}
	}
	return admitPinned(ctx, dmFile, intent, pinnedNow)
}

// Writes the intent JSON, syncs it to disk and closes the descriptor
func writeIntent(f *os.File, intent *pyjson.Object) error {
	text, err := pyjson.Dumps(intent)
	if err == nil {
		_, err = io.WriteString(f, text)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Reads the pinned receipt and delivers or settles under the intent's identity, a held refusal raised as no send
func admitPinned(ctx context.Context, dmFile string, intent any, pinnedNow bool) (map[string]any, error) {
	ownerVal, err := subscript(intent, "owner")
	if err != nil {
		return nil, err
	}
	homeVal, err := subscript(ownerVal, "profile_home")
	if err != nil {
		return nil, err
	}
	idVal, err := subscript(intent, "delivery_id")
	if err != nil {
		return nil, err
	}
	id, _ := idVal.(string)
	home, err := pathArg(homeVal)
	if err != nil {
		if idErr := gateway.CheckDeliveryID(id); idErr != nil {
			return nil, idErr
		}
		return nil, err
	}
	record, err := gateway.ReadDeliveryResult(ctx, home, id)
	if err != nil {
		return nil, err
	}
	owner, _ := pyjson.Dict(ownerVal)
	if record == nil || pyrepr.Truthy(owner["canonical"]) {
		intentDict, _ := pyjson.Dict(intent)
		authorVal := intentDict["author"]
		messageVal, err := subscript(intent, "message")
		if err != nil {
			return nil, err
		}
		message, isText := messageVal.(string)
		if !isText {
			if err := gateway.CheckLiveOwner(home, owner); err != nil {
				return nil, err
			}
			return nil, &pyError{"ValueError", "message must be a string"}
		}
		var author map[string]any
		if pyrepr.Truthy(authorVal) {
			if err := gateway.CheckLiveOwner(home, owner); err != nil {
				return nil, err
			}
			if err := gateway.CheckDeliveryID(id); err != nil {
				return nil, err
			}
			if author, err = pyDict(authorVal); err != nil {
				return nil, err
			}
		}
		record, err = gateway.DeliverToLiveOwnerAuthored(ctx, home, owner, message, id, "result", author)
		var rpcErr *gateway.GatewayRPCError
		var preDial *gateway.PreDialError
		switch {
		case err == nil:
		case errors.As(err, &rpcErr):
			if admitted, ok := rpcErr.Data["admitted"].(bool); rpcErr.Error() != tools.HeldNotSent || !ok || admitted {
				return nil, err
			}
			if record, err = gateway.SettleHeldDelivery(ctx, home, owner, message, id, author); err != nil {
				return nil, err
			}
		case errors.As(err, &preDial):
			if !pinnedNow {
				return nil, err
			}
			UnlinkDMFile(LiveIntentFile(dmFile))
			return nil, &LiveTargetUnavailableError{Msg: fmt.Sprintf("live authority dial failed before admission (%s)", excText(preDial))}
		default:
			return nil, err
		}
	}
	if heldDisposition(record) {
		return nil, &LiveTargetHeldError{LiveTargetUnavailableError{Msg: "Bot Chat is held by another session writer"}}
	}
	return record, nil
}

// Status of a receipt record, the KeyError a record without one raises
func recordStatus(record map[string]any) (any, error) {
	return subscript(record, "status")
}

// Whether a decoded status is one of the given names
func statusIn(status any, names ...string) bool {
	s, ok := status.(string)
	if !ok {
		return false
	}
	for _, name := range names {
		if s == name {
			return true
		}
	}
	return false
}

// Bounded wait on a live admission printing its outcome, exit 0 for settled or still pending and 1 otherwise
func WaitLiveDM(ctx context.Context, stdout io.Writer, home, deliveryID, dmFile string) (int, error) {
	deadline := time.Now().Add(LiveWaitMaxSeconds * time.Second)
	missed := 0
	var record map[string]any
	for {
		budget := max(0, min(LiveWaitSeconds*time.Second, time.Until(deadline)))
		var err error
		if record, err = gateway.AwaitDelivery(ctx, home, deliveryID, &budget, nil); err != nil {
			return 0, err
		}
		if record == nil {
			break
		}
		status, err := recordStatus(record)
		if err != nil {
			return 0, err
		}
		if !statusIn(status, "queued", "claimed") || !time.Now().Before(deadline) {
			break
		}
		held, err := gateway.OwnerHoldsDelivery(ctx, home, record)
		known := err == nil
		if !known {
			slog.Debug("Could not check live DM owner", "error", err)
		}
		if known && held {
			missed = 0
			continue
		}
		if known && statusIn(status, "queued") {
			if record, err = gateway.CancelQueuedDelivery(ctx, home, deliveryID, "Live Bot Chat owner is no longer available", tools.ReasonRuntimeOffline); err != nil {
				return 0, err
			}
			if record == nil {
				break
			}
			if status, err = recordStatus(record); err != nil {
				return 0, err
			}
			if !statusIn(status, "claimed") {
				break
			}
			missed = 0
			continue
		}
		missed++
		if missed > 1 {
			break
		}
	}
	var status any = "ambiguous"
	payload := pyjson.NewObject()
	if len(record) > 0 {
		var err error
		if status, err = recordStatus(record); err != nil {
			return 0, err
		}
		for _, key := range []string{"reply", "error", "reason"} {
			if v := record[key]; pyrepr.Truthy(v) {
				payload.Set(key, v)
			}
		}
	}
	payload.Set("status", status)
	payload.Set("delivery_id", deliveryID)
	if statusIn(status, "queued", "claimed", "ambiguous") {
		payload.Set("detail", "Delivery remains pending or its outcome is unknown. Do not resend; receipt is retained.")
	} else if statusIn(status, "settled") && dmFile != "" {
		UnlinkDMFile(LiveIntentFile(dmFile))
		UnlinkDMFile(dmFile)
	}
	text, err := pyjson.Dumps(payload)
	if err != nil {
		return 0, err
	}
	fmt.Fprintln(stdout, text)
	if statusIn(status, "settled", "queued", "claimed") {
		return 0, nil
	}
	return 1, nil
}
