package photon

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"regexp"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protowire"
)

// Token renewal and cache limits the SDK applies to iMessage lines
const (
	sharedPhone          = "shared"
	renewalRatio         = 0.8
	expiryBuffer         = 30 * time.Second
	minRenewalDelay      = 5 * time.Second
	renewalRetryDelay    = 30 * time.Second
	forceRefreshInterval = 5 * time.Second
	messageCacheSize     = 1000
	pollCacheSize        = 1000
	profileGateTTL       = 60 * time.Second
	profileGateRetry     = 30 * time.Second
	contactShareTTL      = 24 * time.Hour
	contactShareMax      = 10000
)

var chatGUIDRE = regexp.MustCompile(`^any;[-+];.+$`)

// Bounded map evicting the oldest insertion, a rewrite moving the key to the newest slot
type boundedMap[K comparable, V any] struct {
	mu    sync.Mutex
	cap   int
	order *list.List
	items map[K]*list.Element
}

type boundedEntry[K comparable, V any] struct {
	key K
	val V
}

func newBoundedMap[K comparable, V any](capacity int) *boundedMap[K, V] {
	return &boundedMap[K, V]{cap: capacity, order: list.New(), items: map[K]*list.Element{}}
}

func (m *boundedMap[K, V]) get(key K) (V, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		return el.Value.(*boundedEntry[K, V]).val, true
	}
	var zero V
	return zero, false
}

func (m *boundedMap[K, V]) put(key K, val V) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		m.order.Remove(el)
	}
	m.items[key] = m.order.PushBack(&boundedEntry[K, V]{key: key, val: val})
	for m.order.Len() > m.cap {
		oldest := m.order.Front()
		m.order.Remove(oldest)
		delete(m.items, oldest.Value.(*boundedEntry[K, V]).key)
	}
}

func (m *boundedMap[K, V]) remove(key K) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		m.order.Remove(el)
		delete(m.items, key)
	}
}

// One iMessage line, the shared pool or a dedicated number, with its unary and streaming channels
type line struct {
	key      string
	sess     *session
	unary    *grpc.ClientConn
	stream   *grpc.ClientConn
	cache    *boundedMap[string, *smsg]
	contacts *contactShareTracker

	mu    sync.Mutex
	phone string
	token string
}

func (l *line) currentPhone() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.phone
}

func (l *line) close() {
	_ = l.unary.Close()
	_ = l.stream.Close()
}

// Bearer token of the line, renewed first when it is close to expiry
func (l *line) bearer(ctx context.Context) (string, error) {
	if err := l.sess.refreshIfNeeded(ctx); err != nil {
		return "", err
	}
	return l.sess.lineToken(l), nil
}

func (l *line) call(ctx context.Context, method string, req *pbWriter) ([]byte, error) {
	token, err := l.bearer(ctx)
	if err != nil {
		return nil, err
	}
	return unaryCall(ctx, l.unary, token, method, req.b)
}

func (l *line) open(ctx context.Context, method string, req *pbWriter) (*serverStream, error) {
	token, err := l.bearer(ctx)
	if err != nil {
		return nil, err
	}
	return openServerStream(ctx, l.stream, token, method, req.b)
}

// Chat guid checked against the any;-; or any;+; shape the server accepts
func checkChatGUID(chat string) error {
	if chatGUIDRE.MatchString(chat) {
		return nil
	}
	return &rpcError{Code: codes.InvalidArgument, ErrorCode: "invalidArgument",
		Detail: `chat must start with "any;-;" for direct chats or "any;+;" for group chats, got ` + fmt.Sprintf("%q", chat)}
}

// UTF-16 styled range of an outgoing text
type textFormat struct {
	Type   string
	Start  int
	Length int
}

func (l *line) sendText(ctx context.Context, chat, text string, linkPreview bool, formatting []textFormat) (*imMessage, error) {
	if err := checkChatGUID(chat); err != nil {
		return nil, err
	}
	w := &pbWriter{}
	w.str(1, chat)
	w.str(2, text)
	if linkPreview {
		w.b = protowire.AppendTag(w.b, 7, protowire.VarintType)
		w.b = protowire.AppendVarint(w.b, 1)
	}
	for _, f := range formatting {
		fw := &pbWriter{}
		fw.str(1, f.Type)
		fw.varint(2, uint64(f.Start))
		fw.varint(3, uint64(f.Length))
		w.msg(8, fw)
	}
	resp, err := l.call(ctx, "MessageService/SendTextMessage", w)
	if err != nil {
		return nil, err
	}
	return decodeMessageEnvelope(resp)
}

func (l *line) uploadAttachment(ctx context.Context, name string, data []byte) (imAttachment, error) {
	w := &pbWriter{}
	w.str(1, name)
	w.bytes(2, data)
	resp, err := l.call(ctx, "AttachmentService/UploadAttachment", w)
	if err != nil {
		return imAttachment{}, err
	}
	return decodeUploadResponse(resp)
}

func (l *line) sendAttachment(ctx context.Context, chat, attachmentGUID string, audio bool) (*imMessage, error) {
	if err := checkChatGUID(chat); err != nil {
		return nil, err
	}
	ref := &pbWriter{}
	ref.str(2, attachmentGUID)
	w := &pbWriter{}
	w.str(1, chat)
	w.msg(2, ref)
	if audio {
		w.b = protowire.AppendTag(w.b, 5, protowire.VarintType)
		w.b = protowire.AppendVarint(w.b, 1)
	}
	resp, err := l.call(ctx, "MessageService/SendAttachmentMessage", w)
	if err != nil {
		return nil, err
	}
	return decodeMessageEnvelope(resp)
}

func (l *line) getMessage(ctx context.Context, guid string) (*imMessage, error) {
	w := &pbWriter{}
	w.str(2, guid)
	resp, err := l.call(ctx, "MessageService/GetMessage", w)
	if err != nil {
		return nil, err
	}
	return decodeMessageEnvelope(resp)
}

// Adds or removes a tapback, native kinds for the six classic emoji and an emoji reaction otherwise
func (l *line) setReaction(ctx context.Context, chat, guid string, part *int32, emoji string, set bool) (*imMessage, error) {
	if err := checkChatGUID(chat); err != nil {
		return nil, err
	}
	target := &pbWriter{}
	target.str(1, chat)
	target.str(2, guid)
	target.optInt32(3, part)
	reaction := &pbWriter{}
	if kind, ok := emojiToTapback[emoji]; ok {
		value, _ := reactionKindValue(kind)
		reaction.varint(1, value)
	} else {
		value, _ := reactionKindValue("emoji")
		reaction.varint(1, value)
		reaction.strAlways(2, emoji)
	}
	w := &pbWriter{}
	w.msg(1, target)
	w.msg(2, reaction)
	w.flag(3, set)
	resp, err := l.call(ctx, "MessageService/SetReaction", w)
	if err != nil {
		return nil, err
	}
	return decodeMessageEnvelope(resp)
}

func (l *line) chatCall(ctx context.Context, method, chat string, extra func(w *pbWriter)) error {
	if err := checkChatGUID(chat); err != nil {
		return err
	}
	w := &pbWriter{}
	w.str(1, chat)
	if extra != nil {
		extra(w)
	}
	_, err := l.call(ctx, method, w)
	return err
}

func (l *line) markRead(ctx context.Context, chat string) error {
	return l.chatCall(ctx, "ChatService/MarkChatRead", chat, nil)
}

func (l *line) setTyping(ctx context.Context, chat string, on bool) error {
	return l.chatCall(ctx, "ChatService/SetTyping", chat, func(w *pbWriter) { w.flag(2, on) })
}

func (l *line) shareContactInfo(ctx context.Context, chat string) error {
	return l.chatCall(ctx, "ChatService/ShareContactInfo", chat, nil)
}

// Creates or reopens the iMessage chat with the addresses
func (l *line) createChat(ctx context.Context, addresses []string) (*imChat, error) {
	w := &pbWriter{}
	for _, a := range addresses {
		w.strAlways(1, a)
	}
	w.varint(2, 1)
	resp, err := l.call(ctx, "ChatService/CreateChat", w)
	if err != nil {
		return nil, err
	}
	return decodeChatEnvelope(resp)
}

func (l *line) getChat(ctx context.Context, chat string) (*imChat, error) {
	if err := checkChatGUID(chat); err != nil {
		return nil, err
	}
	w := &pbWriter{}
	w.str(1, chat)
	resp, err := l.call(ctx, "ChatService/GetChat", w)
	if err != nil {
		return nil, err
	}
	return decodeChatEnvelope(resp)
}

func (l *line) createPoll(ctx context.Context, chat, title string, options []string) (*imPoll, error) {
	if err := checkChatGUID(chat); err != nil {
		return nil, err
	}
	w := &pbWriter{}
	w.str(1, chat)
	w.str(2, title)
	for _, o := range options {
		w.strAlways(3, o)
	}
	resp, err := l.call(ctx, "PollService/CreatePoll", w)
	if err != nil {
		return nil, err
	}
	return decodePollEnvelope(resp)
}

func (l *line) getPoll(ctx context.Context, guid string) (*imPoll, error) {
	w := &pbWriter{}
	w.str(1, guid)
	resp, err := l.call(ctx, "PollService/GetPoll", w)
	if err != nil {
		return nil, err
	}
	return decodePollEnvelope(resp)
}

// Current group icon bytes and mime, an empty result when the group has none
func (l *line) getIcon(ctx context.Context, chat string) ([]byte, string, error) {
	if err := checkChatGUID(chat); err != nil {
		return nil, "", err
	}
	w := &pbWriter{}
	w.str(1, chat)
	resp, err := l.call(ctx, "GroupService/GetIcon", w)
	if err != nil {
		if re := asRPCError(err); re != nil && re.notFound() && re.ErrorCode == "groupIconNotFound" {
			return nil, "", nil
		}
		return nil, "", err
	}
	return decodeIcon(resp)
}

// Primary file bytes of an attachment, overCap reporting a download stopped past the limit
func (l *line) download(ctx context.Context, guid string, limit int64) (data []byte, overCap bool, err error) {
	w := &pbWriter{}
	w.str(1, guid)
	st, err := l.open(ctx, "AttachmentService/DownloadAttachment", w)
	if err != nil {
		return nil, false, err
	}
	defer st.close()
	for {
		frame, err := st.recv()
		if errors.Is(err, io.EOF) {
			return data, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		chunk, err := decodeDownloadFrame(frame)
		if err != nil {
			return nil, false, err
		}
		if limit > 0 && int64(len(data)+len(chunk)) > limit {
			return nil, true, nil
		}
		data = append(data, chunk...)
	}
}

// Live durable event stream of one domain
func (l *line) subscribe(ctx context.Context, kind string) (*serverStream, error) {
	method := map[string]string{streamMessages: "MessageService/SubscribeMessageEvents",
		streamPolls: "PollService/SubscribePollEvents", streamGroups: "GroupService/SubscribeGroupEvents"}[kind]
	return l.open(ctx, method, &pbWriter{})
}

// Finite replay of every durable event after the sequence
func (l *line) catchUp(ctx context.Context, after uint64) (*serverStream, error) {
	w := &pbWriter{}
	w.optUint64(1, &after)
	return l.open(ctx, "EventService/CatchUpEvents", w)
}

// Observer the session tells when a dedicated line appears or goes away
type lineObserver interface {
	attach(l *line)
	detach(l *line)
}

// Photon project connection, the token set and the iMessage lines it opens
type session struct {
	cloud   *cloudClient
	project *projectData
	gate    *profileGate
	polls   *boundedMap[string, *cachedPoll]
	tele    *telemetry

	mu          sync.Mutex
	tokens      *tokenData
	expiresAt   time.Time
	lastRefresh time.Time
	refreshing  chan struct{}
	refreshErr  error
	lines       []*line
	byKey       map[string]*line
	observer    lineObserver
	timer       *time.Timer
	failures    int
	closed      bool
}

// Fetches the project, mints the iMessage tokens and opens a channel pair per line
func openSession(ctx context.Context, projectID, secret string, tele *telemetry) (*session, error) {
	s := &session{cloud: newCloudClient(projectID, secret), polls: newBoundedMap[string, *cachedPoll](pollCacheSize),
		tele: tele, byKey: map[string]*line{}}
	started := time.Now()
	err := s.start(ctx)
	tele.span("spectrum.init", started, err, map[string]any{"spectrum.provider_count": 1, "spectrum.flatten_groups": true})
	if err != nil {
		s.close()
		return nil, err
	}
	tele.log(logInfo, "spectrum.lifecycle", "Spectrum started", map[string]any{"spectrum.lifecycle.provider_count": 1,
		"spectrum.lifecycle.providers": "imessage", "spectrum.lifecycle.telemetry": tele != nil})
	return s, nil
}

func (s *session) start(ctx context.Context) error {
	project, err := s.cloud.getProject(ctx)
	if err != nil {
		return err
	}
	s.project = project
	s.gate = &profileGate{enabled: project.profileSynced(), validUntil: time.Now().Add(profileGateTTL), cloud: s.cloud}
	started := time.Now()
	tokens, err := s.cloud.issueTokens(ctx)
	s.tele.span("spectrum.provider.create_client", started, err, map[string]any{"spectrum.provider": "imessage"})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens, s.lastRefresh = tokens, time.Now()
	s.expiresAt = s.lastRefresh.Add(time.Duration(tokens.ExpiresIn * float64(time.Second)))
	if tokens.Type == "shared" {
		l, err := s.newLine(sharedPhone, sharedLineAddress(), sharedPhone)
		if err != nil {
			return err
		}
		s.lines, s.byKey[l.key] = []*line{l}, l
	} else if err := s.reconcileLocked(tokens); err != nil {
		return err
	}
	s.scheduleRenewalLocked()
	return nil
}

func (s *session) newLine(key, address, phone string) (*line, error) {
	unary, err := dialChannel(address)
	if err != nil {
		return nil, err
	}
	stream, err := dialChannel(address)
	if err != nil {
		_ = unary.Close()
		return nil, err
	}
	l := &line{key: key, sess: s, unary: unary, stream: stream, phone: phone,
		cache: newBoundedMap[string, *smsg](messageCacheSize)}
	l.contacts = &contactShareTracker{line: l, shared: newBoundedMap[string, time.Time](contactShareMax)}
	if s.tokens != nil && s.tokens.Type == "dedicated" {
		l.token = s.tokens.Auth[key]
	}
	return l, nil
}

// Aligns the dedicated lines with the token payload, new instances opened and vanished ones retired
func (s *session) reconcileLocked(tokens *tokenData) error {
	added, removed := 0, 0
	kept := s.lines[:0]
	for _, l := range s.lines {
		if _, ok := tokens.Auth[l.key]; ok {
			kept = append(kept, l)
			continue
		}
		delete(s.byKey, l.key)
		removed++
		if s.observer != nil {
			go s.observer.detach(l)
		}
		go l.close()
	}
	s.lines = kept
	ids := make([]string, 0, len(tokens.Auth))
	for id := range tokens.Auth {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		var phone string
		if p := tokens.Numbers[id]; p != nil {
			phone = *p
		}
		if existing, ok := s.byKey[id]; ok {
			existing.mu.Lock()
			if phone != "" {
				existing.phone = phone
			} else {
				s.tele.log(logWarn, "spectrum.imessage.auth", "imessage line lost its phone number; keeping the last known number",
					map[string]any{"spectrum.imessage.instance": id})
			}
			existing.mu.Unlock()
			if s.observer != nil {
				s.observer.attach(existing)
			}
			continue
		}
		if phone == "" {
			s.tele.log(logWarn, "spectrum.imessage.auth", "skipping imessage line without a phone number",
				map[string]any{"spectrum.imessage.instance": id})
			continue
		}
		l, err := s.newLine(id, id+dedicatedAddressSuffix, phone)
		if err != nil {
			return err
		}
		s.lines = append(s.lines, l)
		s.byKey[id] = l
		if s.observer != nil {
			s.observer.attach(l)
		}
		added++
	}
	if added > 0 || removed > 0 {
		s.tele.log(logInfo, "spectrum.imessage.auth", "imessage lines reconciled", map[string]any{
			"spectrum.imessage.lines.added": added, "spectrum.imessage.lines.removed": removed,
			"spectrum.imessage.lines.total": len(s.lines)})
	}
	return nil
}

// Installs the stream observer and attaches every current line to it
func (s *session) observe(o lineObserver) {
	s.mu.Lock()
	s.observer = o
	lines := append([]*line(nil), s.lines...)
	s.mu.Unlock()
	for _, l := range lines {
		o.attach(l)
	}
}

func (s *session) shared() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens != nil && s.tokens.Type == "shared"
}

func (s *session) currentLines() []*line {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*line(nil), s.lines...)
}

func (s *session) lineToken(l *line) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens.Type == "shared" {
		return s.tokens.Token
	}
	if token := s.tokens.Auth[l.key]; token != "" {
		return token
	}
	return l.token
}

func (s *session) scheduleRenewalLocked() {
	if s.closed {
		return
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	ttl := time.Duration(s.tokens.ExpiresIn * float64(time.Second))
	delay := max(time.Duration(float64(ttl)*renewalRatio), minRenewalDelay)
	s.timer = time.AfterFunc(delay, s.scheduledRefresh)
}

// Timer driven renewal, retried every thirty seconds until it succeeds
func (s *session) scheduledRefresh() {
	ctx, cancel := context.WithTimeout(context.Background(), cloudRequestTimeout)
	defer cancel()
	if err := s.refresh(ctx); err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return
		}
		s.failures++
		s.tele.log(logWarn, "spectrum.imessage.auth", "imessage token refresh failed; retrying", withError(map[string]any{
			"spectrum.imessage.auth.attempt": s.failures, "spectrum.imessage.auth.retry_in_ms": renewalRetryDelay.Milliseconds()}, err))
		s.timer = time.AfterFunc(renewalRetryDelay, s.scheduledRefresh)
	}
}

func (s *session) refreshIfNeeded(ctx context.Context) error {
	s.mu.Lock()
	fresh := time.Now().Before(s.expiresAt.Add(-expiryBuffer))
	s.mu.Unlock()
	if fresh {
		return nil
	}
	return s.refresh(ctx)
}

// Re-mints the tokens unless a refresh ran in the last five seconds, the stream recover hook
func (s *session) forceRefresh(ctx context.Context) error {
	s.mu.Lock()
	recent := time.Since(s.lastRefresh) < forceRefreshInterval
	s.mu.Unlock()
	if recent {
		return nil
	}
	return s.refresh(ctx)
}

// Single flight token mint, reconciling dedicated lines and rescheduling the renewal
func (s *session) refresh(ctx context.Context) error {
	s.mu.Lock()
	if wait := s.refreshing; wait != nil {
		s.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
		s.mu.Lock()
		err := s.refreshErr
		s.mu.Unlock()
		return err
	}
	done := make(chan struct{})
	s.refreshing = done
	s.mu.Unlock()
	tokens, err := s.cloud.issueTokens(ctx)
	s.mu.Lock()
	defer func() {
		s.refreshing, s.refreshErr = nil, err
		close(done)
		s.mu.Unlock()
	}()
	if err != nil || s.closed {
		return err
	}
	s.tokens, s.lastRefresh = tokens, time.Now()
	s.expiresAt = s.lastRefresh.Add(time.Duration(tokens.ExpiresIn * float64(time.Second)))
	if s.failures > 0 {
		s.tele.log(logInfo, "spectrum.imessage.auth", "imessage token refresh recovered", map[string]any{"spectrum.imessage.auth.attempt": s.failures})
		s.failures = 0
	}
	if tokens.Type == "dedicated" {
		if rerr := s.reconcileLocked(tokens); rerr != nil {
			s.tele.log(logError, "spectrum.imessage.auth", "imessage line reconcile failed", withError(nil, rerr))
		}
	}
	s.scheduleRenewalLocked()
	return nil
}

// Line serving a phone, the only line in shared mode
func (s *session) lineForPhone(phone string) (*line, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lines) == 0 {
		return nil, errors.New("no iMessage lines are configured for this Photon project")
	}
	if s.tokens.Type == "shared" {
		return s.lines[0], nil
	}
	for _, l := range s.lines {
		if l.currentPhone() == phone {
			return l, nil
		}
	}
	phones := make([]string, 0, len(s.lines))
	for _, l := range s.lines {
		phones = append(phones, l.currentPhone())
	}
	return nil, fmt.Errorf("no iMessage line serves phone %s (available: %v)", phone, phones)
}

// Random line for a new chat, the shared line in shared mode
func (s *session) randomLine() (*line, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lines) == 0 {
		return nil, errors.New("no iMessage phones are configured for this Photon project")
	}
	return s.lines[rand.IntN(len(s.lines))], nil
}

// Stops renewal and closes every line channel
func (s *session) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
	}
	lines := s.lines
	s.lines, s.byKey = nil, map[string]*line{}
	s.mu.Unlock()
	started := time.Now()
	for _, l := range lines {
		l.close()
	}
	s.tele.span("spectrum.provider.destroy_client", started, nil, map[string]any{"spectrum.provider": "imessage"})
}

// Cached project profile sync switch refreshed from the cloud at most once a minute
type profileGate struct {
	mu         sync.Mutex
	cloud      *cloudClient
	enabled    bool
	validUntil time.Time
	retryAfter time.Time
}

func (g *profileGate) isEnabled(ctx context.Context) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if now.Before(g.validUntil) {
		return g.enabled
	}
	if now.Before(g.retryAfter) {
		return false
	}
	project, err := g.cloud.getProject(ctx)
	if err != nil {
		g.enabled, g.validUntil, g.retryAfter = false, time.Time{}, time.Now().Add(profileGateRetry)
		return false
	}
	g.enabled, g.validUntil, g.retryAfter = project.profileSynced(), time.Now().Add(profileGateTTL), time.Time{}
	return g.enabled
}

// Chats the line pushed its contact card to, once per chat per day
type contactShareTracker struct {
	line   *line
	shared *boundedMap[string, time.Time]
}

func (t *contactShareTracker) recentlyShared(chat string) bool {
	at, ok := t.shared.get(chat)
	if ok && time.Since(at) < contactShareTTL {
		return true
	}
	if ok {
		t.shared.remove(chat)
	}
	return false
}

// Shares the contact card when the project profile is synced, a precondition failure cached for the day
func (t *contactShareTracker) onInbound(chat string) {
	if chat == "" || t.recentlyShared(chat) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), cloudRequestTimeout)
		defer cancel()
		tele := t.line.sess.tele
		if !t.line.sess.gate.isEnabled(ctx) || t.recentlyShared(chat) {
			return
		}
		t.shared.put(chat, time.Now())
		safeChat := sanitizeErrorMessage(chat)
		if err := t.line.shareContactInfo(ctx, chat); err != nil {
			if re := asRPCError(err); re == nil || re.ErrorCode != "preconditionFailed" {
				t.shared.remove(chat)
			}
			tele.log(logWarn, "spectrum.imessage.contact", "failed to share contact card",
				withError(map[string]any{"spectrum.imessage.contact.chat": safeChat}, err))
			return
		}
		tele.log(logInfo, "spectrum.imessage.contact", "shared contact card", map[string]any{"spectrum.imessage.contact.chat": safeChat})
	}()
}

// Poll title and option titles keyed by the option identifiers vote events carry
type cachedPoll struct {
	title   string
	options []string
	byID    map[string]string
}

func newCachedPoll(title string, options []imPollOption) *cachedPoll {
	p := &cachedPoll{title: title, byID: map[string]string{}}
	for _, o := range options {
		p.options = append(p.options, o.Text)
		if o.Identifier != "" {
			p.byID[o.Identifier] = o.Text
		}
	}
	return p
}
