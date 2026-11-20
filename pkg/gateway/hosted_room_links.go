package gateway

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	MaxRoomLinks         = 512
	maxRoomLinkGrantChar = 16 * 1024
)

var (
	roomLinkRequiredFields = map[string]bool{"room_id": true, "member_id": true, "target_url": true, "target_profile": true, "grant": true,
		"catalog": true, "cancellation_scope_id": true, "trace_id": true, "updated_at": true}
	roomLinkOptionalFields = map[string]bool{"transport_security": true, "status": true}
	roomLinkRecordFields   = []string{"room_id", "member_id", "target_url", "target_profile", "grant", "cancellation_scope_id", "trace_id",
		"transport_security", "status", "updated_at"}
	roomLinkStatuses = map[string]bool{"ready": true, "unavailable": true, "needs_reauthorization": true}
)

// Negotiated hosted room link, its grant never formatted
type StoredRoomLink struct {
	RoomID              string
	MemberID            string
	TargetURL           string
	TargetProfile       string
	Grant               string
	Catalog             *GatewayRoomCatalog
	CancellationScopeID string
	TraceID             string
	TransportSecurity   string
	Status              string
	UpdatedAt           float64
}

// Field view of the link without its grant
func (l StoredRoomLink) String() string {
	return fmt.Sprintf("StoredRoomLink(room_id=%s, member_id=%s, target_url=%s, target_profile=%s, cancellation_scope_id=%s, trace_id=%s, transport_security=%s, status=%s, updated_at=%v)",
		pyrepr.Repr(l.RoomID), pyrepr.Repr(l.MemberID), pyrepr.Repr(l.TargetURL), pyrepr.Repr(l.TargetProfile), pyrepr.Repr(l.CancellationScopeID),
		pyrepr.Repr(l.TraceID), pyrepr.Repr(l.TransportSecurity), pyrepr.Repr(l.Status), l.UpdatedAt)
}

// Python str of a value, empty for a falsy one
func roomLinkText(value any) string {
	if !pyrepr.Truthy(value) {
		return ""
	}
	return pyrepr.Str(value)
}

// Bounded stripped string of any shape
func roomLinkShortString(value any, field string) (string, error) {
	return roomIdentifier(roomLinkText(value), roomIdentifierSpec{label: field, maxChars: 256, anyShape: true, invalid: field + " is invalid"}, hostedPeerError)
}

// Validated stored link of a mapping
func StoredRoomLinkFromMapping(value any) (StoredRoomLink, error) {
	invalid := "stored room link fields are invalid"
	fields, err := roomExactFields(value, "stored room link", roomLinkRequiredFields, roomLinkOptionalFields, hostedPeerError,
		roomFieldFormats{notObject: invalid, missing: invalid, unknown: invalid})
	if err != nil {
		return StoredRoomLink{}, err
	}
	var link StoredRoomLink
	for _, item := range []struct {
		dest  *string
		field string
	}{{&link.RoomID, "room_id"}, {&link.MemberID, "member_id"}, {&link.TargetProfile, "target_profile"}} {
		if *item.dest, err = roomLinkShortString(fields[item.field], item.field); err != nil {
			return StoredRoomLink{}, err
		}
	}
	targetURL, detected, err := ValidateRoomLinkURL(fields["target_url"])
	if err != nil {
		return StoredRoomLink{}, err
	}
	security := roomLinkText(fields["transport_security"])
	if security == "" {
		security = detected
	}
	if security != detected {
		return StoredRoomLink{}, hostedPeerError("transport_security does not match target_url")
	}
	grant := roomLinkText(fields["grant"])
	if grant == "" || len([]rune(grant)) > maxRoomLinkGrantChar {
		return StoredRoomLink{}, hostedPeerError("room grant is missing or too large")
	}
	status := roomLinkText(fields["status"])
	if status == "" {
		status = "ready"
	}
	if !roomLinkStatuses[status] {
		return StoredRoomLink{}, hostedPeerError("stored room link status is invalid")
	}
	updatedAt, err := pyint.Float(fields["updated_at"])
	if err != nil {
		return StoredRoomLink{}, err
	}
	if !(updatedAt > 0) {
		return StoredRoomLink{}, hostedPeerError("updated_at must be positive")
	}
	catalog, err := GatewayRoomCatalogFromMapping(fields["catalog"])
	if err != nil {
		return StoredRoomLink{}, err
	}
	scope, err := roomLinkShortString(fields["cancellation_scope_id"], "cancellation_scope_id")
	if err != nil {
		return StoredRoomLink{}, err
	}
	trace, err := roomLinkShortString(fields["trace_id"], "trace_id")
	if err != nil {
		return StoredRoomLink{}, err
	}
	link.TargetURL, link.Grant, link.Catalog, link.CancellationScopeID, link.TraceID = targetURL, grant, catalog, scope, trace
	link.TransportSecurity, link.Status, link.UpdatedAt = security, status, updatedAt
	return link, nil
}

// Validated stored link of a SQLite record, its catalog JSON decoded
func StoredRoomLinkFromRecord(value map[string]any) (StoredRoomLink, error) {
	catalogJSON, has := value["catalog_json"]
	if !has {
		return StoredRoomLink{}, hostedPeerError("stored room link catalog is unreadable")
	}
	catalog, err := pyjson.Loads(pyrepr.Str(catalogJSON))
	if err != nil {
		return StoredRoomLink{}, hostedPeerError("stored room link catalog is unreadable")
	}
	mapping := map[string]any{"catalog": catalog}
	for _, name := range roomLinkRecordFields {
		field, has := value[name]
		if !has {
			return StoredRoomLink{}, &HostedKeyError{name}
		}
		mapping[name] = field
	}
	return StoredRoomLinkFromMapping(mapping)
}

// Canonical catalog mapping of the link
func (l StoredRoomLink) CatalogMapping() *pyjson.Object { return l.Catalog.AsMapping() }

// SQLite record of the link with its catalog serialized
func (l StoredRoomLink) AsRecord() (map[string]any, error) {
	catalogJSON, err := roomCompactJSON(l.CatalogMapping(), false)
	if err != nil {
		return nil, err
	}
	return map[string]any{"room_id": l.RoomID, "member_id": l.MemberID, "target_url": l.TargetURL, "target_profile": l.TargetProfile,
		"grant": l.Grant, "cancellation_scope_id": l.CancellationScopeID, "trace_id": l.TraceID, "transport_security": l.TransportSecurity,
		"status": l.Status, "updated_at": l.UpdatedAt, "catalog_json": catalogJSON}, nil
}

// Stored link rows, refusing a list past the link bound
func roomLinkRows(ctx context.Context, dbPath string) ([]map[string]any, error) {
	rows, err := ListRoomLinkRecords(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	if len(rows) > MaxRoomLinks {
		return nil, hostedPeerError("stored room link list is invalid")
	}
	return rows, nil
}

// Every stored link, failing on the first malformed row
func LoadRoomLinks(ctx context.Context, dbPath string) ([]StoredRoomLink, error) {
	rows, err := roomLinkRows(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	links := make([]StoredRoomLink, 0, len(rows))
	for _, row := range rows {
		link, err := StoredRoomLinkFromRecord(row)
		if err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, nil
}

// Healthy stored links, malformed rows quarantined by identity
func LoadRoomLinksTolerant(ctx context.Context, dbPath string) ([]StoredRoomLink, []string, error) {
	rows, err := roomLinkRows(ctx, dbPath)
	if err != nil {
		return nil, nil, err
	}
	links, errs := []StoredRoomLink{}, []string{}
	for _, row := range rows {
		link, err := StoredRoomLinkFromRecord(row)
		if err != nil {
			room, member := roomLinkText(row["room_id"]), roomLinkText(row["member_id"])
			if room == "" {
				room = "unknown"
			}
			if member == "" {
				member = "unknown"
			}
			errs = append(errs, room+":"+member+":invalid")
			continue
		}
		links = append(links, link)
	}
	return links, errs, nil
}

// Persists one link and keeps the store private on POSIX
func SaveRoomLink(ctx context.Context, dbPath string, link StoredRoomLink) error {
	record, err := link.AsRecord()
	if err != nil {
		return err
	}
	if err := UpsertRoomLinkRecord(ctx, dbPath, record, MaxRoomLinks); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(dbPath, 0o600)
	}
	return nil
}

// Persists a route health classification of one link
func MarkRoomLinkStatus(ctx context.Context, dbPath, roomID, memberID, status string) (bool, error) {
	if !roomLinkStatuses[status] {
		return false, hostedPeerError("stored room link status is invalid")
	}
	room, err := roomLinkShortString(roomID, "room_id")
	if err != nil {
		return false, err
	}
	member, err := roomLinkShortString(memberID, "member_id")
	if err != nil {
		return false, err
	}
	return UpdateRoomLinkStatus(ctx, dbPath, room, member, status, nil)
}

// Fresh ready link of a negotiated route
func MakeStoredRoomLink(roomID, memberID, targetURL, targetProfile, grant string, catalog *GatewayRoomCatalog, cancellationScopeID, traceID string) (StoredRoomLink, error) {
	url, security, err := ValidateRoomLinkURL(targetURL)
	if err != nil {
		return StoredRoomLink{}, err
	}
	return StoredRoomLinkFromMapping(map[string]any{"room_id": roomID, "member_id": memberID, "target_url": url, "target_profile": targetProfile,
		"grant": grant, "catalog": catalog.AsMapping(), "cancellation_scope_id": cancellationScopeID, "trace_id": traceID,
		"transport_security": security, "status": "ready", "updated_at": float64(time.Now().UnixNano()) / 1e9})
}
