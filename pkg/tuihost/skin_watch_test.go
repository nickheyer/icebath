package tuihost

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Live peer double recording every frame, following a profile skin when skinHome is set
type skinPeer struct {
	skinHome string
	frames   []*jsonutil.Object
}

func (p *skinPeer) Write(obj *jsonutil.Object) bool {
	p.frames = append(p.frames, obj)
	return true
}

func (p *skinPeer) Close() {}

func (p *skinPeer) followedSkinHome() string { return p.skinHome }

// Names of every skin.changed payload the peer received
func peerSkins(p *skinPeer) []string {
	names := []string{}
	for _, frame := range p.frames {
		params, _ := frame.ValueOr("params", nil).(*jsonutil.Object)
		if params.ValueOr("type", nil) != "skin.changed" {
			continue
		}
		payload, _ := params.ValueOr("payload", nil).(map[string]any)
		name, _ := payload["name"].(string)
		names = append(names, name)
	}
	return names
}

// Replaces a home's config.yaml atomically with one naming the skin
func writeSkin(t *testing.T, h, name string) {
	t.Helper()
	tmp := filepath.Join(h, "config.yaml.tmp")
	if err := os.WriteFile(tmp, []byte("display:\n  skin: "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(h, "config.yaml")); err != nil {
		t.Fatal(err)
	}
}

// Launch home and a work profile home under it with fresh skin baselines and no live peers
func skinHomes(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	def, work := filepath.Join(root, "default"), filepath.Join(root, "default", "profiles", "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkin(t, def, "default")
	writeSkin(t, work, "taro")
	t.Setenv("ICEBATH_HOME", def)
	skinBaselines.Lock()
	skinBaselines.last, skinBaselines.byHome = nil, map[string]SkinSignature{}
	skinBaselines.Unlock()
	t.Cleanup(func() {
		liveTransportsMu.Lock()
		liveTransports = nil
		liveTransportsMu.Unlock()
	})
	return def, work
}

func TestWatcherNeverPushesTheLaunchHomeSkinOntoAProfileTui(t *testing.T) {
	_, work := skinHomes(t)
	peer := &skinPeer{skinHome: work}
	RegisterLiveTransport(peer)
	NoteSkinBroadcast(home.WithOverride(context.Background(), work))

	BroadcastSkinIfChanged(context.Background())

	if got := peerSkins(peer); len(got) != 0 {
		t.Fatalf("skins = %v", got)
	}
}

func TestProfileSkinSwitchRepaintsOnlyThatProfilesTui(t *testing.T) {
	def, work := skinHomes(t)
	workPeer, defaultPeer := &skinPeer{skinHome: work}, &skinPeer{}
	RegisterLiveTransport(workPeer)
	RegisterLiveTransport(defaultPeer)
	NoteSkinBroadcast(context.Background())
	BroadcastSkinIfChanged(context.Background())

	writeSkin(t, work, "contrast")
	BroadcastSkinIfChanged(context.Background())
	if got := peerSkins(workPeer); len(got) != 1 || got[0] != "contrast" {
		t.Fatalf("work peer skins = %v", got)
	}
	if got := peerSkins(defaultPeer); len(got) != 0 {
		t.Fatalf("default peer skins = %v", got)
	}

	writeSkin(t, def, "dracula")
	BroadcastSkinIfChanged(context.Background())
	if got := peerSkins(defaultPeer); len(got) != 1 || got[0] != "dracula" {
		t.Fatalf("default peer skins = %v", got)
	}
	if got := peerSkins(workPeer); len(got) != 1 || got[0] != "contrast" {
		t.Fatalf("work peer skins = %v", got)
	}
}

func TestOwnerSkinRpcBaselineIsPerProfile(t *testing.T) {
	_, work := skinHomes(t)
	peer := &skinPeer{skinHome: work}
	RegisterLiveTransport(peer)
	workCtx := home.WithOverride(context.Background(), work)
	NoteSkinBroadcast(context.Background())
	NoteSkinBroadcast(workCtx)
	writeSkin(t, work, "contrast")
	NoteSkinBroadcast(workCtx)

	BroadcastSkinIfChanged(context.Background())

	if got := peerSkins(peer); len(got) != 0 {
		t.Fatalf("skins = %v", got)
	}
}
