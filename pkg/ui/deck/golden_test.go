package deck

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Builds a deck whose every animation has settled so the frame is the same on every run
func scene(t *testing.T, w, h int, opts ...Option) *Model {
	t.Helper()
	cards := []Card{&stub{title: "TRACE", status: "live"}, &stub{title: "ICE", status: "3 run"}, &stub{title: "UPLINK"}, &stub{title: "DAEMONS", status: "idle"}}
	m := New(append([]Option{WithCards(cards...), WithMotion(0)}, opts...)...)
	m.now = func() time.Time { return clock }
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	settle(m)
	m.phase = 0
	m.tick = 0
	return m
}

func TestGoldenSplit(t *testing.T) {
	m := scene(t, 80, 20)
	golden.RequireEqual(t, m.View().Content)
}

func TestGoldenSolo(t *testing.T) {
	m := scene(t, 80, 20)
	run(m.SetMode(Solo))
	settle(m)
	m.phase = 0
	golden.RequireEqual(t, m.View().Content)
}

func TestGoldenTimeline(t *testing.T) {
	m := scene(t, 80, 20)
	run(m.SetMode(Timeline))
	run(m.Focus(1))
	settle(m)
	m.phase = 0
	golden.RequireEqual(t, m.View().Content)
}

func TestGoldenNarrow(t *testing.T) {
	m := scene(t, 30, 16)
	golden.RequireEqual(t, m.View().Content)
}

func TestGoldenOverlayAndToast(t *testing.T) {
	m := scene(t, 80, 20)
	run(m.ShowOverlay(&stub{title: "APPROVE"}))
	m.Toast("saved session", Success, time.Minute)
	settle(m)
	m.phase = 0
	golden.RequireEqual(t, m.View().Content)
}

func TestGoldenPlainTheme(t *testing.T) {
	m := scene(t, 80, 20, WithTheme(theme.Plain()))
	golden.RequireEqual(t, m.View().Content)
}

func TestGoldenLightCrowded(t *testing.T) {
	m := scene(t, 80, 9, WithTheme(theme.Light()))
	run(m.Focus(3))
	settle(m)
	m.phase = 0
	golden.RequireEqual(t, m.View().Content)
}
