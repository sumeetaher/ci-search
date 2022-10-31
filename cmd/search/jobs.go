package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog"

	"github.com/openshift/ci-search/pkg/httpwriter"
	"github.com/openshift/ci-search/prow"
)

func (o *options) handleJobs(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	var success bool

	var index *Index
	var err error
	index, err = parseRequest(req, "text", o.MaxAge)
	if err != nil {
		http.Error(w, fmt.Sprintf("Bad input: %v", err), http.StatusBadRequest)
		return
	}

	defer func() {
		klog.Infof("Render jobs duration=%s success=%t", time.Since(start).Truncate(time.Millisecond), success)
	}()

	if o.jobAccessor == nil {
		http.Error(w, "Unable to serve jobs data because no prow data source was configured.", http.StatusInternalServerError)
		return
	}

	jobs, err := o.jobAccessor.List(labels.Everything())
	var filteredJobs []*prow.Job
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load jobs: %v", err), http.StatusInternalServerError)
		return
	}

	if index.BuildFarm != "all farms" && index.BuildFarm != "unknown" {
		for _, job := range jobs {
			if job.Spec.Cluster == index.BuildFarm {
				filteredJobs = append(filteredJobs, job)
			}
		}
	}
	if index.BuildFarm == "unknown" {
		for _, job := range jobs {
			if job.Spec.Cluster == "" {
				filteredJobs = append(filteredJobs, job)
			}
		}
	}

	if index.BuildFarm == "all farms" {
		filteredJobs = jobs
	}
	// sort uncompleted -> newest completed -> oldest completed
	sort.Slice(filteredJobs, func(i, j int) bool {
		iTime, jTime := filteredJobs[i].Status.CompletionTime.Time, filteredJobs[j].Status.CompletionTime.Time
		if iTime.Equal(jTime) {
			return true
		}
		if iTime.IsZero() && !jTime.IsZero() {
			return true
		}
		if !iTime.IsZero() && jTime.IsZero() {
			return false
		}
		return jTime.Before(iTime)
	})
	list := prow.JobList{Items: filteredJobs}
	data, err := json.Marshal(list)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to write jobs: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	writer := httpwriter.ForRequest(w, req)
	defer writer.Close()
	if _, err := writer.Write(data); err != nil {
		klog.Errorf("Failed to write response: %v", err)
		return
	}

	success = true
}
