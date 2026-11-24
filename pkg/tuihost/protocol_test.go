package tuihost

import (
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Transport stand in that records the frames written to it
type recordingTransport struct{ frames []*jsonutil.Object }

func (r *recordingTransport) Write(obj *jsonutil.Object) bool {
	r.frames = append(r.frames, obj)
	return true
}

func (r *recordingTransport) Close() {}
