// Ollama native chat wire protocol, POST /api/chat relative to the Ollama host such as http://localhost:11434
package ollamachat

import "github.com/nickheyer/icebath/pkg/protocol"

const path = "/api/chat"

type Protocol struct{}

var _ protocol.Protocol = Protocol{}

func init() {
	protocol.Register(Protocol{})
}

func (Protocol) ID() protocol.ID { return protocol.OllamaChat }
