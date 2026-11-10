package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Default per adapter bound of concurrently dispatched updates
const DefaultMaxConcurrentUpdates = 32

const seenCap = 4096

// Seconds a completed update id receipt stays able to match a redelivery
const ReceiptTTLSeconds = 24 * 60 * 60

var nowSeconds = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Admission claim of one update shared by its dispatch and the PTB tasks it schedules
type Claim struct {
	Key          string
	dispatchTask *gateway.AdapterTask
	mu           sync.Mutex
	accepted     bool
	completed    bool
	failed       bool
	owners       int
}

// Marks the handoff irreversible for replay even if later work fails
func (c *Claim) Accept() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accepted = true
}

// Marks a caught preparation error so the update is not recorded as handled
func (c *Claim) Fail() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failed = true
}

// Accepted, completed and failed flags of the claim
func (c *Claim) State() (accepted, completed, failed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accepted, c.completed, c.failed
}

func (c *Claim) live() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.owners > 0
}

type claimKey struct{ state *UpdateAdmission }

// Update admission state an adapter keeps across Application rebuilds
type UpdateAdmission struct {
	ReceiptDir             string
	mu                     sync.Mutex
	seen                   *pydict.Dict[string, float64]
	inflight               map[string]*Claim
	receiptsLoaded         map[int64]struct{}
	receiptsDirty          []string
	receiptFlush           *gateway.AdapterTask
	updatesDispatchedTotal int
}

// Admission state whose receipts live under the given home directory
func NewUpdateAdmission(receiptDir string) *UpdateAdmission {
	return &UpdateAdmission{
		ReceiptDir:     receiptDir,
		seen:           pydict.New[string, float64](),
		inflight:       map[string]*Claim{},
		receiptsLoaded: map[int64]struct{}{},
	}
}

// Claim of the update being dispatched under the context, nil outside admission
func (s *UpdateAdmission) CurrentClaim(ctx context.Context) *Claim {
	c, _ := ctx.Value(claimKey{s}).(*Claim)
	return c
}

func (s *UpdateAdmission) withClaim(ctx context.Context, c *Claim) context.Context {
	return context.WithValue(ctx, claimKey{s}, c)
}

// Updates that reached dispatch, counted even when preparation fails
func (s *UpdateAdmission) UpdatesDispatchedTotal() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updatesDispatchedTotal
}

// Completed update keys with their receipt timestamps in insertion order
func (s *UpdateAdmission) SeenUpdateIDs() *pydict.Dict[string, float64] {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := pydict.New[string, float64]()
	for k, v := range s.seen.All() {
		out.Set(k, v)
	}
	return out
}

// Keys of the updates whose dispatch is still in flight
func (s *UpdateAdmission) InflightUpdateIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.inflight))
	for k := range s.inflight {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Pending receipt writer task, nil before the first completed update
func (s *UpdateAdmission) ReceiptFlush() *gateway.AdapterTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.receiptFlush
}

// Per bot receipt file under the adapter home
func (s *UpdateAdmission) ReceiptPath(botID int64) string {
	return filepath.Join(s.ReceiptDir, fmt.Sprintf("telegram_update_receipts_%d.json", botID))
}

func isDigitString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !pystr.IsDigit(r) {
			return false
		}
	}
	return true
}

// Python float of a JSON number, ok false for a non number and overflow true for an int past the float range
func receiptStamp(v any) (f float64, ok, overflow bool) {
	switch n := v.(type) {
	case float64:
		return n, true, false
	case int64:
		return float64(n), true, false
	case bool:
		if n {
			return 1, true, false
		}
		return 0, true, false
	case *big.Int:
		f, _ := new(big.Float).SetInt(n).Float64()
		return f, true, math.IsInf(f, 0)
	}
	return 0, false, false
}

// Seeds completed history from disk once per bot before its first admission check
func (s *UpdateAdmission) loadReceipts(botID int64) error {
	s.mu.Lock()
	if _, ok := s.receiptsLoaded[botID]; ok {
		s.mu.Unlock()
		return nil
	}
	s.receiptsLoaded[botID] = struct{}{}
	s.mu.Unlock()
	path := s.ReceiptPath(botID)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var payload any
	if err == nil {
		raw = bytes.TrimPrefix(raw, []byte("\uFEFF"))
		if !utf8.Valid(raw) {
			err = errors.New("UnicodeDecodeError: 'utf-8' codec can't decode the receipt bytes")
		} else {
			payload, err = pyjson.Loads(string(raw))
		}
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("[Telegram] Ignoring unreadable update receipts at %s", path), "error", err)
		return nil
	}
	obj, ok := payload.(*pyjson.Object)
	if !ok {
		return nil
	}
	idsVal, _ := obj.Get("update_ids")
	ids, ok := idsVal.(*pyjson.Object)
	if !ok {
		return nil
	}
	type stamped struct {
		ts  float64
		uid string
	}
	cutoff := nowSeconds() - ReceiptTTLSeconds
	var fresh []stamped
	for _, uid := range ids.Keys {
		v, _ := ids.Get(uid)
		ts, isNumber, overflow := receiptStamp(v)
		if !isNumber || !(ts > cutoff) || !isDigitString(pystr.LstripChars(uid, "-")) {
			continue
		}
		if overflow {
			return errors.New("OverflowError: int too large to convert to float")
		}
		fresh = append(fresh, stamped{ts, uid})
	}
	slices.SortStableFunc(fresh, func(a, b stamped) int {
		if a.ts != b.ts {
			if a.ts < b.ts {
				return -1
			}
			return 1
		}
		return strings.Compare(a.uid, b.uid)
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range fresh {
		key := strconv.FormatInt(botID, 10) + ":" + f.uid
		if !s.seen.Has(key) {
			gateway.BoundedPut(s.seen, key, f.ts, seenCap)
		}
	}
	return nil
}

// Records a completed update and starts the coalescing receipt writer when none is running
func (s *UpdateAdmission) recordReceipt(ctx context.Context, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gateway.BoundedPut(s.seen, key, nowSeconds(), seenCap)
	bot, _, _ := strings.Cut(key, ":")
	if !slices.Contains(s.receiptsDirty, bot) {
		s.receiptsDirty = append(s.receiptsDirty, bot)
	}
	if s.receiptFlush == nil || s.receiptFlush.Done() {
		s.receiptFlush = gateway.SpawnAdapterTask(ctx, func(context.Context) error {
			s.flushReceipts(ctx)
			return nil
		})
	}
}

// Writes each dirty bot's fresh receipts to its file until no bot is dirty
func (s *UpdateAdmission) flushReceipts(ctx context.Context) {
	for {
		s.mu.Lock()
		if len(s.receiptsDirty) == 0 {
			s.mu.Unlock()
			return
		}
		bot := s.receiptsDirty[len(s.receiptsDirty)-1]
		s.receiptsDirty = s.receiptsDirty[:len(s.receiptsDirty)-1]
		prefix, cutoff := bot+":", nowSeconds()-ReceiptTTLSeconds
		ids := pyjson.NewObject()
		for key, ts := range s.seen.All() {
			if strings.HasPrefix(key, prefix) && ts > cutoff {
				ids.Set(key[len(prefix):], ts)
			}
		}
		s.mu.Unlock()
		path := filepath.Join(s.ReceiptDir, "telegram_update_receipts_"+bot+".json")
		payload := pyjson.NewObject()
		payload.Set("update_ids", ids)
		_, err := home.Mkdir(ctx, filepath.Dir(path))
		if err == nil {
			err = config.AtomicJSONWriteWith(ctx, path, payload)
		}
		if err != nil {
			slog.Warn(fmt.Sprintf("[Telegram] Failed to persist update receipts to %s", path), "error", err)
		}
	}
}

// Admits the error callback entry while keeping the original callback as its registration key
func (t *TelegramApplication) errorCallback(cb *Callback) *Callback {
	return &Callback{Owner: cb.Owner, admitted: true, Fn: func(ctx context.Context, update any, cbctx *CallbackContext) error {
		if ta := telegramApplicationOf(cbctx); ta != nil {
			if claim := ta.state.CurrentClaim(ctx); claim != nil && claim.live() {
				claim.Accept()
			}
		}
		return cb.Fn(ctx, update, cbctx)
	}}
}

// Processor running chats concurrently and each chat's updates in arrival order
type PerChatUpdateProcessor struct {
	*boundedProcessor
	tails map[int64]chan struct{}
}

// Per chat processor of the bound, a ValueError below one
func NewPerChatUpdateProcessor(maxConcurrent int) (*PerChatUpdateProcessor, error) {
	b, err := newBoundedProcessor(maxConcurrent)
	if err != nil {
		return nil, err
	}
	return &PerChatUpdateProcessor{boundedProcessor: b, tails: map[int64]chan struct{}{}}, nil
}

func (p *PerChatUpdateProcessor) MaxConcurrentUpdates() int { return p.max }

func (p *PerChatUpdateProcessor) ProcessUpdate(ctx context.Context, update any, coroutine func(ctx context.Context) error) error {
	return p.reserve(update).run(ctx, coroutine)
}

// Turn chained behind the previous update of the same chat once its slot is granted, unchained without a chat
func (p *PerChatUpdateProcessor) reserve(update any) *updateTurn {
	var chat *Chat
	if ec, ok := update.(EffectiveChatter); ok {
		chat = ec.EffectiveChat()
	}
	if chat == nil {
		return p.reserveTurn(nil, nil, nil)
	}
	var prev, done chan struct{}
	enter := func() {
		prev = p.tails[chat.ID]
		done = make(chan struct{})
		p.tails[chat.ID] = done
	}
	wait := func(ctx context.Context) error {
		if prev == nil {
			return nil
		}
		select {
		case <-prev:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	release := func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		close(done)
		if p.tails[chat.ID] == done {
			delete(p.tails, chat.ID)
		}
	}
	finish := func() {
		if prev == nil || chanClosed(prev) {
			release()
			return
		}
		go func() {
			<-prev
			release()
		}()
	}
	return p.reserveTurn(enter, wait, finish)
}

func chanClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Per chat processor bounded by platforms.telegram.extra.max_concurrent_updates, a bad value warning and taking the default
func BuildUpdateProcessor(extra map[string]any, name string) *PerChatUpdateProcessor {
	raw, ok := extra["max_concurrent_updates"]
	if !ok {
		raw = DefaultMaxConcurrentUpdates
	}
	limit := gateway.CoercePort(raw, 0)
	if limit < 1 {
		slog.Warn(fmt.Sprintf("[%s] Invalid max_concurrent_updates=%s; using %d", name, pyrepr.Repr(raw), DefaultMaxConcurrentUpdates))
		limit = DefaultMaxConcurrentUpdates
	}
	p, _ := NewPerChatUpdateProcessor(limit)
	return p
}

// PTB Application admitting each update once per adapter before every handler group, native plugins included
type TelegramApplication struct {
	*Application
	Adapter any
	state   *UpdateAdmission
}

// Telegram application of the adapter and its admission state over the processor
func NewTelegramApplication(botID int64, processor UpdateProcessor, adapter any, state *UpdateAdmission) *TelegramApplication {
	t := &TelegramApplication{Application: NewApplication(botID, processor), Adapter: adapter, state: state}
	t.impl = t
	return t
}

func telegramApplicationOf(cbctx *CallbackContext) *TelegramApplication {
	if cbctx == nil || cbctx.Application == nil {
		return nil
	}
	ta, _ := cbctx.Application.impl.(*TelegramApplication)
	return ta
}

func (t *TelegramApplication) addHandler(h Handler, group int) {
	t.Application.addHandler(h, group)
	t.admitCallbacks(h)
}

// Wraps each handler callback so entry accepts the claim and completion records it, composites instrumenting their children
func (t *TelegramApplication) admitCallbacks(h Handler) {
	if composite, ok := h.(CompositeHandler); ok {
		for _, child := range composite.ChildHandlers() {
			t.admitCallbacks(child)
		}
		return
	}
	ch, ok := h.(CallbackHandler)
	if !ok {
		return
	}
	cb := ch.HandlerCallback()
	if cb == nil || cb.admitted {
		return
	}
	ch.SetHandlerCallback(&Callback{Owner: cb.Owner, admitted: true, Fn: func(ctx context.Context, update any, cbctx *CallbackContext) error {
		var claim *Claim
		ta := telegramApplicationOf(cbctx)
		if ta != nil {
			claim = ta.state.CurrentClaim(ctx)
		}
		if claim != nil && cb.Owner != ta.Adapter {
			claim.Accept()
		}
		if err := cb.Fn(ctx, update, cbctx); err != nil {
			return err
		}
		if claim != nil {
			accepted, _, failed := claim.State()
			if failed && !accepted {
				if gateway.CurrentAdapterTask(ctx) == claim.dispatchTask {
					return ErrApplicationHandlerStop
				}
			} else {
				claim.mu.Lock()
				claim.completed = true
				claim.mu.Unlock()
			}
		}
		return nil
	}})
}

func (t *TelegramApplication) addErrorHandler(cb *Callback, block BlockMode) {
	t.Application.addErrorHandlerEntry(cb, t.errorCallback(cb), block)
}

// Pins scheduled work to the live claim of the context so the claim outlasts every task it spawned
func (t *TelegramApplication) createTask(ctx context.Context, fn func(ctx context.Context) error, update any, isErrorHandler bool) *gateway.AdapterTask {
	claim := t.state.CurrentClaim(ctx)
	if claim == nil {
		return t.Application.spawnTask(ctx, fn, update, isErrorHandler, nil)
	}
	claim.mu.Lock()
	pinned := claim.owners > 0
	if pinned {
		claim.owners++
	}
	claim.mu.Unlock()
	if !pinned {
		return t.Application.spawnTask(ctx, fn, update, isErrorHandler, nil)
	}
	return t.Application.spawnTask(ctx, fn, update, isErrorHandler, func(tctx context.Context, cancelled bool) {
		t.releaseClaim(tctx, claim, cancelled)
	})
}

// Drops one owner and records the receipt once the last owner settles accepted or cleanly completed
func (t *TelegramApplication) releaseClaim(ctx context.Context, claim *Claim, cancelled bool) {
	claim.mu.Lock()
	if cancelled {
		claim.failed = true
	}
	claim.owners--
	if claim.owners > 0 {
		claim.mu.Unlock()
		return
	}
	record := claim.accepted || (claim.completed && !claim.failed)
	claim.mu.Unlock()
	t.state.mu.Lock()
	delete(t.state.inflight, claim.Key)
	t.state.mu.Unlock()
	if record {
		t.state.recordReceipt(ctx, claim.Key)
	}
}

// Fails the claim and stops later groups unless something already accepted the update
func (t *TelegramApplication) processError(ctx context.Context, update any, failure error) (bool, error) {
	claim := t.state.CurrentClaim(ctx)
	if claim != nil {
		claim.Fail()
	}
	stopped, err := t.Application.processError(ctx, update, failure)
	if err != nil {
		return false, err
	}
	if stopped {
		return true, nil
	}
	if claim == nil {
		return false, nil
	}
	accepted, _, _ := claim.State()
	return !accepted, nil
}

// Drops a replayed or in flight update and otherwise claims it for this dispatch
func (t *TelegramApplication) processUpdate(ctx context.Context, update any) error {
	u, ok := update.(*Update)
	if !ok {
		return t.Application.processUpdate(ctx, update)
	}
	key := fmt.Sprintf("%d:%d", t.BotID, u.UpdateID)
	if err := t.state.loadReceipts(t.BotID); err != nil {
		return err
	}
	t.state.mu.Lock()
	t.state.updatesDispatchedTotal++
	_, pending := t.state.inflight[key]
	if t.state.seen.Has(key) || pending {
		t.state.mu.Unlock()
		return nil
	}
	claim := &Claim{Key: key, dispatchTask: gateway.CurrentAdapterTask(ctx), owners: 1}
	t.state.inflight[key] = claim
	t.state.mu.Unlock()
	err := t.Application.processUpdate(t.state.withClaim(ctx, claim), update)
	if err != nil {
		claim.Fail()
	}
	t.releaseClaim(ctx, claim, false)
	return err
}
