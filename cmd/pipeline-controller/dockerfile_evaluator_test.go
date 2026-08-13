package main

import (
	"context"
	"errors"
	"testing"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"

	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/github"
)

type fakeGhClient struct {
	files          map[string][]byte
	fileErrors     map[string]error
	fileRequests   map[string]int
	refs           []string
	changes        []github.PullRequestChange
	changeRequests int
	statuses       []github.Status
	statusRefs     []string
}

func (f *fakeGhClient) GetPullRequest(org, repo string, number int) (*github.PullRequest, error) {
	return nil, nil
}
func (f *fakeGhClient) CreateComment(org, repo string, number int, comment string) error {
	return nil
}
func (f *fakeGhClient) GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error) {
	f.changeRequests++
	return f.changes, nil
}
func (f *fakeGhClient) CreateStatus(org, repo, ref string, s github.Status) error {
	f.statusRefs = append(f.statusRefs, ref)
	f.statuses = append(f.statuses, s)
	return nil
}
func (f *fakeGhClient) AddLabel(org, repo string, number int, label string) error { return nil }
func (f *fakeGhClient) GetIssueLabels(org, repo string, number int) ([]github.Label, error) {
	return nil, nil
}
func (f *fakeGhClient) GetFile(org, repo, filepath, commit string) ([]byte, error) {
	if f.fileRequests == nil {
		f.fileRequests = make(map[string]int)
	}
	f.fileRequests[filepath]++
	f.refs = append(f.refs, commit)
	if err, ok := f.fileErrors[filepath]; ok {
		return nil, err
	}
	if content, ok := f.files[filepath]; ok {
		return content, nil
	}
	return nil, github.NewNotFound()
}

func testProwJob() *v1.ProwJob {
	return &v1.ProwJob{
		Spec: v1.ProwJobSpec{
			Refs: &v1.Refs{
				Org:     "openshift",
				Repo:    "hypershift",
				BaseRef: "main",
				BaseSHA: "base-sha",
				Pulls:   []v1.Pull{{Number: 42}},
			},
		},
	}
}

func TestParseDockerfileSources(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantSrcs  []string
		wantBroad bool
	}{
		{
			name:      "selective COPY",
			content:   "FROM golang:1.21\nCOPY go.mod go.sum ./\nCOPY cmd/ cmd/\nCOPY pkg/ pkg/\n",
			wantSrcs:  []string{"go.mod", "go.sum", "cmd", "pkg"},
			wantBroad: false,
		},
		{
			name:      "normalized COPY sources",
			content:   "FROM scratch\nCOPY ./cmd/ /cmd/\nCOPY /pkg/ /pkg/\nCOPY ../../internal/ /internal/\n",
			wantSrcs:  []string{"cmd", "pkg", "internal"},
			wantBroad: false,
		},
		{
			name:      "wildcard COPY source",
			content:   "FROM scratch\nCOPY config/*.json /app/\n",
			wantSrcs:  []string{"config/*.json"},
			wantBroad: false,
		},
		{
			name:      "variable COPY source is conservative",
			content:   "FROM scratch\nARG SOURCE\nCOPY ${SOURCE}/ /app/\n",
			wantSrcs:  nil,
			wantBroad: true,
		},
		{
			name:      "globstar COPY source is conservative",
			content:   "FROM scratch\nCOPY src/**/*.go /app/\n",
			wantSrcs:  nil,
			wantBroad: true,
		},
		{
			name:      "broad COPY dot",
			content:   "FROM golang:1.21\nCOPY . .\nRUN go build\n",
			wantSrcs:  nil,
			wantBroad: true,
		},
		{
			name:      "broad COPY dot slash",
			content:   "FROM golang:1.21\nCOPY . /app\n",
			wantSrcs:  nil,
			wantBroad: true,
		},
		{
			name:      "COPY --from skipped",
			content:   "FROM golang:1.21 AS builder\nCOPY cmd/ cmd/\nFROM scratch\nCOPY --from=builder /app /app\n",
			wantSrcs:  []string{"cmd"},
			wantBroad: false,
		},
		{
			name:      "multi-stage selective",
			content:   "FROM golang:1.21 AS builder\nCOPY go.mod .\nCOPY pkg/ pkg/\nFROM scratch\nCOPY --from=builder /bin/app /app\n",
			wantSrcs:  []string{"go.mod", "pkg"},
			wantBroad: false,
		},
		{
			name:      "ADD instruction",
			content:   "FROM ubuntu\nADD scripts/ /scripts/\n",
			wantSrcs:  []string{"scripts"},
			wantBroad: false,
		},
		{
			name:      "nil content",
			content:   "",
			wantSrcs:  nil,
			wantBroad: true,
		},
		{
			name:      "COPY with --link flag",
			content:   "FROM golang:1.21\nCOPY --link cmd/ cmd/\n",
			wantSrcs:  []string{"cmd"},
			wantBroad: false,
		},
		{
			name:      "COPY from named context is conservative",
			content:   "FROM scratch\nCOPY --from=builder /bin/app /app\n",
			wantSrcs:  nil,
			wantBroad: true,
		},
		{
			name:      "COPY --from=0 numeric stage",
			content:   "FROM golang:1.21\nCOPY cmd/ cmd/\nFROM scratch\nCOPY --from=0 /bin/app /app\n",
			wantSrcs:  []string{"cmd"},
			wantBroad: false,
		},
		{
			name:      "mixed selective and --from",
			content:   "FROM golang:1.21 AS build\nCOPY go.mod go.sum ./\nRUN go mod download\nCOPY cmd/ cmd/\nCOPY pkg/ pkg/\nRUN go build -o /app ./cmd/server\nFROM registry.access.redhat.com/ubi9/ubi-minimal:latest\nCOPY --from=build /app /usr/local/bin/app\n",
			wantSrcs:  []string{"go.mod", "go.sum", "cmd", "pkg"},
			wantBroad: false,
		},
		{
			name:      "broad COPY in builder stage propagates despite --from in final stage",
			content:   "FROM golang:1.21 AS build\nCOPY . .\nRUN go build -o /app\nFROM scratch\nCOPY --from=build /app /app\n",
			wantSrcs:  nil,
			wantBroad: true,
		},
		{
			name:      "RUN bind mount source",
			content:   "FROM golang:1.21\nRUN --mount=type=bind,source=tools,target=/tools /tools/build.sh\n",
			wantSrcs:  []string{"tools"},
			wantBroad: false,
		},
		{
			name:      "RUN bind mount defaults to whole context",
			content:   "FROM golang:1.21\nRUN --mount=type=bind,target=/src make -C /src\n",
			wantSrcs:  nil,
			wantBroad: true,
		},
		{
			name:      "RUN bind mount from previous stage is skipped",
			content:   "FROM golang:1.21 AS build\nCOPY cmd/ cmd/\nFROM scratch\nRUN --mount=type=bind,from=build,source=/out,target=/out cp /out/app /app\n",
			wantSrcs:  []string{"cmd"},
			wantBroad: false,
		},
		{
			name:      "RUN bind mount from named context is conservative",
			content:   "FROM scratch\nRUN --mount=type=bind,from=source,source=/,target=/src cp /src/app /app\n",
			wantSrcs:  nil,
			wantBroad: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var content []byte
			if tt.content != "" {
				content = []byte(tt.content)
			}
			srcs, broad := parseDockerfileSources(content)
			if broad != tt.wantBroad {
				t.Errorf("broadCopy = %v, want %v", broad, tt.wantBroad)
			}
			if !tt.wantBroad {
				if len(srcs) != len(tt.wantSrcs) {
					t.Errorf("got %d sources %v, want %d sources %v", len(srcs), srcs, len(tt.wantSrcs), tt.wantSrcs)
					return
				}
				for i, src := range srcs {
					if src != tt.wantSrcs[i] {
						t.Errorf("source[%d] = %q, want %q", i, src, tt.wantSrcs[i])
					}
				}
			}
		})
	}
}

func TestPathMatchesSource(t *testing.T) {
	tests := []struct {
		changed string
		source  string
		want    bool
	}{
		{"cmd/main.go", "cmd", true},
		{"cmd/sub/main.go", "cmd", true},
		{"pkg/api/types.go", "pkg", true},
		{"README.md", "cmd", false},
		{"go.mod", "go.mod", true},
		{"cmd", "cmd", true},
		{"command/foo.go", "cmd", false},
		{"cmd/main.go", "./cmd/", true},
		{"/cmd/main.go", "/cmd", true},
		{"home.txt", "hom*", true},
		{"foobar/nested.txt", "foo*", true},
		{"config/app.json", "config/*.json", true},
		{"config/nested/app.json", "config/*.json", false},
		{"docs/readme.md", "*.json", false},
		{"anything", "src/**", true},
	}

	for _, tt := range tests {
		t.Run(tt.changed+"_vs_"+tt.source, func(t *testing.T) {
			got := pathMatchesSource(tt.changed, tt.source)
			if got != tt.want {
				t.Errorf("pathMatchesSource(%q, %q) = %v, want %v", tt.changed, tt.source, got, tt.want)
			}
		})
	}
}

func TestEvaluateDockerfileChanges(t *testing.T) {
	selectiveDockerfile := []byte("FROM golang:1.21\nCOPY cmd/ cmd/\nCOPY pkg/ pkg/\n")
	dockerignore := []byte("bin/\nhack/tools/\n")

	tests := []struct {
		name         string
		entries      []dockerfileEntry
		changedFiles []string
		files        map[string][]byte
		fileErrors   map[string]error
		want         bool
	}{
		{
			name:         "always trigger on go.mod change",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{"go.mod"},
			files:        map[string][]byte{"Dockerfile": selectiveDockerfile},
			want:         true,
		},
		{
			name:         "always trigger on Dockerfile change",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{"Dockerfile"},
			files:        map[string][]byte{"Dockerfile": selectiveDockerfile},
			want:         true,
		},
		{
			name:         "trigger on matching COPY source",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{"cmd/main.go"},
			files:        map[string][]byte{"Dockerfile": selectiveDockerfile},
			want:         true,
		},
		{
			name:         "skip on unrelated file",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{"docs/README.md"},
			files:        map[string][]byte{"Dockerfile": selectiveDockerfile},
			want:         false,
		},
		{
			name:         "skip on dockerignored file even if in COPY path",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{"bin/hypershift"},
			files: map[string][]byte{
				"Dockerfile":    []byte("FROM golang:1.21\nCOPY bin/ bin/\n"),
				".dockerignore": dockerignore,
			},
			want: false,
		},
		{
			name:         "Dockerfile-specific ignore takes precedence over root",
			entries:      []dockerfileEntry{{Path: "docker/build.Dockerfile"}},
			changedFiles: []string{"src/main.go"},
			files: map[string][]byte{
				"docker/build.Dockerfile":              []byte("FROM scratch\nCOPY src/ src/\n"),
				"docker/build.Dockerfile.dockerignore": []byte{},
				".dockerignore":                        []byte("src/\n"),
			},
			want: true,
		},
		{
			name:         "Dockerfile-specific ignore excludes copied source",
			entries:      []dockerfileEntry{{Path: "docker/build.Dockerfile"}},
			changedFiles: []string{"src/main.go"},
			files: map[string][]byte{
				"docker/build.Dockerfile":              []byte("FROM scratch\nCOPY src/ src/\n"),
				"docker/build.Dockerfile.dockerignore": []byte("src/\n"),
			},
			want: false,
		},
		{
			name:         "trigger on Dockerfile-specific ignore change",
			entries:      []dockerfileEntry{{Path: "docker/build.Dockerfile"}},
			changedFiles: []string{"docker/build.Dockerfile.dockerignore"},
			files: map[string][]byte{
				"docker/build.Dockerfile": []byte("FROM scratch\nCOPY src/ src/\n"),
			},
			want: true,
		},
		{
			name:         "conservatively trigger when ignore lookup fails",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{"docs/README.md"},
			files: map[string][]byte{
				"Dockerfile": selectiveDockerfile,
			},
			fileErrors: map[string]error{
				"Dockerfile.dockerignore": errors.New("GitHub unavailable"),
			},
			want: true,
		},
		{
			name:         "conservatively trigger on malformed ignore pattern",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{"docs/README.md"},
			files: map[string][]byte{
				"Dockerfile":    selectiveDockerfile,
				".dockerignore": []byte("[\n"),
			},
			want: true,
		},
		{
			name:         "trigger on .dockerignore change",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{".dockerignore"},
			files:        map[string][]byte{"Dockerfile": selectiveDockerfile},
			want:         true,
		},
		{
			name:         "trigger on normalized COPY source",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{"cmd/main.go"},
			files:        map[string][]byte{"Dockerfile": []byte("FROM scratch\nCOPY ./cmd/ /cmd/\n")},
			want:         true,
		},
		{
			name:         "normalize configured Dockerfile path",
			entries:      []dockerfileEntry{{Path: "./docker/build.Dockerfile"}},
			changedFiles: []string{"src/main.go"},
			files:        map[string][]byte{"docker/build.Dockerfile": []byte("FROM scratch\nCOPY src/ src/\n")},
			want:         true,
		},
		{
			name:         "trigger on wildcard COPY source",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{"config/app.json"},
			files:        map[string][]byte{"Dockerfile": []byte("FROM scratch\nCOPY config/*.json /app/\n")},
			want:         true,
		},
		{
			name:         "conservatively trigger on go_binary_targets",
			entries:      []dockerfileEntry{{Path: "Dockerfile", GoBinaryTargets: []string{"./cmd/server"}}},
			changedFiles: []string{"docs/README.md"},
			files:        map[string][]byte{"Dockerfile": selectiveDockerfile},
			want:         true,
		},
		{
			name:         "conservatively trigger on broad COPY",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{"docs/README.md"},
			files:        map[string][]byte{"Dockerfile": []byte("FROM golang:1.21\nCOPY . .\n")},
			want:         true,
		},
		{
			name:         "conservatively trigger on fetch error",
			entries:      []dockerfileEntry{{Path: "Dockerfile.missing"}},
			changedFiles: []string{"docs/README.md"},
			files:        map[string][]byte{},
			want:         true,
		},
		{
			name: "multiple entries, second matches",
			entries: []dockerfileEntry{
				{Path: "Dockerfile"},
				{Path: "Dockerfile.control-plane"},
			},
			changedFiles: []string{"control-plane/main.go"},
			files: map[string][]byte{
				"Dockerfile":               []byte("FROM golang:1.21\nCOPY cmd/ cmd/\n"),
				"Dockerfile.control-plane": []byte("FROM golang:1.21\nCOPY control-plane/ control-plane/\n"),
			},
			want: true,
		},
		{
			name: "multiple entries, none match",
			entries: []dockerfileEntry{
				{Path: "Dockerfile"},
				{Path: "Dockerfile.control-plane"},
			},
			changedFiles: []string{"docs/README.md"},
			files: map[string][]byte{
				"Dockerfile":               []byte("FROM golang:1.21\nCOPY cmd/ cmd/\n"),
				"Dockerfile.control-plane": []byte("FROM golang:1.21\nCOPY control-plane/ control-plane/\n"),
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ghc := &fakeGhClient{files: tt.files, fileErrors: tt.fileErrors}
			got := evaluateDockerfileChanges(tt.entries, tt.changedFiles, testProwJob(), ghc)
			if got != tt.want {
				t.Errorf("evaluateDockerfileChanges() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvaluatePipelineRunCondition(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		changed     []string
		files       map[string][]byte
		want        bool
		wantErr     bool
	}{
		{
			name: "Dockerfile input matches",
			annotations: map[string]string{
				"pipeline_run_if_dockerfile_changed": `[{"path":"Dockerfile"}]`,
			},
			changed: []string{"cmd/main.go"},
			files:   map[string][]byte{"Dockerfile": []byte("FROM scratch\nCOPY cmd/ cmd/\n")},
			want:    true,
		},
		{
			name: "Dockerfile input does not match",
			annotations: map[string]string{
				"pipeline_run_if_dockerfile_changed": `[{"path":"Dockerfile"}]`,
			},
			changed: []string{"docs/README.md"},
			files:   map[string][]byte{"Dockerfile": []byte("FROM scratch\nCOPY cmd/ cmd/\n")},
			want:    false,
		},
		{
			name: "malformed Dockerfile annotation blocks conservatively",
			annotations: map[string]string{
				"pipeline_run_if_dockerfile_changed": `[{`,
			},
			changed: []string{"docs/README.md"},
			want:    true,
			wantErr: true,
		},
		{
			name: "empty Dockerfile annotation blocks conservatively",
			annotations: map[string]string{
				"pipeline_run_if_dockerfile_changed": `[]`,
			},
			changed: []string{"docs/README.md"},
			want:    true,
			wantErr: true,
		},
		{
			name: "run-if-changed takes precedence",
			annotations: map[string]string{
				"pipeline_run_if_changed":            `^docs/`,
				"pipeline_run_if_dockerfile_changed": `[{"path":"Dockerfile"}]`,
			},
			changed: []string{"cmd/main.go"},
			files:   map[string][]byte{"Dockerfile": []byte("FROM scratch\nCOPY cmd/ cmd/\n")},
			want:    false,
		},
		{
			name:        "no pipeline condition",
			annotations: map[string]string{},
			changed:     []string{"cmd/main.go"},
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ghc := &fakeGhClient{files: tt.files}
			got, err := evaluatePipelineRunCondition(tt.annotations, tt.changed, testProwJob(), ghc)
			if (err != nil) != tt.wantErr {
				t.Fatalf("evaluatePipelineRunCondition() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("evaluatePipelineRunCondition() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAcquireConditionalContextsSchedulesMalformedDockerfileAnnotation(t *testing.T) {
	presubmit := config.Presubmit{
		JobBase: config.JobBase{
			Name: "pull-ci-openshift-hypershift-main-e2e",
			Annotations: map[string]string{
				"pipeline_run_if_dockerfile_changed": `[{`,
			},
		},
		RerunCommand: "/test e2e",
	}
	commands, manualMessage, err := acquireConditionalContexts(
		context.Background(),
		testProwJob(),
		[]config.Presubmit{presubmit},
		&fakeGhClient{},
		func() {},
		nil,
		false,
	)
	if err != nil {
		t.Fatalf("acquireConditionalContexts() error = %v", err)
	}
	if commands != "\n/test e2e" {
		t.Errorf("commands = %q, want %q", commands, "\n/test e2e")
	}
	if manualMessage != "" {
		t.Errorf("manual message = %q, want empty", manualMessage)
	}
}

func TestFetchFileUsesImmutableBaseSHA(t *testing.T) {
	ghc := &fakeGhClient{files: map[string][]byte{"Dockerfile": []byte("FROM scratch\n")}}
	if _, err := fetchFile(ghc, testProwJob(), "Dockerfile"); err != nil {
		t.Fatalf("fetchFile() error = %v", err)
	}
	if len(ghc.refs) != 1 || ghc.refs[0] != "base-sha" {
		t.Errorf("GetFile refs = %v, want [base-sha]", ghc.refs)
	}
}

func TestNewPullRequestProwJobPreservesRefs(t *testing.T) {
	pj := newPullRequestProwJob("openshift", "hypershift", "release-4.20", "base-sha", 42, "head-sha")
	refs := pj.Spec.Refs
	if refs == nil {
		t.Fatal("newPullRequestProwJob() returned nil refs")
	}
	if refs.Org != "openshift" || refs.Repo != "hypershift" || refs.BaseRef != "release-4.20" || refs.BaseSHA != "base-sha" {
		t.Errorf("refs = %#v, want complete immutable base refs", refs)
	}
	if len(refs.Pulls) != 1 || refs.Pulls[0].Number != 42 || refs.Pulls[0].SHA != "head-sha" {
		t.Errorf("pulls = %#v, want PR 42 at head-sha", refs.Pulls)
	}
}

func TestChangedFilePathsIncludesRenameSource(t *testing.T) {
	changes := []github.PullRequestChange{
		{Filename: "cmd/new.go", Status: github.PullRequestFileRenamed, PreviousFilename: "pkg/old.go"},
		{Filename: "README.md", Status: string(github.PullRequestFileModified)},
	}
	want := []string{"cmd/new.go", "pkg/old.go", "README.md"}
	got := changedFilePaths(changes)
	if len(got) != len(want) {
		t.Fatalf("changedFilePaths() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changedFilePaths()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDeferredChangedFilesProviderIncludesRenameSourceAndCaches(t *testing.T) {
	ghc := &fakeGhClient{changes: []github.PullRequestChange{{
		Filename:         "docs/new.go",
		PreviousFilename: "pkg/old.go",
		Status:           github.PullRequestFileRenamed,
	}}}
	provider := newDeferredChangedFilesProvider(ghc, "openshift", "hypershift", 42)

	for i := 0; i < 2; i++ {
		got, err := provider()
		if err != nil {
			t.Fatalf("provider() error = %v", err)
		}
		want := []string{"docs/new.go", "pkg/old.go"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("provider() = %v, want %v", got, want)
		}
	}
	if ghc.changeRequests != 1 {
		t.Errorf("GetPullRequestChanges calls = %d, want 1", ghc.changeRequests)
	}
}

func TestExistingPipelineConditionsScheduleRenameSource(t *testing.T) {
	changes := []github.PullRequestChange{{
		Filename:         "docs/new.go",
		PreviousFilename: "pkg/old.go",
		Status:           github.PullRequestFileRenamed,
	}}
	for _, tc := range []struct {
		name       string
		annotation string
		pattern    string
	}{
		{name: "run if changed", annotation: "pipeline_run_if_changed", pattern: `^pkg/`},
		{name: "skip if only changed", annotation: "pipeline_skip_if_only_changed", pattern: `^docs/`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			presubmit := config.Presubmit{
				JobBase: config.JobBase{
					Name:        "pull-ci-openshift-hypershift-main-e2e",
					Annotations: map[string]string{tc.annotation: tc.pattern},
				},
				Trigger:      `^/test e2e$`,
				RerunCommand: "/test e2e",
			}
			paths := changedFilePaths(changes)
			if tc.annotation == "pipeline_run_if_changed" {
				shouldRun, err := matchesPattern(tc.pattern, paths)
				if err != nil || !shouldRun {
					t.Fatalf("context decision = %v, %v; want run", shouldRun, err)
				}
			} else {
				shouldSkip, err := allFilesMatchPattern(tc.pattern, paths)
				if err != nil || shouldSkip {
					t.Fatalf("context decision = skip %v, %v; want run", shouldSkip, err)
				}
			}

			ghc := &fakeGhClient{changes: changes}
			commands, _, err := acquireConditionalContexts(context.Background(), testProwJob(), []config.Presubmit{presubmit}, ghc, func() {}, nil, false)
			if err != nil {
				t.Fatalf("acquireConditionalContexts() error = %v", err)
			}
			if commands != "\n/test e2e" {
				t.Errorf("commands = %q, want %q", commands, "\n/test e2e")
			}
		})
	}
}

func TestAcquireConditionalContextsCachesDockerfileReads(t *testing.T) {
	ghc := &fakeGhClient{
		files:   map[string][]byte{"Dockerfile": []byte("FROM scratch\nCOPY cmd/ cmd/\n")},
		changes: []github.PullRequestChange{{Filename: "docs/README.md", Status: string(github.PullRequestFileModified)}},
	}
	presubmits := []config.Presubmit{
		{
			JobBase:      config.JobBase{Name: "pull-ci-openshift-hypershift-main-first", Annotations: map[string]string{"pipeline_run_if_dockerfile_changed": `[{"path":"Dockerfile"}]`}},
			RerunCommand: "/test first",
		},
		{
			JobBase:      config.JobBase{Name: "pull-ci-openshift-hypershift-main-second", Annotations: map[string]string{"pipeline_run_if_dockerfile_changed": `[{"path":"Dockerfile"}]`}},
			RerunCommand: "/test second",
		},
	}

	commands, _, err := acquireConditionalContexts(context.Background(), testProwJob(), presubmits, ghc, func() {}, nil, false)
	if err != nil {
		t.Fatalf("acquireConditionalContexts() error = %v", err)
	}
	if commands != "" {
		t.Errorf("commands = %q, want no commands", commands)
	}
	for _, filepath := range []string{"Dockerfile", "Dockerfile.dockerignore", ".dockerignore"} {
		if ghc.fileRequests[filepath] != 1 {
			t.Errorf("GetFile(%q) calls = %d, want 1", filepath, ghc.fileRequests[filepath])
		}
	}
	if ghc.changeRequests != 1 {
		t.Errorf("GetPullRequestChanges calls = %d, want 1", ghc.changeRequests)
	}
}

func TestDockerfileConditionContextAndSchedulingAgreeOnRename(t *testing.T) {
	var enabled enabledConfig
	if err := yaml.Unmarshal([]byte(`
orgs:
- org: openshift
  repos:
  - name: hypershift
    branches:
    - main
`), &enabled); err != nil {
		t.Fatalf("failed to create enabled config: %v", err)
	}

	presubmit := config.Presubmit{
		JobBase: config.JobBase{
			Name: "pull-ci-openshift-hypershift-main-e2e",
			Annotations: map[string]string{
				"pipeline_run_if_dockerfile_changed": `[{"path":"Dockerfile"}]`,
			},
		},
		RerunCommand: "/test e2e",
		Reporter:     config.Reporter{Context: "ci/prow/e2e"},
	}
	ghc := &fakeGhClient{
		files: map[string][]byte{"Dockerfile": []byte("FROM scratch\nCOPY pkg/ pkg/\n")},
		changes: []github.PullRequestChange{{
			Filename:         "docs/old.go",
			PreviousFilename: "pkg/old.go",
			Status:           github.PullRequestFileRenamed,
		}},
	}
	cw := &clientWrapper{
		ghc: ghc,
		configDataProvider: &ConfigDataProvider{updatedPresubmits: map[string]presubmitTests{
			"openshift/hypershift": {pipelineConditionallyRequired: []config.Presubmit{presubmit}},
		}},
		watcher:     &watcher{config: enabled},
		lgtmWatcher: &watcher{},
	}
	event := github.PullRequestEvent{
		Action: github.PullRequestActionOpened,
		Repo:   github.Repo{Owner: github.User{Login: "openshift"}, Name: "hypershift"},
		PullRequest: github.PullRequest{
			Number: 42,
			Base:   github.PullRequestBranch{Ref: "main", SHA: "base-sha"},
			Head:   github.PullRequestBranch{SHA: "head-sha"},
		},
	}

	cw.handlePipelineContextCreation(logrus.NewEntry(logrus.New()), event)
	if len(ghc.statuses) != 1 || ghc.statuses[0].Context != "ci/prow/e2e" || ghc.statuses[0].State != "pending" {
		t.Fatalf("webhook statuses = %#v, want one pending e2e status", ghc.statuses)
	}

	commands, _, err := acquireConditionalContexts(context.Background(), testProwJob(), []config.Presubmit{presubmit}, ghc, func() {}, nil, false)
	if err != nil {
		t.Fatalf("acquireConditionalContexts() error = %v", err)
	}
	if commands != "\n/test e2e" {
		t.Errorf("commands = %q, want %q", commands, "\n/test e2e")
	}
}

func TestHandlePipelineContextCreationDockerfileCondition(t *testing.T) {
	var enabled enabledConfig
	if err := yaml.Unmarshal([]byte(`
orgs:
- org: openshift
  repos:
  - name: hypershift
    branches:
    - main
`), &enabled); err != nil {
		t.Fatalf("failed to create enabled config: %v", err)
	}

	ghc := &fakeGhClient{
		files: map[string][]byte{
			"Dockerfile": []byte("FROM scratch\nCOPY cmd/ cmd/\n"),
		},
		changes: []github.PullRequestChange{{Filename: "cmd/main.go", Status: string(github.PullRequestFileModified)}},
	}
	provider := &ConfigDataProvider{updatedPresubmits: map[string]presubmitTests{
		"openshift/hypershift": {
			pipelineConditionallyRequired: []config.Presubmit{{
				JobBase: config.JobBase{
					Name: "pull-ci-openshift-hypershift-main-e2e",
					Annotations: map[string]string{
						"pipeline_run_if_dockerfile_changed": `[{"path":"Dockerfile"}]`,
					},
				},
				Reporter: config.Reporter{Context: "ci/prow/e2e"},
			}},
		},
	}}
	cw := &clientWrapper{
		ghc:                ghc,
		configDataProvider: provider,
		watcher:            &watcher{config: enabled},
		lgtmWatcher:        &watcher{},
	}
	event := github.PullRequestEvent{
		Action: github.PullRequestActionOpened,
		Repo:   github.Repo{Owner: github.User{Login: "openshift"}, Name: "hypershift"},
		PullRequest: github.PullRequest{
			Number: 42,
			Base:   github.PullRequestBranch{Ref: "main", SHA: "base-sha"},
			Head:   github.PullRequestBranch{SHA: "head-sha"},
		},
	}

	cw.handlePipelineContextCreation(logrus.NewEntry(logrus.New()), event)

	if len(ghc.statuses) != 1 {
		t.Fatalf("created statuses = %v, want exactly one", ghc.statuses)
	}
	if ghc.statusRefs[0] != "head-sha" {
		t.Errorf("status ref = %q, want head-sha", ghc.statusRefs[0])
	}
	if got := ghc.statuses[0]; got.Context != "ci/prow/e2e" || got.State != "pending" || got.Description != PipelinePendingMessage {
		t.Errorf("status = %#v, want pending ci/prow/e2e pipeline status", got)
	}
	for _, ref := range ghc.refs {
		if ref != "base-sha" {
			t.Errorf("GetFile ref = %q, want immutable base-sha", ref)
		}
	}
}
