package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog"

	"github.com/openshift/ci-search/pkg/httpwriter"
)

func (o *options) handleSearch(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	var index *Index
	var success bool
	defer func() {
		klog.Infof("Render search %s duration=%s success=%t", index.String(), time.Since(start).Truncate(time.Millisecond), success)
	}()

	var err error
	index, err = parseRequest(req, "text", o.MaxAge)
	if err != nil {
		http.Error(w, fmt.Sprintf("Bad input: %v", err), http.StatusBadRequest)
		return
	}

	if len(index.Search) == 0 {
		http.Error(w, "The 'search' query parameter is required", http.StatusBadRequest)
		return
	}

	result, err := o.searchResult(req.Context(), index)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed search: %v", err), http.StatusInternalServerError)
		return
	}

	if index.BuildFarm != "all farms" && index.BuildFarm != "unknown" {
		for url := range result {
			a := o.urlToJob[url]
			fmt.Println(a)
			if o.urlToJob[url] != index.BuildFarm {
				delete(result, url)
			}
		}
	}
	if index.BuildFarm == "unknown" {
		for url := range result {
			if o.urlToJob[url] != "" {
				delete(result, url)
			}
		}
	}

	data, err := json.Marshal(result)
	if err != nil {
		http.Error(w, fmt.Sprintf("Unable to serialize result: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	writer := httpwriter.ForRequest(w, req)
	defer writer.Close()

	if _, err = writer.Write(data); err != nil {
		klog.Errorf("Failed to write response: %v", err)
		return
	}

	success = true
}

func (o *options) handleSearchV2(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	var index *Index
	var success bool
	defer func() {
		klog.Infof("Render search %s duration=%s success=%t", index.String(), time.Since(start).Truncate(time.Millisecond), success)
	}()

	var err error
	index, err = parseRequest(req, "text", o.MaxAge)
	if err != nil {
		http.Error(w, fmt.Sprintf("Bad input: %v", err), http.StatusBadRequest)
		return
	}

	if len(index.Search) == 0 {
		http.Error(w, "The 'search' query parameter is required", http.StatusBadRequest)
		return
	}

	internalResults, err := o.searchResult(req.Context(), index)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed search: %v", err), http.StatusInternalServerError)
		return
	}

	result := SearchResponse{
		Results: make(map[string]SearchResponseResult),
	}
	for url, searchResults := range internalResults {
		for query, matches := range searchResults {
			for _, match := range matches {
				match.URL = url
				if response, found := result.Results[query]; !found {
					result.Results[query] = SearchResponseResult{Matches: []*Match{match}}
				} else {
					response.Matches = append(response.Matches, match)
					result.Results[query] = response
				}
			}
		}
	}
	data, err := json.Marshal(result)
	if err != nil {
		http.Error(w, fmt.Sprintf("Unable to serialize result: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	writer := httpwriter.ForRequest(w, req)
	defer writer.Close()

	if _, err = writer.Write(data); err != nil {
		klog.Errorf("Failed to write response: %v", err)
		return
	}

	success = true
}

// searchResult returns a result[uri][search][]*Match.
func (o *options) searchResult(ctx context.Context, index *Index) (map[string]map[string][]*Match, error) {
	result := map[string]map[string][]*Match{}

	if index.MaxMatches == 0 {
		index.MaxMatches = 1
	}

	err := executeGrep(ctx, o.generator, index, nil, func(name string, search string, matches []bytes.Buffer, moreLines int) error {
		metadata, err := o.MetadataFor(name)
		if err != nil {
			klog.Errorf("unable to resolve metadata for: %s: %v", name, err)
			return nil
		}
		if metadata.URI == nil {
			klog.Errorf("Failed to compute job URI for %q", name)
			return nil
		}
		if metadata.FileType != "bug" && metadata.FileType != "issue" && index.JobFilter != nil && !index.JobFilter(metadata.Name) {
			return nil
		}
		uri := metadata.URI.String()

		if !(o.urlToJob[uri] == index.BuildFarm || index.BuildFarm == "all farms" || (index.BuildFarm == "unknown" && o.urlToJob[uri] == "")) {
			return nil
		}

		_, ok := result[uri]
		if !ok {
			result[uri] = make(map[string][]*Match, 1)
		}

		_, ok = result[uri][search]
		if !ok {
			result[uri][search] = make([]*Match, 0, 1)
		}

		match := &Match{
			FileType:  metadata.FileType,
			MoreLines: moreLines,
			Name:      metadata.Name,
			Bug:       metadata.Bug,
			Issue:     metadata.Issue,
		}

		for _, m := range matches {
			line := bytes.TrimRightFunc(m.Bytes(), func(r rune) bool { return r == ' ' })
			match.Context = append(match.Context, string(line))
		}
		result[uri][search] = append(result[uri][search], match)
		return nil
	})

	return result, err
}

type SearchJobInstanceResult struct {
	Number  int
	URI     *url.URL
	Matches []Match
}

type SearchJobsResult struct {
	Name      string
	Trigger   string
	Instances []SearchJobInstanceResult
}

type SearchBugResult struct {
	Name    string
	Number  int
	URI     *url.URL
	Matches []Match
}

//jira
type SearchIssuesResult struct {
	Name    string
	Number  int
	Key     string
	URI     *url.URL
	Matches []Match
}

type SearchResult struct {
	Matches int

	Bugs        []SearchBugResult
	bugByNumber map[int]int

	//jira
	Issues        []SearchIssuesResult
	issueByNumber map[int]int

	Jobs      []SearchJobsResult
	JobNames  sets.String
	jobByName map[string]int
}

func (s *SearchResult) BugByNumber(num int) *SearchBugResult {
	i, ok := s.bugByNumber[num]
	if ok {
		return &s.Bugs[i]
	}
	if s.bugByNumber == nil {
		s.bugByNumber = make(map[int]int)
	}
	i = len(s.Bugs)
	s.Bugs = append(s.Bugs, SearchBugResult{Number: num})
	s.bugByNumber[num] = i
	return &s.Bugs[i]
}

//jira
func (s *SearchResult) IssueByNumber(num int) *SearchIssuesResult {
	i, ok := s.issueByNumber[num]
	if ok {
		return &s.Issues[i]
	}
	if s.issueByNumber == nil {
		s.issueByNumber = make(map[int]int)
	}
	i = len(s.Issues)
	s.Issues = append(s.Issues, SearchIssuesResult{Number: num})
	s.issueByNumber[num] = i
	return &s.Issues[i]
}

func (s *SearchResult) JobByName(name string) *SearchJobsResult {
	i, ok := s.jobByName[name]
	if ok {
		return &s.Jobs[i]
	}
	if s.jobByName == nil {
		s.jobByName = make(map[string]int)
	}
	i = len(s.Jobs)
	s.Jobs = append(s.Jobs, SearchJobsResult{Name: name})
	s.jobByName[name] = i
	return &s.Jobs[i]
}

// searchResult returns an ordered struct containing results by job.
func (o *options) orderedSearchResults(ctx context.Context, index *Index) (*SearchResult, error) {
	var result SearchResult
	result.JobNames = make(sets.String, 500)

	if index.MaxMatches == 0 {
		index.MaxMatches = 1
	}

	count := 0
	err := executeGrep(ctx, o.generator, index, result.JobNames, func(name string, search string, matches []bytes.Buffer, moreLines int) error {
		metadata, err := o.MetadataFor(name)
		if err != nil {
			klog.Errorf("unable to resolve metadata for: %s: %v", name, err)
			return nil
		}
		if metadata.URI == nil {
			klog.Errorf("Failed to compute job URI for %q", name)
			return nil
		}
		if metadata.FileType != "bug" && metadata.FileType != "issue" && index.JobFilter != nil && !index.JobFilter(metadata.Name) {
			return nil
		}
		switch metadata.FileType {
		case "bug":
			bug := result.BugByNumber(metadata.Number)
			if len(bug.Name) == 0 {
				bug.Name = metadata.Name
				bug.URI = metadata.URI
			}
			bug.Matches = append(bug.Matches, Match{
				LastModified: metav1.Time{Time: metadata.LastModified},
				FileType:     metadata.FileType,
				MoreLines:    moreLines,
				Context:      trimMatchStrings(matches, make([]string, 0, len(matches))),
			})
			count++
			return nil
		case "issue":
			issue := result.IssueByNumber(metadata.Number)
			if len(issue.Name) == 0 {
				issue.Name = metadata.Name
				issue.URI = metadata.URI
				issue.Key = metadata.Key
			}
			issue.Matches = append(issue.Matches, Match{
				LastModified: metav1.Time{Time: metadata.LastModified},
				FileType:     metadata.FileType,
				MoreLines:    moreLines,
				Context:      trimMatchStrings(matches, make([]string, 0, len(matches))),
			})
			count++
			return nil
		default:
			job := result.JobByName(metadata.Name)
			if len(job.Trigger) == 0 {
				job.Trigger = metadata.Trigger
			}
			if len(job.Instances) == 0 || job.Instances[len(job.Instances)-1].Number != metadata.Number {
				job.Instances = append(job.Instances, SearchJobInstanceResult{
					Number: metadata.Number,
					URI:    metadata.URI,
				})
			}
			instance := &job.Instances[len(job.Instances)-1]
			instance.Matches = append(instance.Matches, Match{
				LastModified: metav1.Time{Time: metadata.LastModified},
				FileType:     metadata.FileType,
				MoreLines:    moreLines,
				Context:      trimMatchStrings(matches, make([]string, 0, len(matches))),
			})
			count++
			return nil
		}
	})
	result.Matches = count
	return &result, err
}
