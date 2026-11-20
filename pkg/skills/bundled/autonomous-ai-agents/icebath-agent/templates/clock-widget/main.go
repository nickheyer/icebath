// Clock widget for the icebath terminal UI, an ambient box refreshed every second
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/pluginsdk"
)

// Zone the clock shows the time of, the local one when it is empty
type clockState struct {
	Zone string `json:"zone"`
}

func init() {
	pluginsdk.Register(func(h pluginsdk.Host) error {
		_, err := h.RegisterWidget(pluginsdk.Widget{
			ID:      "clock",
			Help:    "live clock beside the transcript, /clock [timezone]",
			Usage:   "usage: /clock [timezone], for example /clock UTC or /clock Asia/Tokyo",
			Mode:    "ambient",
			Zone:    "top-right",
			Width:   26,
			Refresh: time.Second,
			Open:    open,
			Render:  render,
		})
		return err
	})
}

// Checks the timezone argument, an unknown zone answering the usage line
func open(_ context.Context, arg string) (any, bool, error) {
	zone := strings.TrimSpace(arg)
	if zone != "" {
		if _, err := time.LoadLocation(zone); err != nil {
			return nil, false, nil
		}
	}
	return clockState{Zone: zone}, true, nil
}

// Time of the zone now, read again on every refresh
func render(_ context.Context, frame pluginsdk.WidgetFrame) (pluginsdk.WidgetView, error) {
	zone := ""
	if state, ok := frame.State.(map[string]any); ok {
		zone, _ = state["zone"].(string)
	}
	loc, label := time.Local, "local"
	if zone != "" {
		parsed, err := time.LoadLocation(zone)
		if err != nil {
			return pluginsdk.WidgetView{}, fmt.Errorf("unknown timezone %s", zone)
		}
		loc, label = parsed, zone
	}
	now := time.Now().In(loc)
	return pluginsdk.WidgetView{Lines: []pluginsdk.WidgetLine{
		{Spans: []pluginsdk.WidgetSpan{{Text: now.Format("15:04:05"), Role: "accent", Bold: true}, {Text: "  " + label, Role: "muted"}}},
		{Spans: []pluginsdk.WidgetSpan{{Text: now.Format("Mon 2 Jan 2006"), Role: "muted"}}},
	}}, nil
}

func main() { pluginsdk.Main() }
