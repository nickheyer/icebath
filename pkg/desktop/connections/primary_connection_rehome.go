package connections

// PrimaryRehomeOptions are the first run resume, the bootstrap latch clear, the teardown and the applied notice of a re-home
type PrimaryRehomeOptions struct {
	ClearLocalBootstrapFailure func()
	Mode                       string
	NotifyConnectionApplied    func()
	ResumeFirstRunRemote       func() bool
	TeardownPrimaryBackend     func(soft bool) error
}

// Resumes a first run remote choice for a remote apply, else tears the primary down softly and announces the apply
func RehomePrimaryConnection(o PrimaryRehomeOptions) (resumedFirstRunRemote bool, err error) {
	if o.Mode == "remote" {
		resumedFirstRunRemote = o.ResumeFirstRunRemote()
		o.ClearLocalBootstrapFailure()
	}
	if resumedFirstRunRemote {
		return true, nil
	}
	if err := o.TeardownPrimaryBackend(true); err != nil {
		return false, err
	}
	o.NotifyConnectionApplied()
	return false, nil
}
