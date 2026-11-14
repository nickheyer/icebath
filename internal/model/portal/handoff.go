package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	HandoffIdle     = "idle"
	HandoffPending  = "pending"
	HandoffRetrying = "retrying"
	HandoffClaimed  = "claimed"
	HandoffGone     = "gone"
	HandoffFailed   = "failed"

	claimTimeout       = 10 * time.Second
	handoffLease       = claimTimeout + 20*time.Second
	handoffLockTimeout = 15 * time.Second
	forceRefreshSkew   = 1e9 * time.Second
	workerFirstTick    = 15 * time.Second
	workerInterval     = 60 * time.Second

	outcomeClaimed = "claimed"
	outcomeGone    = "gone"
	outcomeRetry   = "retry"
	outcomeFail    = "fail"
)

var handoffRetryDelays = []time.Duration{60 * time.Second, 600 * time.Second, 3600 * time.Second, 6 * 3600 * time.Second}

func handoffFailureCopy(code string) string {
	switch code {
	case "client_not_allowed":
		return "This sign-in can't receive them. Sign in again with `icebath auth add portal`."
	case "account_locked":
		return "This Icebath Portal account is locked."
	}
	return ""
}

func handoffRefusal(code string) string {
	switch code {
	case "unknown_token", "already_retired":
		return outcomeGone
	case "client_not_allowed", "account_locked", "invalid_request":
		return outcomeFail
	}
	return outcomeRetry
}

func handoffActive(state any) bool {
	return state == HandoffPending || state == HandoffRetrying
}

func tokenKey(anonToken string) string {
	sum := sha256.Sum256([]byte(anonToken))
	return hex.EncodeToString(sum[:])[:32]
}

func (a *Account) handoffPath() string {
	return filepath.Join(a.sharedDir(), filepath.Base(string(home.GuestHandoff)))
}

func loadHandoff(path string) (*object, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return pairs("_schema", 1, "records", newObject()), nil
	}
	if err != nil {
		return nil, err
	}
	doc, ok := parseObject(data)
	if !ok || doc.obj("records") == nil {
		return nil, fmt.Errorf("portal: %s does not hold a handoff record table", filepath.Base(path))
	}
	return doc, nil
}

func (a *Account) handoffRecords() (*object, error) {
	doc, err := loadHandoff(a.handoffPath())
	if err != nil {
		return nil, err
	}
	return doc.obj("records"), nil
}

func (a *Account) updateHandoff(ctx context.Context, key string, change func(record *object) *object) (*object, error) {
	path := a.handoffPath()
	lctx, unlock, err := openFileLock(ctx, lockPathFor(path), handoffLockTimeout, "Timed out waiting for "+filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer unlock()
	doc, err := loadHandoff(path)
	if err != nil {
		return nil, err
	}
	record := change(doc.obj("records").obj(key))
	if record == nil {
		return nil, nil
	}
	record.set("updated_at", isoUTC(time.Now()))
	doc.obj("records").set(key, record)
	if err := a.savePrivateJSON(lctx, path, doc.plain()); err != nil {
		return nil, err
	}
	return record, nil
}

func listHas(items []any, value string) bool {
	return slices.ContainsFunc(items, func(item any) bool { return item == value })
}

func (a *Account) noteStoreWrite(ctx context.Context, previous, next json.RawMessage) error {
	prior, _ := parseObject(previous)
	state, ok := parseObject(next)
	if !ok {
		return nil
	}
	return a.noteReplacement(ctx, prior, state)
}

func (a *Account) noteReplacement(ctx context.Context, previous, next *object) error {
	if !isGuestState(previous) || isGuestState(next) || next == nil {
		return nil
	}
	anonToken := previous.str("anon_token")
	if anonToken == "" {
		return nil
	}
	homeKey := a.homeKey()
	record, err := a.updateHandoff(ctx, tokenKey(anonToken), func(record *object) *object {
		if record == nil {
			record = pairs("state", HandoffPending, "anon_token", anonToken, "attempts", 0, "profiles", []any{},
				"created_at", isoUTC(time.Now()))
		}
		if !listHas(record.list("profiles"), homeKey) {
			record.set("profiles", append(slices.Clone(record.list("profiles")), homeKey))
		}
		return record
	})
	if err != nil {
		return err
	}
	slog.Info(fmt.Sprintf("Icebath Portal sign-in replaced the free tier; handoff %s (%s)", record.str("state"), tokenKey(anonToken)[:8]))
	return nil
}

func handoffDue(record *object, now float64) bool {
	next, _ := numeric(record.get("next_attempt_at"))
	return handoffActive(record.get("state")) && next <= now
}

func sortedKeys(o *object) []string {
	keys := slices.Clone(o.keys)
	sort.Strings(keys)
	return keys
}

func (a *Account) Drain(ctx context.Context) ([]HandoffStatus, error) {
	ctx = a.bind(ctx)
	records, err := a.handoffRecords()
	if err != nil {
		return nil, err
	}
	homeKey, now := a.homeKey(), epochOf(time.Now())
	views := []HandoffStatus{}
	for _, key := range sortedKeys(records) {
		record := records.obj(key)
		if record == nil || !listHas(record.list("profiles"), homeKey) || !handoffDue(record, now) {
			continue
		}
		view, ok, err := a.runHandoff(ctx, key)
		if err != nil {
			return nil, err
		}
		if ok {
			views = append(views, view)
		}
	}
	return views, nil
}

func (a *Account) handoffBearer(ctx context.Context, force bool) (string, error) {
	a.dropTokenMemo()
	o := AccessTokenOptions{Timeout: claimTimeout}
	if force {
		o.RefreshSkew = forceRefreshSkew
	}
	token, err := a.AccessToken(ctx, o)
	return token.Reveal(), err
}

func (a *Account) runHandoff(ctx context.Context, key string) (HandoffStatus, bool, error) {
	bearer, err := a.handoffBearer(ctx, false)
	if err != nil {
		var busy *fileutil.LockTimeoutError
		ae, isAuth := authErrorOf(err)
		if !isAuth && !errors.As(err, &busy) {
			return HandoffStatus{}, false, err
		}
		cause := fmt.Sprintf("%T", err)
		if isAuth && ae.Code != "" {
			cause = ae.Code
		}
		slog.Info(fmt.Sprintf("Free-tier handoff (%s): account token unavailable (%s)", key[:min(8, len(key))], cause))
		return HandoffStatus{}, false, nil
	}
	record, err := a.updateHandoff(ctx, key, reserveHandoff)
	if err != nil || record == nil {
		return HandoffStatus{}, false, err
	}
	attempts, _ := numeric(record.get("attempts"))
	prior := int(attempts) - 1
	account, err := a.currentState(ctx)
	if err != nil {
		return HandoffStatus{}, false, err
	}
	outcome, payload, retryAfter, err := a.postClaim(ctx, record.str("anon_token"), bearer, account)
	if err != nil {
		return HandoffStatus{}, false, err
	}
	label := outcome
	if v := payload.get("error"); truthy(v) {
		label = textual(v)
	}
	slog.Info(fmt.Sprintf("Free-tier handoff attempt %d (%s): %s", prior+1, key[:min(8, len(key))], label))
	record, err = a.updateHandoff(ctx, key, func(r *object) *object {
		if r == nil {
			return nil
		}
		return applyHandoff(r, outcome, payload, retryAfter, prior)
	})
	if err != nil {
		return HandoffStatus{}, false, err
	}
	if outcome == outcomeClaimed && payload.get("cohort_carried") == true {
		if _, err := a.handoffBearer(ctx, true); err != nil {
			if _, isAuth := authErrorOf(err); !isAuth {
				return HandoffStatus{}, false, err
			}
			slog.Info("Free-tier handoff: account token refresh after the move failed", "error", err)
		}
	}
	if record == nil {
		return HandoffStatus{}, false, nil
	}
	return handoffView(record), true, nil
}

func reserveHandoff(record *object) *object {
	now := epochOf(time.Now())
	if record == nil || !handoffDue(record, now) {
		return nil
	}
	attempts, _ := numeric(record.get("attempts"))
	record.set("attempts", int(attempts)+1)
	record.set("last_attempt_at", isoFromEpoch(now))
	record.set("next_attempt_at", now+handoffLease.Seconds())
	return record
}

func (a *Account) postClaim(ctx context.Context, anonToken, bearer string, account *object) (string, *object, time.Duration, error) {
	if account == nil {
		account = newObject()
	}
	client, _, err := a.verifiedClient(ctx, nil, "", account)
	if err != nil {
		return "", nil, 0, err
	}
	headers := a.anonHeaders(ctx)
	headers["Authorization"] = "Bearer " + bearer
	r, err := postJSON(ctx, client, strings.TrimRight(a.portalBaseURL(ctx, account), "/")+"/api/anonymous/claim", headers,
		pairs("token", anonToken), claimTimeout)
	if err != nil {
		return outcomeRetry, pairs("error", "unreachable", "detail", fmt.Sprintf("%T", err)), 0, nil
	}
	payload := r.object()
	retryAfter, _ := httpclient.RetryAfter(r.header, time.Now())
	return classifyClaim(r.status, payload), payload, retryAfter, nil
}

func classifyClaim(status int, payload *object) string {
	if status == http.StatusOK {
		if payload.str("status") == "claimed" {
			return outcomeClaimed
		}
		return outcomeRetry
	}
	return handoffRefusal(textualOrEmpty(payload.get("error")))
}

func applyHandoff(record *object, outcome string, payload *object, retryAfter time.Duration, priorAttempts int) *object {
	if !handoffActive(record.get("state")) {
		return record
	}
	record.set("error", cloneValue(payload.get("error")))
	record.set("next_attempt_at", nil)
	switch {
	case outcome == outcomeClaimed:
		connectors := payload.obj("connectors")
		kind := any("unknown")
		if v := connectors.get("kind"); truthy(v) {
			kind = v
		}
		record.set("state", HandoffClaimed)
		record.set("error", nil)
		record.set("result", pairs(
			"account_email", cloneValue(payload.get("account_email")),
			"destination_org_id", cloneValue(payload.get("destination_org_id")),
			"connectors_kind", kind,
			"moved_toolkits", slices.Clone(connectors.list("moved_toolkits")),
			"dropped_toolkits", slices.Clone(connectors.list("dropped_toolkits")),
			"cohort_carried", payload.get("cohort_carried") == true,
		))
	case outcome == outcomeGone:
		record.set("state", HandoffGone)
		record.set("probably_moved", payload.get("error") == "already_retired" || priorAttempts > 0)
	case outcome == outcomeFail || priorAttempts >= len(handoffRetryDelays):
		record.set("state", HandoffFailed)
	default:
		wait := retryAfter
		if wait <= 0 {
			wait = handoffRetryDelays[max(0, priorAttempts)]
		}
		record.set("state", HandoffRetrying)
		record.set("next_attempt_at", epochOf(time.Now())+wait.Seconds())
	}
	if state := record.str("state"); state == HandoffClaimed || state == HandoffGone {
		record.del("anon_token")
	}
	return record
}

func (a *Account) startWorker() {
	a.mu.Lock()
	if a.closed || a.worker != nil {
		a.mu.Unlock()
		return
	}
	l := newLoop()
	a.worker = l
	a.mu.Unlock()
	go func() {
		defer close(l.done)
		wait := workerFirstTick
		for !l.wait(wait) {
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				select {
				case <-l.stop:
					cancel()
				case <-ctx.Done():
				}
			}()
			bound, err := a.detached(ctx)
			if err == nil {
				_, err = a.Drain(bound)
			}
			cancel()
			if err != nil {
				slog.Warn("Free-tier handoff retry tick failed; the records stay for the next one", "error", err)
			}
			wait = workerInterval
		}
	}()
}

type HandoffStatus struct {
	State           string
	AccountEmail    string
	ConnectorsKind  string
	MovedToolkits   []string
	DroppedToolkits []string
	ProbablyMoved   bool
	Error           string
	RetryAfter      int
	UpdatedAt       string
	Lines           []string

	idle bool
	wire bool
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (h HandoffStatus) MarshalJSON() ([]byte, error) {
	out := pairs("state", h.State)
	if !h.idle {
		out.set("account_email", nullable(h.AccountEmail))
		out.set("connectors_kind", nullable(h.ConnectorsKind))
		out.set("moved_toolkits", h.MovedToolkits)
		out.set("dropped_toolkits", h.DroppedToolkits)
		out.set("probably_moved", h.ProbablyMoved)
		out.set("error", nullable(h.Error))
		out.set("retry_after", h.RetryAfter)
		out.set("updated_at", nullable(h.UpdatedAt))
	}
	if h.wire {
		out.set("lines", h.Lines)
	}
	return out.MarshalJSON()
}

func stringList(items []any) []string {
	out := []string{}
	for _, item := range items {
		out = append(out, textual(item))
	}
	return out
}

func handoffView(record *object) HandoffStatus {
	result := record.obj("result")
	state := record.str("state")
	if !record.has("state") {
		state = HandoffIdle
	}
	view := HandoffStatus{
		State: state, AccountEmail: textualOrEmpty(result.get("account_email")), ConnectorsKind: textualOrEmpty(result.get("connectors_kind")),
		MovedToolkits: stringList(result.list("moved_toolkits")), DroppedToolkits: stringList(result.list("dropped_toolkits")),
		ProbablyMoved: truthy(record.get("probably_moved")), Error: textualOrEmpty(record.get("error")),
		UpdatedAt: textualOrEmpty(record.get("updated_at")),
	}
	if next, ok := numeric(record.get("next_attempt_at")); ok && next != 0 {
		view.RetryAfter = max(0, int(math.Trunc(next-epochOf(time.Now()))))
	}
	return view
}

func (a *Account) HandoffStatus(ctx context.Context) (HandoffStatus, error) {
	records, err := a.handoffRecords()
	if err != nil {
		return HandoffStatus{}, err
	}
	homeKey := a.homeKey()
	var latest *object
	for _, key := range sortedKeys(records) {
		record := records.obj(key)
		if record == nil || !listHas(record.list("profiles"), homeKey) {
			continue
		}
		if latest == nil || textualOrEmpty(record.get("updated_at")) > textualOrEmpty(latest.get("updated_at")) {
			latest = record
		}
	}
	if latest == nil {
		return HandoffStatus{State: HandoffIdle, idle: true}, nil
	}
	return handoffView(latest), nil
}

func (a *Account) HandoffRecordStatus(anonToken auth.Secret) (HandoffStatus, bool, error) {
	records, err := a.handoffRecords()
	if err != nil {
		return HandoffStatus{}, false, err
	}
	record := records.obj(tokenKey(anonToken.Reveal()))
	if record.size() == 0 {
		return HandoffStatus{}, false, nil
	}
	return handoffView(record), true, nil
}

func (a *Account) WireStatus(status HandoffStatus) HandoffStatus {
	status.Lines = a.ResultLines(status)
	status.wire = true
	return status
}

func (a *Account) StatusLines(status HandoffStatus) []string {
	lines := a.ResultLines(status)
	kind := status.ConnectorsKind
	movedSome := (kind == "moved" || kind == "swapped") && len(status.MovedToolkits) > 0
	if status.State == HandoffClaimed && !movedSome && kind != "conflict" && len(lines) > 0 {
		where := status.AccountEmail
		if where == "" {
			where = "your account"
		}
		lines[0] = fmt.Sprintf("Signed in as %s. %s", where, lines[0])
	}
	return lines
}

func (a *Account) titles(slugs []string) []string {
	out := make([]string, len(slugs))
	for i, slug := range slugs {
		out[i] = a.appTitle(slug)
	}
	return out
}

func (a *Account) ResultLines(status HandoffStatus) []string {
	where := status.AccountEmail
	if where == "" {
		where = "your account"
	}
	moved, dropped := a.titles(status.MovedToolkits), a.titles(status.DroppedToolkits)
	switch status.State {
	case HandoffPending, HandoffRetrying:
		return []string{"Moving your free-tier apps to this account; icebath keeps trying in the background."}
	case HandoffFailed:
		line := "Your free-tier apps could not be moved."
		if reason := handoffFailureCopy(status.Error); reason != "" {
			line += " " + reason
		}
		return []string{line}
	case HandoffGone:
		if status.ProbablyMoved {
			return []string{"Your free-tier apps were probably moved already."}
		}
		return []string{"Nothing left to move from the free tier."}
	case HandoffClaimed:
	default:
		return []string{}
	}
	kind := status.ConnectorsKind
	var lines []string
	switch {
	case (kind == "moved" || kind == "swapped") && len(moved) > 0:
		lines = []string{fmt.Sprintf("Your apps moved to %s: %s.", where, strings.Join(moved, ", "))}
	case kind == "conflict":
		lines = []string{where + " already had its own apps, so the free-tier apps stayed behind."}
	case kind == "none" || kind == "moved" || kind == "swapped":
		lines = []string{"There were no free-tier apps to move."}
	default:
		lines = []string{"Any free-tier apps are in this account now."}
	}
	if len(dropped) > 0 {
		lines = append(lines, fmt.Sprintf("Reconnect in %s: %s.", where, strings.Join(dropped, ", ")))
	}
	return lines
}
