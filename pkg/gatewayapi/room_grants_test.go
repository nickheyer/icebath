package gatewayapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestRoomGrantSecretStaysGatewayOwnedOnNamedProfile(t *testing.T) {
	adapter := &APIServerAdapter{apiKey: "gateway-api-key-1234567890"}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := withRequestProfile(context.Background(), "reviewer")
	got, err := adapter.roomGrantSecret(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want, err := gateway.GatewayRoomGrantSecret(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("adapter secret %x != gateway secret %x", got, want)
	}
}

func TestSupersededRoomAuthorityCannotReuseItsGrant(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	secret, err := gateway.GatewayRoomGrantSecret(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	now := wallClock()
	target, err := gateway.LocalAuthorityGatewayID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ttl := 3600.0
	oldGrant, err := gateway.IssueRoomGrant(ctx, secret, gateway.RoomGrantIssue{GrantID: "grant-old", RoomID: "room-1",
		HomeInstallID: "install-home", AuthorityGatewayID: "gateway-old", AuthorityEpoch: int64(1), MemberID: "member-reviewer",
		TargetInstallID: target, TargetProfile: "reviewer", ExecutionPolicyDigest: strings.Repeat("d", 64), IssuedAt: &now, TTLSeconds: &ttl})
	if err != nil {
		t.Fatal(err)
	}
	oldClaims := pyjson.ObjectOf("room_id", "room-1", "home_install_id", "install-home", "authority_gateway_id", "gateway-old",
		"authority_epoch", int64(1), "member_id", "member-reviewer", "target_install_id", target, "target_profile", "reviewer")
	dbPath := gateway.HostedRoomsDefaultDBPath(ctx)
	if err := gateway.ReservePeerRoom(ctx, dbPath, oldClaims, now+3600, &now); err != nil {
		t.Fatal(err)
	}
	adapter := &APIServerAdapter{}
	request := &gateway.WebRequest{Header: http.Header{"Authorization": {"HermesRoom " + oldGrant}}}
	claims, err := adapter.roomGrantClaims(ctx, request, "status")
	if err != nil {
		t.Fatal(err)
	}
	if claims.GetOr("authority_gateway_id", nil) != "gateway-old" {
		t.Fatalf("authority_gateway_id = %v", claims.GetOr("authority_gateway_id", nil))
	}
	newClaims := oldClaims.Copy()
	newClaims.Set("authority_gateway_id", "gateway-new")
	newClaims.Set("authority_epoch", int64(2))
	newClaims.Set("member_id", "member-new")
	if err := gateway.ReservePeerRoom(ctx, dbPath, newClaims, now+3600, &now); err != nil {
		t.Fatal(err)
	}
	_, err = adapter.roomGrantClaims(ctx, request, "status")
	var reauth *RoomGrantReauthorizationRequired
	if !errors.As(err, &reauth) || !strings.Contains(err.Error(), "no longer current") {
		t.Fatalf("err = %v, want reauthorization naming a grant no longer current", err)
	}
}
