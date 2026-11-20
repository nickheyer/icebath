package main

// One generated file, how it renders from the art and the shape it must have
type target struct {
	rel    string
	render func(a *art) ([]byte, error)
	want   shape
}

// Every generated output relative to the checkout root, in write order
func allTargets() []target {
	return []target{
		{"pkg/app/banner_art.go", func(a *art) ([]byte, error) { return a.src.bannerGo(), nil }, shape{format: "TEXT"}},
	}
}
