package main

import (
	"context"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/prow/pkg/flagutil"
)

func TestAgenticGlobalFlags(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		timeout time.Duration
		authors []string
		wantErr bool
	}{
		{name: "defaults for normal repositories", timeout: 20 * time.Minute},
		{name: "global overrides", args: []string{"--agentic-timeout=5m", "--agentic-trusted-author=chai[bot]", "--agentic-trusted-author=second-bot"}, timeout: 5 * time.Minute, authors: []string{"chai[bot]", "second-bot"}},
		{name: "invalid duration", args: []string{"--agentic-timeout=soon"}, wantErr: true},
		{name: "zero duration", args: []string{"--agentic-timeout=0s"}, wantErr: true},
		{name: "negative duration", args: []string{"--agentic-timeout=-1m"}, wantErr: true},
		{name: "empty identity", args: []string{"--agentic-trusted-author="}, wantErr: true},
		{name: "display name", args: []string{"--agentic-trusted-author=Chai Bot"}, wantErr: true},
		{name: "mention", args: []string{"--agentic-trusted-author=@chai"}, wantErr: true},
		{name: "repository path", args: []string{"--agentic-trusted-author=org/chai"}, wantErr: true},
		{name: "use repeated author flags", args: []string{"--agentic-trusted-author=chai,other"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var o agenticOptions
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			o.addFlags(fs)
			err := fs.Parse(tc.args)
			if err == nil {
				err = o.validate()
			}
			if tc.wantErr {
				if err == nil {
					t.Fatal("accepted invalid global agentic arguments")
				}
				return
			}
			if err != nil || o.timeout != tc.timeout || !reflect.DeepEqual(o.trustedAuthors.Strings(), tc.authors) {
				t.Fatalf("options = %+v, error = %v", o, err)
			}
		})
	}
}

func TestAgenticFlagsRegisteredByController(t *testing.T) {
	var o options
	fs := flag.NewFlagSet("pipeline-controller", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	err := o.parseArgs(fs, []string{"--config-file=pipeline.yaml", "--lgtm-config-file=lgtm.yaml", "--config-path=prow.yaml",
		"--agentic-timeout=3m", "--agentic-trusted-author=chai[bot]"})
	if err != nil || o.agentic.timeout != 3*time.Minute || !reflect.DeepEqual(o.agentic.trustedAuthors.Strings(), []string{"chai[bot]"}) {
		t.Fatalf("controller did not parse agentic flags: %+v / %v", o.agentic, err)
	}
}

func TestAgenticMissingGlobalTrustFailsClosed(t *testing.T) {
	for _, lgtm := range []bool{false, true} {
		f := newAgenticFixture(t, "auto")
		if lgtm {
			f.a.watcher, f.a.lgtmWatcher = f.a.lgtmWatcher, f.a.watcher
		}
		f.a.options.trustedAuthors = flagutil.Strings{}
		if err := f.a.validateEnrollment(); err == nil || !strings.Contains(err.Error(), "--agentic-trusted-author") {
			t.Fatalf("startup accepted agentic enrollment without global trust: %v", err)
		}
		// A repository enabled by a later reload must also block, including after
		// the fallback timeout; it must not silently ignore its Chai integration.
		f.plan(t, "job-a")
		f.passFirstStage(t)
		for range 2 {
			if err := f.a.reconcile(context.Background(), "org", "repo", 42, nil); err == nil {
				t.Fatal("enrolled repository accepted missing global trust")
			}
			f.now = f.now.Add(defaultAgenticTimeout)
		}
		gate, _ := f.gate(t)
		if gate.Conclusion != "failure" || f.jobs.creates != 0 {
			t.Fatal("missing trust allowed dispatch or fallback")
		}
	}
	legacy := &agenticController{watcher: &watcher{}, lgtmWatcher: &watcher{}}
	if err := legacy.validateEnrollment(); err != nil {
		t.Fatalf("normal-only configuration now requires agentic flags: %v", err)
	}
}

func TestAgenticUsesGlobalTimeout(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.a.options.timeout = 5 * time.Minute
	f.passFirstStage(t)
	f.reconcile(t, nil)
	f.now = f.now.Add(5*time.Minute - time.Second)
	f.reconcile(t, nil)
	if f.jobs.creates != 0 {
		t.Fatal("fallback ran before the global deadline")
	}
	f.now = f.now.Add(time.Second)
	f.reconcile(t, nil)
	_, state := f.gate(t)
	if state.Plan == nil || state.Plan.Source != "timeout" || f.jobs.creates != 2 {
		t.Fatalf("global timeout was not used: %+v; creates=%d", state.Plan, f.jobs.creates)
	}
}

func TestAgenticUsesGlobalAuthor(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	f.a.options.trustedAuthors = flagutil.NewStrings("deployment-bot")
	f.plan(t, "job-a") // The fixture's usual Chai author has not been trusted.
	f.passFirstStage(t)
	f.reconcile(t, nil)
	if f.jobs.creates != 0 {
		t.Fatal("default Chai identity bypassed the global allowlist")
	}
	comment := f.plan(t, "job-a")
	f.gh.comments[len(f.gh.comments)-1].User.Login = "deployment-bot"
	f.reconcile(t, &comment)
	if f.jobs.creates != 1 {
		t.Fatal("globally trusted author was not accepted")
	}
}
