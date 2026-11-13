package system

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
)

// IdleSleepMinSeconds is the idle time before a suspend that counts as the machine sleeping on its own
const IdleSleepMinSeconds = 55

// KeepAwakeControllerOptions are the blocker, the persisted file and the hooks the controller reads
type KeepAwakeControllerOptions struct {
	Blocker    PowerSaveBlocker
	ConfigPath string
	IsWorking  func() bool
	Log        func(line string)
	// Shows the one time keep awake offer in the main window, false when it has none
	Offer func() bool
}

// KeepAwakeController owns the one blocker and the persisted mode and offer flag
type KeepAwakeController struct {
	mu           sync.Mutex
	o            KeepAwakeControllerOptions
	keepAwake    *KeepAwake
	mode         KeepAwakeMode
	offered      bool
	sleptMidTurn bool
}

// Controller in mode off until Restore reads the persisted mode
func NewKeepAwakeController(o KeepAwakeControllerOptions) *KeepAwakeController {
	return &KeepAwakeController{o: o, keepAwake: NewKeepAwake(o.Blocker, PreventAppSuspension), mode: KeepAwakeOff}
}

type keepAwakeFile struct {
	Mode    KeepAwakeMode `json:"mode"`
	Offered bool          `json:"offered"`
}

func (c *KeepAwakeController) persist() {
	body, _ := json.MarshalIndent(keepAwakeFile{Mode: c.mode, Offered: c.offered}, "", "  ")
	err := os.MkdirAll(filepath.Dir(c.o.ConfigPath), 0o777)
	if err == nil {
		err = os.WriteFile(c.o.ConfigPath, body, 0o666)
	}
	if err != nil {
		c.o.Log("[keep-awake] write failed: " + hardening.NodeErr(err).Error())
	}
}

func (c *KeepAwakeController) applyLocked(working bool) {
	c.keepAwake.Set(KeepAwakeWanted(c.mode, working))
}

// Holds or releases the blocker for the mode and whether a turn is in flight
func (c *KeepAwakeController) Apply(working bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applyLocked(working)
}

// Applies the mode against the live active work picture
func (c *KeepAwakeController) ApplyCurrent() {
	c.Apply(c.o.IsWorking())
}

// Reads the persisted mode and offer flag, the default mode for a missing or unreadable file, then applies
func (c *KeepAwakeController) Restore() {
	var saved any
	if body, err := os.ReadFile(c.o.ConfigPath); err == nil {
		if json.Unmarshal(body, &saved) != nil {
			saved = nil
		}
	}
	working := c.o.IsWorking()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mode = ReadKeepAwakeMode(saved)
	record, _ := saved.(map[string]any)
	offered, _ := record["offered"].(bool)
	c.offered = offered
	c.applyLocked(working)
}

// Switches to a renderer chosen mode, ignoring values that are no mode, then applies and persists
func (c *KeepAwakeController) SetMode(value any) {
	next := ParseKeepAwakeMode(value)
	if next == nil {
		return
	}
	working := c.o.IsWorking()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mode = *next
	c.applyLocked(working)
	c.persist()
}

// Remembers a suspend that hit a running turn with keep awake off after the machine idled
func (c *KeepAwakeController) NoteSuspend(idleSeconds int) {
	working := c.o.IsWorking()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleptMidTurn = c.mode == KeepAwakeOff && working && idleSeconds >= IdleSleepMinSeconds
}

// Offers keep awake once after a wake from a sleep that interrupted a turn
func (c *KeepAwakeController) NoteResume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sleptMidTurn && !c.offered && c.o.Offer() {
		c.offered = true
		c.persist()
	}
	c.sleptMidTurn = false
}
