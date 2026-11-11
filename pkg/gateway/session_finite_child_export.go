package gateway

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// Headless clarify reply of a finite turn whose viewer cannot answer
func FiniteClarifyReply() *pyjson.Object { return finiteClarifyReply() }
