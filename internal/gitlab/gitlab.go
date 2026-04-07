package gitlab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// Client handles GitLab API interactions for Green CI.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a GitLab API client. The base URL is read from GITLAB_API_URL
// env var, defaulting to "https://gitlab.com".
func NewClient() *Client {
	base := os.Getenv("GITLAB_API_URL")
	if base == "" {
		base = "https://gitlab.com"
	}
	base = strings.TrimRight(base, "/")
	return &Client{
		baseURL:    base,
		httpClient: &http.Client{},
	}
}

// MRRequest is the payload for creating a merge request.
type MRRequest struct {
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Title        string `json:"title"`
	Description  string `json:"description"`
}

// MRResponse represents a GitLab merge request.
type MRResponse struct {
	IID         int    `json:"iid"`
	MergeStatus string `json:"merge_status"`
	SHA         string `json:"sha"`
	State       string `json:"state"`
}

// PipelineResponse represents a GitLab pipeline.
type PipelineResponse struct {
	ID     int    `json:"id"`
	SHA    string `json:"sha"`
	Status string `json:"status"`
}

// PipelineJob represents a single pipeline job.
type PipelineJob struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// MergeRequest payload for accepting a merge request.
type MergeAcceptRequest struct {
	ShouldRemoveSourceBranch bool `json:"should_remove_source_branch"`
}

// MergeAcceptResponse represents the merge accept API response.
type MergeAcceptResponse struct {
	State string `json:"state"`
}

// ProjectID extracts the URL-encoded project path from the remote URL.
// For GitLab API, this returns the URL-encoded "owner/repo" path.
func ProjectID(remoteURL string) string {
	u := remoteURL
	u = strings.TrimSpace(u)
	u = strings.TrimSuffix(u, ".git")

	// SSH format: git@gitlab.com:owner/repo.git
	if strings.HasPrefix(u, "git@") {
		parts := strings.SplitN(u, ":", 2)
		if len(parts) == 2 {
			return strings.ReplaceAll(parts[1], "/", "%2F")
		}
	}

	// HTTP(S) format: https://gitlab.com/owner/repo
	parts := strings.Split(u, "/")
	if len(parts) >= 2 {
		path := parts[len(parts)-2] + "/" + parts[len(parts)-1]
		return strings.ReplaceAll(path, "/", "%2F")
	}

	return ""
}

// CreateMR creates a merge request.
func (c *Client) CreateMR(projectID, title, sourceBranch, targetBranch, description string) (*MRResponse, error) {
	url := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests", c.baseURL, projectID)
	payload := MRRequest{
		SourceBranch: sourceBranch,
		TargetBranch: targetBranch,
		Title:        title,
		Description:  description,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal MR request: %w", err)
	}

	resp, err := c.doRequest("POST", url, data)
	if err != nil {
		return nil, fmt.Errorf("create MR: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("create MR: status %d: %s", resp.StatusCode, string(respBody))
	}

	var mr MRResponse
	if err := json.Unmarshal(respBody, &mr); err != nil {
		return nil, fmt.Errorf("parse MR response: %w", err)
	}
	return &mr, nil
}

// GetMR retrieves a merge request by IID.
func (c *Client) GetMR(projectID string, iid int) (*MRResponse, error) {
	url := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d", c.baseURL, projectID, iid)
	resp, err := c.doRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("get MR: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get MR: status %d: %s", resp.StatusCode, string(respBody))
	}

	var mr MRResponse
	if err := json.Unmarshal(respBody, &mr); err != nil {
		return nil, fmt.Errorf("parse MR response: %w", err)
	}
	return &mr, nil
}

// GetPipelineJobs lists jobs for a pipeline.
func (c *Client) GetPipelineJobs(projectID string, pipelineID int) ([]PipelineJob, error) {
	url := fmt.Sprintf("%s/api/v4/projects/%s/pipelines/%d/jobs", c.baseURL, projectID, pipelineID)
	resp, err := c.doRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("get pipeline jobs: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get pipeline jobs: status %d: %s", resp.StatusCode, string(respBody))
	}

	var jobs []PipelineJob
	if err := json.Unmarshal(respBody, &jobs); err != nil {
		return nil, fmt.Errorf("parse pipeline jobs response: %w", err)
	}
	return jobs, nil
}

// GetPipelineForSHA finds the pipeline for a given SHA.
func (c *Client) GetPipelineForSHA(projectID, sha string) (*PipelineResponse, error) {
	url := fmt.Sprintf("%s/api/v4/projects/%s/pipelines?sha=%s", c.baseURL, projectID, sha)
	resp, err := c.doRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("get pipelines: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get pipelines: status %d: %s", resp.StatusCode, string(respBody))
	}

	var pipelines []PipelineResponse
	if err := json.Unmarshal(respBody, &pipelines); err != nil {
		return nil, fmt.Errorf("parse pipelines response: %w", err)
	}
	if len(pipelines) == 0 {
		return nil, nil
	}
	return &pipelines[0], nil
}

// MergeMR accepts (merges) a merge request with should_remove_source_branch: true.
func (c *Client) MergeMR(projectID string, iid int) (*MergeAcceptResponse, error) {
	url := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/merge", c.baseURL, projectID, iid)
	payload := MergeAcceptRequest{ShouldRemoveSourceBranch: true}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal merge request: %w", err)
	}

	resp, err := c.doRequest("PUT", url, data)
	if err != nil {
		return nil, fmt.Errorf("merge MR: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("merge MR: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result MergeAcceptResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse merge response: %w", err)
	}
	return &result, nil
}

// CloseMR closes a merge request without merging.
func (c *Client) CloseMR(projectID string, iid int) error {
	url := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d", c.baseURL, projectID, iid)
	payload := map[string]string{"state_event": "close"}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal close request: %w", err)
	}

	resp, err := c.doRequest("PUT", url, data)
	if err != nil {
		return fmt.Errorf("close MR: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("close MR: status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (c *Client) doRequest(method, url string, body []byte) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	// Add auth token if available
	token := os.Getenv("GITLAB_TOKEN")
	if token != "" {
		req.Header.Set("PRIVATE-TOKEN", token)
	}

	return c.httpClient.Do(req)
}
