package tuigateway

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/home"
)

// Whether two skin signatures name the same skin at the same file mtime
func sameSkinSig(a, b SkinSignature) bool {
	if a.Name != b.Name || (a.Mtime == nil) != (b.Mtime == nil) {
		return false
	}
	return a.Mtime == nil || *a.Mtime == *b.Mtime
}

// Peer that follows the skin of a served profile home
type skinFollower interface{ followedSkinHome() string }

// Served profile home whose skin the WebSocket peer follows, empty for the launch home
func (t *WSTransport) followedSkinHome() string {
	if t.SkinHome == nil {
		return ""
	}
	return *t.SkinHome
}

// Live peers grouped by the served profile home whose skin each one follows
func skinPeersByHome() (map[string][]Transport, []string) {
	liveTransportsMu.Lock()
	targets := append([]Transport(nil), liveTransports...)
	liveTransportsMu.Unlock()
	peers := map[string][]Transport{}
	order := []string{}
	for _, transport := range targets {
		follower, ok := transport.(skinFollower)
		if !ok {
			continue
		}
		h := follower.followedSkinHome()
		if h == "" {
			continue
		}
		if _, seen := peers[h]; !seen {
			order = append(order, h)
		}
		peers[h] = append(peers[h], transport)
	}
	return peers, order
}

// Runs fn and swallows a panic it raises, the contextlib suppress of the watcher
func suppressPanic(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			serverLog().Debug(fmt.Sprintf("skin watcher step failed: %v", r))
		}
	}()
	fn()
}

// Emits skin.changed when a skin moved, launch peers through the global broadcast and served homes to their own peers
func BroadcastSkinIfChanged(ctx context.Context) {
	byHome, order := skinPeersByHome()
	suppressPanic(func() {
		sig := SkinSig(ctx)
		skinBaselines.Lock()
		moved := skinBaselines.last == nil || !sameSkinSig(sig, *skinBaselines.last)
		if moved {
			skinBaselines.last = &sig
		}
		skinBaselines.Unlock()
		if !moved {
			return
		}
		skip := []Transport{}
		for _, h := range order {
			skip = append(skip, byHome[h]...)
		}
		BroadcastGlobalEvent("skin.changed", SkinPayload(ctx, loadCfg(ctx)), skip...)
	})
	for _, h := range order {
		peers := byHome[h]
		suppressPanic(func() {
			scoped := home.WithOverride(ctx, h)
			sig := SkinSig(scoped)
			skinBaselines.Lock()
			if skinBaselines.byHome == nil {
				skinBaselines.byHome = map[string]SkinSignature{}
			}
			seen, known := skinBaselines.byHome[h]
			skinBaselines.byHome[h] = sig
			skinBaselines.Unlock()
			if !known || sameSkinSig(sig, seen) {
				return
			}
			frame := EventFrame("skin.changed", "", SkinPayload(scoped, loadCfg(scoped)))
			for _, peer := range peers {
				suppressPanic(func() { peer.Write(frame) })
			}
		})
	}
}
