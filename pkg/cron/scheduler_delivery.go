package cron

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/messagetools"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

const liveSendConfirmTimeout = 60 * time.Second

// Bot Chat mailbox doors, the seams tests replace the way pytest patches tools.bot_live_delivery
var (
	readDeliveryResult     = gateway.ReadDeliveryResult
	findCanonicalLiveOwner = gateway.FindCanonicalLiveOwner
	deliverToLiveOwner     = gateway.DeliverToLiveOwner
)

var (
	videoExts = map[string]bool{".mp4": true, ".mov": true, ".avi": true, ".mkv": true, ".webm": true, ".3gp": true}
	imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true}
)

// Gateway event loop a live send is scheduled on, Schedule is false once the loop can take no work
type cronLoop interface {
	IsRunning() bool
	Schedule(fn func()) bool
}

// Adapter capabilities a cron delivery probes for, the Go form of the Python getattr checks
type (
	handoffThreadCreator interface {
		CreateHandoffThread(ctx context.Context, chatID, name string) (any, error)
	}
	chatInfoProber interface {
		GetChatInfo(ctx context.Context, chatID string) (any, error)
	}
	inchannelPlatformChecker interface {
		SupportsInchannelContinuableForPlatform(platform string) (bool, error)
	}
	inchannelChecker interface {
		SupportsInchannelContinuable() bool
	}
	cronSessionStoreHolder interface {
		CronSessionStore() cronSessionStore
	}
	cronSessionStore interface {
		GetOrCreateSession(ctx context.Context, src *gateway.SessionSource) (sessionID string, err error)
	}
	voiceSender interface {
		SendVoice(ctx context.Context, chatID, audioPath string, metadata map[string]any, isVoice bool) (any, error)
	}
	videoSender interface {
		SendVideo(ctx context.Context, chatID, videoPath string, metadata map[string]any) (any, error)
	}
	imageFileSender interface {
		SendImageFile(ctx context.Context, chatID, imagePath string, metadata map[string]any) (any, error)
	}
	documentSender interface {
		SendDocument(ctx context.Context, chatID, filePath string, metadata map[string]any) (any, error)
	}
	namedAdapter        interface{ Name() string }
	platformAdapter     interface{ Platform() gateway.Platform }
	ownerProfileAdapter interface{ OwnerProfile() *string }
)

// Pending result of work scheduled on the gateway loop
type cronFuture struct {
	done   chan struct{}
	value  any
	err    error
	cancel context.CancelFunc
}

// Work scheduled on a running gateway loop, nil when the loop takes no work
func safeScheduleThreadsafe(ctx context.Context, loop any, fn func(ctx context.Context) (any, error)) *cronFuture {
	l, ok := loop.(cronLoop)
	if !ok || l == nil {
		return nil
	}
	cctx, cancel := context.WithCancel(ctx)
	f := &cronFuture{done: make(chan struct{}), cancel: cancel}
	scheduled := l.Schedule(func() {
		defer close(f.done)
		defer func() {
			if r := recover(); r != nil {
				f.err = fmt.Errorf("%v", r)
			}
		}()
		f.value, f.err = fn(cctx)
	})
	if !scheduled {
		cancel()
		return nil
	}
	return f
}

// Value of the scheduled work, errFutureTimeout once the timeout passes
func (f *cronFuture) result(timeout time.Duration) (any, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-f.done:
		return f.value, f.err
	case <-timer.C:
		return nil, errFutureTimeout
	}
}

var errFutureTimeout = errors.New("")

// Attribute of a send result read from a mapping or a struct field the way getattr reads it
func resultField(v any, key string) (any, bool) {
	switch m := v.(type) {
	case nil:
		return nil, false
	case map[string]any:
		x, ok := m[key]
		return x, ok
	case *pyjson.Object:
		return m.Get(key)
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, false
	}
	camel := snakeToCamel(key)
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Tag.Get("py") == key || f.Name == camel {
			return rv.Field(i).Interface(), true
		}
	}
	return nil, false
}

func resultGet(v any, key string, def any) any {
	if x, ok := resultField(v, key); ok {
		return x
	}
	return def
}

// Go field name of a snake case attribute with the ID initialism
func snakeToCamel(key string) string {
	var b strings.Builder
	for _, part := range strings.Split(key, "_") {
		if part == "" {
			continue
		}
		if part == "id" {
			b.WriteString("ID")
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

// Python truthiness of a send result, a struct value is always true
func resultTruthy(v any) bool {
	switch v.(type) {
	case nil:
		return false
	case map[string]any, *pyjson.Object:
		return pyTruthy(v)
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		return !rv.IsNil()
	}
	return true
}

// Class name of a send result the way type(x).__name__ prints it
func resultTypeName(v any) string {
	if v == nil {
		return "NoneType"
	}
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}

// Opens a thread for a continuable cron job, empty when the adapter has no thread primitive or it fails
func (s *Store) openContinuableCronThread(ctx context.Context, job *Job, adapter any, chatID string, loop any) string {
	creator, ok := adapter.(handoffThreadCreator)
	if !ok || loop == nil {
		return ""
	}
	threadName := "Icebath — " + s.cronDisplayName(job)
	future := safeScheduleThreadsafe(ctx, loop, func(ctx context.Context) (any, error) { return creator.CreateHandoffThread(ctx, chatID, threadName) })
	if future == nil {
		return ""
	}
	v, err := future.result(30 * time.Second)
	if err != nil {
		name := "?"
		if n, ok := adapter.(namedAdapter); ok {
			name = n.Name()
		}
		if errors.Is(err, errFutureTimeout) {
			future.cancel()
		}
		s.log.Debug(fmt.Sprintf("Job '%s': create_handoff_thread failed on %s — falling back to DM-session mirror: %v", jobIDOr(job, "?"), name, err))
		return ""
	}
	if !pyTruthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Creates the session row the reply lands on and mirrors the brief into it as a user turn
func (s *Store) seedCronSession(ctx context.Context, job *Job, adapter any, platform, chatID, text string, threadID *string, chatType string, userID, userName, chatName, scopeID *string, discordKeysOnThread bool) (bool, error) {
	seeded := ""
	if holder, ok := adapter.(cronSessionStoreHolder); ok {
		if store := holder.CronSessionStore(); store != nil {
			if p, err := gateway.ParsePlatform(ctx, strings.ToLower(platform)); err == nil {
				seedChatID := chatID
				if discordKeysOnThread && p == "discord" && threadID != nil {
					seedChatID = *threadID
				}
				var scope *string
				if scopeID != nil && *scopeID != "" {
					scope = scopeID
				}
				id, err := store.GetOrCreateSession(ctx, &gateway.SessionSource{Platform: p, ChatID: seedChatID, ChatName: chatName, ChatType: chatType, UserID: userID, UserName: userName, ThreadID: threadID, ScopeID: scope})
				if err != nil {
					return false, err
				}
				seeded = id
			}
		}
	}
	return gateway.MirrorToSession(ctx, platform, chatID, s.cronMirrorMessage(job, text), gateway.MirrorOptions{SourceLabel: "cron", ThreadID: threadID, UserID: userID, Role: "user", SessionID: seeded}), nil
}

// Seeds the brief into the session an in thread reply resolves to
func (s *Store) seedCronThreadSession(ctx context.Context, job *Job, adapter any, platform, chatID, threadID, mirrorText string, chatName *string, isDM bool, scopeID *string) {
	text := pystr.Strip(mirrorText)
	if text == "" {
		return
	}
	chatType := "thread"
	if isDM {
		chatType = "dm"
	} else if v, ok := threadReplyChatType[strings.ToLower(platform)]; ok {
		chatType = v
	}
	ok, err := s.seedCronSession(ctx, job, adapter, platform, chatID, text, &threadID, chatType, strPtr("system:cron"), strPtr("Cron"), chatName, scopeID, true)
	switch {
	case err != nil:
		s.log.Warn(fmt.Sprintf("Job '%s': seeding cron thread session failed for %s:%s:%s: %v", jobIDOr(job, "?"), platform, chatID, threadID, err))
	case ok:
		s.log.Info(fmt.Sprintf("Job '%s': opened continuable thread %s on %s:%s and seeded the brief", jobIDOr(job, "?"), threadID, platform, chatID))
	default:
		s.log.Warn(fmt.Sprintf("Job '%s': thread seed did NOT land on %s:%s thread=%s — an in-thread reply will not see this brief", jobIDOr(job, "?"), platform, chatID, threadID))
	}
}

// Seeds the flat session an in channel delivery continues in, true when the seed landed
func (s *Store) seedCronChannelSession(ctx context.Context, job *Job, adapter any, platform, chatID, mirrorText string, isDM bool, userID, chatName, scopeID *string) bool {
	text := pystr.Strip(mirrorText)
	if text == "" {
		return false
	}
	chatType := "group"
	if isDM {
		chatType = "dm"
	}
	var uid *string
	if userID != nil && *userID != "" {
		uid = userID
	}
	ok, err := s.seedCronSession(ctx, job, adapter, platform, chatID, text, nil, chatType, uid, nil, chatName, scopeID, false)
	if err != nil {
		s.log.Warn(fmt.Sprintf("Job '%s': seeding in_channel session failed for %s:%s: %v", jobIDOr(job, "?"), platform, chatID, err))
		return false
	}
	if ok {
		s.log.Info(fmt.Sprintf("Job '%s': seeded flat in_channel session on %s:%s (chat_type=%s)", jobIDOr(job, "?"), platform, chatID, chatType))
	}
	return ok
}

// Positive cron.standalone_send_timeout_seconds, 60 otherwise
func (s *Store) getStandaloneSendTimeout() int {
	cfg, err := s.loadConfig()
	if err != nil {
		return 60
	}
	cronCfg, ok := cfg["cron"].(map[string]any)
	if !ok {
		return 60
	}
	raw, present := cronCfg["standalone_send_timeout_seconds"]
	if !present {
		return 60
	}
	v, err := pyint.Of(raw)
	if err != nil || !v.IsInt64() || v.Int64() <= 0 {
		return 60
	}
	return int(v.Int64())
}

// Bot Chat turn text framing the redacted job output
func (s *Store) botChatMessage(job *Job, content string) string {
	return s.botChatMessageNamed(jobNameOr(job, jobIDOr(job, "?")), content)
}

// Bot Chat turn text for a job name and its output, both redacted
func (s *Store) botChatMessageNamed(name, content string) string {
	content = s.redactCronPayload(content, "bot-chat payload")
	jobName := s.redactCronPayload(name, "job name")
	return fmt.Sprintf("[Cronjob \"%s\" output — scheduled job, not the user. Review it, act on anything that needs action, and summarize for the chat.]\n\n%s", jobName, content)
}

func (job *Job) botChatReceipts() *pyjson.Object {
	if job.run.botChatDeliveryReceipts == nil {
		job.run.botChatDeliveryReceipts = pyjson.NewObject()
	}
	return job.run.botChatDeliveryReceipts
}

func receiptStatus(job *Job, target string) (string, bool) {
	if job.run.botChatDeliveryReceipts == nil {
		return "", false
	}
	v, ok := job.run.botChatDeliveryReceipts.Get(target)
	if !ok {
		return "", false
	}
	st, _ := v.(*pyjson.Object).Get("status")
	return pyStrOrEmpty(st), true
}

func newReceipt(status, deliveryID string) *pyjson.Object {
	r := pyjson.NewObject()
	r.Set("status", status)
	r.Set("delivery_id", deliveryID)
	return r
}

// Delivers the output into a profile Bot Chat through its live owner, an error string unless settled
func (s *Store) deliverToBotChat(ctx context.Context, job *Job, content, profile string, forFailure bool) *string {
	jobID := jobIDOr(job, "?")
	label := profile
	if label == "" {
		label = "(own)"
	}
	message := s.botChatMessage(job, content)
	out, err := s.deliverToBotChatInner(ctx, job, jobID, label, content, profile, message, forFailure)
	if err != nil {
		return strPtr(fmt.Sprintf("bot-chat delivery to profile '%s' unverified: %v", label, err))
	}
	return out
}

func (s *Store) deliverToBotChatInner(ctx context.Context, job *Job, jobID, label, content, profile, message string, forFailure bool) (out *string, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("%v", r)
		}
	}()
	sourceHome, err := pypath.Resolve(s.home)
	if err != nil {
		return nil, err
	}
	home := sourceHome
	if profile != "" {
		dir, err := profiles.GetProfileDir(profile)
		if err != nil {
			return nil, err
		}
		if home, err = pypath.Resolve(dir); err != nil {
			return nil, err
		}
	}
	suppress := false
	if forFailure {
		userCfg, err := config.LoadUserEffective(filepath.Join(home, "config.yaml"), nil)
		if err != nil {
			return nil, err
		}
		suppress = !config.WarningNotificationsEnabled(ctx, BotChatPolicyPlatform, userCfg)
	}
	runID := ""
	if job.ExecutionID != nil && *job.ExecutionID != "" {
		runID = *job.ExecutionID
	} else {
		if job.run.botChatRunID == "" {
			job.run.botChatRunID = uuid4Hex()
		}
		runID = job.run.botChatRunID
	}
	keyJSON, err := pyjson.Dumps([]any{sourceHome, jobID, runID, home}, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(keyJSON))
	key := hex.EncodeToString(sum[:])
	receipt, err := readDeliveryResult(ctx, home, key)
	if err != nil {
		return nil, err
	}
	target := "bot-chat:" + label
	if receipt == nil && suppress {
		job.botChatReceipts().Set(target, newReceipt("suppressed", key))
		job.run.notificationAllTargetsSuppressed = true
		return nil, nil
	}
	if receipt == nil {
		owner, err := findCanonicalLiveOwner(ctx, home)
		var pyErr *gateway.PyError
		if errors.As(err, &pyErr) && pyErr.Kind == "ValueError" {
			if err := s.botChatLegacyDefer(key, job, content, home, forFailure); err != nil {
				return nil, err
			}
			job.botChatReceipts().Set(target, newReceipt("queued", key))
			return strPtr(fmt.Sprintf("%s queued (receipt %s): target profile not running; admitted once it is", target, key)), nil
		}
		if err != nil {
			return nil, err
		}
		if owner == nil {
			return strPtr(fmt.Sprintf("bot-chat delivery to profile '%s' unverified: no canonical Bot Chat", label)), nil
		}
		category := ""
		if forFailure {
			category = "diagnostic"
		}
		if receipt, err = deliverToLiveOwner(ctx, home, owner, message, key, category); err != nil {
			return nil, err
		}
	}
	wantCategory := "result"
	if forFailure {
		wantCategory = "diagnostic"
	}
	gotCategory, ok := receipt["notification_category"]
	if !ok {
		gotCategory = "result"
	}
	gotMessage, ok := receipt["message"]
	if !ok {
		return nil, errors.New("'message'")
	}
	if gotMessage != message || gotCategory != wantCategory {
		return nil, errors.New("delivery id already belongs to a different payload")
	}
	statusVal, ok := receipt["status"]
	if !ok {
		return nil, errors.New("'status'")
	}
	status := pyrepr.Str(statusVal)
	job.botChatReceipts().Set(target, newReceipt(status, key))
	s.log.Info(fmt.Sprintf("Job '%s': Bot Chat %s receipt=%s status=%s", jobID, label, key, status))
	if status == "settled" {
		return nil, nil
	}
	detail := "completion unverified; do not resend"
	if status != "queued" && status != "claimed" {
		detail = "not completed"
		if pyTruthy(receipt["error"]) {
			detail = pyrepr.Str(receipt["error"])
		} else if pyTruthy(receipt["reason"]) {
			detail = pyrepr.Str(receipt["reason"])
		}
	}
	return strPtr(fmt.Sprintf("%s %s (receipt %s): %s", target, status, key, detail)), nil
}

func noteTargetError(s *Store, jobID, msg string, errs *[]string) {
	s.log.Warn(fmt.Sprintf("Job '%s': %s", jobID, msg))
	*errs = append(*errs, msg)
}

// Sends each attachment through the adapter media method its extension selects, every failure collected
func (s *Store) sendMediaViaAdapter(ctx context.Context, adapter any, chatID string, media []gateway.MediaRef, metadata map[string]any, loop any, job *Job, platform *gateway.Platform) []string {
	jobRef := jobIDOr(job, "?")
	var errs []string
	requested := append([]gateway.MediaRef(nil), media...)
	media, filterErr := gateway.FilterMediaDeliveryPaths(ctx, media)
	if filterErr != nil {
		media = nil
	}
	kept := map[string]bool{}
	for _, m := range media {
		kept[m.Path] = true
	}
	for _, r := range requested {
		validated, err := gateway.ValidateMediaDeliveryPath(ctx, r.Path, "")
		if err != nil || validated == "" || !kept[validated] {
			errs = append(errs, "attachment dropped by media path policy: "+r.Path)
		}
	}
	var route gateway.Platform
	if platform != nil {
		route = *platform
	} else if p, ok := adapter.(platformAdapter); ok {
		route = p.Platform()
	}
	for _, m := range media {
		path, isVoice := m.Path, m.IsVoice
		err := func() error {
			ext := strings.ToLower(pypath.PathSuffix(path))
			var call func(ctx context.Context) (any, error)
			switch {
			case gateway.ShouldSendMediaAsAudio(route, ext, isVoice):
				sender, ok := adapter.(voiceSender)
				if !ok {
					return attributeError(adapter, "send_voice")
				}
				call = func(ctx context.Context) (any, error) { return sender.SendVoice(ctx, chatID, path, metadata, isVoice) }
			case videoExts[ext]:
				sender, ok := adapter.(videoSender)
				if !ok {
					return attributeError(adapter, "send_video")
				}
				call = func(ctx context.Context) (any, error) { return sender.SendVideo(ctx, chatID, path, metadata) }
			case imageExts[ext]:
				sender, ok := adapter.(imageFileSender)
				if !ok {
					return attributeError(adapter, "send_image_file")
				}
				call = func(ctx context.Context) (any, error) { return sender.SendImageFile(ctx, chatID, path, metadata) }
			default:
				sender, ok := adapter.(documentSender)
				if !ok {
					return attributeError(adapter, "send_document")
				}
				call = func(ctx context.Context) (any, error) { return sender.SendDocument(ctx, chatID, path, metadata) }
			}
			future := safeScheduleThreadsafe(ctx, loop, call)
			if future == nil {
				return errLoopUnavailable
			}
			result, err := future.result(time.Duration(s.getMediaSendTimeout(ctx)) * time.Second)
			if errors.Is(err, errFutureTimeout) {
				future.cancel()
				return err
			}
			if err != nil {
				return err
			}
			raw, _ := resultField(result, "raw_response")
			rawMap, isMap := raw.(map[string]any)
			if resultTruthy(result) && pyTruthy(resultGet(result, "success", false)) && isMap && pyTruthy(rawMap["ambiguous"]) {
				job.run.mediaAckLost = true
			} else if resultTruthy(result) && !pyTruthy(resultGet(result, "success", true)) {
				noteTargetError(s, jobRef, fmt.Sprintf("media send failed for %s: %s", path, pyrepr.Str(resultGet(result, "error", "unknown"))), &errs)
			}
			return nil
		}()
		if errors.Is(err, errLoopUnavailable) {
			noteTargetError(s, jobRef, fmt.Sprintf("cannot send media %s: gateway loop unavailable", path), &errs)
			return errs
		}
		if err != nil {
			text := err.Error()
			if text == "" {
				text = pyExceptionName(err)
			}
			noteTargetError(s, jobRef, fmt.Sprintf("failed to send media %s: %s", path, text), &errs)
		}
	}
	return errs
}

var errLoopUnavailable = errors.New("gateway loop unavailable")

// Python exception class name of an error, TimeoutError for the future timeout
func pyExceptionName(err error) string {
	if errors.Is(err, errFutureTimeout) {
		return "TimeoutError"
	}
	var named interface{ ExceptionName() string }
	if errors.As(err, &named) {
		return named.ExceptionName()
	}
	return "Exception"
}

func attributeError(obj any, attr string) error {
	return &pyAttributeError{msg: fmt.Sprintf("'%s' object has no attribute '%s'", resultTypeName(obj), attr)}
}

type pyAttributeError struct{ msg string }

func (e *pyAttributeError) Error() string         { return e.msg }
func (e *pyAttributeError) ExceptionName() string { return "AttributeError" }

// True only for a success result with no delivered false, an evidence gap appended when no id or raw response
func (s *Store) confirmAdapterDelivery(result any, jobID string, unverified *[]bool) bool {
	if result == nil {
		return false
	}
	success, has := resultField(result, "success")
	if !has || !pyTruthy(success) {
		return false
	}
	if delivered, ok := resultField(result, "delivered"); ok && delivered == false {
		return false
	}
	msgID, _ := resultField(result, "message_id")
	raw, _ := resultField(result, "raw_response")
	if msgID == nil && !pyTruthy(raw) {
		s.log.Warn(fmt.Sprintf("Job '%s': live adapter reported success with no delivery evidence (no message_id, no raw_response) — treating as delivered but UNVERIFIED", jobID))
		if unverified != nil {
			*unverified = append(*unverified, true)
		}
	}
	return true
}

// True when get_chat_info reports a channel, whose DM topics route by direct_messages_topic_id
func (s *Store) isChannelDMTopic(ctx context.Context, adapter any, chatID string, loop any, jobID string) bool {
	prober, ok := adapter.(chatInfoProber)
	if !ok {
		return false
	}
	future := safeScheduleThreadsafe(ctx, loop, func(ctx context.Context) (any, error) { return prober.GetChatInfo(ctx, chatID) })
	if future == nil {
		return false
	}
	info, err := future.result(10 * time.Second)
	if err != nil {
		if errors.Is(err, errFutureTimeout) {
			future.cancel()
		}
		s.log.Debug(fmt.Sprintf("Job '%s': get_chat_info probe failed for chat=%s — defaulting to message_thread_id routing", jobID, chatID))
		return false
	}
	m, isMap := info.(map[string]any)
	isChannel := isMap && strings.ToLower(pyStrOrEmpty(m["type"])) == "channel"
	if isChannel {
		s.log.Info(fmt.Sprintf("Job '%s': chat=%s is a channel — routing via direct_messages_topic_id", jobID, chatID))
	}
	return isChannel
}

// Push notification for deliveries unless cron.delivery.notify is false
func cronDeliveryNotifyEnabled(cfg config.Tree) bool {
	cronCfg, ok := cfg["cron"].(map[string]any)
	if !ok {
		return true
	}
	delivery, ok := cronCfg["delivery"].(map[string]any)
	if !ok {
		return true
	}
	v, present := delivery["notify"]
	return !present || v != false
}

// Persists the unverified targets and the queued Bot Chat receipts when either changed
func (s *Store) recordDeliveryVerification(job *Job, unverified []string) {
	var newValue []string
	if len(unverified) > 0 {
		newValue = append([]string(nil), unverified...)
	}
	var queued *pyjson.Object
	if r := job.run.botChatDeliveryReceipts; r != nil {
		for _, target := range r.Keys {
			v, _ := r.Get(target)
			st, _ := v.(*pyjson.Object).Get("status")
			if st == "queued" || st == "claimed" {
				if queued == nil {
					queued = pyjson.NewObject()
				}
				queued.Set(target, v)
			}
		}
	}
	values := map[string]any{}
	if !stringSlicesEqual(job.LastDeliveryUnverified, newValue) {
		if newValue == nil {
			values["last_delivery_unverified"] = nil
		} else {
			values["last_delivery_unverified"] = toAnyList(newValue)
		}
	}
	var current any = job.LastDeliveryQueued
	if !pyTruthy(current) {
		current = nil
	}
	var want any
	if queued != nil {
		want = queued
	}
	if !jsonEqual(current, want) {
		values["last_delivery_queued"] = want
	}
	if len(values) == 0 {
		return
	}
	if _, ok := values["last_delivery_unverified"]; ok {
		job.LastDeliveryUnverified = newValue
	}
	if _, ok := values["last_delivery_queued"]; ok {
		job.LastDeliveryQueued = want
	}
	if _, err := s.updateJob(job.ID, values); err != nil {
		s.log.Debug(fmt.Sprintf("Job '%s': could not record delivery verification: %v", job.ID, err))
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Equality of two JSON shaped values compared by their Python json form
func jsonEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	da, errA := pyjson.Dumps(a, pyjson.SortKeys(true))
	db, errB := pyjson.Dumps(b, pyjson.SortKeys(true))
	return errA == nil && errB == nil && da == db
}

// One prepared delivery to a resolved target
type targetDelivery struct {
	job                  *Job
	platform             gateway.Platform
	platformName         string
	chatID               string
	threadID             string
	transport            *gateway.DeliveryTransport
	pconfig              *gateway.PlatformConfig
	runtimeAdapter       any
	targetAdapters       any
	config               *gateway.GatewayConfig
	loop                 any
	notifyDelivery       bool
	origin               map[string]any
	originTarget         bool
	originUserID         *string
	isDMTarget           bool
	mirrorText           string
	mirrorThisTarget     bool
	inChannelSurface     bool
	inchannelContinuable bool
	openedThreadID       string
	liveAdapterReady     bool
	liveError            *string
}

func (t *targetDelivery) isRelay() bool { return t.transport != nil && t.transport.IsRelay() }

func (t *targetDelivery) where() string {
	base := t.platformName + ":" + t.chatID
	if t.threadID != "" {
		return base + ":" + t.threadID
	}
	return base
}

func (s *Store) warnLiveLaneFailure(job *Job, msg string, isRelay bool) {
	if isRelay {
		s.log.Warn(fmt.Sprintf("Job '%s': %s", job.ID, msg))
	} else {
		s.log.Warn(fmt.Sprintf("Job '%s': %s, falling back to standalone", job.ID, msg))
	}
}

// Live transport, platform config and adapter for a target, or the reason it cannot deliver
type resolvedTransport struct {
	transport      *gateway.DeliveryTransport
	pconfig        *gateway.PlatformConfig
	runtimeAdapter any
	targetAdapters any
}

func (s *Store) resolveTargetTransport(ctx context.Context, platform gateway.Platform, platformName string, target Target, adapters any, cfg *gateway.GatewayConfig) (*resolvedTransport, string) {
	targetAdapters := adapters
	var transport *gateway.DeliveryTransport
	if shared, ok := adapters.(*SharedRouteAdapters); ok {
		adapter := shared.Get(ctx, string(platform), target.dict(), nil)
		targetAdapters = map[string]any{}
		if adapter != nil {
			targetAdapters = map[string]any{string(platform): adapter}
			pc := gateway.NewPlatformConfig()
			if own := cfg.Platforms[platform]; own != nil {
				cp := *own
				pc = &cp
			}
			pc.Enabled = true
			transport = &gateway.DeliveryTransport{Adapter: adapter, Config: pc, Platform: platform}
		}
	}
	if transport == nil {
		transport = gateway.ResolveDeliveryTransport(ctx, platform, cfg, targetAdapters)
	}
	var pconfig *gateway.PlatformConfig
	var runtimeAdapter any
	if transport != nil {
		pconfig, runtimeAdapter = transport.Config, transport.Adapter
	} else {
		if gateway.RelayFrontedPlatforms(ctx)[platformName] {
			return nil, fmt.Sprintf("platform '%s' is relay-fronted and has no live gateway transport; start the gateway (its ticker owns relay-fronted delivery and will fire the job on schedule)", platformName)
		}
		pconfig = cfg.Platforms[platform]
	}
	if transport != nil && (transport.IsRelay() || pconfig == nil) {
		if pconfig == nil {
			pconfig = gateway.NewPlatformConfig()
			pconfig.Enabled = true
		}
	} else if pconfig == nil || !pconfig.Enabled {
		return nil, fmt.Sprintf("platform '%s' not configured/enabled", platformName)
	}
	return &resolvedTransport{transport: transport, pconfig: pconfig, runtimeAdapter: runtimeAdapter, targetAdapters: targetAdapters}, ""
}

// Whether the adapter supports the in channel continuable surface for the platform
func inchannelSurfaceSupported(adapter any, platform string) bool {
	if c, ok := adapter.(inchannelPlatformChecker); ok {
		supported, err := c.SupportsInchannelContinuableForPlatform(platform)
		return err == nil && supported
	}
	if c, ok := adapter.(inchannelChecker); ok {
		return c.SupportsInchannelContinuable()
	}
	return false
}

// Route thread and the text and media metadata of a live send, Telegram channel DM topics rerouted
func (s *Store) liveRouteMetadata(ctx context.Context, t *targetDelivery) (*string, map[string]any, map[string]any) {
	job := t.job
	ambiguousTopic := t.platform == "telegram" && t.threadID != "" && gateway.LooksLikeTelegramPrivateChatID(t.chatID) && gateway.LooksLikeInt(t.threadID)
	var routeThread *string
	var routeMeta, mediaMeta map[string]any
	if ambiguousTopic && s.isChannelDMTopic(ctx, t.runtimeAdapter, t.chatID, t.loop, job.ID) {
		routeMeta = map[string]any{"direct_messages_topic_id": t.threadID, "job_id": job.ID, "notify": t.notifyDelivery}
		mediaMeta = map[string]any{"direct_messages_topic_id": t.threadID, "notify": t.notifyDelivery}
	} else {
		routeMeta = map[string]any{"job_id": job.ID, "notify": t.notifyDelivery}
		mediaMeta = map[string]any{"notify": t.notifyDelivery}
		if t.threadID != "" {
			routeThread = strPtr(t.threadID)
			routeMeta["thread_id"] = t.threadID
			mediaMeta["thread_id"] = t.threadID
		}
	}
	stampOriginDiscriminators(t, routeMeta, mediaMeta)
	return routeThread, routeMeta, mediaMeta
}

// Text send through the delivery router on the live loop, returns ok, timed out after dispatch, message id
func (s *Store) liveSendText(ctx context.Context, t *targetDelivery, text string, routeThread *string, routeMeta map[string]any, targetErrors, deliveryErrors, unverified *[]string) (bool, bool, any, error) {
	job := t.job
	router := gateway.NewDeliveryRouter(t.config, t.targetAdapters)
	routeTarget := gateway.DeliveryTarget{Platform: t.platform, ChatID: t.chatID, ThreadID: routeThread, IsExplicit: true}
	var mu sync.Mutex
	started, abandoned := false, false
	future := safeScheduleThreadsafe(ctx, t.loop, func(ctx context.Context) (any, error) {
		mu.Lock()
		if abandoned {
			mu.Unlock()
			return nil, nil
		}
		started = true
		mu.Unlock()
		return router.DeliverToPlatform(ctx, routeTarget, text, routeMeta, t.transport)
	})
	if future == nil {
		*targetErrors = append(*targetErrors, "live adapter event loop scheduling failed")
		return false, false, nil, nil
	}
	result, err := future.result(liveSendConfirmTimeout)
	if errors.Is(err, errFutureTimeout) {
		mu.Lock()
		abandoned = !started
		gaveUp := abandoned
		mu.Unlock()
		if gaveUp {
			future.cancel()
			msg := fmt.Sprintf("live adapter send to %s timed out before the coroutine was dispatched", t.where())
			s.log.Warn(fmt.Sprintf("Job '%s': %s, falling back to standalone", job.ID, msg))
			*targetErrors = append(*targetErrors, msg)
			return false, false, nil, nil
		}
		s.log.Warn(fmt.Sprintf("Job '%s': live adapter send to %s:%s timed out after 60s; already dispatched (in flight), assuming delivered (skipping standalone fallback to avoid duplicate)", job.ID, t.platformName, t.chatID))
		return true, true, nil, nil
	}
	var partial *gateway.PartialDeliveryError
	if errors.As(err, &partial) {
		raw, _ := resultField(partial.Result, "raw_response")
		rawMap, _ := raw.(map[string]any)
		delivered, total := any("?"), any("?")
		if v, ok := rawMap["delivered_chunks"]; ok {
			delivered = v
		}
		if v, ok := rawMap["total_chunks"]; ok {
			total = v
		}
		noteTargetError(s, job.ID, fmt.Sprintf("live adapter send to %s delivered %s of %s chunks, then failed: %v", t.where(), pyrepr.Str(delivered), pyrepr.Str(total), err), deliveryErrors)
		return true, false, nil, nil
	}
	if err != nil {
		t.liveError = strPtr(err.Error())
		*targetErrors = append(*targetErrors, fmt.Sprintf("live adapter send failed: %v", err))
		return false, false, nil, err
	}
	rawResp, _ := resultField(result, "raw_response")
	messageID, _ := resultField(result, "message_id")
	var gap []bool
	success := s.confirmAdapterDelivery(result, job.ID, &gap)
	if success && len(gap) > 0 {
		*unverified = append(*unverified, t.where())
	}
	if !success {
		var errVal any
		shape := "None"
		switch r := result.(type) {
		case nil:
			errVal = "no response from adapter"
		case map[string]any:
			shape = "dict"
			errVal = "unknown"
			if pyTruthy(r["error"]) {
				errVal = r["error"]
			} else if pyTruthy(r["filtered"]) {
				errVal = r["filtered"]
			}
		default:
			errVal, _ = resultField(result, "error")
			shape = resultTypeName(result)
		}
		msg := fmt.Sprintf("live adapter send to %s returned unconfirmed result (%s, error=%s)", t.where(), shape, pyrepr.Str(errVal))
		t.liveError = nil
		if pyTruthy(errVal) {
			t.liveError = strPtr(pyrepr.Str(errVal))
		}
		s.warnLiveLaneFailure(job, msg, t.isRelay())
		*targetErrors = append(*targetErrors, msg)
		return false, false, nil, nil
	}
	if rawMap, ok := rawResp.(map[string]any); ok && pyTruthy(rawResp) && t.threadID != "" && pyTruthy(rawMap["thread_fallback"]) {
		requested := any(t.threadID)
		if pyTruthy(rawMap["requested_thread_id"]) {
			requested = rawMap["requested_thread_id"]
		}
		noteTargetError(s, job.ID, fmt.Sprintf("configured thread_id %s for %s was not found; delivered without thread_id", pyrepr.Str(requested), t.where()), deliveryErrors)
	}
	return true, false, messageID, nil
}

// Media attachments through the live adapter with the relay logical platform and home discriminators
func (s *Store) liveSendMedia(ctx context.Context, t *targetDelivery, mediaMeta map[string]any, media []gateway.MediaRef, deliveryErrors, unverified *[]string) {
	routed := map[string]any{}
	for k, v := range mediaMeta {
		routed[k] = v
	}
	if t.isRelay() {
		routed["_relay_logical_platform"] = string(t.platform)
		if home := t.config.GetHomeChannel(t.platform); home != nil && home.ChatID == t.chatID {
			if home.UserID != nil && *home.UserID != "" {
				routed["user_id"] = *home.UserID
			}
			if home.ScopeID != nil && *home.ScopeID != "" {
				routed["scope_id"] = *home.ScopeID
			}
		}
	}
	if len(routed) == 0 {
		routed = nil
	}
	platform := t.platform
	mediaErrors := s.sendMediaViaAdapter(ctx, t.runtimeAdapter, t.chatID, media, routed, t.loop, t.job, &platform)
	ackLost := t.job.run.mediaAckLost
	t.job.run.mediaAckLost = false
	if ackLost && !containsString(*unverified, t.where()) {
		*unverified = append(*unverified, t.where())
	}
	for _, e := range mediaErrors {
		*deliveryErrors = append(*deliveryErrors, fmt.Sprintf("%s (target %s)", e, t.where()))
	}
}

// Seeds the opened thread or the in channel session, then the plain mirror when no seed landed
func (s *Store) seedLiveDeliverySessions(ctx context.Context, t *targetDelivery, deliveredMessageID any) {
	job := t.job
	origin := t.origin
	chatName, scopeID := optionalStrPtr(origin["chat_name"]), optionalStrPtr(origin["scope_id"])
	threadSeeded, inchannelSeeded := false, false
	if t.openedThreadID != "" {
		s.seedCronThreadSession(ctx, job, t.runtimeAdapter, t.platformName, t.chatID, t.openedThreadID, t.mirrorText, chatName, t.isDMTarget, scopeID)
		threadSeeded = true
	}
	if t.inChannelSurface && t.inchannelContinuable && !threadSeeded {
		inchannelSeeded = s.seedCronChannelSession(ctx, job, t.runtimeAdapter, t.platformName, t.chatID, t.mirrorText, t.isDMTarget, t.originUserID, chatName, scopeID)
		if !inchannelSeeded {
			s.log.Warn(fmt.Sprintf("Job '%s': in_channel seed did NOT land on %s:%s — a plain reply will not see this brief", job.ID, t.platformName, t.chatID))
		}
		if pyTruthy(deliveredMessageID) {
			s.seedCronThreadSession(ctx, job, t.runtimeAdapter, t.platformName, t.chatID, pyrepr.Str(deliveredMessageID), t.mirrorText, chatName, t.isDMTarget, scopeID)
		}
	} else if t.inChannelSurface && !t.inchannelContinuable {
		s.log.Warn(fmt.Sprintf("Job '%s': in_channel delivery to %s:%s is not a continuable target (origin=%s:%s thread=%s; not the origin conversation, and not a mirror-eligible fallback/opted-in target the seed can key) — seed skipped; the plain mirror below may still apply", job.ID, t.platformName, t.chatID, pyrepr.Str(origin["platform"]), pyrepr.Str(origin["chat_id"]), pyrepr.Str(origin["thread_id"])))
	}
	s.maybeMirrorCronDelivery(ctx, job, t.platformName, t.chatID, t.mirrorText, t.threadID, t.originUserID, t.mirrorThisTarget && !threadSeeded && !inchannelSeeded)
}

func optionalStrPtr(v any) *string {
	if v == nil {
		return nil
	}
	s := pyrepr.Str(v)
	return &s
}

// Live adapter delivery of text then media, true when the target counts as delivered
func (s *Store) deliverViaLiveAdapter(ctx context.Context, t *targetDelivery, cleaned string, media []gateway.MediaRef, targetErrors, deliveryErrors, unverified *[]string) (delivered bool) {
	job := t.job
	routeThread, routeMeta, mediaMeta := s.liveRouteMetadata(ctx, t)
	fail := func(err any) {
		msg := fmt.Sprintf("live adapter delivery to %s failed: %v", t.where(), err)
		found := false
		for _, e := range *targetErrors {
			if strings.Contains(e, msg) {
				found = true
				break
			}
		}
		if !found {
			*targetErrors = append(*targetErrors, msg)
		}
		s.warnLiveLaneFailure(job, msg, t.isRelay())
	}
	defer func() {
		if r := recover(); r != nil {
			fail(r)
			delivered = false
		}
	}()
	text := pystr.Strip(cleaned)
	ok, timedOut := true, false
	var messageID any
	if text == "" && len(media) == 0 {
		noteTargetError(s, job.ID, "live adapter send skipped (empty text and no media) for "+t.where(), targetErrors)
		ok = false
	} else if text != "" {
		var err error
		ok, timedOut, messageID, err = s.liveSendText(ctx, t, text, routeThread, routeMeta, targetErrors, deliveryErrors, unverified)
		if err != nil {
			fail(err)
			return false
		}
	}
	if ok && !timedOut && len(media) > 0 {
		s.liveSendMedia(ctx, t, mediaMeta, media, deliveryErrors, unverified)
	} else if timedOut && len(media) > 0 {
		noteTargetError(s, job.ID, fmt.Sprintf("%d media attachment(s) not delivered to %s (live adapter confirmation timed out)", len(media), t.where()), deliveryErrors)
	}
	if ok {
		threadLog, idLog := "-", "-"
		if routeThread != nil {
			threadLog = *routeThread
		}
		if messageID != nil {
			idLog = pyrepr.Str(messageID)
		}
		s.log.Info(fmt.Sprintf("Job '%s': delivered to %s:%s via live adapter thread=%s message_id=%s", job.ID, t.platformName, t.chatID, threadLog, idLog))
		delivered = true
		s.seedLiveDeliverySessions(ctx, t, messageID)
	}
	return delivered
}

// Standalone platform send without the gateway, the result mapping or the error text
func (s *Store) standaloneSend(ctx context.Context, t *targetDelivery, content string, media []gateway.MediaRef) (map[string]any, *string) {
	job := t.job
	shutdownMsg := fmt.Sprintf("delivery to %s skipped — interpreter is shutting down", t.where())
	timeout := s.getStandaloneSendTimeout()
	warned := func(msg string) (map[string]any, *string) {
		s.log.Warn(fmt.Sprintf("Job '%s': %s", job.ID, msg))
		return nil, &msg
	}
	if interpreterShuttingDown(nil) {
		return warned(shutdownMsg)
	}
	if pystr.Strip(content) == "" && len(media) == 0 {
		return warned("standalone send skipped (empty text and no media) for " + t.where())
	}
	sendCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	type outcome struct {
		result map[string]any
		err    error
	}
	ch := make(chan outcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- outcome{err: fmt.Errorf("%v", r)}
			}
		}()
		var thread *string
		if t.threadID != "" {
			thread = strPtr(t.threadID)
		}
		r, err := messagetools.SendToPlatform(sendCtx, t.platform, t.pconfig, t.chatID, content, messagetools.SendOptions{ThreadID: thread, MediaFiles: media})
		ch <- outcome{r, err}
	}()
	var o outcome
	select {
	case o = <-ch:
	case <-sendCtx.Done():
		if ctx.Err() == nil {
			msg := fmt.Sprintf("standalone send to %s timed out after %ds (the send may still be in flight)", t.where(), timeout)
			s.log.Error(fmt.Sprintf("Job '%s': %s", job.ID, msg))
			return nil, &msg
		}
		o = outcome{err: ctx.Err()}
	}
	if o.err != nil {
		if interpreterShuttingDown(o.err) {
			return warned(shutdownMsg)
		}
		msg := fmt.Sprintf("delivery to %s failed: %v", t.where(), o.err)
		s.log.Error(fmt.Sprintf("Job '%s': %s", job.ID, msg))
		return nil, &msg
	}
	return o.result, nil
}

// Records a reconnect only live failure in the delivery ledger for redelivery once the adapter reconnects
func (s *Store) queueForLiveReconnect(ctx context.Context, t *targetDelivery, content string, media []gateway.MediaRef, deliveryErrors *[]string) {
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		var liveErr any
		if t.liveError != nil {
			liveErr = *t.liveError
		}
		if !gateway.IsReconnectOnly(liveErr) || !gateway.LedgerEnabled(ctx, nil) {
			return errNotQueued
		}
		sessionKey := fmt.Sprintf("cron:%s:%s", t.platformName, t.chatID)
		if t.threadID != "" {
			sessionKey += ":" + t.threadID
		}
		obligationID := gateway.ComputeObligationID(sessionKey, "job:"+jobIDOr(t.job, "?"), content)
		var profile *string
		if t.transport != nil {
			if o, ok := t.transport.Adapter.(ownerProfileAdapter); ok {
				profile = o.OwnerProfile()
			}
		}
		var thread *string
		if t.threadID != "" {
			thread = strPtr(t.threadID)
		}
		if err := gateway.RecordObligation(ctx, gateway.DeliveryObligation{ObligationID: obligationID, SessionKey: sessionKey, Platform: t.platformName, ChatID: t.chatID, ThreadID: thread, Content: content, AdapterProfile: profile}); err != nil {
			return err
		}
		return gateway.MarkFailed(ctx, obligationID, pyrepr.Str(liveErr))
	}()
	if errors.Is(err, errNotQueued) {
		return
	}
	if err != nil {
		s.log.Warn(fmt.Sprintf("Job '%s': could not queue %s for post-reconnect redelivery: %v", jobIDOr(t.job, "None"), t.where(), err))
		return
	}
	note := fmt.Sprintf("queued text for %s for redelivery once the live adapter reconnects", t.where())
	if len(media) > 0 {
		note += fmt.Sprintf(" (%d attachment(s) not queued)", len(media))
	}
	s.log.Warn(fmt.Sprintf("Job '%s': %s", jobIDOr(t.job, "None"), note))
	*deliveryErrors = append(*deliveryErrors, note)
}

var errNotQueued = errors.New("not queued")

// Standalone fallback of a target the live lane did not deliver, relay targets fail closed
func (s *Store) deliverStandalone(ctx context.Context, t *targetDelivery, content string, media []gateway.MediaRef, targetErrors, deliveryErrors *[]string) {
	job := t.job
	if t.isRelay() {
		if len(*targetErrors) == 0 {
			*targetErrors = append(*targetErrors, fmt.Sprintf("relay delivery to %s failed", t.where()))
		}
		*deliveryErrors = append(*deliveryErrors, *targetErrors...)
		return
	}
	result, errText := s.standaloneSend(ctx, t, content, media)
	if errText == nil && len(result) > 0 && pyTruthy(result["error"]) {
		errText = strPtr(fmt.Sprintf("delivery error: %s (target %s)", pyrepr.Str(result["error"]), t.where()))
		s.log.Error(fmt.Sprintf("Job '%s': %s", job.ID, *errText))
	}
	if errText != nil {
		*targetErrors = append(*targetErrors, *errText)
		*deliveryErrors = append(*deliveryErrors, *targetErrors...)
		s.queueForLiveReconnect(ctx, t, content, media, deliveryErrors)
		return
	}
	if warnings, ok := result["warnings"].([]any); ok {
		for _, w := range warnings {
			msg := fmt.Sprintf("delivery warning: %s (target %s)", pyrepr.Str(w), t.where())
			s.log.Error(fmt.Sprintf("Job '%s': %s", job.ID, msg))
			*deliveryErrors = append(*deliveryErrors, msg)
		}
	}
	s.log.Info(fmt.Sprintf("Job '%s': delivered to %s:%s", job.ID, t.platformName, t.chatID))
	s.maybeMirrorCronDelivery(ctx, job, t.platformName, t.chatID, t.mirrorText, t.threadID, t.originUserID, t.mirrorThisTarget)
}

// Transport, surface, mirror and continuable thread decisions for one target, nil with the error noted
func (s *Store) prepareTargetDelivery(ctx context.Context, job *Job, target Target, adapters, loop any, cfg *gateway.GatewayConfig, notify, mirrorEnabled bool, mirrorText string, deliveryErrors *[]string) *targetDelivery {
	platformName, chatID, threadID := target.Platform, target.ChatID, target.ThreadID
	origin := resolveOrigin(job)
	if origin == nil {
		origin = map[string]any{}
	}
	if pyTruthy(origin["thread_id"]) && threadID == "" {
		deliver := "local"
		if job.Deliver != nil {
			deliver = *job.Deliver
		}
		s.log.Warn(fmt.Sprintf("Job '%s': origin has thread_id=%s but delivery target lost it (deliver=%s, target=%s)", job.ID, pyrepr.Str(origin["thread_id"]), deliver, pyrepr.Str(target.dict())))
	} else if threadID != "" {
		s.log.Debug(fmt.Sprintf("Job '%s': delivering to %s:%s thread_id=%s", job.ID, platformName, chatID, threadID))
	}
	originTarget := targetMatchesOrigin(origin, platformName, chatID, threadID)
	mirrorThis := mirrorEnabled && targetMirrorEligible(job, target, mirrorEnabled, &originTarget)
	var originUserID *string
	if originTarget {
		originUserID = optionalStrPtr(origin["user_id"])
	}
	chatType := strings.ToLower(pyStrOrEmpty(origin["chat_type"]))
	isDM := chatType == "dm" || (chatType == "" && strings.HasPrefix(chatID, "D"))
	continuable := originTarget || (mirrorThis && inchannelSeedAllowed(isDM, originUserID))
	platform, err := gateway.ParsePlatform(ctx, strings.ToLower(platformName))
	if err != nil {
		noteTargetError(s, job.ID, fmt.Sprintf("unknown platform '%s'", platformName), deliveryErrors)
		return nil
	}
	resolved, reason := s.resolveTargetTransport(ctx, platform, platformName, target, adapters, cfg)
	if resolved == nil {
		noteTargetError(s, job.ID, reason, deliveryErrors)
		return nil
	}
	l, isLoop := loop.(cronLoop)
	ready := resolved.runtimeAdapter != nil && loop != nil && isLoop && l.IsRunning()
	inChannel := resolveCronSurfaceMode(resolved.pconfig, platformName) == "in_channel"
	if inChannel && resolved.runtimeAdapter != nil && !inchannelSurfaceSupported(resolved.runtimeAdapter, platformName) {
		s.log.Debug(fmt.Sprintf("Job '%s': cron_continuable_surface=in_channel not supported on %s, using thread", jobIDOr(job, "?"), platformName))
		inChannel = false
	}
	if inChannel && continuable && ready {
		threadID = ""
	}
	opened := ""
	if mirrorThis && !inChannel && resolved.runtimeAdapter != nil && loop != nil && threadID == "" {
		if opened = s.openContinuableCronThread(ctx, job, resolved.runtimeAdapter, chatID, loop); opened != "" {
			threadID = opened
		}
	}
	return &targetDelivery{job: job, platform: platform, platformName: platformName, chatID: chatID, threadID: threadID, transport: resolved.transport, pconfig: resolved.pconfig, runtimeAdapter: resolved.runtimeAdapter, targetAdapters: resolved.targetAdapters, config: cfg, loop: loop, notifyDelivery: notify, origin: origin, originTarget: originTarget, originUserID: originUserID, isDMTarget: isDM, mirrorText: mirrorText, mirrorThisTarget: mirrorThis, inChannelSurface: inChannel, inchannelContinuable: continuable, openedThreadID: opened, liveAdapterReady: ready}
}

// Outcome of a lane with no resolved target, an error only for an explicit unresolvable deliver
func (s *Store) unresolvedDeliveryOutcome(job *Job, forFailure bool) *string {
	deliver := normalizeDeliverValue(deliveryLaneValue(job, forFailure))
	if deliver == "local" {
		return nil
	}
	if deliver == "origin" {
		name := jobNameOr(job, jobIDOr(job, "?"))
		s.log.Info(fmt.Sprintf("Job '%s': deliver=origin but no origin or home channels — skipping delivery (output saved in last_output)", name))
		return nil
	}
	msg := "no delivery target resolved for deliver=" + deliver
	s.log.Warn(fmt.Sprintf("Job '%s': %s", job.ID, msg))
	return &msg
}

// Delivers job output to every resolved target of the lane, the joined errors or nil
func (s *Store) deliverJobResult(ctx context.Context, job *Job, content string, adapters, loop any, forFailure bool) (*string, error) {
	job.run.botChatDeliveryReceipts = nil
	job.run.notificationAllTargetsSuppressed = false
	targets := resolveDeliveryTargets(ctx, s, job, forFailure)
	if len(targets) == 0 {
		s.recordDeliveryVerification(job, nil)
		return s.unresolvedDeliveryOutcome(job, forFailure), nil
	}
	external := os.Getenv(externalWorkerEnv)
	if external != "" && adapters == nil && job.ExecutionID != nil && external == *job.ExecutionID && anyNonBotChat(targets) {
		s.recordDeliveryVerification(job, nil)
		q := openDeliveryQueue(s)
		errText, err := q.EnqueueAndWait(ctx, external, job, content, forFailure, nil)
		if err != nil {
			return nil, err
		}
		status, err := q.GetStatus(ctx, external)
		if err != nil {
			return nil, err
		}
		if status != nil && status["status"] == "suppressed" {
			job.run.notificationAllTargetsSuppressed = true
		}
		refreshed, err := s.getJob(job.ID)
		if err != nil {
			return nil, err
		}
		job.LastDeliveryQueued = nil
		if refreshed != nil {
			job.LastDeliveryQueued = refreshed.LastDeliveryQueued
		}
		return errText, nil
	}
	wrap := true
	userCfg, cfgErr := s.loadConfig()
	if cfgErr == nil {
		if cronCfg, ok := userCfg["cron"].(map[string]any); ok {
			if v, ok := cronCfg["wrap_response"]; ok {
				wrap = pyTruthy(v)
			}
		}
	} else {
		userCfg = nil
	}
	notify := cronDeliveryNotifyEnabled(userCfg)
	var unverified []string
	deliveryContent := content
	if wrap {
		taskName := jobNameOr(job, job.ID)
		deliveryContent = fmt.Sprintf("Cronjob Response: %s\n(job_id: %s)\n-------------\n\n%s\n\nTo stop or manage this job, send me a new message (e.g. \"stop reminder %s\").", taskName, job.ID, content, taskName)
	}
	gateway.ApplyMediaPolicyEnv(ctx, userCfg)
	media, cleaned, err := gateway.ExtractMedia(ctx, deliveryContent)
	if err != nil {
		return nil, err
	}
	cleaned = s.redactCronPayload(cleaned, "delivery content")
	requested := len(media)
	if media, err = gateway.FilterMediaDeliveryPaths(ctx, media); err != nil {
		return nil, err
	}
	var policyDropErrors []string
	if dropped := requested - len(media); dropped > 0 {
		policyDropErrors = []string{fmt.Sprintf("%d media attachment(s) dropped by media path policy (missing file, denied prefix, or strict-mode miss); see gateway.strict / media_delivery_allow_dirs in config.yaml", dropped)}
	}
	mirrorEnabled := s.cronMirrorDeliveryEnabled(job, userCfg)
	_, mirrorText, err := gateway.ExtractMedia(ctx, content)
	if err != nil {
		return nil, err
	}
	mirrorText = s.redactCronPayload(pystr.Strip(mirrorText), "mirror payload")
	gwCfg, err := gateway.LoadGatewayConfig(ctx)
	if err != nil {
		msg := fmt.Sprintf("failed to load gateway config: %v", err)
		s.log.Error(fmt.Sprintf("Job '%s': %s", job.ID, msg))
		return &msg, nil
	}
	var deliveryErrors []string
	suppressed := 0
	for _, target := range targets {
		if forFailure && target.Platform != BotChatPlatform && !config.WarningNotificationsEnabled(ctx, target.Platform, userCfg) {
			suppressed++
			continue
		}
		if target.Platform == BotChatPlatform {
			botErr := s.deliverToBotChat(ctx, job, content, target.ChatID, forFailure)
			if job.run.notificationAllTargetsSuppressed {
				suppressed++
			}
			job.run.notificationAllTargetsSuppressed = false
			if botErr != nil {
				label := target.ChatID
				if label == "" {
					label = "(own)"
				}
				status, has := receiptStatus(job, "bot-chat:"+label)
				if !has || (status != "queued" && status != "claimed") {
					deliveryErrors = append(deliveryErrors, *botErr)
				}
				if has && status == "ambiguous" {
					unverified = append(unverified, *botErr)
				}
			}
			continue
		}
		if target.Platform == "local" {
			localErr, err := gateway.DeliverToLocalChat(ctx, s.home, target.ChatID, cleaned, forFailure)
			if err != nil {
				return nil, err
			}
			if localErr != nil {
				deliveryErrors = append(deliveryErrors, *localErr)
			}
			continue
		}
		t := s.prepareTargetDelivery(ctx, job, target, adapters, loop, gwCfg, notify, mirrorEnabled, mirrorText, &deliveryErrors)
		if t == nil {
			continue
		}
		var targetErrors []string
		delivered := t.liveAdapterReady && s.deliverViaLiveAdapter(ctx, t, cleaned, media, &targetErrors, &deliveryErrors, &unverified)
		if !delivered {
			s.deliverStandalone(ctx, t, cleaned, media, &targetErrors, &deliveryErrors)
		}
	}
	if suppressed == len(targets) {
		job.run.notificationAllTargetsSuppressed = true
	} else {
		deliveryErrors = append(deliveryErrors, policyDropErrors...)
	}
	s.recordDeliveryVerification(job, unverified)
	if len(deliveryErrors) == 0 {
		return nil, nil
	}
	joined := strings.Join(deliveryErrors, "; ")
	return &joined, nil
}

func anyNonBotChat(targets []Target) bool {
	for _, t := range targets {
		if t.Platform != BotChatPlatform {
			return true
		}
	}
	return false
}

// Delivery of job output, the seam tests replace the way pytest patches the module attribute
var deliverResultSeam = (*Store).deliverJobResult

func (s *Store) deliverResult(ctx context.Context, job *Job, content string, adapters, loop any, forFailure bool) (*string, error) {
	return deliverResultSeam(s, ctx, job, content, adapters, loop, forFailure)
}
