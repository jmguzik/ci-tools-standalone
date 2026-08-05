package gitops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
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
