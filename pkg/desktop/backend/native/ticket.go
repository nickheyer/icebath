package native

import (
	"errors"
	"math/big"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/gateway"
)

// ticketRefusal carries the discovery reason of a gateway ticket refusal
type ticketRefusal struct{ err *gateway.DiscoveryError }

func (r ticketRefusal) Error() string        { return r.err.Error() }
func (r ticketRefusal) Unwrap() error        { return r.err }
func (r ticketRefusal) TicketReason() string { return r.err.Reason }

// Mints a session ticket through the gateway control client and its private home policy
func MintGatewayTicket(endpoint backend.GatewayEndpoint, purpose string) (string, error) {
	ep := &gateway.GatewayEndpoint{ProfileID: endpoint.ProfileID, InstanceID: endpoint.InstanceID, RuntimeProtocol: endpoint.RuntimeProtocol,
		APIOrigin: endpoint.APIOrigin, Supervisor: endpoint.Supervisor, Capabilities: map[string]bool{}}
	if endpoint.ControlHome != nil {
		ep.ControlHome = *endpoint.ControlHome
	}
	if epoch, ok := new(big.Int).SetString(endpoint.AuthorityEpoch.String(), 10); ok {
		ep.AuthorityEpoch = epoch
	}
	for _, c := range endpoint.Capabilities {
		ep.Capabilities[c] = true
	}
	if endpoint.CodeSHA != nil {
		ep.CodeSHA = *endpoint.CodeSHA
	}
	ticket, err := gateway.MintSessionTicket(endpoint.ProfileID, ep, purpose, backend.TicketScope(purpose))
	var de *gateway.DiscoveryError
	if errors.As(err, &de) {
		return "", ticketRefusal{de}
	}
	return ticket, err
}
