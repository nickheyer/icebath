package install

type LogStream string

const (
	Stdout LogStream = "stdout"
	Stderr LogStream = "stderr"
)

// One output line of the install on one stream
type Event struct {
	Line   string
	Stream LogStream
}
