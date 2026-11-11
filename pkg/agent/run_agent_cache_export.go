package agent

import "github.com/nickheyer/icebath/pkg/state"

// Wall time of the agent's latest activity
func (a *AIAgent) LastActivityTS() float64 { return a.lastActivityTS }

// Transcript index the latest fully successful session database flush reached
func (a *AIAgent) LastFlushedDBIdx() int { return a.lastFlushedDBIdx }

// Alias and backing model pair of a recorded Nous welcome switch, nil when none
func (a *AIAgent) NousModelSwitch() *[2]string { return a.nousModelSwitch }

// Drops the live transcript so an evicted agent frees its history
func (a *AIAgent) ClearSessionMessages() { a.sessionMessages = []*state.Message{} }

// Drops the flushed prefix snapshot that pins the transcript rows
func (a *AIAgent) ClearDBFlushScanPrefix() { a.dbFlushScanPrefix = nil }
