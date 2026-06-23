package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"

	"github.com/openshift/ci-tools-standalone/cmd/cluster-manifest-verifier/internal/verifier"
)

const defaultArgoCDServer = "openshift-gitops-server-openshift-gitops.apps.master.ci.devcluster.openshift.com"

type options struct {
	ReleaseRepoDir string
	ArgoCDServer   string
}

func parseOptions(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("cluster-manifest-verifier", flag.ContinueOnError)
	fs.StringVar(&o.ReleaseRepoDir, "release-repo-dir", "", "path to the openshift/release repository root (must contain clusters/gitops/apps)")
	fs.StringVar(&o.ArgoCDServer, "argocd-server", defaultArgoCDServer, "Argo CD API hostname")
	if err := fs.Parse(args); err != nil {
		return options{}, fmt.Errorf("parse flags: %w", err)
	}
	return o, nil
}

func (o *options) validate() error {
	if o.ReleaseRepoDir == "" {
		return errors.New("--release-repo-dir is required")
	}
	info, err := os.Stat(o.ReleaseRepoDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("--release-repo-dir must be a directory")
	}
	if _, err := os.Stat(filepath.Join(o.ReleaseRepoDir, "clusters", "gitops", "apps")); err != nil {
		return errors.New("--release-repo-dir must contain clusters/gitops/apps")
	}
	return nil
}

func main() {
	opts, err := parseOptions(os.Args[1:])
	if err != nil {
		logrus.Fatal(err)
	}

	if err := opts.validate(); err != nil {
		logrus.Fatalf("invalid options: %v", err)
	}

	v, err := verifier.New(verifier.Config{
		ReleaseRepoDir: opts.ReleaseRepoDir,
		ArgoCDServer:   opts.ArgoCDServer,
	})
	if err != nil {
		logrus.Fatal(err)
	}

	if err := v.Run(context.Background()); err != nil {
		logrus.Fatal(err)
	}
	logrus.Info("cluster manifest verification passed")
}
