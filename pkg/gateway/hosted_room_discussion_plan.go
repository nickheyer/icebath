package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One deterministic member turn compatible with the driver schema
type DiscussionTaskPlan struct {
	Identity          TaskIdentity
	Payload           map[string]any
	DiscussionEventID string
	Member            DiscussionMember
	MemberIndex       int64
	RoundIndex        int64
	SeenThroughSeq    int64
}

// Current result of replaying one room's Discussion policy
type DiscussionDecision struct {
	Status            string
	Reason            string
	DiscussionEventID *string
	SourceEventSeq    *int64
	ThreadID          *string
	Task              *DiscussionTaskPlan
}

// One idempotent room event append
type EventPlan struct {
	EventID            string
	Kind               string
	Actor              map[string]any
	Payload            map[string]any
	AuthorityGatewayID string
	AuthorityEpoch     int64
}

// Append input of the planned event in room
func (p EventPlan) AppendInput(roomID string) AppendEventInput {
	return AppendEventInput{RoomID: roomID, EventID: p.EventID, Kind: p.Kind, Actor: p.Actor, Payload: p.Payload,
		AuthorityGatewayID: p.AuthorityGatewayID, AuthorityEpoch: p.AuthorityEpoch}
}

// Ordered visible and terminal effects of one driver task
type PublicationPlan struct {
	TaskID       string
	TerminalKind string
	Events       []EventPlan
}

// Members rotated left by the round
func discussionRotate(members []DiscussionMember, round int64) []DiscussionMember {
	if len(members) == 0 {
		return nil
	}
	count := int64(len(members))
	shift := int((round%count + count) % count)
	return append(append([]DiscussionMember{}, members[shift:]...), members[:shift]...)
}

// One prompt line of a thread message, member control frame openers relabelled
func discussionFormatMessage(event discussionEvent, room DiscussionRoom) (string, error) {
	if event.kind == "message.user" {
		return "User (user): " + pyrepr.Str(event.payload["text"]), nil
	}
	text := memberControlFrameRE.ReplaceAllStringFunc(pyrepr.Str(event.payload["text"]), func(opener string) string { return memberControlRelabel + opener[1:] })
	member, err := discussionMemberByID(room, event.payload["member_id"])
	if err != nil {
		return "", err
	}
	return "@" + member.Handle + ": " + text, nil
}

// Text cut to maxBytes of UTF-8 at a whole character, the suffix appended after a cut
func discussionTruncateUTF8(value any, maxBytes int, suffix string) string {
	text := roomLinkText(value)
	if len(text) <= maxBytes {
		return text
	}
	prefix := strings.ToValidUTF8(text[:max(0, maxBytes-len(suffix))], "")
	if prefix != "" {
		return prefix + suffix
	}
	return pystr.Strip(suffix)
}

// Bounded member prompt of the thread delta after the watermark
func discussionBuildPrompt(room DiscussionRoom, member DiscussionMember, messages []discussionEvent, watermark, seenThrough int64, replyRequired bool) (string, error) {
	var delta []discussionEvent
	for _, event := range messages {
		if watermark < event.seq && event.seq <= seenThrough {
			delta = append(delta, event)
		}
	}
	if len(delta) > maxDiscussionDeltaLines {
		delta = delta[len(delta)-maxDiscussionDeltaLines:]
	}
	var peers []string
	for _, candidate := range room.Members {
		if candidate.MemberID != member.MemberID {
			peers = append(peers, "@"+candidate.Handle)
		}
	}
	peerText := strings.Join(peers, ", ")
	if peerText == "" {
		peerText = "no other members"
	}
	opening := []string{fmt.Sprintf(`[Discussion: "%s"] You are @%s, one participant with %s and the user.`, room.Name, member.Handle, peerText), "",
		"New messages in this thread since your last turn (oldest first):"}
	participation := []string{"- Reply with one conversational message only when you have something new worth adding.",
		`- If you have nothing new to add, reply with exactly "(pass)".`}
	if replyRequired {
		participation = []string{"- The user directly addressed you. Reply with your answer, result, or concrete blocker.",
			`- Do not return "(pass)". If you cannot proceed, explain why and what you need from the user.`}
	}
	rules := append(append([]string{"", "Rules for this Discussion:"}, participation...),
		"- Mention a teammate by handle to pull them into the next round; do not repeat points already made.",
		"- Never reveal content from private conversations. Your reply is published verbatim.")
	fixed := len(strings.Join(append(append([]string{}, opening...), rules...), "\n"))
	available := max(0, driverMaxPromptBytes-fixed-1)
	var selected []string
	for i := len(delta) - 1; i >= 0; i-- {
		formatted, err := discussionFormatMessage(delta[i], room)
		if err != nil {
			return "", err
		}
		line := "  " + formatted
		if lineBytes := len(line) + 1; lineBytes > available {
			if len(selected) == 0 && available > 32 {
				selected = append(selected, discussionTruncateUTF8(line, available, ""))
			}
			selected = append(selected, "  [Earlier content omitted to fit this turn.]")
			break
		} else {
			selected = append(selected, line)
			available -= lineBytes
		}
	}
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}
	prompt := strings.Join(append(append(append([]string{}, opening...), selected...), rules...), "\n")
	if len(prompt) > driverMaxPromptBytes {
		return "", discussionInvalid("Discussion prompt exceeds the driver limit")
	}
	return prompt, nil
}

// Whether the user's initial address requires this member to reply
func discussionRequiresDirectReply(room DiscussionRoom, source *discussionEvent, member DiscussionMember, round int64) bool {
	if round != 0 || source == nil || source.kind != "message.user" {
		return false
	}
	for _, mentioned := range ResolveMentions([]string{pyrepr.Str(source.payload["text"])}, room.Members, false) {
		if mentioned.Equal(member) {
			return true
		}
	}
	return false
}

// Deterministic task plan of one member turn
func discussionMakeTaskPlan(room DiscussionRoom, discussion discussionEvent, member DiscussionMember, memberIndex, round, seenThrough int64, prompt string,
	inputContext map[string]any, attachments []any) (DiscussionTaskPlan, error) {
	turnID := fmt.Sprintf("d%d.r%d.p%d.s%d.m%s", discussion.seq, round, memberIndex, seenThrough, discussionMemberDigest(member))
	if len(attachments) > 0 {
		manifest, err := validateBoundTaskManifest(attachments)
		if err != nil {
			return DiscussionTaskPlan{}, err
		}
		attachments = toAnyList(manifest)
	}
	promptSum := sha256.Sum256([]byte(prompt))
	seedFields := map[string]any{"discussion_event_id": discussion.eventID, "member_id": member.MemberID, "member_index": memberIndex,
		"prompt_sha256": hex.EncodeToString(promptSum[:]), "room_id": room.RoomID, "round_index": round, "seen_through_seq": seenThrough,
		"source_event_seq": discussion.seq, "thread_id": discussion.payload["thread_id"]}
	if inputContext != nil {
		seedFields["input_context"] = inputContext
	}
	if len(attachments) > 0 {
		seedFields["attachments"] = attachments
	}
	seed, err := roomCompactJSON(seedFields, true)
	if err != nil {
		return DiscussionTaskPlan{}, err
	}
	seedSum := sha256.Sum256([]byte(seed))
	identity, err := NewTaskIdentity(room.RoomID, "dtask:"+hex.EncodeToString(seedSum[:])[:48], pyrepr.Str(discussion.payload["thread_id"]), turnID)
	if err != nil {
		return DiscussionTaskPlan{}, err
	}
	payload := map[string]any{"target_member_id": member.MemberID, "target_profile": member.Profile, "prompt": prompt, "source_event_seq": discussion.seq}
	if len(attachments) > 0 {
		payload["attachments"] = attachments
	}
	if inputContext != nil {
		payload["input_context"] = pyjson.Plain(inputContext)
	}
	return DiscussionTaskPlan{identity, payload, discussion.eventID, member, memberIndex, round, seenThrough}, nil
}

// Oldest latest per thread user message neither stopped nor completed
func discussionPending(validated []discussionEvent) *discussionEvent {
	var stopped int64
	completed := map[string]bool{}
	latest := map[string]discussionEvent{}
	for _, event := range validated {
		switch event.kind {
		case "room.stop_requested":
			stopped = max(stopped, event.seq)
		case "room.activity":
			if status := event.payload["status"]; status == "settled" || status == "bounded" {
				completed[pyrepr.Str(event.payload["discussion_event_id"])] = true
			}
		case "message.user":
			latest[pyrepr.Str(event.payload["thread_id"])] = event
		}
	}
	candidates := make([]discussionEvent, 0, len(latest))
	for _, event := range latest {
		candidates = append(candidates, event)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].seq < candidates[j].seq })
	for _, event := range candidates {
		if event.seq > stopped && !completed[event.eventID] {
			return &event
		}
	}
	return nil
}

// Thread messages, those since the discussion, and this discussion's committed member messages
func discussionThreadMessages(validated []discussionEvent, discussion discussionEvent) ([]discussionEvent, []discussionEvent, []discussionEvent) {
	threadID := discussion.payload["thread_id"]
	committed := map[string]bool{}
	for _, event := range validated {
		if event.kind == "turn.settled" && event.payload["message_event_id"] != nil {
			committed[pyrepr.Str(event.payload["message_event_id"])] = true
		}
	}
	var thread, since, members []discussionEvent
	for _, event := range validated {
		if !pyjson.Equal(event.payload["thread_id"], threadID) || !(event.kind == "message.user" || (event.kind == "message.member" && committed[event.eventID])) {
			continue
		}
		thread = append(thread, event)
		if event.seq >= discussion.seq {
			since = append(since, event)
		}
		if event.kind == "message.member" && event.payload["discussion_event_id"] == discussion.eventID {
			members = append(members, event)
		}
	}
	return thread, since, members
}

// Initial watermarks merged with those the log derives
func discussionEffectiveWatermarks(validated []discussionEvent, initial map[[2]string]int64) (map[[2]string]int64, error) {
	watermarks := map[[2]string]int64{}
	for key, value := range initial {
		if value >= 0 {
			watermarks[key] = value
		}
	}
	derived, err := discussionDeriveWatermarks(validated)
	if err != nil {
		return nil, err
	}
	for key, value := range derived {
		watermarks[key] = max(watermarks[key], value)
	}
	return watermarks, nil
}

// Attachments of an event a member receives, each bound to its source event
func discussionEventAttachments(event discussionEvent, member DiscussionMember) []any {
	if event.kind == "message.member" {
		if event.payload["member_id"] == member.MemberID {
			return []any{}
		}
		if recipients, has := event.payload["recipient_member_ids"]; has && recipients != nil {
			found := false
			for _, recipient := range recipients.([]any) {
				found = found || recipient == member.MemberID
			}
			if !found {
				return []any{}
			}
		}
	}
	list, _ := event.payload["attachments"].([]any)
	out := make([]any, 0, len(list))
	for _, item := range list {
		entry := pyjson.Plain(item).(map[string]any)
		entry["event_id"] = event.eventID
		out = append(out, entry)
	}
	return out
}

// Whole event prefix after the watermark that fits the task attachment bounds
func discussionBoundedDelta(messages []discussionEvent, watermark int64, member DiscussionMember) ([]discussionEvent, []any) {
	var selected []discussionEvent
	attachments := []any{}
	size := int64(0)
	for _, event := range messages {
		if event.seq <= watermark {
			continue
		}
		entries := discussionEventAttachments(event, member)
		next := size
		for _, entry := range entries {
			n, _ := hostedStrictInt(entry.(map[string]any)["size"])
			next += n
		}
		if len(attachments)+len(entries) > maxTaskAttachments || next > maxTaskAttachmentBytes {
			break
		}
		selected = append(selected, event)
		attachments = append(attachments, entries...)
		size = next
	}
	return selected, attachments
}

// Replays the complete room log and returns at most one next member task
func PlanNextTask(roomValue any, events []any, localProfiles []string, initialWatermarks map[[2]string]int64, freezeInputContext bool, limits DiscussionLimits) (DiscussionDecision, error) {
	room, err := ValidateDiscussionRoom(roomValue, localProfiles)
	if err != nil {
		return DiscussionDecision{}, err
	}
	validated, err := discussionValidatedEvents(events, room)
	if err != nil {
		return DiscussionDecision{}, err
	}
	discussion := discussionPending(validated)
	if discussion == nil {
		return DiscussionDecision{Status: "idle", Reason: "no_pending_user_event"}, nil
	}
	threadID := pyrepr.Str(discussion.payload["thread_id"])
	decide := func(status, reason string, task *DiscussionTaskPlan) DiscussionDecision {
		eventID, seq := discussion.eventID, discussion.seq
		return DiscussionDecision{status, reason, &eventID, &seq, &threadID, task}
	}
	threadMessages, discussionMessages, memberMessages := discussionThreadMessages(validated, *discussion)
	if int64(len(memberMessages)) >= limits.MaxMessages {
		return decide("bounded", "max_messages", nil), nil
	}
	terminals := map[string]bool{}
	for _, event := range validated {
		if discussionTerminalExtras[event.kind] != nil && event.payload["discussion_event_id"] == discussion.eventID {
			round, _ := hostedStrictInt(event.payload["round_index"])
			terminals[fmt.Sprint(round)+"\x00"+pyrepr.Str(event.payload["member_id"])] = true
		}
	}
	watermarks, err := discussionEffectiveWatermarks(validated, initialWatermarks)
	if err != nil {
		return DiscussionDecision{}, err
	}
	for round := int64(0); round < limits.MaxRounds; round++ {
		responders := discussionUnaddressedMentions(discussionMessages, room)
		if round == 0 {
			responders = ResolveMentions([]string{pyrepr.Str(discussion.payload["text"])}, room.Members, true)
		}
		for index, member := range discussionRotate(responders, round) {
			watermark := watermarks[[2]string{threadID, member.MemberID}]
			delta, attachments := discussionBoundedDelta(threadMessages, watermark, member)
			if len(delta) == 0 || (terminals[fmt.Sprint(round)+"\x00"+member.MemberID] && len(attachments) == 0) {
				continue
			}
			seenThrough := delta[len(delta)-1].seq
			prompt, err := discussionBuildPrompt(room, member, threadMessages, watermark, seenThrough, discussionRequiresDirectReply(room, discussion, member, round))
			if err != nil {
				return DiscussionDecision{}, err
			}
			var inputContext map[string]any
			if freezeInputContext || len(attachments) > 0 {
				seqs := make([]any, len(delta))
				for i, event := range delta {
					seqs[i] = event.seq
				}
				if inputContext, err = ValidateTaskInput(map[string]any{"watermark": watermark, "event_seqs": seqs}); err != nil {
					return DiscussionDecision{}, err
				}
			}
			plan, err := discussionMakeTaskPlan(room, *discussion, member, int64(index), round, seenThrough, prompt, inputContext, attachments)
			if err != nil {
				return DiscussionDecision{}, err
			}
			return decide("task", "member_turn", &plan), nil
		}
		spoke := false
		for _, event := range memberMessages {
			if value, _ := hostedStrictInt(event.payload["round_index"]); value == round {
				spoke = true
			}
		}
		if !spoke {
			return decide("settled", "silent_round", nil), nil
		}
		if round == limits.MaxRounds-1 {
			return decide("bounded", "max_rounds", nil), nil
		}
	}
	return DiscussionDecision{}, errors.New("bounded Discussion loop exhausted unexpectedly")
}

// Reconstructs and verifies one persisted driver task after a restart
func ReconstructTaskPlan(roomValue any, events []any, task map[string]any, localProfiles []string) (DiscussionTaskPlan, error) {
	room, err := ValidateDiscussionRoom(roomValue, localProfiles)
	if err != nil {
		return DiscussionTaskPlan{}, err
	}
	validated, err := discussionValidatedEvents(events, room)
	if err != nil {
		return DiscussionTaskPlan{}, err
	}
	identity, identityOK := task["identity"].(TaskIdentity)
	payload, payloadOK := hostedMapping(task["payload"])
	if !identityOK || !payloadOK {
		return DiscussionTaskPlan{}, discussionUnreconstructable("driver task has no valid identity or payload")
	}
	payload = pyjson.Plain(payload).(map[string]any)
	for _, key := range driverPayloadRequired {
		if _, has := payload[key]; !has {
			return DiscussionTaskPlan{}, discussionUnreconstructable("driver task payload shape changed")
		}
	}
	for key := range payload {
		if !keySet(driverPayloadRequired...)[key] && !driverPayloadOptional[key] {
			return DiscussionTaskPlan{}, discussionUnreconstructable("driver task payload shape changed")
		}
	}
	match := discussionTurnIDRE.FindStringSubmatch(identity.TurnID)
	if match == nil {
		return DiscussionTaskPlan{}, discussionUnreconstructable("turn_id is not a Discussion coordinate")
	}
	coordinate := func(i int) int64 {
		n, _ := strconv.ParseInt(match[i], 10, 64)
		return n
	}
	source := coordinate(1)
	if seq, isInt := hostedStrictInt(payload["source_event_seq"]); !isInt || seq != source {
		return DiscussionTaskPlan{}, discussionUnreconstructable("task source event does not match turn_id")
	}
	var discussion *discussionEvent
	for i := range validated {
		if validated[i].seq == source && validated[i].kind == "message.user" {
			discussion = &validated[i]
			break
		}
	}
	if discussion == nil {
		return DiscussionTaskPlan{}, discussionUnreconstructable("task source user event is missing")
	}
	if identity.RoomID != room.RoomID || identity.ThreadID != discussion.payload["thread_id"] {
		return DiscussionTaskPlan{}, discussionUnreconstructable("task identity does not match its room thread")
	}
	var member *DiscussionMember
	targetMember, hasTarget := payload["target_member_id"]
	for i := range room.Members {
		candidate := room.Members[i]
		if candidate.Profile == payload["target_profile"] && (!hasTarget || targetMember == nil || candidate.MemberID == targetMember) {
			member = &candidate
			break
		}
	}
	if member == nil || discussionMemberDigest(*member) != match[5] {
		return DiscussionTaskPlan{}, discussionUnreconstructable("task target member does not match turn_id")
	}
	prompt, ok := payload["prompt"].(string)
	if !ok || pystr.Strip(prompt) == "" {
		return DiscussionTaskPlan{}, discussionUnreconstructable("task prompt is missing")
	}
	if len(prompt) > driverMaxPromptBytes {
		return DiscussionTaskPlan{}, discussionUnreconstructable("task prompt exceeds the driver limit")
	}
	var inputContext map[string]any
	if raw, has := payload["input_context"]; has {
		if inputContext, err = ValidateTaskInput(raw); err != nil {
			return DiscussionTaskPlan{}, discussionUnreconstructable(err.Error())
		}
		seqs := inputContext["event_seqs"].([]any)
		if last, _ := hostedStrictInt(seqs[len(seqs)-1]); last != coordinate(4) {
			return DiscussionTaskPlan{}, discussionUnreconstructable("task input does not match turn_id")
		}
		messageSeqs := map[int64]bool{}
		for _, event := range validated {
			if (event.kind == "message.user" || event.kind == "message.member") && event.payload["thread_id"] == identity.ThreadID {
				messageSeqs[event.seq] = true
			}
		}
		for _, item := range seqs {
			if seq, _ := hostedStrictInt(item); !messageSeqs[seq] {
				return DiscussionTaskPlan{}, discussionUnreconstructable("task input message is missing")
			}
		}
	}
	attachments := []any{}
	if _, has := payload["attachments"]; has {
		if inputContext == nil {
			return DiscussionTaskPlan{}, discussionUnreconstructable("attachment task requires frozen input_context")
		}
		bySeq := map[int64]discussionEvent{}
		for _, event := range validated {
			bySeq[event.seq] = event
		}
		for _, item := range inputContext["event_seqs"].([]any) {
			seq, _ := hostedStrictInt(item)
			attachments = append(attachments, discussionEventAttachments(bySeq[seq], *member)...)
		}
	}
	reconstructed, err := discussionMakeTaskPlan(room, *discussion, *member, coordinate(3), coordinate(2), coordinate(4), prompt, inputContext, attachments)
	if err != nil {
		return DiscussionTaskPlan{}, err
	}
	if reconstructed.Identity != identity || !pyjson.Equal(reconstructed.Payload, payload) {
		return DiscussionTaskPlan{}, discussionUnreconstructable("driver task failed deterministic reconstruction")
	}
	return reconstructed, nil
}

// Coordinate fields every member message and terminal event carries
func discussionTurnCoordinateFields(task DiscussionTaskPlan) map[string]any {
	return map[string]any{"discussion_event_id": task.DiscussionEventID, "member_id": task.Member.MemberID, "member_index": task.MemberIndex,
		"round_index": task.RoundIndex, "task_id": task.Identity.TaskID, "thread_id": task.Identity.ThreadID, "turn_id": task.Identity.TurnID}
}

// Stripped terminal text of a result field, the fallback when empty
func discussionTerminalText(result any, field, fallback string) string {
	if mapping, ok := hostedMapping(result); ok {
		if field != "error" || mapping["error"] != nil {
			result = mapping[field]
		} else {
			result = mapping["text"]
		}
	}
	if text := pystr.Strip(roomLinkText(result)); text != "" {
		return text
	}
	return fallback
}

// Plans idempotent room effects of one terminal driver task, a newer same thread send cancelling a late result
func PlanPublication(roomValue any, events []any, task DiscussionTaskPlan, status string, result any, executionGeneration *int64, localProfiles []string) (PublicationPlan, error) {
	room, err := ValidateDiscussionRoom(roomValue, localProfiles)
	if err != nil {
		return PublicationPlan{}, err
	}
	validated, err := discussionValidatedEvents(events, room)
	if err != nil {
		return PublicationPlan{}, err
	}
	statuses := map[string]bool{"settled": true, "failed": true, "cancelled": true, "deferred": true}
	for _, check := range []struct {
		failed  bool
		message string
	}{{task.Identity.RoomID != room.RoomID, "task belongs to a different room"}, {!room.hasMember(task.Member), "task member is not in the frozen roster"},
		{!statuses[status], "invalid terminal publication status"}} {
		if check.failed {
			return PublicationPlan{}, discussionInvalid(check.message)
		}
	}
	if status == "deferred" {
		var generation any
		if executionGeneration != nil {
			generation = *executionGeneration
		}
		if _, err := roomBoundedInt(generation, discussionInvalid, "deferred publication requires an execution generation", 1, nil); err != nil {
			return PublicationPlan{}, err
		}
	}
	sourceSeq, _ := hostedStrictInt(task.Payload["source_event_seq"])
	newer := false
	var source *discussionEvent
	for i, event := range validated {
		if event.kind == "message.user" && event.seq > sourceSeq && event.payload["thread_id"] == task.Identity.ThreadID {
			newer = true
		}
		if source == nil && event.eventID == task.DiscussionEventID {
			source = &validated[i]
		}
	}
	effective := status
	if newer && status != "deferred" {
		effective = "cancelled"
	}
	if effective == "settled" && discussionRequiresDirectReply(room, source, task.Member, task.RoundIndex) && IsPassText(discussionTerminalText(result, "text", "")) {
		effective = "failed"
		result = map[string]any{"error": "@" + task.Member.Handle + " was directly addressed but returned no visible reply."}
	}
	digest := strings.TrimPrefix(task.Identity.TaskID, "dtask:")
	terminalEventID := "dterminal:" + digest
	if effective == "deferred" {
		terminalEventID = fmt.Sprintf("ddeferred:%s:g%d", digest, *executionGeneration)
	}
	extra, effects := map[string]any{}, []EventPlan{}
	switch effective {
	case "settled":
		text := discussionTruncateUTF8(discussionTerminalText(result, "text", ""), maxMemberTextBytes, discussionTruncatedNote)
		if IsPassText(text) {
			extra = map[string]any{"message_event_id": nil, "passed": true}
		} else {
			messageEventID := "dmessage:" + digest
			extra = map[string]any{"message_event_id": messageEventID, "passed": false}
			payload := discussionTurnCoordinateFields(task)
			payload["text"] = text
			effects = append(effects, EventPlan{messageEventID, "message.member", discussionMemberActor(task.Member, true), payload, room.GatewayID, room.AuthorityEpoch})
		}
	case "failed":
		errorText := discussionTerminalText(result, "error", "member turn failed")
		supplied := ""
		if mapping, ok := hostedMapping(result); ok {
			value := mapping["reason_code"]
			if !pyrepr.Truthy(value) {
				value = mapping["reason"]
			}
			supplied = pystr.Strip(roomLinkText(value))
		}
		reason := supplied
		if !tools.AllReasons[supplied] {
			reason = tools.ClassifyAgentError(errorText)
		}
		extra = map[string]any{"error": errorText, "reason_code": reason}
	case "cancelled":
		if newer {
			extra = map[string]any{"reason": "superseded_by_newer_user_event"}
		} else {
			extra = map[string]any{"reason": discussionTerminalText(result, "reason", "member turn cancelled")}
		}
	case "deferred":
		var generation any
		if executionGeneration != nil {
			generation = *executionGeneration
		}
		extra = map[string]any{"execution_generation": generation, "reason": discussionTerminalText(result, "reason", "member_unavailable")}
	}
	terminalKind := "turn." + effective
	payload := discussionTurnCoordinateFields(task)
	payload["seen_through_seq"] = task.SeenThroughSeq
	for key, value := range extra {
		payload[key] = value
	}
	effects = append(effects, EventPlan{terminalEventID, terminalKind, map[string]any{"kind": "gateway", "id": room.GatewayID}, payload, room.GatewayID, room.AuthorityEpoch})
	return PublicationPlan{task.Identity.TaskID, terminalKind, effects}, nil
}
