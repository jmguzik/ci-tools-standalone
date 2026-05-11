/*
Copyright 2017 The Kubernetes Authors.

This file was extracted from kubernetes/test-infra (robots/commenter) and
adjusted for github.com/openshift/ci-tools-standalone (auth via Prow GitHub
options; no legacy --token flag).

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Commenter searches for issues (--query) and appends a comment (--comment).
// Authenticate with a PAT (--github-token-path) or a GitHub App
// (--github-app-id, --github-app-private-key-path, and --github-org).
// Without --confirm, runs in dry-run mode.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"sigs.k8s.io/prow/pkg/flagutil"
	"sigs.k8s.io/prow/pkg/github"
)

const (
	templateHelp = `--comment is a golang text/template if set.
	Valid placeholders:
		.Org - github org
		.Repo - github repo
		.Number - issue number
	Advanced (see kubernetes/test-infra/prow/github/types.go):
		.Issue.User.Login - github account
		.Issue.Title
		.Issue.State
		.Issue.HTMLURL
		.Issue.Assignees - list of assigned .Users
		.Issue.Labels - list of applied labels (.Name)
`
)

func flagOptions() options {
	o := options{}
	flag.StringVar(&o.query, "query", "", "See https://help.github.com/articles/searching-issues-and-pull-requests/")
	flag.DurationVar(&o.updated, "updated", 2*time.Hour, "Filter to issues unmodified for at least this long if set")
	flag.BoolVar(&o.includeArchived, "include-archived", false, "Match archived issues if set")
	flag.BoolVar(&o.includeClosed, "include-closed", false, "Match closed issues if set")
	flag.BoolVar(&o.includeLocked, "include-locked", false, "Match locked issues if set")
	flag.BoolVar(&o.confirm, "confirm", false, "Mutate github if set")
	flag.StringVar(&o.comment, "comment", "", "Append the following comment to matching issues")
	flag.BoolVar(&o.useTemplate, "template", false, templateHelp)
	flag.IntVar(&o.ceiling, "ceiling", 3, "Maximum number of issues to modify, 0 for infinite")
	flag.BoolVar(&o.random, "random", false, "Choose random issues to comment on from the query")
	flag.StringVar(&o.githubOrg, "github-org", "openshift", "GitHub org for App auth installation resolution")
	o.github.AddFlags(flag.CommandLine)
	flag.Parse()
	return o
}

type meta struct {
	Number int
	Org    string
	Repo   string
	Issue  github.Issue
}

type options struct {
	ceiling         int
	comment         string
	includeArchived bool
	includeClosed   bool
	includeLocked   bool
	useTemplate     bool
	query           string
	updated         time.Duration
	confirm         bool
	random          bool
	githubOrg       string
	github          flagutil.GitHubOptions
}

func parseHTMLURL(url string) (string, string, int, error) {
	// Example: https://github.com/batterseapower/pinyin-toolkit/issues/132
	re := regexp.MustCompile(`.+/(.+)/(.+)/(issues|pull)/(\d+)$`)
	mat := re.FindStringSubmatch(url)
	if mat == nil {
		return "", "", 0, fmt.Errorf("failed to parse: %s", url)
	}
	n, err := strconv.Atoi(mat[4])
	if err != nil {
		return "", "", 0, err
	}
	return mat[1], mat[2], n, nil
}

func makeQuery(query string, includeArchived, includeClosed, includeLocked bool, minUpdated time.Duration) (string, error) {
	// GitHub used to allow \n but changed it at some point to result in no results at all
	query = strings.ReplaceAll(query, "\n", " ")
	parts := []string{query}
	if !includeArchived {
		if strings.Contains(query, "archived:true") {
			return "", errors.New("archived:true requires --include-archived")
		}
		parts = append(parts, "archived:false")
	} else if strings.Contains(query, "archived:false") {
		return "", errors.New("archived:false conflicts with --include-archived")
	}
	if !includeClosed {
		if strings.Contains(query, "is:closed") {
			return "", errors.New("is:closed requires --include-closed")
		}
		parts = append(parts, "is:open")
	} else if strings.Contains(query, "is:open") {
		return "", errors.New("is:open conflicts with --include-closed")
	}
	if !includeLocked {
		if strings.Contains(query, "is:locked") {
			return "", errors.New("is:locked requires --include-locked")
		}
		parts = append(parts, "is:unlocked")
	} else if strings.Contains(query, "is:unlocked") {
		return "", errors.New("is:unlocked conflicts with --include-locked")
	}
	if minUpdated != 0 {
		latest := time.Now().Add(-minUpdated)
		parts = append(parts, "updated:<="+latest.Format(time.RFC3339))
	}
	return strings.Join(parts, " "), nil
}

type client interface {
	CreateComment(owner, repo string, number int, comment string) error
	FindIssuesWithOrg(org, query, sort string, asc bool) ([]github.Issue, error)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	o := flagOptions()

	if o.query == "" {
		log.Fatal("empty --query")
	}
	if o.comment == "" {
		log.Fatal("empty --comment")
	}
	if o.github.TokenPath == "" && o.github.AppPrivateKeyPath == "" {
		log.Fatal("set --github-token-path (PAT) or --github-app-id and --github-app-private-key-path (GitHub App)")
	}
	if o.github.AppPrivateKeyPath != "" && o.githubOrg == "" {
		log.Fatal("--github-org is required when using GitHub App auth (defaults to 'openshift')")
	}

	if err := o.github.Validate(!o.confirm); err != nil {
		log.Fatalf("invalid GitHub options: %v", err)
	}

	c, err := o.github.GitHubClient(!o.confirm)
	if err != nil {
		log.Fatalf("failed to construct GitHub client: %v", err)
	}

	query, err := makeQuery(o.query, o.includeArchived, o.includeClosed, o.includeLocked, o.updated)
	if err != nil {
		log.Fatalf("bad query %q: %v", o.query, err)
	}
	sort := ""
	asc := false
	if o.updated > 0 {
		sort = "updated"
		asc = true
	}
	commenter := makeCommenter(o.comment, o.useTemplate)
	if err := run(c, o.githubOrg, query, sort, asc, o.random, commenter, o.ceiling); err != nil {
		log.Fatalf("failed run: %v", err)
	}
}

func makeCommenter(comment string, useTemplate bool) func(meta) (string, error) {
	if !useTemplate {
		return func(_ meta) (string, error) {
			return comment, nil
		}
	}
	t := template.Must(template.New("comment").Parse(comment))
	return func(m meta) (string, error) {
		out := bytes.Buffer{}
		err := t.Execute(&out, m)
		return out.String(), err
	}
}

func run(c client, org, query, sort string, asc, random bool, commenter func(meta) (string, error), ceiling int) error {
	log.Printf("Searching: %s", query)
	issues, err := c.FindIssuesWithOrg(org, query, sort, asc)
	if err != nil {
		return fmt.Errorf("search failed: %w", err)
	}
	problems := []string{}
	log.Printf("Found %d matches", len(issues))
	if random {
		rand.Shuffle(len(issues), func(i, j int) {
			issues[i], issues[j] = issues[j], issues[i]
		})

	}
	for n, i := range issues {
		if ceiling > 0 && n == ceiling {
			log.Printf("Stopping at --ceiling=%d of %d results", n, len(issues))
			break
		}
		log.Printf("Matched %s (%s)", i.HTMLURL, i.Title)
		org, repo, number, err := parseHTMLURL(i.HTMLURL)
		if err != nil {
			msg := fmt.Sprintf("Failed to parse %s: %v", i.HTMLURL, err)
			log.Print(msg)
			problems = append(problems, msg)
		}
		comment, err := commenter(meta{Number: number, Org: org, Repo: repo, Issue: i})
		if err != nil {
			msg := fmt.Sprintf("Failed to create comment for %s/%s#%d: %v", org, repo, number, err)
			log.Print(msg)
			problems = append(problems, msg)
			continue
		}
		if err := c.CreateComment(org, repo, number, comment); err != nil {
			msg := fmt.Sprintf("Failed to apply comment to %s/%s#%d: %v", org, repo, number, err)
			log.Print(msg)
			problems = append(problems, msg)
			continue
		}
		log.Printf("Commented on %s", i.HTMLURL)
	}
	if len(problems) > 0 {
		return fmt.Errorf("encoutered %d failures: %v", len(problems), problems)
	}
	return nil
}
