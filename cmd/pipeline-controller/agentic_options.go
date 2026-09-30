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
}

func (o *agenticOptions) addFlags(fs *flag.FlagSet) {
	fs.DurationVar(&o.timeout, "agentic-timeout", defaultAgenticTimeout, "Time to wait for Chai after dispatch prerequisites hold before using normal selection.")
	fs.Var(&o.trustedAuthors, "agentic-trusted-author", "Exact GitHub login trusted to post Chai test plans. Repeat for multiple authors; no default.")
}

func (o *agenticOptions) validate() error {
	if o.timeout <= 0 {
		return fmt.Errorf("--agentic-timeout must be a positive duration")
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
	for _, watcher := range []*watcher{a.watcher, a.lgtmWatcher} {
		if watcher == nil {
			continue
		}
		for _, repos := range watcher.getConfig() {
			for _, cfg := range repos {
				if cfg.Agentic.enabled() {
					return a.options.validateEnabled()
				}
			}
		}
	}
	return nil
}
