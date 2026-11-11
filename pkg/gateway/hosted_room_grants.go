package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Room scoped grant that is invalid or expired, a HostedRoomPeerError
type HostedRoomGrantError struct{ *HostedRoomPeerError }

func (e *HostedRoomGrantError) Unwrap() error { return e.HostedRoomPeerError }

// Python class name of the error
func (e *HostedRoomGrantError) PyTypeName() string { return "HostedRoomGrantError" }

func hostedGrantError(msg string) error {
	return &HostedRoomGrantError{&HostedRoomPeerError{msg}}
}

const (
	hostedMaxTokenBytes             = 16 * 1024
	hostedRoomGrantSecretFile       = ".room-link-grant-secret"
	MaxDispatchGrantTTLSeconds      = 24 * 60 * 60
	MaxStatusGrantTTLSeconds        = 30 * 24 * 60 * 60
	hostedInstallationGrantDomain   = "hermes-hosted-room-installation-grant-v1"
	hostedGrantSecretCacheMaxSize   = 32
	hostedGrantDefaultTTLSeconds    = 3600.0
	hostedGrantClockSkewToleranceS  = 30
	hostedGrantSignatureSeparator   = "."
	hostedGrantVersionField         = "version"
	hostedGrantPermissionsField     = "permissions"
	hostedGrantStatusExpiresAtField = "status_expires_at"
)

// Scope fields of a room grant in issue order
var hostedGrantScope = []string{"grant_id", "room_id", "home_install_id", "authority_gateway_id", "authority_epoch", "member_id",
	"target_install_id", "target_profile"}

// Exact field set of a grant, and of a refreshed grant carrying its status horizon
var hostedGrantFields = func() map[string]bool {
	fields := map[string]bool{"version": true, "execution_policy_digest": true, "permissions": true, "issued_at": true, "expires_at": true}
	for _, name := range hostedGrantScope {
		fields[name] = true
	}
	return fields
}()

var hostedGrantPermissions = map[string]bool{"approve": true, "dispatch": true, "status": true, "stop": true}

// Restart scoped grant secrets already derived, keyed by resolved installation root, least recent first
var hostedGrantSecrets = struct {
	sync.Mutex
	order []string
	byKey map[string][]byte
}{byKey: map[string][]byte{}}

// Reads the installation's raw secret, refusing a wrong length and tightening loose permissions
func hostedReadGrantSecret(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) != 32 {
		return nil, hostedGrantError("gateway RoomLink secret is invalid")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// Loads or atomically mints one installation root's secret and derives its grant signing key
func hostedGrantSecretForHome(homeValue string) ([]byte, error) {
	hostedGrantSecrets.Lock()
	if cached, ok := hostedGrantSecrets.byKey[homeValue]; ok {
		for i, key := range hostedGrantSecrets.order {
			if key == homeValue {
				hostedGrantSecrets.order = append(append(hostedGrantSecrets.order[:i:i], hostedGrantSecrets.order[i+1:]...), homeValue)
				break
			}
		}
		hostedGrantSecrets.Unlock()
		return cached, nil
	}
	hostedGrantSecrets.Unlock()
	if err := os.MkdirAll(homeValue, 0o777); err != nil {
		return nil, err
	}
	path := filepath.Join(homeValue, hostedRoomGrantSecretFile)
	material, err := hostedReadGrantSecret(path)
	if errors.Is(err, fs.ErrNotExist) {
		material, err = hostedMintGrantSecret(homeValue, path)
	}
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, material)
	mac.Write([]byte(hostedInstallationGrantDomain))
	derived := mac.Sum(nil)
	hostedGrantSecrets.Lock()
	defer hostedGrantSecrets.Unlock()
	if _, ok := hostedGrantSecrets.byKey[homeValue]; !ok {
		hostedGrantSecrets.order = append(hostedGrantSecrets.order, homeValue)
		if len(hostedGrantSecrets.order) > hostedGrantSecretCacheMaxSize {
			delete(hostedGrantSecrets.byKey, hostedGrantSecrets.order[0])
			hostedGrantSecrets.order = hostedGrantSecrets.order[1:]
		}
	}
	hostedGrantSecrets.byKey[homeValue] = derived
	return derived, nil
}

// Mints secret bytes in an exclusive temp file hard linked into place, a losing racer reading the winner's
func hostedMintGrantSecret(homeValue, path string) (material []byte, err error) {
	material = make([]byte, 32)
	if _, err := rand.Read(material); err != nil {
		return nil, err
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return nil, err
	}
	temporary := filepath.Join(homeValue, fmt.Sprintf(".%s.%d.%s", hostedRoomGrantSecretFile, os.Getpid(), hex.EncodeToString(suffix)))
	stream, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if removeErr := os.Remove(temporary); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) && err == nil {
			material, err = nil, removeErr
		}
	}()
	if _, err := stream.Write(material); err != nil {
		stream.Close()
		return nil, err
	}
	if err := stream.Sync(); err != nil {
		stream.Close()
		return nil, err
	}
	if err := stream.Close(); err != nil {
		return nil, err
	}
	if err := os.Link(temporary, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return hostedReadGrantSecret(path)
		}
		return nil, err
	}
	pyfs.FsyncDirectory(homeValue)
	return material, nil
}

// Gateway only RoomLink signing secret of the installation root, minted atomically on first use
func GatewayRoomGrantSecret(ctx context.Context, root string) ([]byte, error) {
	if root == "" {
		root = os.Getenv("ICEBATH_HOME")
		if root == "" {
			root = home.Dir(ctx)
		}
	}
	expanded, err := pypath.PathExpanduser(root)
	if err != nil {
		return nil, err
	}
	resolved, err := pypath.Resolve(expanded)
	if err != nil {
		return nil, err
	}
	return hostedGrantSecretForHome(resolved)
}

// Unpadded URL safe base64 of bytes
func hostedB64Encode(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}

// Bytes of padded URL safe base64 text, a grant error for any malformed input
func hostedB64Decode(value string) ([]byte, error) {
	decoded, err := pybase64.URLSafeB64Decode(value + strings.Repeat("=", (4-len(value)%4)%4))
	if err != nil {
		return nil, hostedGrantError("room grant encoding is invalid")
	}
	return decoded, nil
}

// Decoded payload and signature halves of a grant token
func hostedSplitToken(token string) ([]byte, []byte, error) {
	encoded, signature, found := strings.Cut(token, hostedGrantSignatureSeparator)
	if !found {
		return nil, nil, hostedGrantError("room grant is invalid")
	}
	payload, err := hostedB64Decode(encoded)
	if err != nil {
		return nil, nil, err
	}
	sig, err := hostedB64Decode(signature)
	if err != nil {
		return nil, nil, err
	}
	return payload, sig, nil
}

// Keyword arguments of one room grant issue, the identity values validated as the issuer receives them
type RoomGrantIssue struct {
	GrantID, RoomID, HomeInstallID, AuthorityGatewayID any
	AuthorityEpoch                                     any
	MemberID, TargetInstallID, TargetProfile           any
	ExecutionPolicyDigest                              string
	Permissions                                        []any
	IssuedAt                                           *float64
	TTLSeconds                                         *float64
	StatusTTLSeconds                                   *float64
	StatusExpiresAt                                    *float64
}

// Signed target verifiable bearer grant scoped to one room member
func IssueRoomGrant(ctx context.Context, secret []byte, o RoomGrantIssue) (string, error) {
	if len(secret) < 32 {
		return "", hostedGrantError("room grant secret must be at least 32 bytes")
	}
	now := hostedClock(o.IssuedAt)
	ttl := hostedGrantDefaultTTLSeconds
	if o.TTLSeconds != nil {
		ttl = *o.TTLSeconds
	}
	statusExpiry := 0.0
	switch {
	case o.StatusExpiresAt != nil:
		statusExpiry = *o.StatusExpiresAt
	case o.StatusTTLSeconds != nil:
		statusExpiry = now + *o.StatusTTLSeconds
	default:
		statusExpiry = now + ttl
	}
	if math.IsInf(now, 0) || math.IsNaN(now) || ttl <= 0 || ttl > MaxDispatchGrantTTLSeconds || math.IsInf(statusExpiry, 0) ||
		math.IsNaN(statusExpiry) || statusExpiry < now+ttl || statusExpiry > now+MaxStatusGrantTTLSeconds {
		return "", hostedGrantError("room grant lifetime is invalid")
	}
	permissions := o.Permissions
	if permissions == nil {
		permissions = []any{"approve", "dispatch", "status", "stop"}
	}
	unique := map[string]bool{}
	for _, permission := range permissions {
		name, isText := permission.(string)
		if !isText || !hostedGrantPermissions[name] {
			return "", hostedGrantError("room grant permissions are invalid")
		}
		unique[name] = true
	}
	if len(unique) == 0 {
		return "", hostedGrantError("room grant permissions are invalid")
	}
	allowed := make([]string, 0, len(unique))
	for name := range unique {
		allowed = append(allowed, name)
	}
	sort.Strings(allowed)
	scope := map[string]any{"grant_id": o.GrantID, "room_id": o.RoomID, "home_install_id": o.HomeInstallID,
		"authority_gateway_id": o.AuthorityGatewayID, "authority_epoch": o.AuthorityEpoch, "member_id": o.MemberID,
		"target_install_id": o.TargetInstallID, "target_profile": o.TargetProfile}
	payload := pyjson.ObjectOf("version", int64(HostedProtocolVersion))
	for _, name := range hostedGrantScope {
		if name == "authority_epoch" {
			epoch, err := hostedPositiveInt(scope[name], name)
			if err != nil {
				return "", err
			}
			payload.Set(name, pyint.Value(epoch))
			continue
		}
		value, err := hostedIdentifier(scope[name], name)
		if err != nil {
			return "", err
		}
		payload.Set(name, value)
	}
	digestSource := o.ExecutionPolicyDigest
	if digestSource == "" {
		policy, err := ExecutionPolicyMapping(ctx, payload.GetOr("target_profile", "").(string), nil)
		if err != nil {
			return "", err
		}
		digestSource, _ = policy.GetOr("policy_digest", "").(string)
	}
	digest, err := hostedDigest(digestSource, "execution_policy_digest")
	if err != nil {
		return "", err
	}
	allowedList := make([]any, len(allowed))
	for i, name := range allowed {
		allowedList[i] = name
	}
	payload.Set("execution_policy_digest", digest)
	payload.Set(hostedGrantPermissionsField, allowedList)
	payload.Set("issued_at", now)
	payload.Set("expires_at", now+ttl)
	payload.Set(hostedGrantStatusExpiresAtField, statusExpiry)
	encoded, err := roomCompactJSON(payload, true)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(encoded))
	token := hostedB64Encode([]byte(encoded)) + hostedGrantSignatureSeparator + hostedB64Encode(mac.Sum(nil))
	if len(token) > hostedMaxTokenBytes {
		return "", hostedGrantError("room grant is too large")
	}
	return token, nil
}

// Grant payload once its signature, lifetime and operation verify, without a dispatch
func DecodeRoomGrant(secret []byte, token, permission string, now *float64) (*pyjson.Object, error) {
	if len(token) > hostedMaxTokenBytes {
		return nil, hostedGrantError("room grant is invalid")
	}
	encoded, supplied, err := hostedSplitToken(token)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(encoded)
	if !hmac.Equal(mac.Sum(nil), supplied) {
		return nil, hostedGrantError("room grant signature is invalid")
	}
	invalidPayload := hostedGrantError("room grant payload is invalid")
	for _, b := range encoded {
		if b >= utf8.RuneSelf {
			return nil, invalidPayload
		}
	}
	decoded, err := pyjson.Loads(string(encoded))
	if err != nil {
		return nil, invalidPayload
	}
	payload, isDict := decoded.(*pyjson.Object)
	if !isDict || !hostedGrantFieldSetValid(payload) {
		return nil, hostedGrantError("room grant fields are invalid")
	}
	checkedNow := hostedClock(now)
	if math.IsInf(checkedNow, 0) || math.IsNaN(checkedNow) {
		return nil, hostedGrantError("room grant clock is invalid")
	}
	lifetimeInvalid := hostedGrantError("room grant lifetime is invalid")
	issuedAt, err := pyint.Float(payload.GetOr("issued_at", nil))
	if err != nil {
		return nil, lifetimeInvalid
	}
	expiresRaw := payload.GetOr("expires_at", nil)
	expiresAt, err := pyint.Float(expiresRaw)
	if err != nil {
		return nil, lifetimeInvalid
	}
	statusExpiresAt, err := pyint.Float(payload.GetOr(hostedGrantStatusExpiresAtField, expiresRaw))
	if err != nil {
		return nil, lifetimeInvalid
	}
	for _, value := range []float64{issuedAt, expiresAt, statusExpiresAt} {
		if math.IsInf(value, 0) || math.IsNaN(value) {
			return nil, lifetimeInvalid
		}
	}
	if !(issuedAt < expiresAt && expiresAt <= statusExpiresAt) {
		return nil, lifetimeInvalid
	}
	operationExpiresAt := expiresAt
	if permission == "approve" || permission == "status" || permission == "stop" {
		operationExpiresAt = statusExpiresAt
	}
	if checkedNow < issuedAt-hostedGrantClockSkewToleranceS || checkedNow >= operationExpiresAt {
		return nil, hostedGrantError("room grant is expired or not active")
	}
	permissions, isList := payload.GetOr(hostedGrantPermissionsField, nil).([]any)
	allowed := false
	for _, granted := range permissions {
		allowed = allowed || pyjson.Equal(granted, permission)
	}
	if !isList || !allowed {
		return nil, hostedGrantError("room grant does not allow this operation")
	}
	return payload, nil
}

// Whether the payload's keys are exactly the grant fields, or those plus the status horizon
func hostedGrantFieldSetValid(payload *pyjson.Object) bool {
	_, refreshed := payload.Get(hostedGrantStatusExpiresAtField)
	want := len(hostedGrantFields)
	if refreshed {
		want++
	}
	if payload.Len() != want {
		return false
	}
	for _, key := range payload.Keys {
		if !hostedGrantFields[key] && key != hostedGrantStatusExpiresAtField {
			return false
		}
	}
	return true
}

// Grant payload once it verifies against the exact recipient dispatch coordinates
func VerifyRoomGrant(secret []byte, token string, dispatch *HostedMemberDispatch, permission string, now *float64) (*pyjson.Object, error) {
	payload, err := DecodeRoomGrant(secret, token, permission, now)
	if err != nil {
		return nil, err
	}
	if !pyjson.Equal(payload.GetOr(hostedGrantVersionField, nil), pyint.Value(dispatch.ProtocolVersion)) {
		return nil, hostedGrantError("room grant protocol does not match dispatch")
	}
	coordinates := dispatch.AsMapping()
	for _, field := range append(append([]string(nil), hostedGrantScope[1:]...), "execution_policy_digest") {
		if !pyjson.Equal(payload.GetOr(field, nil), coordinates.GetOr(field, nil)) {
			return nil, hostedGrantError("room grant scope does not match dispatch")
		}
	}
	return payload, nil
}

// Canonical wire mapping of the dispatch in field order, the one fingerprinting reads
func (d *HostedMemberDispatch) AsMapping() *pyjson.Object {
	value := func(n *big.Int) any {
		if n == nil {
			return nil
		}
		return pyint.Value(n)
	}
	return pyjson.ObjectOf("protocol_version", value(d.ProtocolVersion), "room_id", d.RoomID, "home_install_id", d.HomeInstallID,
		"authority_gateway_id", d.AuthorityGatewayID, "authority_epoch", value(d.AuthorityEpoch), "member_id", d.MemberID,
		"target_install_id", d.TargetInstallID, "target_profile", d.TargetProfile, "task_id", d.TaskID,
		"execution_generation", value(d.ExecutionGeneration), "source_event_seq", value(d.SourceEventSeq),
		"cancellation_scope_id", d.CancellationScopeID, "prompt", d.Prompt, "prompt_digest", d.PromptDigest,
		"capability_digest", d.CapabilityDigest, "execution_policy_digest", d.ExecutionPolicyDigest, "trace_id", d.TraceID)
}

// Room grant secret domain separated from the configured API key, refused for a weak key
func DeriveRoomGrantSecret(apiKey string) ([]byte, error) {
	if utf8.RuneCountInString(apiKey) < 8 {
		return nil, hostedGrantError("room grants require a strong gateway API key")
	}
	mac := hmac.New(sha256.New, []byte(apiKey))
	mac.Write([]byte("hermes-hosted-room-grant-v1"))
	return mac.Sum(nil), nil
}
