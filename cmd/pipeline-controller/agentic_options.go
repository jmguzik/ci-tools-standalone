package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/prow/pkg/flagutil"
)

const defaultAgenticTimeout = 20 * time.Minute

// Planner trust and deadlines are deployment-wide, not repository-controlled.
type agenticOptions struct {
	timeout        time.Duration
	trustedAuthors flagutil.Strings
	stateDir       string
	stateTTL       time.Duration
}

func (o *agenticOptions) addFlags(fs *flag.FlagSet) {
	fs.DurationVar(&o.timeout, "agentic-timeout", defaultAgenticTimeout, "Time to wait for Chai after dispatch prerequisites hold before using normal selection.")
	fs.Var(&o.trustedAuthors, "agentic-trusted-author", "Exact GitHub login trusted to post Chai test plans. Repeat for multiple authors; no default.")
	fs.StringVar(&o.stateDir, "agentic-state-dir", "", "Directory on a persistent volume for agentic recovery records; required for enrolled repositories outside dry-run.")
	fs.DurationVar(&o.stateTTL, "agentic-state-ttl", 0, "Expire local agentic records after this duration without modification; 0 disables expiry (720h is 30 days). Enabling permanently requires fresh plans when local state is missing.")
}

func (o *agenticOptions) validate() error {
	if o.timeout <= 0 {
		return fmt.Errorf("--agentic-timeout must be a positive duration")
	}
	if o.stateTTL < 0 {
		return fmt.Errorf("--agentic-state-ttl must not be negative")
	}
	for _, author := range o.trustedAuthors.Strings() {
		if author == "" || strings.ContainsAny(author, " \t\r\n/@,") {
			return fmt.Errorf("invalid --agentic-trusted-author %q: use an exact GitHub login", author)
		}
	}
	return nil
}

func (o *agenticOptions) validateEnabled() error {
	if err := o.validate(); err != nil {
		return err
	}
	if len(o.trustedAuthors.Strings()) == 0 {
		return fmt.Errorf("agentic mode requires --agentic-trusted-author")
	}
	return nil
}

func (a *agenticController) validateEnrollment() error {
	if a.hasAgenticEnrollment() {
		return a.options.validateEnabled()
	}
	return nil
}
