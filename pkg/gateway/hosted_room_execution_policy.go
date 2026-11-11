package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"sort"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	roomPolicyVersion   = 1
	maxPolicyToolsets   = 128
	MaxPolicyIterations = 1<<53 - 1
)

var roomPolicyFields = map[string]bool{
	"version": true, "target_profile": true, "enabled_toolsets": true, "approval_mode": true,
	"max_iterations": true, "policy_digest": true}

// A RoomLink execution policy that is malformed or no longer current, a ValueError
type RoomExecutionPolicyError struct{ Msg string }

func (e *RoomExecutionPolicyError) Error() string { return e.Msg }

// Python class name of the error
func (e *RoomExecutionPolicyError) PyTypeName() string { return "RoomExecutionPolicyError" }

func roomPolicyError(msg string) error { return &RoomExecutionPolicyError{msg} }

// Immutable target policy applied at the agent and approval boundaries
type RoomExecutionPolicy struct {
	Version         int64
	TargetProfile   string
	EnabledToolsets []string
	ApprovalMode    string
	MaxIterations   int64
	PolicyDigest    string
}

// Sha256 hex of the compact ASCII JSON of the unsigned policy
func roomPolicyDigest(unsigned *pyjson.Object) (string, error) {
	text, err := roomCompactJSON(unsigned, true)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:]), nil
}

// Identifier of a policy field after str of the value, so non strings also fail as invalid
func roomPolicyIdentifier(value any, field string) (string, error) {
	text := ""
	if pyrepr.Truthy(value) {
		text = pyrepr.Str(value)
	}
	return roomIdentifier(text, roomIdentifierSpec{label: field, invalid: field + " is invalid"}, roomPolicyError)
}

// Policy validated from its wire mapping with its digest recomputed and checked
func RoomExecutionPolicyFromMapping(value any) (*RoomExecutionPolicy, error) {
	fields, ok := hostedMapping(value)
	if !ok || len(fields) != len(roomPolicyFields) {
		return nil, roomPolicyError("execution policy fields are invalid")
	}
	for key := range fields {
		if !roomPolicyFields[key] {
			return nil, roomPolicyError("execution policy fields are invalid")
		}
	}
	if !pyjson.Equal(fields["version"], int64(roomPolicyVersion)) {
		return nil, roomPolicyError("execution policy version is unsupported")
	}
	targetProfile, err := roomPolicyIdentifier(fields["target_profile"], "target_profile")
	if err != nil {
		return nil, err
	}
	rawToolsets, isList := fields["enabled_toolsets"].([]any)
	if !isList || len(rawToolsets) < 1 || len(rawToolsets) > maxPolicyToolsets {
		return nil, roomPolicyError("enabled_toolsets are invalid")
	}
	toolsets := make([]string, 0, len(rawToolsets))
	seen := map[string]bool{}
	for _, item := range rawToolsets {
		name, err := roomPolicyIdentifier(item, "enabled_toolset")
		if err != nil {
			return nil, err
		}
		toolsets = append(toolsets, name)
		seen[name] = true
	}
	sort.Strings(toolsets)
	if len(seen) != len(toolsets) || !seen["bot_room"] {
		return nil, roomPolicyError("enabled_toolsets are invalid")
	}
	approvalMode := ""
	if pyrepr.Truthy(fields["approval_mode"]) {
		approvalMode = pyrepr.Str(fields["approval_mode"])
	}
	approvalMode = pystr.Lower(pystr.Strip(approvalMode))
	if approvalMode != "manual" && approvalMode != "smart" && approvalMode != "off" {
		return nil, roomPolicyError("approval_mode is invalid")
	}
	maxIterations, err := roomBoundedInt(fields["max_iterations"], roomPolicyError, "max_iterations is invalid", 1, big.NewInt(MaxPolicyIterations))
	if err != nil {
		return nil, err
	}
	policy := &RoomExecutionPolicy{Version: roomPolicyVersion, TargetProfile: targetProfile, EnabledToolsets: toolsets,
		ApprovalMode: approvalMode, MaxIterations: maxIterations.Int64()}
	digest, err := roomPolicyDigest(policy.unsigned())
	if err != nil {
		return nil, err
	}
	supplied := ""
	if pyrepr.Truthy(fields["policy_digest"]) {
		supplied = pyrepr.Str(fields["policy_digest"])
	}
	supplied = pystr.Lower(pystr.Strip(supplied))
	if supplied != digest {
		return nil, roomPolicyError("policy_digest does not match the execution policy")
	}
	policy.PolicyDigest = supplied
	return policy, nil
}

// Policy fields without the digest, in dataclass order
func (p *RoomExecutionPolicy) unsigned() *pyjson.Object {
	toolsets := make([]any, len(p.EnabledToolsets))
	for i, name := range p.EnabledToolsets {
		toolsets[i] = name
	}
	return pyjson.ObjectOf("version", p.Version, "target_profile", p.TargetProfile, "enabled_toolsets", toolsets,
		"approval_mode", p.ApprovalMode, "max_iterations", p.MaxIterations)
}

// Wire mapping of the policy with its toolsets as a list
func (p *RoomExecutionPolicy) AsMapping() *pyjson.Object {
	mapping := p.unsigned()
	mapping.Set("policy_digest", p.PolicyDigest)
	return mapping
}

// Runtime scope of the served profile, unchanged for the active home and refused for an unknown profile
func servedProfileScope(ctx context.Context, targetProfile string) (context.Context, error) {
	canon, err := profiles.NormalizeProfileName(targetProfile)
	if err != nil {
		return nil, err
	}
	if canon == "default" || profiles.ProfileMatchesHome(ctx, targetProfile, "") {
		return ctx, nil
	}
	if !profiles.ProfileExists(targetProfile) {
		return nil, roomPolicyError("target_profile " + pyrepr.StrRepr(targetProfile) + " is not a live profile")
	}
	dir, err := profiles.GetProfileDir(targetProfile)
	if err != nil {
		return nil, err
	}
	return profileRuntimeScope(ctx, dir, true), nil
}

// Effective API server policy from the served profile's own config, loaded inside its scope when cfg is nil
func ExecutionPolicyMapping(ctx context.Context, targetProfile string, cfg any) (*pyjson.Object, error) {
	targetProfile, err := roomPolicyIdentifier(targetProfile, "target_profile")
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		scoped, err := servedProfileScope(ctx, targetProfile)
		if err != nil {
			return nil, err
		}
		return ExecutionPolicyMapping(scoped, targetProfile, loadGatewayConfig(scoped, ""))
	}
	tree, ok := hostedMapping(cfg)
	if !ok {
		return nil, roomPolicyError("gateway config is invalid")
	}
	copied := make(config.Tree, len(tree))
	for key, item := range tree {
		copied[key] = item
	}
	enabled := tools.Default.PlatformTools(ctx, "", copied, "api_server", tools.PlatformToolsOptions{IncludeDefaultMCPServers: true})
	names := []string{"bot_room"}
	for name, on := range enabled {
		if on && name != "bot_room" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	toolsets := make([]any, len(names))
	for i, name := range names {
		toolsets[i] = name
	}
	agentCfg, _ := hostedMapping(tree["agent"])
	approvals, _ := hostedMapping(tree["approvals"])
	approvalMode := "off"
	if !tools.YoloModeFrozen() {
		mode, present := approvals["mode"]
		if !present {
			mode = "manual"
		}
		approvalMode = tools.NormalizeApprovalMode(mode)
	}
	maxIterations := int64(config.ResolveTurnLimit(agentCfg["max_turns"], config.TurnLimitUnlimited))
	if maxIterations > MaxPolicyIterations {
		maxIterations = MaxPolicyIterations
	}
	unsigned := pyjson.ObjectOf("version", int64(roomPolicyVersion), "target_profile", targetProfile,
		"enabled_toolsets", toolsets, "approval_mode", approvalMode, "max_iterations", maxIterations)
	digest, err := roomPolicyDigest(unsigned)
	if err != nil {
		return nil, err
	}
	value := unsigned.Copy()
	value.Set("policy_digest", digest)
	policy, err := RoomExecutionPolicyFromMapping(value)
	if err != nil {
		return nil, err
	}
	return policy.AsMapping(), nil
}

type roomExecutionPolicyKey struct{}

// Context carrying policy as the current room execution policy
func BindRoomExecutionPolicy(ctx context.Context, policy *RoomExecutionPolicy) context.Context {
	return context.WithValue(ctx, roomExecutionPolicyKey{}, policy)
}

// Room execution policy the context carries, nil outside a room turn
func CurrentRoomExecutionPolicy(ctx context.Context) *RoomExecutionPolicy {
	policy, _ := ctx.Value(roomExecutionPolicyKey{}).(*RoomExecutionPolicy)
	return policy
}

const roomPolicyEnvKey = "hosted_room_execution_policy"

// Binds policy into the tool env a room turn's approvals read and returns the call restoring the previous one
func BindRoomExecutionPolicyEnv(env *tools.Env, policy *RoomExecutionPolicy) (reset func()) {
	if env.Values == nil {
		env.Values = &sync.Map{}
	}
	previous, had := env.Values.Load(roomPolicyEnvKey)
	env.Values.Store(roomPolicyEnvKey, policy)
	return func() {
		if had {
			env.Values.Store(roomPolicyEnvKey, previous)
		} else {
			env.Values.Delete(roomPolicyEnvKey)
		}
	}
}

func init() {
	tools.RoomApprovalMode = func(env *tools.Env) (string, bool) {
		if env == nil || env.Values == nil {
			return "", false
		}
		value, _ := env.Values.Load(roomPolicyEnvKey)
		policy, _ := value.(*RoomExecutionPolicy)
		if policy == nil {
			return "", false
		}
		return policy.ApprovalMode, true
	}
}
