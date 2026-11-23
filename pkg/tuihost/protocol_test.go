package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Transport stand in that records the frames written to it
type recordingTransport struct{ frames []*pyjson.Object }

func (r *recordingTransport) Write(obj *pyjson.Object) bool {
	r.frames = append(r.frames, obj)
	return true
}

func (r *recordingTransport) Close() {}
