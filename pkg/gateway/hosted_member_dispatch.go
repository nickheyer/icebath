package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Largest UTF-8 prompt one hosted room dispatch may carry
const hostedMaxPromptBytes = 256 * 1024

var (
	hostedIdentifierRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,255}$`)
	hostedDigestRE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Malformed or unauthorized peer room input, a ValueError
type HostedRoomPeerError struct{ Msg string }

func (e *HostedRoomPeerError) Error() string { return e.Msg }

// Python class name of the error
func (e *HostedRoomPeerError) PyTypeName() string { return "HostedRoomPeerError" }

// Recipient validated identity of one remote room member attempt
type HostedMemberDispatch struct {
	ProtocolVersion       *big.Int
	RoomID                string
	HomeInstallID         string
	AuthorityGatewayID    string
	AuthorityEpoch        *big.Int
	MemberID              string
	TargetInstallID       string
	TargetProfile         string
	TaskID                string
	ExecutionGeneration   *big.Int
	SourceEventSeq        *big.Int
	CancellationScopeID   string
	Prompt                string
	PromptDigest          string
	CapabilityDigest      string
	ExecutionPolicyDigest string
	TraceID               string
}

// Dispatch fields in validation order with the checker kind of each
var hostedDispatchFields = []struct{ name, kind string }{
	{"protocol_version", "int"}, {"room_id", "id"}, {"home_install_id", "id"}, {"authority_gateway_id", "id"},
	{"authority_epoch", "int"}, {"member_id", "id"}, {"target_install_id", "id"}, {"target_profile", "id"},
	{"task_id", "id"}, {"execution_generation", "int"}, {"source_event_seq", "int"}, {"cancellation_scope_id", "id"},
	{"capability_digest", "digest"}, {"execution_policy_digest", "digest"}, {"trace_id", "id"}}

// Stripped bounded identifier of a peer field
func hostedIdentifier(value any, field string) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", &HostedRoomPeerError{field + " must be a string"}
	}
	text = pystr.Strip(text)
	if text == "" || utf8.RuneCountInString(text) > 256 || !hostedIdentifierRE.MatchString(text) {
		return "", &HostedRoomPeerError{field + " is invalid"}
	}
	return text, nil
}

// Positive non bool integer of a peer field
func hostedPositiveInt(value any, field string) (*big.Int, error) {
	if _, isBool := value.(bool); !isBool {
		if n, ok := pyrepr.Int(value); ok && n.Sign() >= 1 {
			return n, nil
		}
	}
	return nil, &HostedRoomPeerError{field + " must be a positive integer"}
}

// Lowercase sha256 hex digest of a peer field
func hostedDigest(value any, field string) (string, error) {
	text, ok := value.(string)
	if !ok || !hostedDigestRE.MatchString(text) {
		return "", &HostedRoomPeerError{field + " must be a sha256 digest"}
	}
	return text, nil
}

// Mapping view of a peer value, false for anything that is no object
func hostedMapping(value any) (map[string]any, bool) {
	switch v := value.(type) {
	case map[string]any:
		return v, v != nil
	case *pyjson.Object:
		if v == nil {
			return nil, false
		}
		out := make(map[string]any, len(v.Keys))
		for _, key := range v.Keys {
			out[key], _ = v.Get(key)
		}
		return out, true
	}
	return nil, false
}

// Requires exactly the required keys of a peer object, naming the missing or unknown ones sorted
func hostedExactFields(value any, label string, required map[string]bool) (map[string]any, error) {
	fields, ok := hostedMapping(value)
	if !ok {
		return nil, &HostedRoomPeerError{label + " must be an object"}
	}
	var missing, unknown []string
	for key := range required {
		if _, present := fields[key]; !present {
			missing = append(missing, key)
		}
	}
	for key := range fields {
		if !required[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(unknown)
	if len(missing) > 0 {
		return nil, &HostedRoomPeerError{label + " missing fields: " + strings.Join(missing, ", ")}
	}
	if len(unknown) > 0 {
		return nil, &HostedRoomPeerError{label + " unknown fields: " + strings.Join(unknown, ", ")}
	}
	return fields, nil
}

// Dispatch validated from its wire mapping, its prompt checked against the prompt digest
func HostedMemberDispatchFromMapping(value any) (*HostedMemberDispatch, error) {
	required := map[string]bool{"prompt": true, "prompt_digest": true}
	for _, field := range hostedDispatchFields {
		required[field.name] = true
	}
	fields, err := hostedExactFields(value, "dispatch", required)
	if err != nil {
		return nil, err
	}
	prompt, ok := fields["prompt"].(string)
	if !ok || pystr.Strip(prompt) == "" {
		return nil, &HostedRoomPeerError{"prompt must be a non-empty string"}
	}
	if len(prompt) > hostedMaxPromptBytes {
		return nil, &HostedRoomPeerError{"prompt is too large"}
	}
	promptDigest, err := hostedDigest(fields["prompt_digest"], "prompt_digest")
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(prompt))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(promptDigest)) != 1 {
		return nil, &HostedRoomPeerError{"prompt_digest does not match prompt"}
	}
	texts := map[string]string{}
	ints := map[string]*big.Int{}
	for _, field := range hostedDispatchFields {
		switch field.kind {
		case "int":
			n, err := hostedPositiveInt(fields[field.name], field.name)
			if err != nil {
				return nil, err
			}
			ints[field.name] = n
		case "id":
			text, err := hostedIdentifier(fields[field.name], field.name)
			if err != nil {
				return nil, err
			}
			texts[field.name] = text
		default:
			text, err := hostedDigest(fields[field.name], field.name)
			if err != nil {
				return nil, err
			}
			texts[field.name] = text
		}
	}
	return &HostedMemberDispatch{
		ProtocolVersion: ints["protocol_version"], RoomID: texts["room_id"], HomeInstallID: texts["home_install_id"],
		AuthorityGatewayID: texts["authority_gateway_id"], AuthorityEpoch: ints["authority_epoch"], MemberID: texts["member_id"],
		TargetInstallID: texts["target_install_id"], TargetProfile: texts["target_profile"], TaskID: texts["task_id"],
		ExecutionGeneration: ints["execution_generation"], SourceEventSeq: ints["source_event_seq"],
		CancellationScopeID: texts["cancellation_scope_id"], Prompt: prompt, PromptDigest: promptDigest,
		CapabilityDigest: texts["capability_digest"], ExecutionPolicyDigest: texts["execution_policy_digest"],
		TraceID: texts["trace_id"]}, nil
}

// Home install, room, member and target profile a hosted member dispatch names
func hostedMemberIdentity(hostedDispatch map[string]any) ([]string, error) {
	dispatch, err := HostedMemberDispatchFromMapping(hostedDispatch)
	if err != nil {
		return nil, err
	}
	return []string{dispatch.HomeInstallID, dispatch.RoomID, dispatch.MemberID, dispatch.TargetProfile}, nil
}
