package system

import "sync"

// PowerEvents are the OS power transitions the monitor reports
type PowerEvents struct {
	Suspend      func()
	Resume       func()
	UnlockScreen func()
	OnBattery    func()
	OnAC         func()
}

// PowerMonitor is the OS power source and sleep watcher behind Electron powerMonitor
type PowerMonitor interface {
	// Starts delivering events, the battery state readable once it returns
	Start(ev PowerEvents) error
	IsOnBatteryPower() bool
	// Seconds since the last user input
	SystemIdleTime() int
}

// powerState mirrors the AC or battery state for the renderer, nil until the monitor first reads it
type powerState struct {
	mu        sync.Mutex
	onBattery *bool
	resume    []func()
}

// Answers getOnBattery, false until the monitor read a state
func (s *Service) GetOnBattery() bool {
	s.power.mu.Lock()
	defer s.power.mu.Unlock()
	return s.power.onBattery != nil && *s.power.onBattery
}

// Pushes a changed AC or battery state to every window
func (s *Service) broadcastBatteryState(next bool) {
	s.power.mu.Lock()
	if s.power.onBattery != nil && *s.power.onBattery == next {
		s.power.mu.Unlock()
		return
	}
	s.power.onBattery = &next
	s.power.mu.Unlock()
	s.o.Host.Broadcast("power-battery", next)
}

// Tells the main window the machine woke so it reconnects its socket
func (s *Service) sendPowerResume() {
	s.o.Host.SendMain("power-resume", nil)
}

// Adds a listener run on every resume, the pooled remote revalidation of the connections group
func (s *Service) OnPowerResume(f func()) {
	s.power.mu.Lock()
	defer s.power.mu.Unlock()
	s.power.resume = append(s.power.resume, f)
}

// Registers the power listeners once after app ready, the logged monitor failure leaving the battery state unread
func (s *Service) registerPowerListeners() {
	resume := func() {
		s.sendPowerResume()
		s.keepAwake.NoteResume()
		s.power.mu.Lock()
		listeners := append([]func(){}, s.power.resume...)
		s.power.mu.Unlock()
		for _, f := range listeners {
			f()
		}
	}
	err := s.o.Power.Start(PowerEvents{
		Suspend:      func() { s.keepAwake.NoteSuspend(s.o.Power.SystemIdleTime()) },
		Resume:       resume,
		UnlockScreen: s.sendPowerResume,
		OnBattery:    func() { s.broadcastBatteryState(true) },
		OnAC:         func() { s.broadcastBatteryState(false) },
	})
	if err != nil {
		s.o.Log("[power] monitor unavailable: " + err.Error())
		return
	}
	on := s.o.Power.IsOnBatteryPower()
	s.power.mu.Lock()
	s.power.onBattery = &on
	s.power.mu.Unlock()
}
