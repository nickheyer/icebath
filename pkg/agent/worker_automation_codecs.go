package agent

import (
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/state"
)

type goalAutomationState struct{ s *goals.GoalState }

func (a goalAutomationState) Status() any             { return a.s.Status }
func (a goalAutomationState) SetStatus(status string) { a.s.Status = status }
func (a goalAutomationState) ToJSON() (string, error) { return a.s.ToJSON() }

type heartbeatAutomationState struct{ s *HeartbeatState }

func (a heartbeatAutomationState) Status() any             { return a.s.Status }
func (a heartbeatAutomationState) SetStatus(status string) { a.s.Status = status }
func (a heartbeatAutomationState) ToJSON() (string, error) { return a.s.ToJSON() }

type loopAutomationState struct{ s *SessionLoopState }

func (a loopAutomationState) Status() any             { return a.s.Status }
func (a loopAutomationState) SetStatus(status string) { a.s.Status = status }
func (a loopAutomationState) ToJSON() (string, error) { return a.s.ToJSON() }

// Installs the goal, heartbeat and loop codecs the worker rotation migrates automation rows with
func init() {
	state.WorkerAutomationCodecs["goal"] = func(raw string) (state.AutomationState, error) {
		s, err := goals.GoalStateFromJSON(raw)
		if err != nil {
			return nil, err
		}
		return goalAutomationState{s}, nil
	}
	state.WorkerAutomationCodecs["heartbeat"] = func(raw string) (state.AutomationState, error) {
		s, err := HeartbeatStateFromJSON(raw)
		if err != nil {
			return nil, err
		}
		return heartbeatAutomationState{s}, nil
	}
	state.WorkerAutomationCodecs["loop"] = func(raw string) (state.AutomationState, error) {
		s, err := SessionLoopStateFromJSON(raw)
		if err != nil {
			return nil, err
		}
		return loopAutomationState{s}, nil
	}
}
