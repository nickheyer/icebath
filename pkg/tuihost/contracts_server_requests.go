package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var ApprovalChoice = pydantic.Enum("ApprovalChoice", "once", "once", "session", "session", "always", "always", "deny", "deny")
