package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/tools"
)

func imageReplayLog() *slog.Logger { return slog.Default().With("logger", "agent.image_replay") }

// Newest user images resent on turns rebuilt from the session store
const DefaultReplayRecentImages = 3

var (
	replayHintRe   = regexp.MustCompile(`^\[Image attached(?P<local> at)?: (?P<ref>.+)\]$`)
	replayMarkerRe = regexp.MustCompile(`^\[screenshot(?: detail=(?P<detail>low|high|auto))?\]$`)
)

var replayImagePartTypes = map[string]bool{"image": true, "image_url": true, "input_image": true}

// Per turn replay window keyed by the admission clock, provider and model
type imageReplayState struct {
	imageReplayWindow *imageReplayWindow
}

type imageReplayWindow struct {
	turnTimestamp   float64
	provider, model string
	keep            int
}

// vision.replay_recent_images as a non negative int, an unusable value warning and disabling replay
func ResolveReplayRecentImages(cfg map[string]any) int {
	vision, _ := cfg["vision"].(map[string]any)
	raw, ok := vision["replay_recent_images"]
	if !ok || raw == nil {
		return DefaultReplayRecentImages
	}
	switch n := raw.(type) {
	case int:
		if n >= 0 {
			return n
		}
	case int64:
		if n >= 0 {
			return int(n)
		}
	}
	imageReplayLog().Warn(fmt.Sprintf("vision.replay_recent_images=%s is not a non-negative integer (0 = off, default %d); not replaying earlier images",
		pyrepr.Repr(raw), DefaultReplayRecentImages))
	return 0
}

// One part of a rebuilt user row, text or an image reference with its detail and marker line
type rebuiltPart struct {
	image               bool
	text                string
	ref, detail, marker string
}

// A store rebuilt user row as its original parts in order, false when markers and hints disagree
func splitRebuiltRow(content string) ([]rebuiltPart, bool) {
	lines := strings.Split(content, "\n")
	type marker struct {
		index  int
		detail string
	}
	var markers []marker
	var hints [][]string
	for i, line := range lines {
		if m := replayMarkerRe.FindStringSubmatch(line); m != nil {
			markers = append(markers, marker{i, m[1]})
		}
		if m := replayHintRe.FindStringSubmatch(line); m != nil {
			hints = append(hints, m)
		}
	}
	if len(markers) == 0 || len(hints) != len(markers) {
		return nil, false
	}
	var parts []rebuiltPart
	start := 0
	for k, mk := range markers {
		if mk.index > start {
			parts = append(parts, rebuiltPart{text: strings.Join(lines[start:mk.index], "\n")})
		}
		ref := ""
		if hints[k][1] != "" {
			ref = hints[k][2]
		}
		parts = append(parts, rebuiltPart{image: true, ref: ref, detail: mk.detail, marker: lines[mk.index]})
		start = mk.index + 1
	}
	if start < len(lines) {
		parts = append(parts, rebuiltPart{text: strings.Join(lines[start:], "\n")})
	}
	return parts, true
}

// Parent of a pure path the way pathlib names it
func purePathParent(p string) string {
	i := strings.LastIndex(p, "/")
	switch {
	case i < 0:
		return "."
	case i == 0:
		return "/"
	}
	return p[:i]
}

// Resolved content addressed store of this profile's retained native inputs
func NativeInputsRoot(ctx context.Context) (string, error) {
	docs, err := mediacache.DocumentCacheDir(ctx, home.Dir(ctx))
	if err != nil {
		return "", err
	}
	return pypath.Resolve(pypath.PurePathStr(docs + "/native-inputs"))
}

// Data URL of a retained local image the profile still holds unchanged, empty otherwise
func (a *AIAgent) retainedDataURL(ctx context.Context, ref string) string {
	if ref == "" {
		return ""
	}
	path := pypath.PurePathStr(ref)
	root, err := NativeInputsRoot(ctx)
	if err != nil || purePathParent(purePathParent(path)) != root {
		return ""
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	parent := purePathParent(path)
	if hex.EncodeToString(sum[:]) != parent[strings.LastIndex(parent, "/")+1:] {
		return ""
	}
	url, ok, err := tools.FileToDataURL(a.env, path)
	if err != nil {
		panic(err)
	}
	if !ok {
		return ""
	}
	return url
}

// Oldest image slot still replayed, moving forward only in whole batches of keep
func ReplayFrontier(total, keep int) int {
	if total > keep {
		return pyFloorDivInt(total-keep, keep) * keep
	}
	return 0
}

// Python floor division of ints
func pyFloorDivInt(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// Rebuilds retained images at or after the frontier in place on the wire copies, returning the count attached
func (a *AIAgent) replayRecentImages(ctx context.Context, apiMessages []map[string]any, keep int) int {
	type row struct {
		msg   map[string]any
		parts []rebuiltPart
		slot  int
	}
	slots := 0
	var rows []row
	for _, msg := range apiMessages {
		if msg["role"] != "user" {
			continue
		}
		if list, ok := listOfAny(msg["content"]); ok {
			for _, p := range list {
				if part, ok := p.(map[string]any); ok {
					if t, _ := part["type"].(string); replayImagePartTypes[t] {
						slots++
					}
				}
			}
			continue
		}
		text, ok := msg["content"].(string)
		if !ok {
			continue
		}
		parts, ok := splitRebuiltRow(text)
		if !ok {
			continue
		}
		rows = append(rows, row{msg, parts, slots})
		for _, p := range parts {
			if p.image {
				slots++
			}
		}
	}
	frontier := ReplayFrontier(slots, keep)
	attached := 0
	for _, r := range rows {
		rebuilt := make([]any, 0, len(r.parts))
		urls := 0
		slot := r.slot
		for _, p := range r.parts {
			if !p.image {
				rebuilt = append(rebuilt, map[string]any{"type": "text", "text": p.text})
				continue
			}
			url := ""
			if slot >= frontier {
				url = a.retainedDataURL(ctx, p.ref)
			}
			slot++
			if url == "" {
				rebuilt = append(rebuilt, map[string]any{"type": "text", "text": p.marker})
				continue
			}
			image := map[string]any{"url": url}
			if p.detail != "" {
				image["detail"] = p.detail
			}
			rebuilt = append(rebuilt, map[string]any{"type": "image_url", "image_url": image})
			urls++
		}
		if urls > 0 {
			r.msg["content"] = rebuilt
			attached += urls
		}
	}
	return attached
}

// Images to replay this turn, zero unless the model takes images natively, resolved once per turn
func (a *AIAgent) turnReplayWindow(ctx context.Context) int {
	provider, model := strings.TrimSpace(a.provider), strings.TrimSpace(a.model)
	if w := a.imageReplayWindow; w != nil && w.turnTimestamp == a.currentTurnTimestamp && w.provider == provider && w.model == model {
		return w.keep
	}
	tree, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
	if err != nil {
		tree = nil
	}
	keep := ResolveReplayRecentImages(map[string]any(tree))
	if keep > 0 && tools.DecideImageInputMode(ctx, a.env, provider, model, "") != "native" {
		keep = 0
	}
	a.imageReplayWindow = &imageReplayWindow{turnTimestamp: a.currentTurnTimestamp, provider: provider, model: model, keep: keep}
	return keep
}

// Replays earlier user images on the wire copies when this turn's model takes images natively
func replayForAgent(ctx context.Context, a *AIAgent, apiMessages []map[string]any) {
	found := false
	for _, m := range apiMessages {
		if text, ok := m["content"].(string); ok && m["role"] == "user" && strings.Contains(text, "[screenshot") {
			found = true
			break
		}
	}
	if !found {
		return
	}
	keep := a.turnReplayWindow(ctx)
	if keep <= 0 {
		return
	}
	if attached := a.replayRecentImages(ctx, apiMessages, keep); attached > 0 {
		imageReplayLog().Info(fmt.Sprintf("image_replay: re-attached %d earlier image(s) to the request", attached))
	}
}
