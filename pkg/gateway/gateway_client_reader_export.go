package gateway

// Whether the reader finished, every pending answer already failed and no further frame arriving
func (c *GatewayClient) ReaderDone() bool {
	select {
	case <-c.readDone:
		return true
	default:
		return false
	}
}
