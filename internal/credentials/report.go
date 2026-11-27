package credentials

import "context"

// One thing a status says of a sign-in, such as its region or who it signs in as
type Detail struct {
	Name  string
	Value string
}

// Registrant that says more of an account's sign-in than its credential does, such as the identity and region AWS signs as; it may ask
// the provider. False for an account it doesn't keep
type Reporter interface {
	Report(ctx context.Context, account Account) ([]Detail, bool)
}

// What the account's registrant says of its sign-in beyond its credential
func (r *Registry) Report(ctx context.Context, account Account) ([]Detail, bool) {
	for _, registrant := range r.registrants {
		if reporter, ok := registrant.(Reporter); ok {
			if details, ok := reporter.Report(ctx, account); ok {
				return details, true
			}
		}
	}
	return nil, false
}
