package gateway

import (
	"context"
	"math"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Witness outcome of a tick probe, absent when no witness node exists
type tickWitness int

const (
	tickAbsent tickWitness = iota
	tickAnswered
	tickSilent
)

// Knobs of the loop liveness probe, zero values select the Python defaults
type LoopLivenessOptions struct {
	StaleAfterS  *float64
	Home         string
	TickTimeoutS float64
	TickStrikes  *int
	TickGapS     *float64
}

var (
	lifecycleWallNow             = time.Now
	lifecycleHeartbeatPath       = GetLoopHeartbeatPath
	lifecycleTickSocketPath      = GetLoopTickSocketPath
	lifecycleProbeTickSustained  = probeLoopTickSocketSustained
	lifecycleProbeTickSocketOnce = probeLoopTickSocket
)

// Pings the unix socket witness of pid, absent when no socket node exists
func probeLoopTickSocket(ctx context.Context, pid int, h string, timeoutS float64) tickWitness {
	path := lifecycleTickSocketPath(ctx, h, &pid)
	st, err := os.Stat(path)
	if err != nil || st.Mode()&os.ModeSocket == 0 {
		return tickAbsent
	}
	return pingLoopTickWitness("unix", path, timeoutS)
}

// Connects to a witness and expects the single byte 1, silent on refusal, timeout or any error
func pingLoopTickWitness(network, address string, timeoutS float64) tickWitness {
	timeout := time.Duration(math.Max(timeoutS, 0) * float64(time.Second))
	conn, err := net.DialTimeout(network, address, timeout)
	if err != nil {
		return tickSilent
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if err != nil || n != 1 || buf[0] != '1' {
		return tickSilent
	}
	return tickAnswered
}

// Pings the loopback TCP witness used on Windows, absent for an invalid port
func probeLoopTickTCP(port int, timeoutS float64) tickWitness {
	if port <= 0 || port > 65535 {
		return tickAbsent
	}
	return pingLoopTickWitness("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), timeoutS)
}

// Probes the witness up to strikes times gap apart, answered once it replies and absent when the node vanishes
func probeLoopTickSocketSustained(ctx context.Context, pid int, h string, timeoutS float64, strikes int, gapS float64, tcpPort int, haveTCP bool) tickWitness {
	total := max(strikes, 0)
	for attempt := 0; attempt < total; attempt++ {
		var result tickWitness
		if haveTCP {
			result = probeLoopTickTCP(tcpPort, timeoutS)
		} else {
			result = lifecycleProbeTickSocketOnce(ctx, pid, h, timeoutS)
		}
		if result == tickAnswered {
			return tickAnswered
		}
		if result == tickAbsent {
			return tickAbsent
		}
		if attempt < total-1 && gapS > 0 {
			lifecycleSleep(time.Duration(gapS * float64(time.Second)))
		}
	}
	return tickSilent
}

// Python int() of a heartbeat payload value, false where int() raises
func payloadInt(v any) (int, bool) {
	switch x := v.(type) {
	case nil:
		return 0, false
	case string:
		n, ok := pyIntOf(pystr.Strip(x))
		if !ok || x == "" || !n.IsInt64() {
			return 0, false
		}
		return int(n.Int64()), true
	}
	n, ok := pyIntOf(v)
	if !ok || !n.IsInt64() {
		return 0, false
	}
	return int(n.Int64()), true
}

// Classifies the event loop of a gateway pid as alive, wedged or unknown from its heartbeat and tick witness
func ProbeGatewayLoopLiveness(ctx context.Context, pid int, o LoopLivenessOptions) string {
	staleBudget := DefaultLoopLivenessStaleAfterS
	if o.StaleAfterS != nil {
		staleBudget = math.Max(*o.StaleAfterS, 0)
		if math.IsNaN(*o.StaleAfterS) {
			staleBudget = *o.StaleAfterS
		}
	}
	tickTimeout := o.TickTimeoutS
	if tickTimeout == 0 {
		tickTimeout = tickProbeDefaultTimeoutS
	}
	strikes := tickProbeDefaultStrikes
	if o.TickStrikes != nil {
		strikes = *o.TickStrikes
	}
	gap := tickProbeDefaultGapS
	if o.TickGapS != nil {
		gap = *o.TickGapS
	}
	path := lifecycleHeartbeatPath(ctx, o.Home)
	st, err := os.Stat(path)
	if err != nil {
		return GatewayLoopUnknown
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return GatewayLoopUnknown
	}
	decoded, err := pyjson.LoadsBytes(raw)
	if err != nil {
		return GatewayLoopUnknown
	}
	payload, ok := decoded.(*pyjson.Object)
	if !ok {
		return GatewayLoopUnknown
	}
	pidValue, present := payload.Get("pid")
	if !present {
		pidValue = int64(0)
	}
	heartbeatPid, ok := payloadInt(pidValue)
	if !ok {
		return GatewayLoopUnknown
	}
	if heartbeatPid <= 0 || pid <= 0 || heartbeatPid != pid {
		return GatewayLoopUnknown
	}
	portValue, _ := payload.Get("loop_tick_tcp_port")
	tcpPort, haveTCP := payloadInt(portValue)
	var witness tickWitness
	armed, armedPresent := any(true), true
	if haveTCP && tcpPort > 0 {
		witness = probeLoopTickTCP(tcpPort, tickTimeout)
	} else {
		witness = lifecycleProbeTickSocketOnce(ctx, pid, o.Home, tickTimeout)
		armed, armedPresent = payload.Get("loop_tick_socket")
	}
	if witness == tickAnswered {
		return GatewayLoopAlive
	}
	age := float64(lifecycleWallNow().UnixNano()-st.ModTime().UnixNano()) / 1e9
	if age <= staleBudget {
		if witness == tickSilent {
			return GatewayLoopUnknown
		}
		return GatewayLoopAlive
	}
	if !armedPresent {
		return GatewayLoopWedged
	}
	if b, isBool := armed.(bool); !isBool || !b {
		return GatewayLoopUnknown
	}
	if witness == tickSilent {
		switch lifecycleProbeTickSustained(ctx, pid, o.Home, tickTimeout, strikes-1, gap, tcpPort, haveTCP) {
		case tickSilent:
			return GatewayLoopWedged
		case tickAnswered:
			return GatewayLoopAlive
		}
		return GatewayLoopUnknown
	}
	return GatewayLoopUnknown
}
