/*
Copyright 2018 The Kubernetes Authors.

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

// Package adapter implements a controller that interacts with gerrit instances
package adapter

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/andygrunwald/go-gerrit"
	"github.com/sirupsen/logrus"

	prowapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/gerrit/client"
	"k8s.io/test-infra/prow/gerrit/reporter"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/pjutil"
)

type kubeClient interface {
	CreateProwJob(prowapi.ProwJob) (prowapi.ProwJob, error)
}

type gerritClient interface {
	QueryChanges(lastUpdate time.Time, rateLimit int) map[string][]client.ChangeInfo
	GetBranchRevision(instance, project, branch string) (string, error)
	SetReview(instance, id, revision, message string, labels map[string]string) error
	Account(instance string) *gerrit.AccountInfo
}

type configAgent interface {
	Config() *config.Config
}

// Controller manages gerrit changes.
type Controller struct {
	config config.Getter
	kc     kubeClient
	gc     gerritClient

	lastSyncFallback string

	lastUpdate time.Time
}

// NewController returns a new gerrit controller client
func NewController(lastSyncFallback, cookiefilePath string, projects map[string][]string, kc *kube.Client, cfg config.Getter) (*Controller, error) {
	if lastSyncFallback == "" {
		return nil, errors.New("empty lastSyncFallback")
	}

	var lastUpdate time.Time
	if buf, err := ioutil.ReadFile(lastSyncFallback); err == nil {
		unix, err := strconv.ParseInt(string(buf), 10, 64)
		if err != nil {
			return nil, err
		}
		lastUpdate = time.Unix(unix, 0)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to read lastSyncFallback: %v", err)
	} else {
		logrus.Warnf("lastSyncFallback not found: %s", lastSyncFallback)
		lastUpdate = time.Now()
	}

	c, err := client.NewClient(projects)
	if err != nil {
		return nil, err
	}
	c.Start(cookiefilePath)

	return &Controller{
		kc:               kc,
		config:           cfg,
		gc:               c,
		lastUpdate:       lastUpdate,
		lastSyncFallback: lastSyncFallback,
	}, nil
}

func copyFile(srcPath, destPath string) error {
	// fallback to copying the file instead
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	dst, err := os.OpenFile(destPath, os.O_WRONLY, 0666)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	if err != nil {
		return err
	}
	dst.Sync()
	dst.Close()
	src.Close()
	return nil
}

// SaveLastSync saves last sync time in Unix to a volume
func (c *Controller) SaveLastSync(lastSync time.Time) error {
	if c.lastSyncFallback == "" {
		return nil
	}

	lastSyncUnix := strconv.FormatInt(lastSync.Unix(), 10)
	logrus.Infof("Writing last sync: %s", lastSyncUnix)

	tempFile, err := ioutil.TempFile(filepath.Dir(c.lastSyncFallback), "temp")
	if err != nil {
		return err
	}
	defer os.Remove(tempFile.Name())

	err = ioutil.WriteFile(tempFile.Name(), []byte(lastSyncUnix), 0644)
	if err != nil {
		return err
	}

	err = os.Rename(tempFile.Name(), c.lastSyncFallback)
	if err != nil {
		logrus.WithError(err).Info("Rename failed, fallback to copyfile")
		return copyFile(tempFile.Name(), c.lastSyncFallback)
	}
	return nil
}

// Sync looks for newly made gerrit changes
// and creates prowjobs according to specs
func (c *Controller) Sync() error {
	syncTime := c.lastUpdate

	for instance, changes := range c.gc.QueryChanges(c.lastUpdate, c.config().Gerrit.RateLimit) {
		for _, change := range changes {
			if err := c.ProcessChange(instance, change); err != nil {
				logrus.WithError(err).Errorf("Failed process change %v", change.CurrentRevision)
			}
			if syncTime.Before(change.Updated.Time) {
				syncTime = change.Updated.Time
			}
		}

		logrus.Infof("Processed %d changes for instance %s", len(changes), instance)
	}

	c.lastUpdate = syncTime
	if err := c.SaveLastSync(syncTime); err != nil {
		logrus.WithError(err).Errorf("last sync %v, cannot save to path %v", syncTime, c.lastSyncFallback)
	}

	return nil
}

func makeCloneURI(instance, project string) (*url.URL, error) {
	u, err := url.Parse(instance)
	if err != nil {
		return nil, fmt.Errorf("instance %s is not a url: %v", instance, err)
	}
	if u.Host == "" {
		return nil, errors.New("instance does not set host")
	}
	if u.Path != "" {
		return nil, errors.New("instance cannot set path (this is set by project)")
	}
	u.Path = project
	return u, nil
}

// listChangedFiles lists (in lexicographic order) the files changed as part of a Gerrit patchset
func listChangedFiles(changeInfo client.ChangeInfo) config.ChangedFilesProvider {
	return func() ([]string, error) {
		var changed []string
		revision := changeInfo.Revisions[changeInfo.CurrentRevision]
		for file := range revision.Files {
			changed = append(changed, file)
		}
		return changed, nil
	}
}

func createRefs(reviewHost string, change client.ChangeInfo, cloneURI *url.URL, baseSHA string) (prowapi.Refs, error) {
	rev, ok := change.Revisions[change.CurrentRevision]
	if !ok {
		return prowapi.Refs{}, fmt.Errorf("cannot find current revision for change %v", change.ID)
	}
	var codeHost string // Something like https://android.googlesource.com
	parts := strings.SplitN(reviewHost, ".", 2)
	codeHost = strings.TrimSuffix(parts[0], "-review")
	if len(parts) > 1 {
		codeHost += "." + parts[1]
	}
	refs := prowapi.Refs{
		Org:      cloneURI.Host,  // Something like android-review.googlesource.com
		Repo:     change.Project, // Something like platform/build
		BaseRef:  change.Branch,
		BaseSHA:  baseSHA,
		CloneURI: cloneURI.String(), // Something like https://android-review.googlesource.com/platform/build
		RepoLink: fmt.Sprintf("%s/%s", codeHost, change.Project),
		BaseLink: fmt.Sprintf("%s/%s/+/%s", codeHost, change.Project, baseSHA),
		Pulls: []prowapi.Pull{
			{
				Number:     change.Number,
				Author:     rev.Commit.Author.Name,
				SHA:        change.CurrentRevision,
				Ref:        rev.Ref,
				Link:       fmt.Sprintf("%s/c/%s/+/%d", reviewHost, change.Project, change.Number),
				CommitLink: fmt.Sprintf("%s/%s/+/%s", codeHost, change.Project, change.CurrentRevision),
				AuthorLink: fmt.Sprintf("%s/q/%s", reviewHost, rev.Commit.Author.Email),
			},
		},
	}
	return refs, nil
}

// ProcessChange creates new presubmit prowjobs base off the gerrit changes
func (c *Controller) ProcessChange(instance string, change client.ChangeInfo) error {
	logger := logrus.WithField("gerrit change", change.Number)

	cloneURI, err := makeCloneURI(instance, change.Project)
	if err != nil {
		return fmt.Errorf("failed to create clone uri: %v", err)
	}

	baseSHA, err := c.gc.GetBranchRevision(instance, change.Project, change.Branch)
	if err != nil {
		return fmt.Errorf("failed to get SHA from base branch: %v", err)
	}

	triggeredJobs := []string{}

	refs, err := createRefs(instance, change, cloneURI, baseSHA)
	if err != nil {
		return fmt.Errorf("failed to get refs: %v", err)
	}

	type jobSpec struct {
		spec   prowapi.ProwJobSpec
		labels map[string]string
	}

	var jobSpecs []jobSpec

	changedFiles := listChangedFiles(change)

	switch change.Status {
	case client.Merged:
		postsubmits := c.config().Postsubmits[cloneURI.String()]
		postsubmits = append(postsubmits, c.config().Postsubmits[cloneURI.Host+"/"+cloneURI.Path]...)
		for _, postsubmit := range postsubmits {
			if shouldRun, err := postsubmit.ShouldRun(change.Branch, changedFiles); err != nil {
				return fmt.Errorf("failed to determine if postsubmit %q should run: %v", postsubmit.Name, err)
			} else if shouldRun {
				jobSpecs = append(jobSpecs, jobSpec{
					spec:   pjutil.PostsubmitSpec(postsubmit, refs),
					labels: postsubmit.Labels,
				})
			}
		}
	case client.New:
		presubmits := c.config().Presubmits[cloneURI.String()]
		presubmits = append(presubmits, c.config().Presubmits[cloneURI.Host+"/"+cloneURI.Path]...)

		var filters []pjutil.Filter
		var latestReport *reporter.JobReport
		account := c.gc.Account(instance)
		// Should not happen, since this means auth failed
		if account == nil {
			return fmt.Errorf("unable to get gerrit account")
		}

		for _, message := range change.Messages {
			// If message status report is not from the prow account ignore
			if message.Author.AccountID != account.AccountID {
				continue
			}
			report := reporter.ParseReport(message.Message)
			if report != nil {
				logrus.Infof("Found latest report: %s", message.Message)
				latestReport = report
				break
			}
		}
		filter, err := messageFilter(c.lastUpdate, change, presubmits, latestReport, logger)
		if err != nil {
			logger.WithError(err).Warn("failed to create filter on messages for presubmits")
		} else {
			filters = append(filters, filter)
		}
		if change.Revisions[change.CurrentRevision].Created.Time.After(c.lastUpdate) {
			filters = append(filters, pjutil.TestAllFilter())
		}
		toTrigger, _, err := pjutil.FilterPresubmits(pjutil.AggregateFilter(filters), listChangedFiles(change), change.Branch, presubmits, logger)
		if err != nil {
			return fmt.Errorf("failed to filter presubmits: %v", err)
		}
		for _, presubmit := range toTrigger {
			jobSpecs = append(jobSpecs, jobSpec{
				spec:   pjutil.PresubmitSpec(presubmit, refs),
				labels: presubmit.Labels,
			})
		}
	}

	annotations := map[string]string{
		client.GerritID:       change.ID,
		client.GerritInstance: instance,
	}

	for _, jSpec := range jobSpecs {
		labels := make(map[string]string)
		for k, v := range jSpec.labels {
			labels[k] = v
		}
		labels[client.GerritRevision] = change.CurrentRevision

		if gerritLabel, ok := labels[client.GerritReportLabel]; !ok || gerritLabel == "" {
			labels[client.GerritReportLabel] = client.CodeReview
		}

		pj := pjutil.NewProwJobWithAnnotation(jSpec.spec, labels, annotations)
		if _, err := c.kc.CreateProwJob(pj); err != nil {
			logger.WithError(err).Errorf("fail to create prowjob %v", pj)
		} else {
			logger.Infof("Triggered Prowjob %s", jSpec.spec.Job)
			triggeredJobs = append(triggeredJobs, jSpec.spec.Job)
		}
	}

	if len(triggeredJobs) > 0 {
		// comment back to gerrit
		message := fmt.Sprintf("Triggered %d prow jobs:", len(triggeredJobs))
		for _, job := range triggeredJobs {
			message += fmt.Sprintf("\n  * Name: %s", job)
		}

		if err := c.gc.SetReview(instance, change.ID, change.CurrentRevision, message, nil); err != nil {
			return err
		}
	}

	return nil
}
