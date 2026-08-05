package gitops

import (
	"testing"

	argov1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestApplicationSetsForChanges(t *testing.T) {
	testCases := []struct {
		name         string
		appsets      []argov1alpha1.ApplicationSet
		changedPaths []string
		want         []argov1alpha1.ApplicationSet
	}{
		{
			name: "replaces glob with changed app dir",
			appsets: []argov1alpha1.ApplicationSet{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "shared"},
					Spec: argov1alpha1.ApplicationSetSpec{
						Generators: []argov1alpha1.ApplicationSetGenerator{{
							Matrix: &argov1alpha1.MatrixGenerator{
								Generators: []argov1alpha1.ApplicationSetNestedGenerator{{
									Git: &argov1alpha1.GitGenerator{
										Directories: []argov1alpha1.GitDirectoryGeneratorItem{
											{Path: "clusters/build-clusters/build-shared/*"},
										},
									},
								}},
							},
						}},
					},
				},
			},
			changedPaths: []string{"clusters/build-clusters/build-shared/my-app/config.yaml"},
			want: []argov1alpha1.ApplicationSet{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "shared"},
					Spec: argov1alpha1.ApplicationSetSpec{
						Generators: []argov1alpha1.ApplicationSetGenerator{{
							Matrix: &argov1alpha1.MatrixGenerator{
								Generators: []argov1alpha1.ApplicationSetNestedGenerator{{
									Git: &argov1alpha1.GitGenerator{
										Directories: []argov1alpha1.GitDirectoryGeneratorItem{
											{Path: "clusters/build-clusters/build-shared/my-app"},
										},
									},
								}},
							},
						}},
					},
				},
			},
		},
		{
			name: "drops unrelated appset",
			appsets: []argov1alpha1.ApplicationSet{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "app-ci"},
					Spec: argov1alpha1.ApplicationSetSpec{
						Generators: []argov1alpha1.ApplicationSetGenerator{{
							Git: &argov1alpha1.GitGenerator{
								Directories: []argov1alpha1.GitDirectoryGeneratorItem{
									{Path: "clusters/app.ci/*"},
								},
							},
						}},
					},
				},
			},
			changedPaths: []string{"clusters/build-clusters/build-shared/my-app/config.yaml"},
			want:         nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := ApplicationSetsForChanges(tc.appsets, tc.changedPaths)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("ApplicationSetsForChanges() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
