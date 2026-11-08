package gateway

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/kanban"
)

func TestKanbanStatusProbesDelegateToTheStatusSeams(t *testing.T) {
	p := kanban.GatewayStatusProbes
	if p.PidExists == nil || p.ProcessStartTime == nil || p.StartTimesAgree == nil || p.InstantiationEpoch == nil {
		t.Fatal("kanban status probes not installed at init")
	}
	oldExists, oldStart := statusPidExists, statusProcessStartTime
	statusPidExists = func(pid int) bool { return pid == 42 }
	statusProcessStartTime = func(pid int) any { return int64(pid * 10) }
	defer func() { statusPidExists, statusProcessStartTime = oldExists, oldStart }()
	if !p.PidExists(42) || p.PidExists(43) || p.ProcessStartTime(7) != int64(70) {
		t.Fatal("probes bypass the status seams")
	}
	if ok, err := p.StartTimesAgree(int64(5), 5.0005); !ok || err != nil {
		t.Fatal("start times within a millisecond disagree", ok, err)
	}
	if ok, err := p.StartTimesAgree(int64(5), 6); ok || err != nil {
		t.Fatal("distinct start times agree", ok, err)
	}
	if p.InstantiationEpoch() != CurrentInstantiationEpoch() {
		t.Fatal("epoch differs from drain control")
	}
}
