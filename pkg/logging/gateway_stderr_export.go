package logging

// Gateway stderr record format, timestamp, level, logger name and message
const gatewayStderrFormat = "%(asctime)s %(levelname)s %(name)s: %(message)s"

// Redacting formatter of the gateway stderr stream
func GatewayStderrFormatter() (*Formatter, error) {
	return newRedactingFormatter(gatewayStderrFormat, "")
}

// Adds the gateway console handler at level to the root logger, lowering the root level to reach it
func AddGatewayStderrHandler(level int) error {
	formatter, err := GatewayStderrFormatter()
	if err != nil {
		return err
	}
	h := newStreamHandler(safeStderr())
	h.SetLevel(level)
	h.SetFormatter(formatter)
	root.AddHandler(h)
	if level < root.Level() {
		root.SetLevel(level)
	}
	return nil
}
