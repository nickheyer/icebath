package scenario

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"
)

// A foreground gateway is reported as running with its pid, stopped by the stop command and reported as gone
func TestScenarioGatewayRunStatusStop(t *testing.T) {
	s := Start(t, Setup{})
	s.Run("gateway", "status").Exit(0).Has("✗ Gateway is not running")
	gw := s.Spawn("gateway", "run")
	var state map[string]any
	s.Eventually("gateway_state.json reporting running", waitLimit, func() bool {
		raw, err := os.ReadFile(s.Path("gateway_state.json"))
		return err == nil && json.Unmarshal(raw, &state) == nil && state["gateway_state"] == "running"
	})
	pid := strconv.Itoa(int(state["pid"].(float64)))
	s.HasJSON(s.Read("gateway.pid"), `{"pid": `+pid+`, "kind": "icebath-gateway"}`)
	s.Run("gateway", "status").Exit(0).Has("✓ Gateway is running (PID: "+pid+")", "(Running manually, not as a system service)")
	s.Run("status").Exit(0).Match(`Gateway:\s+✓ running`)

	s.RunStep(Step{Args: []string{"gateway", "stop"}, Timeout: 90 * time.Second}).Exit(0).Has("Stopped")
	ended := gw.Wait()
	if ended.Code != 0 {
		s.Failf("a gateway stopped by the stop command exits 0, got %d\n%s", ended.Code, clip(ended.Text()))
	}
	s.Run("gateway", "status").Exit(0).Has("✗ Gateway is not running")
	s.Run("status").Exit(0).Match(`Gateway:\s+✗ stopped`)
	if s.Exists("gateway.pid") {
		s.Failf("gateway.pid survived the stop")
	}
}
