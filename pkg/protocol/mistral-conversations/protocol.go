// Mistral Conversations API, POST /v1/conversations under the api.mistral.ai base URL
package mistralconversations

import "github.com/nickheyer/icebath/pkg/protocol"

const (
	path       = "/v1/conversations"
	objectName = "entry"
)

type Protocol struct{}

var _ protocol.Protocol = Protocol{}

func init() {
	protocol.Register(Protocol{})
}

func (Protocol) ID() protocol.ID { return protocol.MistralConversations }
