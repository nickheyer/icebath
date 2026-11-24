package gateway

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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
	obj := jsonutil.ObjectOf("contract_version", d.ContractVersion, "platform", d.Platform, "label", d.Label,
		"max_message_length", d.MaxMessageLength, "supports_draft_streaming", d.SupportsDraftStreaming,
		"supports_edit", d.SupportsEdit, "supports_threads", d.SupportsThreads, "markdown_dialect", d.MarkdownDialect,
		"len_unit", d.LenUnit, "emoji", d.Emoji, "platform_hint", d.PlatformHint, "pii_safe", d.PIISafe,
		"supports_context", d.SupportsContext, "supports_inchannel_continuable", d.SupportsInchannelContinuable,
		"supports_block_formatting", d.SupportsBlockFormatting, "supported_ops", ops)
	return jsonutil.Encode(obj, jsonutil.SortKeys())
}

var relayDescriptorRequired = []string{"contract_version", "platform", "label", "max_message_length",
	"supports_draft_streaming", "supports_edit", "supports_threads", "markdown_dialect", "len_unit"}

// Descriptor from handshake JSON, unknown keys dropped, garbage lengths and ops normalized, missing required keys an error
func CapabilityDescriptorFromJSON(data string) (CapabilityDescriptor, error) {
	parsed, err := jsonutil.Decode(data)
	if err != nil {
		return CapabilityDescriptor{}, err
	}
	raw, ok := jsonutil.AsObject(parsed)
	if !ok {
		return CapabilityDescriptor{}, fmt.Errorf("the capability descriptor is %s, expected a mapping", errchain.Kind(parsed))
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
	get := func(k string) any { return raw.ValueOr(k, nil) }
	d.ContractVersion, _ = relayDescriptorInt(get("contract_version"), 0)
	d.Platform = anyutil.String(get("platform"))
	d.Label = anyutil.String(get("label"))
	if d.MaxMessageLength, err = relayDescriptorInt(get("max_message_length"), 4096); err != nil {
		return CapabilityDescriptor{}, err
	}
	if d.MaxMessageLength <= 0 {
		d.MaxMessageLength = 4096
	}
	d.SupportsDraftStreaming = anyutil.IsTrue(get("supports_draft_streaming"))
	d.SupportsEdit = anyutil.IsTrue(get("supports_edit"))
	d.SupportsThreads = anyutil.IsTrue(get("supports_threads"))
	d.MarkdownDialect = anyutil.String(get("markdown_dialect"))
	d.LenUnit = anyutil.String(get("len_unit"))
	if v, has := raw.Get("emoji"); has {
		d.Emoji = anyutil.String(v)
	}
	if v, has := raw.Get("platform_hint"); has {
		d.PlatformHint = anyutil.String(v)
	}
	d.PIISafe = anyutil.IsTrue(get("pii_safe"))
	d.SupportsContext = anyutil.IsTrue(get("supports_context"))
	d.SupportsInchannelContinuable = anyutil.IsTrue(get("supports_inchannel_continuable"))
	d.SupportsBlockFormatting = anyutil.IsTrue(get("supports_block_formatting"))
	if ops, ok := get("supported_ops").([]any); ok {
		for _, op := range ops {
			if s, ok := op.(string); ok && s != "" {
				d.SupportedOps = append(d.SupportedOps, s)
			}
		}
	}
	return d, nil
}

// Integer of a JSON number, bool or decimal string, the fallback for any other value, a range error for an infinite or overflowing float
func relayDescriptorInt(v any, fallback int) (int, error) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case float64:
		n, err := anyutil.ToInt(x)
		if errors.Is(err, anyutil.ErrRange) {
			return 0, err
		}
		if err != nil {
			return fallback, nil
		}
		return int(n), nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			return fallback, nil
		}
		return int(n), nil
	}
	n, ok := anyutil.Int(v)
	if !ok {
		return fallback, nil
	}
	return int(n), nil
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
