package arena

import (
	"fmt"
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Longest wall time the replay waits between two changes, longer silences of the turn squeezed to it
const replayGap = 1500 * time.Millisecond

// Rates the replay plays at
var speeds = []float64{1, 2, 4, 8}

// Replay of a turn along its tape, the cursor the moment the floor shows
type replay struct {
	sprint   string
	cursor   time.Time
	playing  bool
	speed    int
	epoch    int
	epochSeq int
}

// Moves the cursor by a step of wall time, squeezing silences, reporting whether it reached the end
func (r *replay) advance(t tape, dt time.Duration) bool {
	if !r.playing {
		return false
	}
	next := r.cursor.Add(time.Duration(float64(dt) * speeds[r.speed]))
	if i := sort.Search(len(t.times), func(i int) bool { return t.times[i].After(r.cursor) }); i < len(t.times) {
		if t.times[i].Sub(next) > replayGap {
			next = t.times[i].Add(-replayGap)
		}
	} else if t.end.Sub(next) > replayGap {
		next = t.end.Add(-replayGap)
	}
	if !next.Before(t.end) {
		r.cursor, r.playing = t.end, false
		return true
	}
	r.cursor = next
	return false
}

// Moves the cursor to the change before it, or the start, resetting the floor
func (r *replay) back(t tape) {
	i := sort.Search(len(t.times), func(i int) bool { return !t.times[i].Before(r.cursor) })
	r.cursor = t.start
	if i > 0 {
		r.cursor = t.times[i-1]
	}
	r.playing = false
	r.jump()
}

// Moves the cursor to the next change, letting that change play out on the floor
func (r *replay) forward(t tape) {
	i := sort.Search(len(t.times), func(i int) bool { return t.times[i].After(r.cursor) })
	r.cursor = t.end
	if i < len(t.times) {
		r.cursor = t.times[i]
	}
	r.playing = false
}

// Puts the cursor at a moment of the tape, resetting the floor
func (r *replay) seek(t tape, at time.Time) {
	switch {
	case at.Before(t.start):
		at = t.start
	case at.After(t.end):
		at = t.end
	}
	r.cursor = at
	r.jump()
}

// Starts a new epoch so the next view resets the floor at the cursor
func (r *replay) jump() {
	r.epoch++
	r.epochSeq = -1
}

// Share of the tape the cursor stands at
func (r *replay) share(t tape) float64 {
	span := t.end.Sub(t.start)
	if span <= 0 {
		return 1
	}
	return min(max(float64(r.cursor.Sub(t.start))/float64(span), 0), 1)
}

// Clock text of a span, minutes and seconds
func clockText(d time.Duration) string {
	d = max(d, 0).Round(time.Second)
	return fmt.Sprintf("%02d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

// Carries out a replay action on the turn shown
func (a *arena) replayAct(id string) tea.Cmd {
	s := a.snap
	if id == ActLive {
		a.replay = nil
		return nil
	}
	if s == nil {
		return wm.Notify("no turn is shown, pick one with [ or ] to replay it", wm.Info)
	}
	t, ok := a.tape(s.Sprint)
	if !ok {
		return nil
	}
	r := a.replay
	switch id {
	case ActReplay:
		switch {
		case r == nil:
			a.replay = &replay{sprint: s.Sprint, cursor: t.start, playing: true}
			a.replay.jump()
		case !r.playing && !r.cursor.Before(t.end):
			r.seek(t, t.start)
			r.playing = true
		default:
			r.playing = !r.playing
		}
	case ActSpeed:
		if r == nil {
			return wm.Notify("start a replay with space first", wm.Info)
		}
		r.speed = (r.speed + 1) % len(speeds)
	case ActStepBack, ActStepForward:
		if r == nil {
			r = &replay{sprint: s.Sprint, cursor: t.end}
			r.jump()
			a.replay = r
		}
		if id == ActStepBack {
			r.back(t)
		} else {
			r.forward(t)
		}
	}
	return nil
}

// Puts the replay at the moment under a column of the timeline, starting a paused replay when none runs
func (a *arena) seekTape(x int) {
	s := a.snap
	tb := a.lay.tape
	if s == nil || tb.w <= 0 {
		return
	}
	t, ok := a.tape(s.Sprint)
	if !ok {
		return
	}
	track := a.track()
	if track.w <= 1 {
		return
	}
	share := min(max(float64(x-track.x)/float64(track.w-1), 0), 1)
	at := t.start.Add(time.Duration(share * float64(t.end.Sub(t.start))))
	if a.replay == nil {
		a.replay = &replay{sprint: s.Sprint}
	}
	a.replay.seek(t, at)
}
