package gateway

import (
	"fmt"
	"math"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// RelayContractVersion is the additive only handshake schema version
const RelayContractVersion = 1

// RelayLegacyOps are the ops assumed when a legacy connector advertises none
var RelayLegacyOps = []string{"send", "edit", "typing", "follow_up"}

// CapabilityDescriptor is the immutable capability profile negotiated at the relay handshake
type CapabilityDescriptor struct {
	ContractVersion              int
	Platform                     string
	Label                        string
	MaxMessageLength             int
	SupportsDraftStreaming       bool
	SupportsEdit                 bool
	SupportsThreads              bool
	MarkdownDialect              string
	LenUnit                      string
	Emoji                        string
	PlatformHint                 string
	PIISafe                      bool
	SupportsContext              bool
	SupportsInchannelContinuable bool
	SupportsBlockFormatting      bool
	SupportedOps                 []string
}

// Descriptor with the dataclass defaults for the optional fields
func NewCapabilityDescriptor(contractVersion int, platform, label string, maxMessageLength int, supportsDraftStreaming, supportsEdit, supportsThreads bool, markdownDialect, lenUnit string) CapabilityDescriptor {
	return CapabilityDescriptor{ContractVersion: contractVersion, Platform: platform, Label: label, MaxMessageLength: maxMessageLength,
		SupportsDraftStreaming: supportsDraftStreaming, SupportsEdit: supportsEdit, SupportsThreads: supportsThreads,
		MarkdownDialect: markdownDialect, LenUnit: lenUnit, Emoji: "\U0001f50c", SupportedOps: []string{}}
}

// Whether the connector advertises the op, the legacy set standing in when none is advertised
func (d CapabilityDescriptor) SupportsOp(op string) bool {
	if len(d.SupportedOps) > 0 {
		return slices.Contains(d.SupportedOps, op)
	}
	return slices.Contains(RelayLegacyOps, op)
}

// Sorted key JSON of every field for the handshake frame
func (d CapabilityDescriptor) ToJSON() (string, error) {
	ops := make([]any, len(d.SupportedOps))
	for i, op := range d.SupportedOps {
		ops[i] = op
	}
	obj := pyjson.ObjectOf("contract_version", d.ContractVersion, "platform", d.Platform, "label", d.Label,
		"max_message_length", d.MaxMessageLength, "supports_draft_streaming", d.SupportsDraftStreaming,
		"supports_edit", d.SupportsEdit, "supports_threads", d.SupportsThreads, "markdown_dialect", d.MarkdownDialect,
		"len_unit", d.LenUnit, "emoji", d.Emoji, "platform_hint", d.PlatformHint, "pii_safe", d.PIISafe,
		"supports_context", d.SupportsContext, "supports_inchannel_continuable", d.SupportsInchannelContinuable,
		"supports_block_formatting", d.SupportsBlockFormatting, "supported_ops", ops)
	return pyjson.Dumps(obj, pyjson.SortKeys(true), pyjson.EnsureASCII(false))
}

var relayDescriptorRequired = []string{"contract_version", "platform", "label", "max_message_length",
	"supports_draft_streaming", "supports_edit", "supports_threads", "markdown_dialect", "len_unit"}

// Descriptor from handshake JSON, unknown keys dropped, garbage lengths and ops normalized, missing required keys an error
func CapabilityDescriptorFromJSON(data string) (CapabilityDescriptor, error) {
	parsed, err := pyjson.Loads(data)
	if err != nil {
		return CapabilityDescriptor{}, err
	}
	raw, ok := pyjson.AsObject(parsed)
	if !ok {
		return CapabilityDescriptor{}, fmt.Errorf("'%s' object has no attribute 'items'", pyrepr.TypeName(parsed))
	}
	var missing []string
	for _, k := range relayDescriptorRequired {
		if _, has := raw.Get(k); !has {
			missing = append(missing, "'"+k+"'")
		}
	}
	if len(missing) > 0 {
		return CapabilityDescriptor{}, relayMissingArgsError(missing)
	}
	d := CapabilityDescriptor{Emoji: "\U0001f50c", SupportedOps: []string{}}
	get := func(k string) any { return raw.GetOr(k, nil) }
	d.ContractVersion, _ = relayDescriptorInt(get("contract_version"), 0)
	d.Platform = pyrepr.Str(get("platform"))
	d.Label = pyrepr.Str(get("label"))
	if d.MaxMessageLength, err = relayDescriptorInt(get("max_message_length"), 4096); err != nil {
		return CapabilityDescriptor{}, err
	}
	if d.MaxMessageLength <= 0 {
		d.MaxMessageLength = 4096
	}
	d.SupportsDraftStreaming = pyrepr.Truthy(get("supports_draft_streaming"))
	d.SupportsEdit = pyrepr.Truthy(get("supports_edit"))
	d.SupportsThreads = pyrepr.Truthy(get("supports_threads"))
	d.MarkdownDialect = pyrepr.Str(get("markdown_dialect"))
	d.LenUnit = pyrepr.Str(get("len_unit"))
	if v, has := raw.Get("emoji"); has {
		d.Emoji = pyrepr.Str(v)
	}
	if v, has := raw.Get("platform_hint"); has {
		d.PlatformHint = pyrepr.Str(v)
	}
	d.PIISafe = pyrepr.Truthy(get("pii_safe"))
	d.SupportsContext = pyrepr.Truthy(get("supports_context"))
	d.SupportsInchannelContinuable = pyrepr.Truthy(get("supports_inchannel_continuable"))
	d.SupportsBlockFormatting = pyrepr.Truthy(get("supports_block_formatting"))
	if ops, ok := get("supported_ops").([]any); ok {
		for _, op := range ops {
			if s, ok := op.(string); ok && s != "" {
				d.SupportedOps = append(d.SupportedOps, s)
			}
		}
	}
	return d, nil
}

// Python int of a JSON number, bool or numeric string, the fallback where int raises TypeError or ValueError
func relayDescriptorInt(v any, fallback int) (int, error) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case float64:
		if math.IsInf(x, 0) {
			return 0, &PyError{"OverflowError", "cannot convert float infinity to integer"}
		}
		if math.IsNaN(x) {
			return fallback, nil
		}
		return int(x), nil
	case string:
		n, ok := pyint.Parse(x)
		if !ok || !n.IsInt64() {
			return fallback, nil
		}
		return int(n.Int64()), nil
	}
	n, ok := pyrepr.Int(v)
	if !ok || !n.IsInt64() {
		return fallback, nil
	}
	return int(n.Int64()), nil
}

// TypeError text of a dataclass call missing required positional arguments
func relayMissingArgsError(missing []string) error {
	noun := "argument"
	if len(missing) > 1 {
		noun = "arguments"
	}
	list := missing[0]
	if len(missing) == 2 {
		list = missing[0] + " and " + missing[1]
	} else if len(missing) > 2 {
		list = ""
		for i, m := range missing[:len(missing)-1] {
			if i > 0 {
				list += ", "
			}
			list += m
		}
		list += ", and " + missing[len(missing)-1]
	}
	return &PyError{"TypeError", fmt.Sprintf("CapabilityDescriptor.__init__() missing %d required positional %s: %s", len(missing), noun, list)}
}
