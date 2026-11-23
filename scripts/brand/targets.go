package main

// One generated file and how it renders from the art
type target struct {
	rel    string
	render func(a *art) ([]byte, error)
}

// Every generated output relative to the checkout root, in write order
func allTargets() []target {
	return []target{
		{"pkg/ui/theme/art.go", func(a *art) ([]byte, error) { return a.src.artGo(), nil }},
	}
}
