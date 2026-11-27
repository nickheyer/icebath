package provider

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
)

// Cells of a usage bar, and what fills them
const (
	barWidth = 10
	barFill  = "|"
	barEmpty = " "
)

// One quota window of an account, such as its session or its week
type UsageWindow struct {
	Label string
	// Share used in percent, nil when the provider didn't say
	Used *float64
	// When the window starts over, nil when not known
	Resets *time.Time
	// What to show when the reset is not known
	Detail string
	// Whether the window caps the whole account or one family of its models
	Model bool
}

// What a provider says of the limits of an account
type AccountUsage struct {
	// Kind of the provider asked
	Provider string
	// Where the reading came from, such as usage_api
	Source  string
	Fetched time.Time
	Title   string
	Plan    string
	Windows []UsageWindow
	// Lines of their own after the windows, such as a credit balance
	Details []string
	// Why the account's limits can't be read, empty when they could
	Unavailable string
	// Stable non secret id of the account, empty when not known
	Identity string
	// Windows shown as bars under the plan's name, as for a plan sold by its usage windows
	Bars bool
}

// Reading of an account fetched now, titled as account limits
func NewAccountUsage(provider, source string, windows []UsageWindow, details []string) *AccountUsage {
	return &AccountUsage{Provider: provider, Source: source, Fetched: time.Now().UTC(), Title: "Account limits", Windows: windows, Details: details}
}

// Window of the whole account
func AccountWindow(label string, used *float64, resets *time.Time, detail string) UsageWindow {
	return UsageWindow{Label: label, Used: used, Resets: resets, Detail: detail}
}

// Reading as the provider service sends it
func (u *AccountUsage) Message() *providerv1.AccountUsage {
	message := &providerv1.AccountUsage{Provider: u.Provider, Source: u.Source, Title: u.Title, Plan: u.Plan, Fetched: timestamppb.New(u.Fetched),
		Details: u.Details, Unavailable: u.Unavailable, Bars: u.Bars}
	for _, window := range u.Windows {
		w := &providerv1.UsageWindow{Label: window.Label, Used: window.Used, Detail: window.Detail, Model: window.Model}
		if window.Resets != nil {
			w.Resets = timestamppb.New(*window.Resets)
		}
		message.Windows = append(message.Windows, w)
	}
	return message
}

// Reading the provider service sent
func UsageOf(message *providerv1.AccountUsage) *AccountUsage {
	u := &AccountUsage{Provider: message.GetProvider(), Source: message.GetSource(), Fetched: message.GetFetched().AsTime(), Title: message.GetTitle(),
		Plan: message.GetPlan(), Details: message.GetDetails(), Unavailable: message.GetUnavailable(), Bars: message.GetBars()}
	for _, window := range message.GetWindows() {
		w := UsageWindow{Label: window.GetLabel(), Detail: window.GetDetail(), Model: window.GetModel()}
		if window.Used != nil {
			w.Used = new(window.GetUsed())
		}
		if window.GetResets() != nil {
			w.Resets = new(window.GetResets().AsTime())
		}
		u.Windows = append(u.Windows, w)
	}
	return u
}

// Lines of the reading: bars under the plan's name for a plan sold by its windows, else each window's share left and when it resets
func (u *AccountUsage) Lines(markdown bool) []string {
	if u == nil {
		return nil
	}
	if u.Bars {
		return u.barLines()
	}
	bold := ""
	if markdown {
		bold = "**"
	}
	plan := ""
	if u.Plan != "" {
		plan = " (" + u.Plan + ")"
	}
	lines := []string{"📈 " + bold + u.Title + bold, "Provider: " + u.Provider + plan}
	for _, w := range u.Windows {
		line := w.Label + ": unavailable"
		if w.Used != nil {
			line = fmt.Sprintf("%s: %d%% remaining (%d%% used)", w.Label, max(0, roundEven(100-*w.Used)), max(0, roundEven(*w.Used)))
		}
		switch {
		case w.Resets != nil:
			line += " • resets " + resetStamp(*w.Resets)
		case w.Detail != "":
			line += " • " + w.Detail
		}
		lines = append(lines, line)
	}
	lines = append(lines, u.Details...)
	if u.Unavailable != "" {
		lines = append(lines, "Unavailable: "+u.Unavailable)
	}
	return lines
}

// Plain stacked bars with no emoji or markdown, so their columns line up
func (u *AccountUsage) barLines() []string {
	lines := []string{u.Title, "Provider: " + u.Provider}
	if u.Plan != "" {
		lines = []string{u.Plan}
	}
	width := 0
	for _, w := range u.Windows {
		width = max(width, utf8.RuneCountInString(w.Label))
	}
	for _, w := range u.Windows {
		line := w.Label + strings.Repeat(" ", width-utf8.RuneCountInString(w.Label)) + "  unavailable"
		if w.Used != nil {
			used := min(max(*w.Used, 0), 100) / 100
			line = w.Label + strings.Repeat(" ", width-utf8.RuneCountInString(w.Label)) + "  " + Bar(used, barWidth) + "  " + PercentUsed(used) + " used"
		}
		switch {
		case w.Resets != nil:
			line += "  ·  " + ResetsIn(*w.Resets, time.Now())
		case w.Detail != "":
			line += "  ·  " + w.Detail
		}
		lines = append(lines, line)
	}
	lines = append(lines, u.Details...)
	if u.Unavailable != "" {
		lines = append(lines, "Unavailable: "+u.Unavailable)
	}
	return lines
}

func roundEven(v float64) int {
	return int(math.RoundToEven(v))
}

// Local time of a reset with the time left, now when it is past
func resetStamp(at time.Time) string {
	stamp := at.Local().Format("2006-01-02 15:04 MST")
	total := int64(time.Until(at).Seconds())
	if total <= 0 {
		return fmt.Sprintf("now (%s)", stamp)
	}
	hours, minutes := total/3600, total%3600/60
	switch {
	case hours >= 24:
		return fmt.Sprintf("in %dd %dh (%s)", hours/24, hours%24, stamp)
	case hours > 0:
		return fmt.Sprintf("in %dh %dm (%s)", hours, minutes, stamp)
	}
	return fmt.Sprintf("in %dm (%s)", minutes, stamp)
}

// Bracketed bar whose filled cells show the share, never empty above nothing nor full below all of it
func Bar(fraction float64, width int) string {
	fraction = min(max(fraction, 0), 1)
	filled := int(math.RoundToEven(fraction * float64(width)))
	if fraction > 0 && filled == 0 {
		filled = 1
	}
	if fraction < 1 && filled == width {
		filled = width - 1
	}
	return "[" + strings.Repeat(barFill, filled) + strings.Repeat(barEmpty, width-filled) + "]"
}

// Share used as a whole percent rounded down, a sliver reading as below one percent
func PercentUsed(fraction float64) string {
	points := int64(math.Round(min(max(fraction, 0), 1)*10000*1e6) / 1e6)
	switch {
	case points <= 0:
		return "0%"
	case points < 100:
		return "<1%"
	}
	return fmt.Sprintf("%d%%", points/100)
}

// When a window starts over as resets in a span of days and hours, hours or minutes; resets soon once that is under a minute
func ResetsIn(at, now time.Time) string {
	seconds := int64(math.Floor(at.Sub(now).Seconds()))
	if seconds < 60 {
		return "resets soon"
	}
	days, hours, minutes := seconds/86_400, seconds%86_400/3_600, seconds%3_600/60
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("resets in %dd %dh", days, hours)
	case days > 0:
		return fmt.Sprintf("resets in %dd", days)
	case hours > 0:
		return fmt.Sprintf("resets in %dh", hours)
	}
	return fmt.Sprintf("resets in %dm", minutes)
}
