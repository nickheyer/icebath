package gateway

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func hrLinkCatalog(t *testing.T, installation string) *GatewayRoomCatalog {
	t.Helper()
	mapping := hrOK(CatalogMapping(context.Background(), CatalogOptions{TargetProfile: "default", InstallationID: installation, PersistentProcess: true}))
	return hrOK(GatewayRoomCatalogFromMapping(mapping))
}

func hrLink(t *testing.T, roomID, memberID, url, profile, grant string, catalog *GatewayRoomCatalog, scope, trace string) StoredRoomLink {
	t.Helper()
	return hrOK(MakeStoredRoomLink(roomID, memberID, url, profile, grant, catalog, scope, trace))
}

func TestRoomLinkStoreIsPrivateTransactionalAndUpserted(t *testing.T) {
	db := hrDB(t)
	ctx := context.Background()
	first := hrLink(t, "room-1", "member-1", "https://peer.example.test", "reviewer", "grant.one", hrLinkCatalog(t, "install-peer"), "cancel-1", "trace-1")
	if err := SaveRoomLink(ctx, db, first); err != nil {
		t.Fatal(err)
	}
	if loaded := hrOK(LoadRoomLinks(ctx, db)); !reflect.DeepEqual(loaded, []StoredRoomLink{first}) {
		t.Fatalf("loaded %v", loaded)
	}
	replacement := hrLink(t, "room-1", "member-1", "https://relay.example.test", "reviewer", "grant.two", hrLinkCatalog(t, "install-peer"), "cancel-1", "trace-2")
	if err := SaveRoomLink(ctx, db, replacement); err != nil {
		t.Fatal(err)
	}
	if loaded := hrOK(LoadRoomLinks(ctx, db)); !reflect.DeepEqual(loaded, []StoredRoomLink{replacement}) {
		t.Fatalf("loaded %v", loaded)
	}
}

func TestRoomLinkStoreHasOwnerOnlyMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix")
	}
	db := hrDB(t)
	if err := SaveRoomLink(context.Background(), db, hrLink(t, "room", "member", "https://peer.example.test", "default", "grant", hrLinkCatalog(t, "install-peer"), "cancel", "trace")); err != nil {
		t.Fatal(err)
	}
	if mode := hrOK(os.Stat(db)).Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode %o", mode)
	}
}

func TestRoomLinkStoreKeepsDistinctRoomMemberRoutes(t *testing.T) {
	db := hrDB(t)
	ctx := context.Background()
	for _, member := range []string{"member-a", "member-b"} {
		link := hrLink(t, "room-1", member, "https://"+member+".example.test", "default", "grant."+member, hrLinkCatalog(t, "install-"+member), "cancel-room-1", "trace-"+member)
		if err := SaveRoomLink(ctx, db, link); err != nil {
			t.Fatal(err)
		}
	}
	members := map[string]bool{}
	for _, link := range hrOK(LoadRoomLinks(ctx, db)) {
		members[link.MemberID] = true
	}
	if len(members) != 2 || !members["member-a"] || !members["member-b"] {
		t.Fatalf("members %v", members)
	}
}

func TestConcurrentRoomLinkRegistrationsDoNotLoseUpdates(t *testing.T) {
	db := hrDB(t)
	ctx := context.Background()
	links := make([]StoredRoomLink, 24)
	for index := range links {
		links[index] = hrLink(t, "room-1", fmt.Sprintf("member-%d", index), fmt.Sprintf("https://member-%d.example.test", index), fmt.Sprintf("profile-%d", index),
			fmt.Sprintf("grant.%d", index), hrLinkCatalog(t, fmt.Sprintf("install-%d", index)), "cancel-room-1", fmt.Sprintf("trace-%d", index))
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, link := range links {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := SaveRoomLink(ctx, db, link); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if loaded := hrOK(LoadRoomLinks(ctx, db)); len(loaded) != 24 {
		t.Fatalf("loaded %d", len(loaded))
	}
}

func TestRoomLinkReprNeverContainsGrant(t *testing.T) {
	link := hrLink(t, "room-1", "member-1", "https://peer.example.test", "reviewer", "top.secret.grant", hrLinkCatalog(t, "install-peer"), "cancel-1", "trace-1")
	if strings.Contains(link.String(), "top.secret.grant") || strings.Contains(fmt.Sprint(link), "top.secret.grant") {
		t.Fatal("grant leaked into the link repr")
	}
}
