package tools

import (
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Relayed turn author as an in process value that only the relay builds and a JSON client never can
type DeliveryAuthor struct {
	Author map[string]any
}

// Author holding a copy of the given fields
func NewDeliveryAuthor(author map[string]any) *DeliveryAuthor {
	copied := make(map[string]any, len(author))
	for key, value := range author {
		copied[key] = value
	}
	return &DeliveryAuthor{Author: copied}
}

// Whether other is an author with equal fields
func (d *DeliveryAuthor) Equal(other any) bool {
	o, ok := other.(*DeliveryAuthor)
	return ok && o != nil && d != nil && pyjson.Equal(o.Author, d.Author)
}

// Repr naming the author fields
func (d *DeliveryAuthor) String() string { return "DeliveryAuthor(" + pyrepr.Repr(d.Author) + ")" }
