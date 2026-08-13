package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/command"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	"github.com/sirupsen/logrus"

	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/github"
)

type dockerfileEntry struct {
	Path            string   `json:"path"`
	ContextDir      string   `json:"context_dir,omitempty"`
	GoBinaryTargets []string `json:"go_binary_targets,omitempty"`
}

var alwaysTriggerContextFiles = map[string]bool{
	"go.mod":   true,
	"go.sum":   true,
	"Makefile": true,
}

func evaluateDockerfileAnnotation(annotation string, changedFiles []string, pj *v1.ProwJob, ghc minimalGhClient) (bool, error) {
	var entries []dockerfileEntry
	if err := json.Unmarshal([]byte(annotation), &entries); err != nil {
		return true, fmt.Errorf("failed to parse pipeline_run_if_dockerfile_changed annotation: %w", err)
	}
	if len(entries) == 0 {
		return true, fmt.Errorf("pipeline_run_if_dockerfile_changed annotation must contain at least one Dockerfile")
	}
	return evaluateDockerfileChanges(entries, changedFiles, pj, ghc), nil
}

func evaluateDockerfileChanges(entries []dockerfileEntry, changedFiles []string, pj *v1.ProwJob, ghc minimalGhClient) bool {
	if len(entries) == 0 {
		return true
	}

	for _, entry := range entries {
		contextDir := normalizeBuildContextPath(entry.ContextDir)
		dockerfilePath := buildContextRepoPath(contextDir, entry.Path)
		if dockerfilePath == "" {
			return true
		}
		contextDockerignorePath := buildContextRepoPath(contextDir, ".dockerignore")
		var contextChangedFiles []string

		for _, f := range changedFiles {
			changedPath := normalizeBuildContextPath(f)
			if changedPath == dockerfilePath || changedPath == dockerfilePath+".dockerignore" || changedPath == contextDockerignorePath {
				return true
			}
			contextPath, inContext := buildContextRelativePath(contextDir, changedPath)
			if !inContext {
				continue
			}
			contextChangedFiles = append(contextChangedFiles, contextPath)
			if alwaysTriggerContextFiles[contextPath] {
				return true
			}
		}
		if len(contextChangedFiles) == 0 {
			continue
		}

		if len(entry.GoBinaryTargets) > 0 {
			// COPY-all with go_binary_targets is handled by CNTRLPLANE-3781.
			// Conservatively trigger the test.
			return true
		}

		dockerfileContent, err := fetchFile(ghc, pj, dockerfilePath)
		if err != nil {
			logrus.WithError(err).WithField("dockerfile", dockerfilePath).Warn("Failed to fetch Dockerfile, conservatively triggering test")
			return true
		}

		sourcePaths, broadCopy := parseDockerfileSources(dockerfileContent)
		if broadCopy {
			return true
		}

		dockerignoreContent, err := fetchDockerignore(ghc, pj, dockerfilePath, contextDir)
		if err != nil {
			logrus.WithError(err).WithField("dockerfile", dockerfilePath).Warn("Failed to fetch Docker ignore rules, conservatively triggering test")
			return true
		}
		pm, err := buildIgnoreMatcher(dockerignoreContent)
		if err != nil {
			logrus.WithError(err).WithField("dockerfile", dockerfilePath).Warn("Failed to parse Docker ignore rules, conservatively triggering test")
			return true
		}

		for _, contextPath := range contextChangedFiles {
			if pm != nil {
				excluded, err := pm.MatchesOrParentMatches(contextPath)
				if err != nil {
					logrus.WithError(err).WithField("dockerfile", dockerfilePath).Warn("Failed to match Docker ignore rules, conservatively triggering test")
					return true
				}
				if excluded {
					continue
				}
			}
			for _, src := range sourcePaths {
				if pathMatchesSource(contextPath, src) {
					return true
				}
			}
		}
	}
	return false
}

func fetchFile(ghc minimalGhClient, pj *v1.ProwJob, path string) ([]byte, error) {
	if pj.Spec.Refs == nil {
		return nil, nil
	}
	ref := pj.Spec.Refs.BaseSHA
	if ref == "" {
		ref = pj.Spec.Refs.BaseRef
	}
	return ghc.GetFile(pj.Spec.Refs.Org, pj.Spec.Refs.Repo, path, ref)
}

func fetchDockerignore(ghc minimalGhClient, pj *v1.ProwJob, dockerfilePath, contextDir string) ([]byte, error) {
	for _, ignorePath := range []string{dockerfilePath + ".dockerignore", buildContextRepoPath(contextDir, ".dockerignore")} {
		content, err := fetchFile(ghc, pj, ignorePath)
		if err == nil {
			return content, nil
		}
		if isFileNotFound(err) {
			continue
		}
		return nil, fmt.Errorf("failed to fetch %s: %w", ignorePath, err)
	}
	return nil, nil
}

func isFileNotFound(err error) bool {
	var fileNotFound *github.FileNotFound
	return errors.As(err, &fileNotFound) || github.IsNotFound(err)
}

// parseDockerfileSources uses the buildkit parser to extract build-context
// paths from COPY/ADD instructions and RUN bind mounts. It returns the list of
// source paths and whether an input could not be narrowed safely.
func parseDockerfileSources(content []byte) (sources []string, broadCopy bool) {
	if content == nil {
		return nil, true
	}

	result, err := parser.Parse(bytes.NewReader(content))
	if err != nil {
		logrus.WithError(err).Warn("Failed to parse Dockerfile, treating as broad copy")
		return nil, true
	}

	stages := make(map[string]int)
	currentStage := -1
	for _, child := range result.AST.Children {
		switch strings.ToLower(child.Value) {
		case command.From:
			currentStage++
			if alias := dockerfileStageAlias(child); alias != "" {
				stages[strings.ToLower(alias)] = currentStage
			}
		case command.Copy, command.Add:
			srcs, isBroad := extractCopySources(child, stages, currentStage)
			if isBroad {
				return nil, true
			}
			sources = append(sources, srcs...)
		case command.Run:
			srcs, isBroad := extractRunMountSources(child, stages, currentStage)
			if isBroad {
				return nil, true
			}
			sources = append(sources, srcs...)
		}
	}

	return sources, false
}

func dockerfileStageAlias(node *parser.Node) string {
	for arg := node.Next; arg != nil && arg.Next != nil; arg = arg.Next {
		if strings.EqualFold(arg.Value, "as") {
			return arg.Next.Value
		}
	}
	return ""
}

// extractCopySources extracts the source paths from a COPY or ADD AST node.
// Returns nil, true if the instruction copies from "." or an external context.
// Returns an empty slice for copies from an earlier Dockerfile stage.
func extractCopySources(node *parser.Node, stages map[string]int, currentStage int) ([]string, bool) {
	for _, flag := range node.Flags {
		if strings.HasPrefix(flag, "--from=") {
			from := strings.TrimPrefix(flag, "--from=")
			if isPreviousDockerfileStage(from, stages, currentStage) {
				return nil, false
			}
			// --from can also name an additional build context. Its local inputs
			// cannot be derived from the Dockerfile alone.
			return nil, true
		}
	}

	var args []string
	for n := node.Next; n != nil; n = n.Next {
		args = append(args, n.Value)
	}

	if len(args) < 2 {
		return nil, true
	}

	// Last arg is destination; everything else is source
	srcs := args[:len(args)-1]
	var result []string
	for _, src := range srcs {
		// Build arguments cannot be resolved without the build invocation, and
		// path.Match does not support Docker's globstar extension. Run the test
		// conservatively instead of risking a false negative.
		if strings.Contains(src, "$") || strings.Contains(src, "**") {
			return nil, true
		}
		cleaned := normalizeBuildContextPath(src)
		if cleaned == "" {
			return nil, true
		}
		result = append(result, cleaned)
	}
	return result, false
}

func extractRunMountSources(node *parser.Node, stages map[string]int, currentStage int) ([]string, bool) {
	var sources []string
	for _, flag := range node.Flags {
		if !strings.HasPrefix(flag, "--mount=") {
			continue
		}
		options := make(map[string]string)
		for _, option := range strings.Split(strings.TrimPrefix(flag, "--mount="), ",") {
			key, value, found := strings.Cut(option, "=")
			if found {
				options[strings.ToLower(key)] = value
			}
		}
		mountType := strings.ToLower(options["type"])
		if mountType != "" && mountType != "bind" {
			continue
		}
		if from := options["from"]; from != "" {
			if isPreviousDockerfileStage(from, stages, currentStage) {
				continue
			}
			return nil, true
		}
		source := options["source"]
		if source == "" {
			source = options["src"]
		}
		if source == "" || strings.Contains(source, "$") || strings.Contains(source, "**") {
			return nil, true
		}
		source = normalizeBuildContextPath(source)
		if source == "" {
			return nil, true
		}
		sources = append(sources, source)
	}
	return sources, false
}

func isPreviousDockerfileStage(from string, stages map[string]int, currentStage int) bool {
	if index, err := strconv.Atoi(from); err == nil {
		return index >= 0 && index < currentStage
	}
	index, ok := stages[strings.ToLower(from)]
	return ok && index < currentStage
}

// normalizeBuildContextPath applies BuildKit's treatment of COPY/ADD sources:
// sources are rooted in the build context, leading slashes and parent traversal
// are removed, and trailing slashes are insignificant.
func normalizeBuildContextPath(source string) string {
	return strings.TrimPrefix(path.Clean("/"+source), "/")
}

func buildContextRepoPath(contextDir, contextPath string) string {
	return normalizeBuildContextPath(path.Join(normalizeBuildContextPath(contextDir), normalizeBuildContextPath(contextPath)))
}

// buildContextRelativePath converts a repository-relative changed path to the
// path visible inside the Docker build context.
func buildContextRelativePath(contextDir, repoPath string) (string, bool) {
	repoPath = normalizeBuildContextPath(repoPath)
	if contextDir == "" {
		return repoPath, true
	}
	if repoPath == contextDir {
		return "", true
	}
	prefix := contextDir + "/"
	if !strings.HasPrefix(repoPath, prefix) {
		return "", false
	}
	return strings.TrimPrefix(repoPath, prefix), true
}

func buildIgnoreMatcher(dockerignoreContent []byte) (*patternmatcher.PatternMatcher, error) {
	if dockerignoreContent == nil {
		return nil, nil
	}
	patterns, err := ignorefile.ReadAll(bytes.NewReader(dockerignoreContent))
	if err != nil {
		return nil, fmt.Errorf("failed to read .dockerignore: %w", err)
	}
	pm, err := patternmatcher.New(patterns)
	if err != nil {
		return nil, fmt.Errorf("failed to compile .dockerignore patterns: %w", err)
	}
	return pm, nil
}

// pathMatchesSource checks whether a changed file falls within a COPY source path.
func pathMatchesSource(changedFile, source string) bool {
	changedFile = normalizeBuildContextPath(changedFile)
	source = normalizeBuildContextPath(source)
	if source == "" || strings.Contains(source, "**") {
		return true
	}

	if strings.ContainsAny(source, "*?[") {
		// A wildcard may select either the changed file or a parent directory
		// whose contents are copied recursively.
		for candidate := changedFile; candidate != "" && candidate != "."; candidate = path.Dir(candidate) {
			matched, err := path.Match(source, candidate)
			if err != nil {
				return true
			}
			if matched {
				return true
			}
			parent := path.Dir(candidate)
			if parent == candidate || parent == "." {
				break
			}
		}
		return false
	}

	if changedFile == source {
		return true
	}
	if strings.HasPrefix(changedFile, source+"/") {
		return true
	}
	return false
}
