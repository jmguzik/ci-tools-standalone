package gitchanges

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParseDiffStatusLine(t *testing.T) {
	testCases := []struct {
		name    string
		line    string
		want    FileChange
		wantErr bool
	}{
		{
			name: "modified",
			line: "M\tclusters/build-clusters/foo/config.yaml",
			want: FileChange{Path: "clusters/build-clusters/foo/config.yaml", Status: 'M'},
		},
		{
			name: "added",
			line: "A\tclusters/build-clusters/foo/new.yaml",
			want: FileChange{Path: "clusters/build-clusters/foo/new.yaml", Status: 'A'},
		},
		{
			name: "deleted",
			line: "D\tclusters/build-clusters/foo/old.yaml",
			want: FileChange{Path: "clusters/build-clusters/foo/old.yaml", Status: 'D'},
		},
		{
			name:    "invalid",
			line:    "broken",
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseDiffStatusLine(tc.line)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDiffStatusLine() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("ParseDiffStatusLine() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
