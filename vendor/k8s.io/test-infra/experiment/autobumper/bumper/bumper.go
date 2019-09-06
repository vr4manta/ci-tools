/*
Copyright 2019 The Kubernetes Authors.

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

package bumper

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/experiment/image-bumper/bumper"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/robots/pr-creator/updater"
)

const (
	prowPrefix      = "gcr.io/k8s-prow/"
	testImagePrefix = "gcr.io/k8s-testimages/"
	prowRepo        = "https://github.com/kubernetes/test-infra"
	testImageRepo   = prowRepo
)

func Call(cmd string, args ...string) error {
	c := exec.Command(cmd, args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// UpdatePR updates with github client "gc" the PR of github repo org/repo
// with "matchTitle" from "source" to "branch"
// "images" contains the tag replacements that have been made which is returned from "UpdateReferences([]string{"."}, extraFiles)"
// "images" and "extraLineInPRBody" are used to generate commit summary and body of the PR
func UpdatePR(gc github.Client, org, repo string, images map[string]string, extraLineInPRBody string, matchTitle, source, branch string) error {
	return updatePR(gc, org, repo, makeCommitSummary(images), generatePRBody(images, extraLineInPRBody), matchTitle, source, branch)
}

func updatePR(gc github.Client, org, repo, title, body, matchTitle, source, branch string) error {
	logrus.Info("Creating PR...")
	n, err := updater.UpdatePR(org, repo, title, body, matchTitle, gc)
	if err != nil {
		return fmt.Errorf("failed to update %d: %v", n, err)
	}
	if n == nil {
		pr, err := gc.CreatePullRequest(org, repo, title, body, source, branch, true)
		if err != nil {
			return fmt.Errorf("failed to create PR: %v", err)
		}
		n = &pr
	}

	logrus.Infof("PR %s/%s#%d will merge %s into %s: %s", org, repo, *n, source, branch, title)
	return nil
}

// UpdateReferences update the references of prow-images and testimages
// in the files in any of "subfolders" of the current dir
// if the file is a yaml file (*.yaml) or extraFiles[file]=true
func UpdateReferences(subfolders []string, extraFiles map[string]bool) (map[string]string, error) {
	logrus.Info("Bumping image references...")
	filter := regexp.MustCompile(prowPrefix + "|" + testImagePrefix)

	for _, dir := range subfolders {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if strings.HasSuffix(path, ".yaml") || extraFiles[path] {
				if err := bumper.UpdateFile(path, filter); err != nil {
					logrus.WithError(err).Errorf("Failed to update path %s.", path)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return bumper.GetReplacements(), nil
}

func getNewProwVersion(images map[string]string) string {
	for k, v := range images {
		if strings.HasPrefix(k, prowPrefix) {
			return v
		}
	}
	return ""
}

func makeCommitSummary(images map[string]string) string {
	return fmt.Sprintf("Update prow to %s, and other images as necessary.", getNewProwVersion(images))
}

// MakeGitCommit runs a sequence of git commands to
// commit and push the changes the "remote" on "remoteBranch"
// "name" and "email" are used for git-commit command
// "images" contains the tag replacements that have been made which is returned from "UpdateReferences([]string{"."}, extraFiles)"
// "images" is used to generate commit message
func MakeGitCommit(remote, remoteBranch, name, email string, images map[string]string) error {
	logrus.Info("Making git commit...")
	if err := Call("git", "add", "-A"); err != nil {
		return fmt.Errorf("failed to git add: %v", err)
	}
	message := makeCommitSummary(images)
	commitArgs := []string{"commit", "-m", message}
	if name != "" && email != "" {
		commitArgs = append(commitArgs, "--author", fmt.Sprintf("%s <%s>", name, email))
	}
	if err := Call("git", commitArgs...); err != nil {
		return fmt.Errorf("failed to git commit: %v", err)
	}
	logrus.Info("Pushing to remote...")
	if err := Call("git", "push", "-f", remote, fmt.Sprintf("HEAD:%s", remoteBranch)); err != nil {
		return fmt.Errorf("failed to git push: %v", err)
	}
	return nil
}

func tagFromName(name string) string {
	parts := strings.Split(name, ":")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

func componentFromName(name string) string {
	s := strings.Split(strings.Split(name, ":")[0], "/")
	return s[len(s)-1]
}

func formatTagDate(d string) string {
	if len(d) != 8 {
		return d
	}
	// &#x2011; = U+2011 NON-BREAKING HYPHEN, to prevent line wraps.
	return fmt.Sprintf("%s&#x2011;%s&#x2011;%s", d[0:4], d[4:6], d[6:8])
}

func generateSummary(name, repo, prefix string, summarise bool, images map[string]string) string {
	type delta struct {
		oldCommit string
		newCommit string
		oldDate   string
		newDate   string
		variant   string
		component string
	}
	versions := map[string][]delta{}
	for image, newTag := range images {
		if !strings.HasPrefix(image, prefix) {
			continue
		}
		if strings.HasSuffix(image, ":"+newTag) {
			continue
		}
		oldDate, oldCommit, oldVariant := bumper.DeconstructTag(tagFromName(image))
		newDate, newCommit, _ := bumper.DeconstructTag(newTag)
		k := oldCommit + ":" + newCommit
		d := delta{
			oldCommit: oldCommit,
			newCommit: newCommit,
			oldDate:   oldDate,
			newDate:   newDate,
			variant:   oldVariant,
			component: componentFromName(image),
		}
		versions[k] = append(versions[k], d)
	}

	switch {
	case len(versions) == 0:
		return fmt.Sprintf("No %s changes.", name)
	case len(versions) == 1 && summarise:
		for k, v := range versions {
			s := strings.Split(k, ":")
			return fmt.Sprintf("%s changes: %s/compare/%s...%s (%s → %s)", name, repo, s[0], s[1], formatTagDate(v[0].oldDate), formatTagDate(v[0].newDate))
		}
	default:
		changes := make([]string, 0, len(versions))
		for k, v := range versions {
			s := strings.Split(k, ":")
			names := make([]string, 0, len(v))
			for _, d := range v {
				names = append(names, d.component+d.variant)
			}
			sort.Strings(names)
			changes = append(changes, fmt.Sprintf("%s/compare/%s...%s | %s&nbsp;&#x2192;&nbsp;%s | %s",
				repo, s[0], s[1], formatTagDate(v[0].oldDate), formatTagDate(v[0].newDate), strings.Join(names, ", ")))
		}
		sort.Slice(changes, func(i, j int) bool { return strings.Split(changes[i], "|")[1] < strings.Split(changes[j], "|")[1] })
		return fmt.Sprintf("Multiple distinct %s changes:\n\nCommits | Dates | Images\n--- | --- | ---\n%s\n", name, strings.Join(changes, "\n"))
	}
	panic("unreachable!")
}

func generatePRBody(images map[string]string, assignment string) string {
	prowSummary := generateSummary("Prow", prowRepo, prowPrefix, true, images)
	testImagesSummary := generateSummary("test-image", testImageRepo, testImagePrefix, false, images)
	return prowSummary + "\n\n" + testImagesSummary + "\n\n" + assignment + "\n"
}
