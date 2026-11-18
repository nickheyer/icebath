package pm

// UpdateArgs are pm update's arguments, Target empty for every target
type UpdateArgs struct {
	Names      []string
	Check      bool
	Target     string
	SkipTarget []string
	UV         bool
	NPM        bool
	Termux     bool
}
