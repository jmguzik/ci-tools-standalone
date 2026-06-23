package gitchanges

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

const clustersPrefix = "clusters/"

type FileChange struct {
	Path   string
	Status byte
}

type RepoChanges struct {
	RepoDir      string
	BaseRevision string
	HeadRevision string
}

func (r *RepoChanges) List(ctx context.Context) ([]FileChange, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-status",
		fmt.Sprintf("%s..%s", r.BaseRevision, r.HeadRevision), "--", clustersPrefix)
	cmd.Dir = r.RepoDir
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("git diff %s..%s: %s", r.BaseRevision, r.HeadRevision, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("git diff %s..%s: %w", r.BaseRevision, r.HeadRevision, err)
	}

	var changes []FileChange
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		change, err := ParseDiffStatusLine(line)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func ParseDiffStatusLine(line string) (FileChange, error) {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return FileChange{}, fmt.Errorf("invalid git diff line %q", line)
	}
	status := parts[0]
	path := parts[len(parts)-1]
	if len(status) == 0 {
		return FileChange{}, fmt.Errorf("invalid git diff status %q", line)
	}
	return FileChange{Path: filepath.ToSlash(path), Status: status[0]}, nil
}
