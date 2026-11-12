package gateway

// Whether a value is a string of 32 to 64 lowercase hex characters, the bot delivery id shape
func ValidDeliveryID(value any) bool {
	text, isText := value.(string)
	if !isText {
		return false
	}
	_, err := checkDeliveryID(text)
	return err == nil
}
