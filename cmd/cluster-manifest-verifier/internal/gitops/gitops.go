package gitops

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	argov1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"sigs.k8s.io/yaml"
)

type Apps struct {
	ApplicationSets []argov1alpha1.ApplicationSet
	AppProjects     []argov1alpha1.AppProject
	Applications    []argov1alpha1.Application
}

func LoadApps(dir string) (*Apps, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	apps := &Apps{}
	var loadErr error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		ext := path.Ext(name)
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		if err := loadAppFile(filepath.Join(dir, name), apps); err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("%s: %w", name, err))
		}
	}
	if loadErr != nil {
		return nil, loadErr
	}
	return apps, nil
}

func loadAppFile(filePath string, apps *Apps) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	var header struct {
		Kind string `json:"kind"`
	}
	if err := yaml.Unmarshal(data, &header); err != nil {
		return fmt.Errorf("decode kind: %w", err)
	}

	switch header.Kind {
	case "ApplicationSet":
		var appset argov1alpha1.ApplicationSet
		if err := yaml.Unmarshal(data, &appset); err != nil {
			return fmt.Errorf("decode ApplicationSet: %w", err)
		}
		apps.ApplicationSets = append(apps.ApplicationSets, appset)
	case "AppProject":
		var project argov1alpha1.AppProject
		if err := yaml.Unmarshal(data, &project); err != nil {
			return fmt.Errorf("decode AppProject: %w", err)
		}
		apps.AppProjects = append(apps.AppProjects, project)
	case "Application":
		var app argov1alpha1.Application
		if err := yaml.Unmarshal(data, &app); err != nil {
			return fmt.Errorf("decode Application: %w", err)
		}
		apps.Applications = append(apps.Applications, app)
	default:
		return fmt.Errorf("unsupported kind %q", header.Kind)
	}
	return nil
}

func (g *Apps) AllApplications(generated []argov1alpha1.Application) map[string]argov1alpha1.Application {
	apps := make(map[string]argov1alpha1.Application, len(generated)+len(g.Applications))
	for _, app := range generated {
		if app.Name == "" {
			continue
		}
		apps[app.Name] = app
	}
	for _, app := range g.Applications {
		if app.Name == "" {
			continue
		}
		if _, exists := apps[app.Name]; exists {
			continue
		}
		apps[app.Name] = app
	}
	return apps
}

// ApplicationSetsForChanges copies appsets and sets each git directories path to the
// concrete app dirs under that glob that changedPaths touch. Unrelated appsets are dropped.
func ApplicationSetsForChanges(appsets []argov1alpha1.ApplicationSet, changedPaths []string) []argov1alpha1.ApplicationSet {
	var paths []string
	for _, p := range changedPaths {
		if rel, err := filepath.Rel("clusters/gitops/apps", path.Clean(p)); err == nil && (rel == "." || filepath.IsLocal(rel)) {
			continue
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return nil
	}

	var out []argov1alpha1.ApplicationSet
	for _, appset := range appsets {
		as := appset.DeepCopy()
		if !setDirectoriesToChangedApps(as, paths) {
			continue
		}
		out = append(out, *as)
	}
	return out
}

func setDirectoriesToChangedApps(as *argov1alpha1.ApplicationSet, paths []string) bool {
	var gits []*argov1alpha1.GitGenerator
	for _, g := range as.Spec.Generators {
		if g.Git != nil {
			gits = append(gits, g.Git)
		}
		if g.Matrix == nil {
			continue
		}
		for _, nested := range g.Matrix.Generators {
			if nested.Git != nil {
				gits = append(gits, nested.Git)
			}
		}
	}
	if len(gits) == 0 {
		return true
	}

	for _, git := range gits {
		if setGitDirectoryPaths(git, paths) {
			return true
		}
	}
	return false
}

// setGitDirectoryPaths replaces globs like "clusters/foo/*" with the changed app dirs under them.
func setGitDirectoryPaths(git *argov1alpha1.GitGenerator, paths []string) bool {
	var dirs []argov1alpha1.GitDirectoryGeneratorItem
	seen := map[string]struct{}{}
	for _, d := range git.Directories {
		pattern := path.Clean(d.Path)
		base := pattern
		glob := path.Base(pattern) == "*"
		if glob {
			base = path.Dir(pattern)
		}
		for _, p := range paths {
			rel, err := filepath.Rel(base, path.Clean(p))
			if err != nil || !filepath.IsLocal(rel) {
				continue
			}
			appDir := base
			if glob {
				appDir = path.Join(base, filepath.ToSlash(rel))
				for path.Dir(appDir) != base {
					appDir = path.Dir(appDir)
				}
			}
			if _, ok := seen[appDir]; ok {
				continue
			}
			seen[appDir] = struct{}{}
			dirs = append(dirs, argov1alpha1.GitDirectoryGeneratorItem{Path: appDir, Exclude: d.Exclude})
		}
	}
	if len(dirs) == 0 {
		return false
	}
	git.Directories = dirs
	return true
}
