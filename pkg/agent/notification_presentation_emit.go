package agent

import "github.com/nickheyer/icebath/pkg/tuigateway"

func init() { tuigateway.SetEventPresentationMuted(EventPresentationMuted) }
