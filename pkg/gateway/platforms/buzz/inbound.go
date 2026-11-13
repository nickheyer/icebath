package buzz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mediacache"
)

const (
	maxInboundAttachments      = 4
	maxInboundAttachmentBytes  = 20 * 1024 * 1024
	attachmentDownloadTimeout  = 30 * time.Second
	maxAttachmentMimeTypeChars = 255
)

// Message type of each cached attachment kind
var attachmentKindTypes = map[string]gateway.MessageType{
	"image": gateway.MessageTypePhoto, "video": gateway.MessageTypeVideo,
	"audio": gateway.MessageTypeAudio, "document": gateway.MessageTypeDocument,
}

// Localized media kinds in the priority order that picks the message type
var mediaKindPriority = []struct {
	kind string
	mt   gateway.MessageType
}{{"image", gateway.MessageTypePhoto}, {"audio", gateway.MessageTypeAudio}, {"video", gateway.MessageTypeVideo}}

// Adopts the restored cursor of a channel, true when one existed so history is not reseeded over downtime arrivals
func (a *Adapter) restoreChannelState(channelID, chatType string) bool {
	restored, ok := a.restoredCursors[channelID]
	if !ok {
		return false
	}
	delete(a.restoredCursors, channelID)
	if restored.ChatType != "" {
		chatType = restored.ChatType
	}
	st := newChannelState(chatType)
	st.lastTS = restored.LastTS
	for _, id := range restored.Seen {
		st.seen.set(id, struct{}{})
	}
	a.state.set(channelID, st)
	return true
}

// Starts watching a channel from its newest events so a restart never replays history
func (a *Adapter) seedChannel(ctx context.Context, channelID, chatType string) error {
	a.mu.Lock()
	if a.restoreChannelState(channelID, chatType) {
		a.mu.Unlock()
		return nil
	}
	st := newChannelState(chatType)
	a.state.set(channelID, st)
	a.mu.Unlock()
	res, err := a.runCLI(ctx, []string{"messages", "get", "--channel", channelID, "--limit", strconv.Itoa(fetchLimit)}, nil)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if res.code != 0 {
		slog.Warn(fmt.Sprintf("Buzz: could not seed channel %s — %s", channelID, cliErrorMessage(res.stderr, res.code, "")))
		st.lastTS = time.Now().Unix()
		return nil
	}
	for _, event := range parseJSONList(res.stdout) {
		if truthy(event["id"]) {
			st.seen.set(textOf(event["id"]), struct{}{})
		}
		if ts := intOr0(event["created_at"]); ts > st.lastTS {
			st.lastTS = ts
		}
		a.rememberEvent(st, event)
		a.maybeLatchDM(channelID, st, event)
	}
	st.trim()
	return nil
}

// Starts watching a DM conversation, seeded at startup and otherwise restored or dispatched from its start
func (a *Adapter) adoptConversation(ctx context.Context, channelID string, seed bool) error {
	if seed {
		return a.seedChannel(ctx, channelID, "dm")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.restoreChannelState(channelID, "dm") {
		a.state.set(channelID, newChannelState("dm"))
	}
	return nil
}

// Watches DMs from the DM list and DM shaped channel entries, and adopts channels joined mid run in watch all mode
func (a *Adapter) discoverDMs(ctx context.Context, seed bool) error {
	res, err := a.runCLI(ctx, []string{"dms", "list"}, nil)
	if err != nil {
		return err
	}
	if res.code == 0 {
		for _, dm := range parseJSONList(res.stdout) {
			dmID := ""
			if truthy(dm["dm_id"]) {
				dmID = textOf(dm["dm_id"])
			}
			a.mu.Lock()
			fresh := dmID != "" && !a.state.has(dmID) && !a.restricted[dmID]
			a.mu.Unlock()
			if !fresh {
				continue
			}
			if err := a.adoptConversation(ctx, dmID, seed); err != nil {
				return err
			}
			a.mu.Lock()
			if _, ok := a.channelNames[dmID]; !ok {
				a.channelNames[dmID] = "DM"
			}
			a.mu.Unlock()
		}
	}
	res, err = a.runCLI(ctx, []string{"channels", "list"}, nil)
	if err != nil || res.code != 0 {
		return err
	}
	for _, ch := range parseJSONList(res.stdout) {
		chID := ""
		if truthy(ch["channel_id"]) {
			chID = textOf(ch["channel_id"])
		}
		if chID == "" {
			continue
		}
		a.mu.Lock()
		a.channelMeta[chID] = ch
		if _, ok := a.channelNames[chID]; !ok {
			name := chID
			if truthy(ch["name"]) {
				name = textOf(ch["name"])
			}
			a.channelNames[chID] = name
		}
		if a.restricted[chID] {
			a.mu.Unlock()
			continue
		}
		dmShaped := a.mayReclassifyAsDM(chID)
		existing, watched := a.state.get(chID)
		if dmShaped && watched {
			existing.chatType = "dm"
		}
		a.mu.Unlock()
		switch {
		case dmShaped && !watched:
			if err := a.adoptConversation(ctx, chID, seed); err != nil {
				return err
			}
		case !dmShaped && !watched && !seed && len(a.channels) == 0:
			if err := a.seedChannel(ctx, chID, "group"); err != nil {
				return err
			}
			a.mu.Lock()
			name := a.channelNames[chID]
			a.mu.Unlock()
			slog.Info(fmt.Sprintf("Buzz: adopted newly joined channel %s (%s)", chID, name))
		}
	}
	return nil
}

// Watched channel ids in watch order
func (a *Adapter) watchedChannels() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.state.keys...)
}

// Polls every watched channel each interval, rediscovering DMs every few sweeps, until cancelled
func (a *Adapter) pollLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(a.pollInterval):
		}
		a.mu.Lock()
		a.pollCount++
		count := a.pollCount
		a.mu.Unlock()
		err := func() error {
			if count%dmDiscoveryEvery == 0 {
				a.discoverMu.Lock()
				err := a.discoverDMs(ctx, false)
				a.discoverMu.Unlock()
				if err != nil {
					return err
				}
			}
			for _, channelID := range a.watchedChannels() {
				if err := a.pollChannel(ctx, channelID); err != nil {
					return err
				}
			}
			return nil
		}()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			slog.Warn(fmt.Sprintf("Buzz: poll sweep failed: %v", err))
		}
	}
}

// Fetches one channel's events since its high water mark and handles them
func (a *Adapter) pollChannel(ctx context.Context, channelID string) error {
	a.mu.Lock()
	st, ok := a.state.get(channelID)
	var since int64
	if ok {
		since = st.lastTS
	}
	a.mu.Unlock()
	if !ok {
		return nil
	}
	args := []string{"messages", "get", "--channel", channelID, "--limit", strconv.Itoa(fetchLimit)}
	if since != 0 {
		args = append(args, "--since", strconv.FormatInt(since, 10))
	}
	res, err := a.runCLI(ctx, args, nil)
	if err != nil {
		return err
	}
	if res.code != 0 {
		slog.Debug(fmt.Sprintf("Buzz: poll of channel %s failed — %s", channelID, cliErrorMessage(res.stderr, res.code, "")))
		return nil
	}
	return a.handleEvents(ctx, channelID, st, parseJSONList(res.stdout))
}

// Handles a batch of events and persists the cursor once every returned input is durable, only when it moved
func (a *Adapter) handleEvents(ctx context.Context, channelID string, st *channelState, events []map[string]any) error {
	a.mu.Lock()
	before := st.mark()
	a.mu.Unlock()
	for _, event := range events {
		if err := a.handleEvent(ctx, channelID, st, event); err != nil {
			return err
		}
	}
	a.mu.Lock()
	st.trim()
	moved := st.mark() != before
	a.mu.Unlock()
	if !moved {
		return nil
	}
	return gateway.WhenReturnedInputsDurable(ctx, a, a.cursorWriter(ctx))
}

// Snapshot of every watched channel cursor, caller holding the state lock
func (a *Adapter) cursorPayload() cursorFile {
	channels := map[string]savedCursor{}
	for _, id := range a.state.keys {
		st := a.state.vals[id]
		chatType := st.chatType
		if chatType == "" {
			chatType = "group"
		}
		channels[id] = savedCursor{ChatType: chatType, LastTS: st.lastTS, Seen: append([]string{}, st.seen.keys...)}
	}
	return cursorFile{Identity: a.selfPubkey, Relay: a.relayURL, Channels: channels}
}

// Persists every watched channel cursor now
func (a *Adapter) saveCursors(ctx context.Context) {
	a.mu.Lock()
	payload := a.cursorPayload()
	a.mu.Unlock()
	writeCursors(cursorPath(ctx), payload)
}

// Write of the cursor as it stands now, skipped when a newer snapshot already landed so the file never moves back
func (a *Adapter) cursorWriter(ctx context.Context) func(context.Context) error {
	a.mu.Lock()
	payload := a.cursorPayload()
	a.mu.Unlock()
	path := cursorPath(ctx)
	a.cursorWriteMu.Lock()
	a.cursorSnapshots++
	generation := a.cursorSnapshots
	a.cursorWriteMu.Unlock()
	return func(context.Context) error {
		a.cursorWriteMu.Lock()
		defer a.cursorWriteMu.Unlock()
		if generation > a.cursorWritten {
			a.cursorWritten = generation
			writeCursors(path, payload)
		}
		return nil
	}
}

// Records author and clipped content of an event for later reply parent lookup, caller holding the state lock
func (a *Adapter) rememberEvent(st *channelState, event map[string]any) {
	eventID := ""
	if truthy(event["id"]) {
		eventID = textOf(event["id"])
	}
	if eventID == "" {
		return
	}
	content, _ := event["content"].(string)
	author := ""
	if truthy(event["pubkey"]) {
		author = strings.ToLower(textOf(event["pubkey"]))
	}
	st.eventMeta.put(eventID, eventMeta{author: author, snippet: clipRunes(content, eventMetaContentCap)})
	st.eventMeta.trim(seenCap)
}

// Records a sent message as a reply parent candidate even when its echo never arrives
func (a *Adapter) rememberEventMeta(channelID, eventID, pubkey, content string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st, ok := a.state.get(channelID); ok {
		a.rememberEvent(st, map[string]any{"id": eventID, "pubkey": pubkey, "content": content})
	}
}

// Marks an event id seen so its echo never dispatches
func (a *Adapter) markSeen(channelID, eventID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st, ok := a.state.get(channelID); ok {
		st.seen.set(eventID, struct{}{})
		st.trim()
	}
}

// Caches the thread root of an inbound message so a reply joins that thread, caller not holding the state lock
func (a *Adapter) recordThreadRoot(eventID string, event map[string]any) {
	if eventID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.threadRoots.put(eventID, extractThreadRoot(event))
	a.threadRoots.trim(threadRootCacheSize)
}

// Thread root of the anchor when the trigger sat inside a thread, else the anchor itself
func (a *Adapter) resolveReplyAnchor(anchor string) string {
	if anchor == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if root, ok := a.threadRoots.get(anchor); ok && root != "" {
		return root
	}
	return anchor
}

// Whether channel metadata marks a relay materialized DM, a name of DM and an empty description, caller holding the lock
func (a *Adapter) mayReclassifyAsDM(channelID string) bool {
	meta, ok := a.channelMeta[channelID]
	if !ok {
		return false
	}
	return strip(textOf(meta["name"])) == "DM" && strip(textOf(meta["description"])) == ""
}

// Whether a chat event from someone else is structurally addressed to us without a typed mention, caller holding the lock
func (a *Adapter) isDirectMessageEvent(channelID string, event map[string]any) bool {
	pubkey := strings.ToLower(textOf(event["pubkey"]))
	content, isStr := event["content"].(string)
	kind, _ := intOf(event["kind"])
	return a.selfPubkey != "" && a.mayReclassifyAsDM(channelID) && kind == chatKind && pubkey != "" && pubkey != a.selfPubkey &&
		pTagged(event, a.selfPubkey) && isStr && !isMentioned(a.identityLocked(), content)
}

// Identity read while the state lock is already held
func (a *Adapter) identityLocked() identity { return identity{a.selfPubkey, a.selfNpub, a.displayName} }

// Latches a group conversation to DM once a direct message is seen there, caller holding the lock
func (a *Adapter) maybeLatchDM(channelID string, st *channelState, event map[string]any) {
	if st.chatType == "dm" || !a.isDirectMessageEvent(channelID, event) {
		return
	}
	st.chatType = "dm"
	if _, ok := a.channelNames[channelID]; !ok {
		a.channelNames[channelID] = "DM"
	}
	slog.Info(fmt.Sprintf("Buzz: conversation %s reclassified as DM (message p-tagged to self)", channelID))
}

// Whether text explicitly addresses the agent by hex pubkey, npub or at sign display name
func isMentioned(id identity, content string) bool {
	lowered := strings.ToLower(content)
	if id.pubkey != "" && hex64RE.MatchString(id.pubkey) &&
		boundedContains(lowered, id.pubkey, isHexDigit, func(r rune, end bool) bool { return end || !isHexDigit(r) }) {
		return true
	}
	if id.npub != "" && boundedContains(lowered, strings.ToLower(id.npub), isASCIIAlnum,
		func(r rune, end bool) bool { return end || !isASCIIAlnum(r) }) {
		return true
	}
	return id.displayName != "" && boundedContains(lowered, "@"+strings.ToLower(id.displayName),
		func(r rune) bool { return isWord(r) || r == '@' }, mentionTerminator)
}

// Whether a group event addresses the agent in text or by p tag
func isAddressed(id identity, event map[string]any) bool {
	content, ok := event["content"].(string)
	return ok && (isMentioned(id, content) || (id.pubkey != "" && pTagged(event, id.pubkey)))
}

// Text with a leading mention of the agent removed so a command right after it is recognized
func stripMention(id identity, content string) string {
	text := strip(content)
	matched := 0
	if id.displayName != "" {
		if n, ok := foldPrefix(text, "@"+id.displayName); ok {
			if r, has := runeAt(text, n); mentionTerminator(r, !has) {
				matched = n
			}
		}
	}
	tryKey := func(key string, bad func(rune) bool) {
		if matched != 0 || key == "" {
			return
		}
		for _, prefix := range []string{"@" + key, key} {
			if n, ok := foldPrefix(text, prefix); ok {
				if r, has := runeAt(text, n); !has || !bad(r) {
					matched = n
					return
				}
			}
		}
	}
	tryKey(id.npub, isASCIIAlnum)
	tryKey(id.pubkey, isHexDigit)
	if matched == 0 {
		return text
	}
	rest := strings.TrimLeftFunc(text[matched:], func(r rune) bool { return isSpace(r) || r == ':' || r == ',' })
	return strip(rest)
}

// Display name of a pubkey cached even when empty, an npub prefix when the profile has none
func (a *Adapter) resolveUserName(ctx context.Context, pubkey string) (string, error) {
	a.mu.Lock()
	cached, ok := a.userNames[pubkey]
	a.mu.Unlock()
	if ok {
		return cached, nil
	}
	res, err := a.runCLI(ctx, []string{"users", "get", "--pubkey", pubkey}, nil)
	if err != nil {
		return "", err
	}
	name := ""
	if res.code == 0 {
		if profiles := parseJSONList(res.stdout); len(profiles) > 0 {
			name = strip(textOf(profiles[0]["display_name"]))
		}
	}
	if name == "" {
		fallback := hexToNpub(pubkey)
		if fallback == "" {
			fallback = pubkey
		}
		name = clipRunes(fallback, 16)
	}
	a.mu.Lock()
	a.userNames[pubkey] = name
	a.mu.Unlock()
	return name, nil
}

// One structurally valid NIP-94 attachment declared by an imeta tag
type imetaAttachment struct {
	url, sha256, filename, mimeType string
	size                            int64
}

// Accepted imeta attachments of an event and the count rejected as malformed or over limits
func parseImetaAttachments(event map[string]any) ([]imetaAttachment, int) {
	var attachments []imetaAttachment
	rejected := 0
	var totalDeclared int64
	for _, tag := range eventTags(event) {
		if tag[0] != "imeta" {
			continue
		}
		if len(attachments) >= maxInboundAttachments {
			rejected++
			continue
		}
		fields := map[string]string{}
		for _, f := range tag[1:] {
			s, ok := f.(string)
			if !ok {
				continue
			}
			key, value, found := strings.Cut(s, " ")
			if _, seen := fields[key]; found && !seen {
				fields[key] = strip(value)
			}
		}
		rawURL, digest := fields["url"], strings.ToLower(fields["x"])
		size, err := strconv.ParseInt(fields["size"], 10, 64)
		if err != nil {
			rejected++
			continue
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			rejected++
			continue
		}
		if _, _, valid := urlPort(u); !valid {
			rejected++
			continue
		}
		hasUser := u.User != nil && u.User.Username() != ""
		if u.User != nil {
			if pw, set := u.User.Password(); set && pw != "" {
				hasUser = true
			}
		}
		if u.Scheme != "https" || u.Hostname() == "" || hasUser || u.Fragment != "" || !hex64RE.MatchString(digest) ||
			size <= 0 || size > maxInboundAttachmentBytes || totalDeclared+size > maxInboundAttachmentBytes {
			rejected++
			continue
		}
		totalDeclared += size
		attachments = append(attachments, imetaAttachment{url: rawURL, sha256: digest, size: size,
			filename: safeAttachmentFilename(fields["filename"]), mimeType: clipRunes(fields["m"], maxAttachmentMimeTypeChars)})
	}
	return attachments, rejected
}

// Fixed width note for malformed or excess attachment metadata
func attachmentRejectionNote(rejected int) string {
	count := strconv.Itoa(rejected)
	if rejected > 999 {
		count = "999+"
	}
	return "[" + count + " Buzz attachment(s) rejected as malformed or over limits.]"
}

// Downloads one attachment from a trusted origin, checks its declared size and SHA-256 and caches it
func (a *Adapter) downloadAttachment(ctx context.Context, meta imetaAttachment) *gateway.CachedMedia {
	u, err := url.Parse(meta.url)
	o := origin{}
	if err == nil {
		port, has, _ := urlPort(u)
		if !has || port == 0 {
			port = 443
		}
		o = origin{strings.TrimRight(strings.ToLower(u.Hostname()), "."), port}
	}
	if err != nil || u.Scheme != "https" || !a.attachmentOrigins[o] {
		host := o.host
		if host == "" {
			host = "<missing>"
		}
		slog.Warn(fmt.Sprintf("Buzz: refusing attachment from untrusted origin %s:%d", host, o.port))
		return nil
	}
	dctx, cancel := context.WithTimeout(ctx, attachmentDownloadTimeout)
	defer cancel()
	data, err := fetchAttachment(dctx, meta)
	if err != nil {
		slog.Warn(fmt.Sprintf("Buzz: attachment download failed: %v", err))
		return nil
	}
	if data == nil {
		return nil
	}
	if int64(len(data)) != meta.size {
		slog.Warn("Buzz: attachment size does not match imeta")
		return nil
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != meta.sha256 {
		slog.Warn("Buzz: attachment SHA-256 does not match imeta")
		return nil
	}
	cached, err := gateway.CacheMediaBytesAsync(ctx, data, meta.filename, meta.mimeType, "")
	if err != nil {
		slog.Warn(fmt.Sprintf("Buzz: attachment cache write failed: %v", err))
		return nil
	}
	return cached
}

// Attachment body without redirects or transfer compression, nil without error when the relay answer is refused
func fetchAttachment(ctx context.Context, meta imetaAttachment) ([]byte, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	client := &http.Client{Transport: transport, Timeout: attachmentDownloadTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, meta.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn(fmt.Sprintf("Buzz: attachment download returned HTTP %d", resp.StatusCode))
		return nil, nil
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		declared, err := strconv.ParseInt(strip(cl), 10, 64)
		if err != nil {
			return nil, nil
		}
		if declared != meta.size {
			slog.Warn("Buzz: attachment Content-Length does not match imeta size")
			return nil, nil
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, meta.size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > meta.size {
		slog.Warn("Buzz: attachment exceeded its declared size")
		return nil, nil
	}
	return data, nil
}

// Whether the gateway explicitly authorizes the sender, the only answer that may spend credentials on their URLs
func (a *Adapter) explicitlyAuthorized(userID, chatType, chatID string) bool {
	verdict := a.IsSenderAuthorized(userID, &chatType, &chatID, gateway.AuthorizationOpts{})
	return verdict != nil && *verdict
}

// De-dupes, gates and dispatches one relay event of a watched conversation
func (a *Adapter) handleEvent(ctx context.Context, channelID string, st *channelState, event map[string]any) error {
	eventID := ""
	if truthy(event["id"]) {
		eventID = textOf(event["id"])
	}
	createdAt := intOr0(event["created_at"])
	a.mu.Lock()
	if eventID == "" || st.seen.has(eventID) {
		a.mu.Unlock()
		return nil
	}
	st.seen.set(eventID, struct{}{})
	if createdAt > st.lastTS {
		st.lastTS = createdAt
	}
	a.mu.Unlock()
	if kind := intOr0(event["kind"]); !dispatchKinds[kind] {
		return nil
	}
	pubkey := ""
	if truthy(event["pubkey"]) {
		pubkey = strings.ToLower(textOf(event["pubkey"]))
	}
	content, isStr := event["content"].(string)
	attachments, rejected := parseImetaAttachments(event)
	if pubkey == "" || !isStr || !(strip(content) != "" || len(attachments) > 0 || rejected > 0) {
		return nil
	}
	a.mu.Lock()
	a.rememberEvent(st, event)
	id := a.identityLocked()
	if pubkey == id.pubkey {
		a.mu.Unlock()
		return nil
	}
	a.maybeLatchDM(channelID, st, event)
	isDM := st.chatType == "dm"
	replyParent := eventReplyParentID(event)
	var meta *eventMeta
	if replyParent != "" {
		if m, ok := st.eventMeta.get(replyParent); ok {
			meta = &m
		}
	}
	a.mu.Unlock()
	replyToOwn := meta != nil && meta.author == id.pubkey
	if !isDM && a.requireMention && !isAddressed(id, event) && !replyToOwn {
		return nil
	}
	if len(a.allowedPubkeys) > 0 && !a.allowedPubkeys[pubkey] {
		if a.reactionOnly[pubkey] && pTagged(event, id.pubkey) && isMentioned(id, content) {
			if _, err := a.sendReaction(ctx, channelID, eventID, "👀"); err != nil {
				return err
			}
		}
		slog.Debug(fmt.Sprintf("Buzz: ignoring message from unauthorized pubkey %s…", clipRunes(pubkey, 8)))
		return nil
	}
	dispatchText := stripMention(id, content)
	threadID := extractThreadRoot(event)
	a.recordThreadRoot(eventID, event)
	chatType := "group"
	if isDM {
		chatType = "dm"
	}
	fetchAllowed := len(attachments) > 0 && a.explicitlyAuthorized(pubkey, chatType, channelID)
	var cached []*gateway.CachedMedia
	if fetchAllowed {
		for _, m := range attachments {
			if c := a.downloadAttachment(ctx, m); c != nil {
				cached = append(cached, c)
			}
		}
	}
	if rejected > 0 {
		dispatchText = strip(dispatchText + "\n" + attachmentRejectionNote(rejected))
	}
	if failed := len(attachments) - len(cached); fetchAllowed && failed > 0 {
		dispatchText = strip(fmt.Sprintf("%s\n[%d Buzz attachment(s) could not be downloaded or failed integrity checks.]", dispatchText, failed))
	}
	messageType := gateway.MessageTypeText
	if len(cached) > 0 {
		kinds := map[string]bool{}
		for _, c := range cached {
			kinds[c.Kind] = true
		}
		messageType = gateway.MessageTypeDocument
		if len(kinds) == 1 {
			if mt, ok := attachmentKindTypes[cached[0].Kind]; ok {
				messageType = mt
			}
		}
	}
	userName, err := a.resolveUserName(ctx, pubkey)
	if err != nil {
		return err
	}
	return a.dispatchMessage(ctx, inbound{text: dispatchText, chatID: channelID, chatType: chatType, userID: pubkey,
		userName: userName, messageID: eventID, createdAt: createdAt, threadID: threadID, replyParent: replyParent,
		replyMeta: meta, replyToOwn: replyToOwn, media: cached, messageType: messageType, raw: event})
}

// One gated inbound message ready for the gateway
type inbound struct {
	text, chatID, chatType, userID, userName, messageID string
	createdAt                                           int64
	threadID, replyParent                               string
	replyMeta                                           *eventMeta
	replyToOwn                                          bool
	media                                               []*gateway.CachedMedia
	messageType                                         gateway.MessageType
	raw                                                 map[string]any
}

// Caches same relay media referenced in the text through the CLI, only for an explicitly authorized sender
func (a *Adapter) localizeInboundMedia(ctx context.Context, msg inbound) (string, []*gateway.CachedMedia, gateway.MessageType, error) {
	urls, replacements := findRelayMediaRefs(msg.text, a.relayURL)
	if len(urls) == 0 {
		return msg.text, nil, gateway.MessageTypeText, nil
	}
	if !a.explicitlyAuthorized(msg.userID, msg.chatType, msg.chatID) {
		who := msg.userID
		if who == "" {
			who = "?"
		}
		slog.Warn(fmt.Sprintf("Buzz: not localizing %d media reference(s) in message %s — sender %s… is not explicitly authorized",
			len(urls), clipRunes(msg.messageID, 12), clipRunes(who, 8)))
		return msg.text, nil, gateway.MessageTypeText, nil
	}
	cleaned := replaceMediaRefs(msg.text, replacements)
	var localized []*gateway.CachedMedia
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		pm := mediaPathRE.FindStringSubmatch(u.EscapedPath())
		if pm == nil {
			continue
		}
		ext := strings.ToLower(pm[2])
		if ext == "" {
			ext = ".bin"
		}
		label := clipRunes(pm[1], 12) + ext
		cached, err := a.localizeOne(ctx, raw, label)
		if ctx.Err() != nil {
			return "", nil, "", ctx.Err()
		}
		if err != nil {
			slog.Warn(fmt.Sprintf("Buzz: failed to localize inbound media %s (%v)", label, err))
			continue
		}
		if cached == nil {
			slog.Warn("Buzz: rejected invalid inbound media " + label)
			continue
		}
		localized = append(localized, cached)
	}
	if len(localized) > 0 {
		slog.Info(fmt.Sprintf("Buzz: localized %d inbound media attachment(s) for message %s", len(localized), clipRunes(msg.messageID, 12)))
	}
	messageType := gateway.MessageTypeText
	if len(localized) > 0 {
		messageType = gateway.MessageTypeDocument
		kinds := map[string]bool{}
		for _, c := range localized {
			kinds[c.Kind] = true
		}
		for _, p := range mediaKindPriority {
			if kinds[p.kind] {
				messageType = p.mt
				break
			}
		}
	}
	if cleaned == "" {
		cleaned = "(Buzz media attachment unavailable)"
		if len(localized) > 0 {
			cleaned = "(attachment)"
		}
	}
	return cleaned, localized, messageType, nil
}

// Fetches one relay media object with the authenticated CLI into a scratch file and caches it
func (a *Adapter) localizeOne(ctx context.Context, rawURL, label string) (*gateway.CachedMedia, error) {
	dir, err := os.MkdirTemp("", "icebath-buzz-media-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	downloadPath := filepath.Join(dir, "buzz_"+label)
	res, err := a.runCLI(ctx, []string{"media", "get", "-o", downloadPath, rawURL}, nil)
	if err != nil {
		return nil, err
	}
	if res.code != 0 || !isFile(downloadPath) {
		return nil, fmt.Errorf("exit %d", res.code)
	}
	info, err := os.Stat(downloadPath)
	if err != nil {
		return nil, err
	}
	if err := mediacache.ValidateInboundMediaSize(ctx, home.Dir(ctx), info.Size(), "Buzz media", nil); err != nil {
		return nil, err
	}
	mimeType := "application/octet-stream"
	if guessed := mime.TypeByExtension(filepath.Ext(downloadPath)); guessed != "" {
		if mt, _, err := mime.ParseMediaType(guessed); err == nil {
			mimeType = mt
		}
	}
	data, err := os.ReadFile(downloadPath)
	if err != nil {
		return nil, err
	}
	return gateway.CacheMediaBytesAsync(ctx, data, filepath.Base(downloadPath), mimeType, "")
}

// Builds the message event, hands it to the gateway and marks the message seen with an eyes reaction
func (a *Adapter) dispatchMessage(ctx context.Context, msg inbound) error {
	if a.MessageHandler() == nil {
		return nil
	}
	text, localized, localizedType, err := a.localizeInboundMedia(ctx, msg)
	if err != nil {
		return err
	}
	var mediaURLs, mediaTypes []string
	for _, c := range msg.media {
		mediaURLs = append(mediaURLs, c.Path)
		mediaTypes = append(mediaTypes, c.MediaType)
	}
	for _, c := range localized {
		dup := false
		for _, p := range mediaURLs {
			if p == c.Path {
				dup = true
				break
			}
		}
		if !dup {
			mediaURLs = append(mediaURLs, c.Path)
			mediaTypes = append(mediaTypes, c.MediaType)
		}
	}
	messageType := msg.messageType
	if messageType == gateway.MessageTypeText {
		messageType = localizedType
	} else if len(localized) > 0 && localizedType != messageType && localizedType != gateway.MessageTypeText {
		messageType = gateway.MessageTypeDocument
	}
	a.mu.Lock()
	chatName, ok := a.channelNames[msg.chatID]
	a.mu.Unlock()
	if !ok {
		chatName = msg.chatID
	}
	userID, userName, messageID, threadID := msg.userID, msg.userName, msg.messageID, msg.threadID
	source := a.BuildSource(ctx, msg.chatID, gateway.SourceFields{ChatName: &chatName, ChatType: msg.chatType, UserID: &userID,
		UserName: &userName, ThreadID: &threadID, MessageID: &messageID})
	event := gateway.NewMessageEvent(text)
	event.MessageType, event.Source, event.RawMessage, event.MessageID = messageType, source, msg.raw, &messageID
	event.MediaURLs, event.MediaTypes = mediaURLs, mediaTypes
	for range mediaURLs {
		inlined := false
		event.MediaTextInlined = append(event.MediaTextInlined, &inlined)
	}
	if msg.createdAt != 0 {
		event.Timestamp = time.Unix(msg.createdAt, 0)
	}
	if msg.replyParent != "" {
		parent := msg.replyParent
		event.ReplyToMessageID = &parent
	}
	if msg.replyMeta != nil {
		snippet, author := msg.replyMeta.snippet, msg.replyMeta.author
		event.ReplyToText, event.ReplyToAuthorID = &snippet, &author
	}
	event.ReplyToIsOwnMessage = msg.replyToOwn
	if err := a.HandleMessage(ctx, event); err != nil {
		return err
	}
	if _, err := a.sendReaction(ctx, msg.chatID, messageID, "👀"); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Debug(fmt.Sprintf("Buzz: reaction failed for message %s: %v", clipRunes(messageID, 12), err))
	}
	return nil
}
