package tools

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Beat and run states of the onboarding record
const (
	onbPending      = "pending"
	onbShown        = "shown"
	onbDone         = "done"
	onbSkipped      = "skipped"
	onbPhaseRunning = "onboarding"
	onbPhaseDone    = "complete"
	onbPhaseSkipped = "skipped"
	// OnboardingCardTimeout is how long a card waits for the user in seconds
	OnboardingCardTimeout = 300
	onboardingMaxPlugins  = 5
)

var (
	onboardingLayoutOptions = []map[string]any{{"id": "sidebar-left", "label": "Basic"}, {"id": "terminal-deck", "label": "Elite"}}
	onboardingTourOptions   = []map[string]any{{"id": "quick", "label": "Quick tour"}, {"id": "full", "label": "Show me everything"}, {"id": "skip", "label": "Skip, let's build something"}}
)

// OnboardingFacts are the safe facts one run is built from, the full name reaching only the name card
type OnboardingFacts struct {
	OS             string
	Arch           string
	MachineKind    string
	NvidiaPlatform string
	FactsSource    string
	Locale         string
	Plugins        []string
	Tours          bool
	FullName       string
}

// Stored is the record shape of the facts without the full name
func (f OnboardingFacts) Stored() map[string]any {
	plugins := make([]any, len(f.Plugins))
	for i, p := range f.Plugins {
		plugins[i] = p
	}
	return map[string]any{"os": f.OS, "arch": f.Arch, "machine_kind": f.MachineKind, "nvidia_platform": emptyAsNull(f.NvidiaPlatform),
		"facts_source": f.FactsSource, "locale": emptyAsNull(f.Locale), "plugins": plugins, "tours": f.Tours}
}

// OnboardingFactsFromStored rebuilds facts from a record, defaults filling absent keys
func OnboardingFactsFromStored(data map[string]any) OnboardingFacts {
	f := OnboardingFacts{MachineKind: "computer", FactsSource: "local", Tours: true}
	if v, ok := data["os"].(string); ok {
		f.OS = v
	}
	if v, ok := data["arch"].(string); ok {
		f.Arch = v
	}
	if v, ok := data["machine_kind"].(string); ok {
		f.MachineKind = v
	}
	if v, ok := data["nvidia_platform"].(string); ok {
		f.NvidiaPlatform = v
	}
	if v, ok := data["facts_source"].(string); ok {
		f.FactsSource = v
	}
	if v, ok := data["locale"].(string); ok {
		f.Locale = v
	}
	if _, ok := data["tours"]; ok {
		f.Tours = truthy(data["tours"])
	}
	f.Plugins = anyStrings(data["plugins"])
	return f
}

// OnboardingEvent is one thing that moves the run record
type OnboardingEvent interface{ onboardingEvent() }

// OnbShown shows a beat's card, options being the build picks and suggest the connector slugs pinned
type OnbShown struct {
	Beat       string
	ToolCallID string
	Options    any
	Suggest    any
}

// OnbAnswered is a card answer, Said being words typed in the composer that the chat shows as the user's bubble
type OnbAnswered struct {
	Beat   string
	Answer map[string]any
	Said   string
}

// OnbWrote is a message sent instead of answering the card
type OnbWrote struct {
	Beat    string
	Present map[string]any
}

// OnbTimedOut is a card that closed at its deadline
type OnbTimedOut struct {
	Beat    string
	Present map[string]any
}

// OnbRecorded is a value the user gave in words
type OnbRecorded struct {
	Beat    string
	Value   string
	Present map[string]any
}

// OnbSkipped is a beat the user skipped in words
type OnbSkipped struct {
	Beat    string
	Present map[string]any
}

// OnbEnd ends the run outside the model, skipped, replaced, or deleted
type OnbEnd struct{ Outcome string }

// OnbDone ends onboarding after the first build
type OnbDone struct{}

// OnbApplied is a general tool's result during the run
type OnbApplied struct {
	Tool   string
	Args   map[string]any
	Result map[string]any
}

func (OnbShown) onboardingEvent()    {}
func (OnbAnswered) onboardingEvent() {}
func (OnbWrote) onboardingEvent()    {}
func (OnbTimedOut) onboardingEvent() {}
func (OnbRecorded) onboardingEvent() {}
func (OnbSkipped) onboardingEvent()  {}
func (OnbEnd) onboardingEvent()      {}
func (OnbDone) onboardingEvent()     {}
func (OnbApplied) onboardingEvent()  {}

// OnboardingStep is the record after an event, the model facing result, and the reminder key
type OnboardingStep struct {
	Record   map[string]any
	Result   map[string]any
	Reminder string
}

// Ids the build beat accepts, what its card showed plus a task named in words
func onboardingBuildValues(_ OnboardingFacts, picks map[string]any) []string {
	return append(onboardingOptionIDs(toAnyList(picks["build_options"])), onboardingFixedID, onboardingOwnID)
}

// The id of every option row
func onboardingOptionIDs(options []any) []string {
	var out []string
	for _, o := range options {
		if m, ok := o.(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				out = append(out, id)
			}
		}
	}
	return out
}

// Option maps as a generic list
func optionMaps(options []map[string]any) []any {
	out := make([]any, len(options))
	for i, o := range options {
		out[i] = copyMap(o)
	}
	return out
}

// onboardingBeat is one beat with its card data, pick keys, typed value key, accepted ids and machine gate
type onboardingBeat struct {
	ID     string
	Card   func(facts OnboardingFacts, picks map[string]any) map[string]any
	Picks  []string
	Value  string
	Values func(facts OnboardingFacts, picks map[string]any) []string
	When   func(facts OnboardingFacts, picks map[string]any) bool
}

// Reminder key for the result that settles this beat
func (b onboardingBeat) after(picks map[string]any) string {
	if (b.ID == "tour" || b.ID == "build") && truthy(picks[b.ID]) {
		return fmt.Sprintf("%s_%v", b.ID, picks[b.ID])
	}
	return "after_" + b.ID
}

func onboardingAlways(OnboardingFacts, map[string]any) bool { return true }

var onboardingBeats = []onboardingBeat{
	{ID: "name", Card: func(OnboardingFacts, map[string]any) map[string]any { return map[string]any{} }, Picks: []string{"name"}, Value: "name", When: onboardingAlways},
	{ID: "theme", Card: func(OnboardingFacts, map[string]any) map[string]any { return map[string]any{"picker": "accent"} }, Picks: []string{"accent", "mode"}, Value: "accent", When: onboardingAlways},
	{ID: "layout", Card: func(OnboardingFacts, map[string]any) map[string]any {
		return map[string]any{"options": optionMaps(onboardingLayoutOptions)}
	}, Picks: []string{"layout"}, Value: "layout",
		Values: func(OnboardingFacts, map[string]any) []string {
			return onboardingOptionIDs(optionMaps(onboardingLayoutOptions))
		}, When: onboardingAlways},
	{ID: "connectors", Card: func(OnboardingFacts, map[string]any) map[string]any { return map[string]any{} }, Picks: []string{"connected", "newly_connected"}, When: onboardingAlways},
	{ID: "plugins", Card: func(facts OnboardingFacts, _ map[string]any) map[string]any {
		plugins := facts.Plugins
		if len(plugins) > onboardingMaxPlugins {
			plugins = plugins[:onboardingMaxPlugins]
		}
		return map[string]any{"plugins": enumValues(plugins)}
	}, Picks: []string{"installed"}, When: func(facts OnboardingFacts, _ map[string]any) bool { return len(facts.Plugins) > 0 }},
	{ID: "tour", Card: func(OnboardingFacts, map[string]any) map[string]any {
		return map[string]any{"options": optionMaps(onboardingTourOptions)}
	}, Picks: []string{"tour"}, Value: "tour",
		Values: func(OnboardingFacts, map[string]any) []string {
			return onboardingOptionIDs(optionMaps(onboardingTourOptions))
		},
		When: func(facts OnboardingFacts, _ map[string]any) bool { return facts.Tours }},
	{ID: "build", Card: func(_ OnboardingFacts, picks map[string]any) map[string]any {
		return map[string]any{"options": firstTaskCardOptions(picks["build_options"])}
	}, Picks: []string{"build"}, Value: "build", Values: onboardingBuildValues, When: onboardingAlways},
}

// OnboardingBeatIDs lists the beats in order
var OnboardingBeatIDs = func() []string {
	out := make([]string, len(onboardingBeats))
	for i, b := range onboardingBeats {
		out[i] = b.ID
	}
	return out
}()

// Beat by id, the zero beat for unknown ids
func onboardingBeatByID(id string) onboardingBeat {
	for _, b := range onboardingBeats {
		if b.ID == id {
			return b
		}
	}
	return onboardingBeat{}
}

// Beat and picks map views of a record
func onbBeats(record map[string]any) map[string]any {
	m, _ := record["beats"].(map[string]any)
	if m == nil {
		m = map[string]any{}
		record["beats"] = m
	}
	return m
}

func onbPicks(record map[string]any) map[string]any {
	m, _ := record["picks"].(map[string]any)
	if m == nil {
		m = map[string]any{}
		record["picks"] = m
	}
	return m
}

// NewOnboardingRecord is a fresh run record with the beats this machine cannot run already skipped
func NewOnboardingRecord(facts OnboardingFacts, now float64, runID string) map[string]any {
	beats := map[string]any{}
	for _, b := range onboardingBeats {
		beats[b.ID] = onbPending
	}
	record := map[string]any{
		"run_id": runID, "phase": onbPhaseRunning, "beats": beats, "waiting": nil,
		"picks": map[string]any{"name": nil, "accent": nil, "mode": nil, "layout": nil, "connected": []any{},
			"newly_connected": []any{}, "installed": []any{}, "tour": nil, "build": nil, "build_options": []any{}},
		"look_around": "not_offered", "facts": facts.Stored(), "ended_by": nil,
		"started_at": now, "updated_at": now,
	}
	return onboardingSettleUnfit(record, facts)
}

// Marks pending beats this machine does not run as skipped
func onboardingSettleUnfit(record map[string]any, facts OnboardingFacts) map[string]any {
	beats, picks := onbBeats(record), onbPicks(record)
	for _, b := range onboardingBeats {
		if beats[b.ID] == onbPending && !b.When(facts, picks) {
			beats[b.ID] = onbSkipped
		}
	}
	return record
}

// OnboardingNextBeat is the first beat still pending or shown, empty when every beat is settled
func OnboardingNextBeat(record map[string]any) string {
	beats := onbBeats(record)
	for _, id := range OnboardingBeatIDs {
		if beats[id] == onbPending || beats[id] == onbShown {
			return id
		}
	}
	return ""
}

// OnboardingActive reports whether the record is a run still in progress
func OnboardingActive(record map[string]any) bool {
	return record != nil && record["phase"] == onbPhaseRunning
}

// Next beat or done for the result's next field
func onbNextField(record map[string]any) string {
	if beat := OnboardingNextBeat(record); beat != "" {
		return beat
	}
	return "done"
}

// Marks a beat done and records the patch keys the beat owns
func onbSettle(record map[string]any, beat string, patch map[string]any) {
	onbBeats(record)[beat] = onbDone
	picks := onbPicks(record)
	for _, key := range onboardingBeatByID(beat).Picks {
		if v, ok := patch[key]; ok {
			picks[key] = v
		}
	}
}

// Stores the model's build picks, a bring back without picks reusing the ones already on the card
func onbBuildPicksError(record map[string]any, event OnbShown, facts OnboardingFacts) string {
	picks := onbPicks(record)
	if !truthy(event.Options) && len(toAnyList(picks["build_options"])) > 0 {
		return ""
	}
	if err := firstTaskPickError(event.Options, facts, picks); err != "" {
		return err
	}
	var stored []any
	for _, o := range toAnyList(event.Options) {
		m := o.(map[string]any)
		stored = append(stored, map[string]any{"id": m["id"], "label": strings.TrimSpace(m["label"].(string))})
	}
	picks["build_options"] = stored
	return ""
}

// Reports whether the suggest value carries at least one non blank slug
func onbHasSuggestions(suggest any) bool {
	for _, s := range toAnyList(suggest) {
		if strings.TrimSpace(fmt.Sprint(s)) != "" {
			return true
		}
	}
	return false
}

func onbShownHandler(record map[string]any, event OnbShown, facts OnboardingFacts, now float64) OnboardingStep {
	beats, picks := onbBeats(record), onbPicks(record)
	if !onboardingBeatByID(event.Beat).When(facts, picks) {
		beats[event.Beat] = onbSkipped
		return OnboardingStep{record, map[string]any{"beat": event.Beat, "skipped": true, "next": onbNextField(record)}, "moved_on"}
	}
	if event.Beat == "build" {
		if err := onbBuildPicksError(record, event, facts); err != "" {
			return OnboardingStep{record, map[string]any{"error": err}, ""}
		}
	}
	if event.Beat == "connectors" && record["look_around"] == "not_offered" {
		record["look_around"] = "offered"
		if !onbHasSuggestions(event.Suggest) {
			return OnboardingStep{record, map[string]any{"error": "offer first"}, "offer_first"}
		}
	}
	for _, earlier := range OnboardingBeatIDs {
		if earlier == event.Beat {
			break
		}
		if beats[earlier] == onbShown {
			beats[earlier] = onbDone
		}
	}
	if beats[event.Beat] != onbDone {
		beats[event.Beat] = onbShown
	}
	record["waiting"] = map[string]any{"beat": event.Beat, "tool_call_id": event.ToolCallID, "since": now}
	return OnboardingStep{record, map[string]any{}, ""}
}

// Reports whether the value is a list of strings
func onbIsNames(value any) bool {
	list, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		if _, ok := item.(string); !ok {
			return false
		}
	}
	return true
}

// OnboardingValidAnswer checks a card answer carries only its beat's pick keys with ids from the card's own options
func OnboardingValidAnswer(beat string, answer map[string]any, facts OnboardingFacts, picks map[string]any) bool {
	spec := onboardingBeatByID(beat)
	if label, present := answer["label"]; present {
		if _, ok := label.(string); !ok {
			return false
		}
	}
	filtered := map[string]any{}
	for k, v := range answer {
		if k != "label" {
			filtered[k] = v
		}
	}
	if len(filtered) == 0 {
		return false
	}
	for k := range filtered {
		if !containsString(spec.Picks, k) {
			return false
		}
	}
	if spec.Values != nil {
		v, _ := filtered[spec.Value].(string)
		return containsString(spec.Values(facts, picks), v)
	}
	switch beat {
	case "name":
		name, ok := filtered["name"].(string)
		return ok && strings.TrimSpace(name) != ""
	case "theme":
		for _, v := range filtered {
			if v != nil {
				if _, ok := v.(string); !ok {
					return false
				}
			}
		}
		return true
	case "plugins":
		if !onbIsNames(filtered["installed"]) {
			return false
		}
		for _, name := range anyStrings(filtered["installed"]) {
			if !containsString(facts.Plugins, name) {
				return false
			}
		}
		return true
	}
	if _, ok := filtered["connected"]; !ok {
		return false
	}
	for _, v := range filtered {
		if !onbIsNames(v) {
			return false
		}
	}
	return true
}

func onbAnsweredHandler(record map[string]any, event OnbAnswered, facts OnboardingFacts, now float64) OnboardingStep {
	record["waiting"] = nil
	if !OnboardingValidAnswer(event.Beat, event.Answer, facts, onbPicks(record)) {
		return OnboardingStep{record, map[string]any{"beat": event.Beat, "error": "The card sent an answer this beat does not accept. " +
			fmt.Sprintf("Bring it back with setup_beat(beat=\"%s\").", event.Beat)}, ""}
	}
	onbSettle(record, event.Beat, event.Answer)
	result := map[string]any{"beat": event.Beat, "answer": copyMap(event.Answer), "next": onbNextField(record)}
	if event.Said != "" {
		result["said"] = event.Said
	}
	return OnboardingStep{record, result, onboardingBeatByID(event.Beat).after(onbPicks(record))}
}

// Records the beat's picks as the machine has them now and returns them for the result
func onbNotePresent(record map[string]any, beat string, present map[string]any) map[string]any {
	kept := map[string]any{}
	picks := onbPicks(record)
	for k, v := range present {
		if containsString(onboardingBeatByID(beat).Picks, k) {
			kept[k] = v
			picks[k] = v
		}
	}
	return kept
}

// Result map with the present picks merged in
func onbWithPresent(base map[string]any, present map[string]any) map[string]any {
	for k, v := range present {
		base[k] = v
	}
	return base
}

func onbWroteHandler(record map[string]any, event OnbWrote, _ OnboardingFacts, _ float64) OnboardingStep {
	record["waiting"] = nil
	return OnboardingStep{record, onbWithPresent(map[string]any{"beat": event.Beat, "wrote_instead": true}, onbNotePresent(record, event.Beat, event.Present)), "wrote"}
}

func onbTimedOutHandler(record map[string]any, event OnbTimedOut, _ OnboardingFacts, _ float64) OnboardingStep {
	record["waiting"] = nil
	return OnboardingStep{record, onbWithPresent(map[string]any{"beat": event.Beat, "timed_out": true}, onbNotePresent(record, event.Beat, event.Present)), "timed_out"}
}

// Beats a general tool applies, so a value given in words would only record a pick that never reaches the screen
var onboardingAppliedBy = map[string]string{"theme": "set_theme", "layout": "set_theme"}

func onbRecordedHandler(record map[string]any, event OnbRecorded, facts OnboardingFacts, _ float64) OnboardingStep {
	beat := onboardingBeatByID(event.Beat)
	if tool, ok := onboardingAppliedBy[event.Beat]; ok {
		return OnboardingStep{record, map[string]any{"error": fmt.Sprintf("Apply the %s with %s; its result settles this beat.", event.Beat, tool), "next": onbNextField(record)}, ""}
	}
	if beat.Values != nil {
		accepted := beat.Values(facts, onbPicks(record))
		if !containsString(accepted, event.Value) {
			return OnboardingStep{record, map[string]any{"error": fmt.Sprintf("value for %s must be one of: %s.", event.Beat, strings.Join(accepted, ", ")), "next": onbNextField(record)}, ""}
		}
	}
	patch := map[string]any{}
	if beat.Value != "" {
		patch[beat.Value] = event.Value
	}
	onbSettle(record, event.Beat, patch)
	present := onbNotePresent(record, event.Beat, event.Present)
	return OnboardingStep{record, onbWithPresent(map[string]any{"recorded": event.Beat, "next": onbNextField(record)}, present), beat.after(onbPicks(record))}
}

func onbSkippedHandler(record map[string]any, event OnbSkipped, _ OnboardingFacts, _ float64) OnboardingStep {
	onbBeats(record)[event.Beat] = onbSkipped
	present := onbNotePresent(record, event.Beat, event.Present)
	return OnboardingStep{record, onbWithPresent(map[string]any{"recorded": event.Beat, "skipped": true, "next": onbNextField(record)}, present), "moved_on"}
}

func onbEndHandler(record map[string]any, event OnbEnd, _ OnboardingFacts, _ float64) OnboardingStep {
	record["phase"], record["ended_by"], record["waiting"] = onbPhaseSkipped, event.Outcome, nil
	beats := onbBeats(record)
	for _, id := range OnboardingBeatIDs {
		if beats[id] != onbDone {
			beats[id] = onbSkipped
		}
	}
	return OnboardingStep{record, map[string]any{"skipped_all": true}, "skip_all"}
}

func onbDoneHandler(record map[string]any, _ OnbDone, _ OnboardingFacts, _ float64) OnboardingStep {
	beats := onbBeats(record)
	if beats["build"] == onbPending {
		return OnboardingStep{record, map[string]any{"error": "Onboarding ends after the first build is shown and its close card is " +
			"answered. If they want to stop, tell them about the Skip setup button.", "next": onbNextField(record)}, ""}
	}
	beats["build"] = onbDone
	return OnboardingStep{record, map[string]any{}, "complete"}
}

// onbAppliedBeat is one beat a general tool's result settles with the picks it recorded
type onbAppliedBeat struct {
	beat  string
	patch map[string]any
}

// The beats a general tool's result settles
func onbAppliedBeats(event OnbApplied) []onbAppliedBeat {
	result, args := event.Result, event.Args
	switch event.Tool {
	case "set_theme":
		values, _ := result["applied"].(map[string]any)
		if !truthy(result["applied"]) {
			values, _ = result["picked"].(map[string]any)
		}
		theme := map[string]any{}
		for _, key := range []string{"accent", "mode"} {
			if v, ok := values[key]; ok {
				theme[key] = v
			}
		}
		var settled []onbAppliedBeat
		if len(theme) > 0 {
			settled = append(settled, onbAppliedBeat{"theme", theme})
		}
		if truthy(values["layout"]) {
			settled = append(settled, onbAppliedBeat{"layout", map[string]any{"layout": values["layout"]}})
		}
		return settled
	case "manage_connections":
		if args["action"] == "display" {
			if _, ok := result["connected"]; ok {
				return []onbAppliedBeat{{"connectors", map[string]any{"connected": listOrEmpty(result["connected"]), "newly_connected": listOrEmpty(result["newly_connected"])}}}
			}
		}
	case "gui_tour":
		if args["action"] == "start" && !truthy(result["error"]) {
			preset := "full"
			if truthy(args["preset"]) {
				preset = fmt.Sprint(args["preset"])
			}
			return []onbAppliedBeat{{"tour", map[string]any{"tour": preset}}}
		}
	}
	return nil
}

// List copy of a value, empty for anything that is not a list
func listOrEmpty(v any) []any {
	out := toAnyList(v)
	if out == nil {
		return []any{}
	}
	return append([]any(nil), out...)
}

func onbAppliedHandler(record map[string]any, event OnbApplied, _ OnboardingFacts, _ float64) OnboardingStep {
	settled, repeated := "", ""
	beats := onbBeats(record)
	for _, applied := range onbAppliedBeats(event) {
		if beats[applied.beat] != onbDone {
			settled = applied.beat
		} else if _, ok := onboardingAppliedBy[applied.beat]; ok {
			repeated = applied.beat
		}
		onbSettle(record, applied.beat, applied.patch)
		if waiting, ok := record["waiting"].(map[string]any); ok && waiting["beat"] == applied.beat {
			record["waiting"] = nil
		}
	}
	reminder := ""
	if settled != "" {
		reminder = "after_" + settled
	} else if repeated != "" {
		reminder = "already_" + repeated
	}
	return OnboardingStep{record, map[string]any{}, reminder}
}

const onboardingEndedText = "Onboarding is over in this chat; it is a normal chat now."

// An ended run ignores general tool results and refuses setup_beat except a repeat of its own end
func onbEnded(record map[string]any, event OnboardingEvent) OnboardingStep {
	switch event.(type) {
	case OnbApplied:
		return OnboardingStep{record, map[string]any{}, ""}
	case OnbDone:
		if record["phase"] == onbPhaseDone {
			return OnboardingStep{record, map[string]any{"completed": true}, "complete"}
		}
	case OnbEnd:
		return OnboardingStep{record, map[string]any{"skipped_all": true}, ""}
	}
	return OnboardingStep{record, map[string]any{"error": onboardingEndedText}, ""}
}

// Deep copy of a record through JSON
func onbDeepCopy(record map[string]any) map[string]any {
	raw, _ := json.Marshal(record)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

// Structural equality of two records through JSON
func onbEqual(a, b map[string]any) bool {
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return string(ra) == string(rb)
}

// OnboardingApply returns the record after the event, the model facing result, and the reminder key
func OnboardingApply(record map[string]any, event OnboardingEvent, facts OnboardingFacts, now float64) OnboardingStep {
	working := onbDeepCopy(record)
	if !OnboardingActive(working) {
		return onbEnded(working, event)
	}
	var step OnboardingStep
	switch e := event.(type) {
	case OnbShown:
		step = onbShownHandler(working, e, facts, now)
	case OnbAnswered:
		step = onbAnsweredHandler(working, e, facts, now)
	case OnbWrote:
		step = onbWroteHandler(working, e, facts, now)
	case OnbTimedOut:
		step = onbTimedOutHandler(working, e, facts, now)
	case OnbRecorded:
		step = onbRecordedHandler(working, e, facts, now)
	case OnbSkipped:
		step = onbSkippedHandler(working, e, facts, now)
	case OnbEnd:
		step = onbEndHandler(working, e, facts, now)
	case OnbDone:
		step = onbDoneHandler(working, e, facts, now)
	case OnbApplied:
		step = onbAppliedHandler(working, e, facts, now)
	}
	if OnboardingActive(step.Record) && onbBeats(step.Record)["build"] == onbDone {
		step.Record["phase"], step.Record["ended_by"], step.Record["waiting"] = onbPhaseDone, "done", nil
		result := copyMap(step.Result)
		result["completed"] = true
		step.Result = result
	}
	if !onbEqual(step.Record, record) {
		step.Record["updated_at"] = now
	}
	return step
}

// OnboardingAppliesTo reports whether a general tool's result can settle a beat
func OnboardingAppliesTo(tool string) bool {
	return tool == "set_theme" || tool == "manage_connections" || tool == "gui_tour"
}
