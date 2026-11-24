package tuiserver

import (
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Typed value of a generic row a sender projects, a misfit reported against the method
func typedRow[T any](method string, raw any) T {
	var row T
	if err := tuigateway.Strict(raw, &row); err != nil {
		tuigateway.ReportMisfit("result of", method, err)
	}
	return row
}
