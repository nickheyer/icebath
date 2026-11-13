package config

import (
	"math"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Mixture of Agents section, with the flat preset keys of older configs that read as the default preset
type MoAConfig struct {
	DefaultPreset string        `yaml:"default_preset"`
	ActivePreset  string        `yaml:"active_preset"`
	SaveTraces    bool          `yaml:"save_traces"`
	TraceDir      string        `yaml:"trace_dir"`
	PrivacyFilter Scalar        `yaml:"privacy_filter"`
	Presets       MoAPresets    `yaml:"presets"`
	Flat          moaFlatPreset `yaml:",inline"`
	rest          rest
}

// Preset keys written directly under moa by older configs
type moaFlatPreset struct {
	ReferenceModels         *MoASlots `yaml:"reference_models,omitempty"`
	Aggregator              *MoASlot  `yaml:"aggregator,omitempty"`
	ReferenceTemperature    *float64  `yaml:"reference_temperature,omitempty"`
	AggregatorTemperature   *float64  `yaml:"aggregator_temperature,omitempty"`
	ReferenceTimeout        Scalar    `yaml:"reference_timeout,omitempty"`
	DegradedReferencePolicy string    `yaml:"degraded_reference_policy,omitempty"`
	Fanout                  *Fanout   `yaml:"fanout,omitempty"`
	Enabled                 Scalar    `yaml:"enabled,omitempty"`
}

type moaFields MoAConfig

// Decodes the mapping and keeps unknown keys
func (m *MoAConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*moaFields)(m), &m.rest)
}

// Writes the mapping with unknown keys after the named ones
func (m MoAConfig) MarshalYAML() (any, error) {
	return encodeMapping(moaFields(m), m.rest)
}

// Advisor privacy filter levels, true reading as full
func MoAPrivacyFilters() []string {
	return []string{"", "display", "full"}
}

// One named MoA preset
type MoAPreset struct {
	ReferenceModels         MoASlots `yaml:"reference_models,omitempty"`
	Aggregator              *MoASlot `yaml:"aggregator,omitempty"`
	ReferenceTemperature    *float64 `yaml:"reference_temperature,omitempty"`
	AggregatorTemperature   *float64 `yaml:"aggregator_temperature,omitempty"`
	ReferenceTimeout        Scalar   `yaml:"reference_timeout,omitempty"`
	DegradedReferencePolicy string   `yaml:"degraded_reference_policy,omitempty"`
	Fanout                  *Fanout  `yaml:"fanout,omitempty"`
	Enabled                 Scalar   `yaml:"enabled,omitempty"`
	raw                     *yaml.Node
	rest                    rest
}

type moaPresetFields MoAPreset

// Decodes the mapping keeping unknown keys, holding any other value as written since it reads as default settings
func (p *MoAPreset) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if n.Kind != yaml.MappingNode {
		*p = MoAPreset{raw: plainCopy(n)}
		return nil
	}
	p.raw = nil
	return decodeEntry(n, (*moaPresetFields)(p), &p.rest)
}

// Writes the mapping, or the other value exactly as read
func (p MoAPreset) MarshalYAML() (any, error) {
	if p.raw != nil {
		return emitted(p.raw), nil
	}
	return encodeMapping(moaPresetFields(p), p.rest)
}

// Keeps an explicit null as a preset of defaults
func (MoAPreset) holdsNull() {}

// Failed-advisor disclosure policies, unknown values reading as loud
func DegradedReferencePolicies() []string {
	return []string{"loud", "silent"}
}

// One MoA advisor or aggregator model slot
type MoASlot struct {
	Provider        string           `yaml:"provider,omitempty"`
	Model           string           `yaml:"model,omitempty"`
	ReasoningEffort *ReasoningEffort `yaml:"reasoning_effort,omitempty"`
	Enabled         Scalar           `yaml:"enabled,omitempty"`
	raw             *yaml.Node
	rest            rest
}

type moaSlotFields MoASlot

// Decodes a mapping slot keeping unknown keys, and holds a stray non-mapping item as written
func (s *MoASlot) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if n.Kind != yaml.MappingNode {
		*s = MoASlot{raw: plainCopy(n)}
		return nil
	}
	s.raw = nil
	return decodeEntry(n, (*moaSlotFields)(s), &s.rest)
}

// Writes the slot, or the stray item exactly as read
func (s MoASlot) MarshalYAML() (any, error) {
	if s.raw != nil {
		return emitted(s.raw), nil
	}
	return encodeMapping(moaSlotFields(s), s.rest)
}

// Keeps an explicit null as a stray item
func (MoASlot) holdsNull() {}

// Reports a mapping slot rather than a stray scalar or list item
func (s MoASlot) Valid() bool {
	return s.raw == nil
}

// Advisor list written as a list, a single slot mapping, or a JSON array string, kept in that form
type MoASlots struct {
	Slots  []MoASlot
	Single bool
	JSON   string
	isJSON bool
}

// Builds the list form
func SlotsOf(slots ...MoASlot) MoASlots {
	if slots == nil {
		slots = []MoASlot{}
	}
	return MoASlots{Slots: slots}
}

// Accepts a list of slots, one slot mapping, or a string holding a JSON array of slots
func (s *MoASlots) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	switch n.Kind {
	case yaml.SequenceNode:
		var slots []MoASlot
		_, err := decodeValue(n, reflect.ValueOf(&slots).Elem(), coerceDefault)
		*s = SlotsOf(slots...)
		return err
	case yaml.MappingNode:
		var one MoASlot
		err := one.UnmarshalYAML(n)
		*s = MoASlots{Slots: []MoASlot{one}, Single: true}
		return err
	}
	*s = MoASlots{JSON: scalarText(n), isJSON: true}
	var doc yaml.Node
	if yaml.Unmarshal([]byte(s.JSON), &doc) == nil && len(doc.Content) == 1 && doc.Content[0].Kind == yaml.SequenceNode {
		var slots []MoASlot
		if hold, err := decodeValue(doc.Content[0], reflect.ValueOf(&slots).Elem(), coerceDefault); !hold && err == nil {
			s.Slots = slots
		}
	}
	return nil
}

// Writes the form that was read
func (s MoASlots) MarshalYAML() (any, error) {
	switch {
	case s.isJSON:
		return strNode(s.JSON), nil
	case s.Single && len(s.Slots) == 1:
		return s.Slots[0], nil
	case s.Slots == nil:
		return []MoASlot{}, nil
	}
	return s.Slots, nil
}

// Reports an unset list so omitempty drops it
func (s MoASlots) IsZero() bool {
	return s.Slots == nil && !s.isJSON
}

// Advisor fan-out cadence written as a word or a mode and count mapping, kept in that form
type Fanout struct {
	Mode    string
	N       Scalar
	Mapping bool
	rest    rest
}

// Accepts per_iteration, user_turn, every_n with a count, or a mapping of mode and n
func (f *Fanout) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	switch n.Kind {
	case yaml.ScalarNode:
		*f = Fanout{Mode: scalarText(n)}
		return nil
	case yaml.MappingNode:
		out := Fanout{Mapping: true}
		for _, p := range mappingPairs(n) {
			switch p.key.Value {
			case "mode":
				out.Mode = scalarText(p.value)
			case "n":
				if out.N.UnmarshalYAML(p.value) != nil {
					out.rest.set(p.key, p.value)
				}
			default:
				out.rest.set(p.key, p.value)
			}
		}
		*f = out
		return nil
	}
	return shapeError(n, yaml.ScalarNode)
}

// Writes the form that was read
func (f Fanout) MarshalYAML() (any, error) {
	if !f.Mapping {
		return strNode(f.Mode), nil
	}
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap}
	m.Content = append(m.Content, strNode("mode"), strNode(f.Mode))
	if !f.N.IsZero() {
		nn, err := f.N.MarshalYAML()
		if err != nil {
			return nil, err
		}
		m.Content = append(m.Content, strNode("n"), nn.(*yaml.Node))
	}
	m.Content = append(m.Content, f.rest.emit()...)
	return m, nil
}

// Canonical cadence per_iteration, user_turn or every_n with N of at least 2, user_turn when unreadable
func (f *Fanout) Cadence() string {
	if f == nil {
		return "user_turn"
	}
	everyN := func(n int) string {
		switch {
		case n >= 2:
			return "every_n:" + strconv.Itoa(n)
		case n == 1:
			return "per_iteration"
		}
		return "user_turn"
	}
	mode := strings.ToLower(strings.TrimSpace(f.Mode))
	if f.Mapping && mode == "every_n" {
		return everyN(countOf(f.N))
	}
	switch {
	case mode == "per_iteration", mode == "user_turn":
		return mode
	case strings.HasPrefix(mode, "every_n"):
		_, count, ok := strings.Cut(mode, ":")
		if !ok {
			return "user_turn"
		}
		return everyN(countOf(Text(strings.TrimSpace(count))))
	}
	return "user_turn"
}

// Whole count a cadence value names, booleans as one and zero, floats and float text truncated, zero when unreadable
func countOf(s Scalar) int {
	switch s.Kind() {
	case KindBool:
		if b, _ := s.AsBool(); b {
			return 1
		}
		return 0
	case KindInt, KindFloat:
		f, ok := s.AsFloat()
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) >= math.MaxInt32 {
			return 0
		}
		return int(f)
	case KindString:
		text := strings.TrimSpace(s.String())
		if v, err := strconv.Atoi(text); err == nil {
			return v
		}
		f, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) >= math.MaxInt32 {
			return 0
		}
		return int(f)
	}
	return 0
}

// Defaults of the MoA section
func defaultMoA() MoAConfig {
	return MoAConfig{
		DefaultPreset: "default",
		PrivacyFilter: Text(""),
		Presets: MoAPresets{MapOf([]string{"default"}, []MoAPreset{{
			ReferenceModels: SlotsOf(
				MoASlot{Provider: "openai-codex", Model: "gpt-5.5"},
				MoASlot{Provider: "openrouter", Model: "deepseek/deepseek-v4-pro"},
			),
			Aggregator: &MoASlot{Provider: "openrouter", Model: "anthropic/claude-opus-4.8"},
			Enabled:    Bool(true),
		}})},
	}
}

// Documentation of the MoA section
func moaDocs() map[string]string {
	return map[string]string{
		"moa":                "Mixture of Agents: named presets used by /moa. A preset is an execution mode around the main model, not a model itself: references + aggregator synthesize private guidance before each main-model iteration.",
		"moa.default_preset": "Preset /moa uses when none is named.",
		"moa.active_preset":  "Preset currently engaged; empty = off.",
		"moa.save_traces":    "Write each MoA turn (reference + aggregator exact input, output and usage) as JSONL to <icebath_home>/moa-traces/<session_id>.jsonl (or trace_dir) for auditing.",
		"moa.trace_dir":      "Directory for MoA trace files; empty = <icebath_home>/moa-traces.",
		"moa.privacy_filter": "PII and credential redaction of advisor outputs: \"\" off | \"display\" (UI reference blocks + traces only; the aggregator sees raw advisor text) | \"full\" (also the advisor text injected into the aggregator prompt). Advisors can echo PII from the conversation (emails, formatted phone numbers) and credential shapes into reference blocks, traces, and the aggregator prompt.",
		"moa.presets":        "Named presets: reference_models (provider, model, optional reasoning_effort and enabled per slot), aggregator, reference_temperature, aggregator_temperature, reference_timeout, degraded_reference_policy (loud | silent), fanout (user_turn | per_iteration | every_n:<N>), enabled.",
	}
}

// Preset map that a user-written presets key replaces whole, so deleted built-in presets stay deleted
type MoAPresets struct {
	Map[MoAPreset]
}

// Replaces every preset with the presets the file names, a value of another shape leaving no presets at all
func (p *MoAPresets) UnmarshalYAML(n *yaml.Node) error {
	return p.decodeCoerced(n, coerceDefault)
}

// Replacing decode the field decoder calls in place of the promoted merging one
func (p *MoAPresets) decodeCoerced(n *yaml.Node, c coercion) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if isNull(n) {
		return nil
	}
	p.Map = Map[MoAPreset]{}
	return p.Map.decodeCoerced(n, c)
}
