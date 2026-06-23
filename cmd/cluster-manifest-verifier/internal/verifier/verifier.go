package verifier

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/sirupsen/logrus"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/prow/pkg/pod-utils/downwardapi"

	"github.com/openshift/ci-tools-standalone/cmd/cluster-manifest-verifier/internal/argocd"
	"github.com/openshift/ci-tools-standalone/cmd/cluster-manifest-verifier/internal/gitchanges"
	"github.com/openshift/ci-tools-standalone/cmd/cluster-manifest-verifier/internal/gitops"
	"github.com/openshift/ci-tools-standalone/cmd/cluster-manifest-verifier/internal/syncplanner"
)

type Config struct {
	ReleaseRepoDir string
	ArgoCDServer   string
}

type Verifier struct {
	appsetsDir  string
	repoChanges *gitchanges.RepoChanges
	planner     *syncplanner.Planner
	argo        *argocd.Client
}

func New(cfg Config) (*Verifier, error) {
	spec, err := downwardapi.ResolveSpecFromEnv()
	if err != nil {
		return nil, fmt.Errorf("resolve prow job spec: %w", err)
	}
	if spec.Refs == nil || len(spec.Refs.Pulls) == 0 {
		return nil, fmt.Errorf("prow job spec has no pull request refs")
	}
	pull := spec.Refs.Pulls[0]
	headRevision := pull.SHA
	baseRevision := spec.Refs.BaseSHA
	argocdRevision := fmt.Sprintf("refs/pull/%d/merge", pull.Number)

	argo, err := argocd.New(argocd.Config{Server: cfg.ArgoCDServer, Revision: argocdRevision})
	if err != nil {
		return nil, err
	}

	return &Verifier{
		appsetsDir: filepath.Join(cfg.ReleaseRepoDir, "clusters", "gitops", "apps"),
		repoChanges: &gitchanges.RepoChanges{
			RepoDir:      cfg.ReleaseRepoDir,
			BaseRevision: baseRevision,
			HeadRevision: headRevision,
		},
		planner: syncplanner.New(cfg.ReleaseRepoDir),
		argo:    argo,
	}, nil
}

func (v *Verifier) Run(ctx context.Context) error {
	defer func() {
		if err := v.argo.Close(); err != nil {
			logrus.WithError(err).Warn("close Argo CD client")
		}
	}()
	var errs []error

	apps, err := gitops.LoadApps(v.appsetsDir)
	if err != nil {
		return fmt.Errorf("load gitops apps: %w", err)
	}

	logrus.WithFields(logrus.Fields{
		"applicationSets":   len(apps.ApplicationSets),
		"appProjects":       len(apps.AppProjects),
		"applications":      len(apps.Applications),
		"gitRevision":       v.repoChanges.HeadRevision,
		"gitBaseRevision":   v.repoChanges.BaseRevision,
		"argocdGitRevision": v.argo.Revision(),
	}).Info("loaded gitops apps")

	generated, err := v.argo.GenerateApplications(ctx, apps.ApplicationSets)
	if err != nil {
		errs = append(errs, err)
	}

	changes, err := v.repoChanges.List(ctx)
	if err != nil {
		return fmt.Errorf("list PR changes: %w", err)
	}
	if len(changes) == 0 {
		logrus.Info("no changes under clusters/, skipping verification")
		return utilerrors.NewAggregate(errs)
	}

	plans, err := v.planner.Build(apps.AllApplications(generated), changes)
	if err != nil {
		errs = append(errs, err)
	}

	for name, plan := range plans {
		if err := v.argo.DryRunSync(ctx, plan); err != nil {
			errs = append(errs, fmt.Errorf("application %q: %w", name, err))
		}
	}

	return utilerrors.NewAggregate(errs)
}
