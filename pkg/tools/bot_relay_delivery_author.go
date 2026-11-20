package tools

// Relayed turn author as an in process value that only the relay builds and a JSON client never can
type DeliveryAuthor struct {
	Author map[string]any
}
