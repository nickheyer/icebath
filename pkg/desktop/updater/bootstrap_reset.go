package updater

import "fmt"

// FirstRunGate is the first run setup choice gate of the connections group
type FirstRunGate interface {
	ContinueLocal()
	ResetForRetry()
	ResetForRepair()
}

// BootstrapReply is the reply of the bootstrap reset, repair and continue calls
type BootstrapReply struct {
	OK      bool   `json:"ok"`
	Bundled bool   `json:"bundled,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Whether the next backend resolution bypasses a usable active runtime to rerun the installer
func (s *Service) BootstrapRepairRequested() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bootstrapRepairRequested
}

// Clears the latched failure and connection state so the next start reruns the whole backend flow
func (s *Service) ResetBootstrap() BootstrapReply {
	s.log("[bootstrap] reset requested by renderer; clearing latched failure")
	_ = s.backend.TeardownPrimary(false)
	s.SetBootstrapFailure(nil)
	s.backend.ClearStartFailures()
	s.firstRun.ResetForRetry()
	s.resetBootstrapSnapshot()
	return BootstrapReply{OK: true}
}

// Forces the next start through the installer when the guard calls for it, refused on a bundled install
func (s *Service) RepairBootstrap() BootstrapReply {
	if s.installShape() == "bundled" {
		s.log("[bootstrap] repair refused on a bundled install; repair means reinstalling the app")
		return BootstrapReply{Error: "bundled-immutable"}
	}
	s.mu.Lock()
	s.bootstrapRepairAttempt++
	attempt := s.bootstrapRepairAttempt
	s.mu.Unlock()
	alive := s.backend.PrimaryProcessAlive()
	decision := decideBootstrapRepair(attempt, maxBootstrapRepairSoft, alive)
	s.log(fmt.Sprintf("[bootstrap] repair requested by renderer; forcing reinstall + clearing latched failure (attempt=%d/%d, primaryBackendAlive=%t, hardReinstall=%t): %s",
		decision.Attempt, maxBootstrapRepairSoft, alive, decision.HardReinstall, decision.Reason))
	s.mu.Lock()
	s.bootstrapRepairRequested = decision.HardReinstall
	s.bootstrapFailure = nil
	s.mu.Unlock()
	s.backend.ClearStartFailures()
	s.firstRun.ResetForRepair()
	_ = s.backend.TeardownPrimary(false)
	return BootstrapReply{OK: true}
}

// Resumes the first launch bootstrap the setup choice gated, the local install chosen
func (s *Service) ContinueBootstrapLocal() BootstrapReply {
	s.log("[bootstrap] local install selected by renderer; continuing first-launch bootstrap")
	s.firstRun.ContinueLocal()
	return BootstrapReply{OK: true}
}
