package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// Client handles GitHub API interactions for Green CI.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a GitHub API client. The base URL is read from GITHUB_API_URL
// env var, defaulting to "https://api.github.com".
func NewClient() *Client {
	base := os.Getenv("GITHUB_API_URL")
	if base == "" {
		base = "https://api.github.com"
	}
	base = strings.TrimRight(base, "/")
	return &Client{
		baseURL:    base,
		httpClient: &http.Client{},
	}
}

// PRRequest is the payload for creating a pull request.
type PRRequest struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body"`
}

// PRResponse represents a GitHub pull request.
type PRResponse struct {
	Number    int    `json:"number"`
	Mergeable *bool  `json:"mergeable"`
	Head      PRRef  `json:"head"`
	State     string `json:"state"`
}

// PRRef represents a PR head/base ref.
type PRRef struct {
	SHA string `json:"sha"`
}

// CheckRunsResponse represents the check runs API response.
type CheckRunsResponse struct {
	TotalCount int        `json:"total_count"`
	CheckRuns  []CheckRun `json:"check_runs"`
}

// CheckRun represents a single check run.
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// MergeRequest is the payload for merging a PR.
type MergeRequest struct {
	MergeMethod string `json:"merge_method"`
}

// MergeResponse represents the merge API response.
type MergeResponse struct {
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
	SHA     string `json:"sha"`
}

// ownerRepo extracts owner and repo from the remote URL of a git repository.
// Returns owner, repo strings.
func OwnerRepo(remoteURL string) (string, string) {
	// Handle various formats:
	// https://github.com/owner/repo.git
	// git@github.com:owner/repo.git
	// http://localhost:8080/owner/repo
	u := remoteURL
	u = strings.TrimSpace(u)
	u = strings.TrimSuffix(u, ".git")

	// SSH format
	if strings.HasPrefix(u, "git@") {
		parts := strings.SplitN(u, ":", 2)
		if len(parts) == 2 {
			segments := strings.Split(parts[1], "/")
			if len(segments) >= 2 {
				return segments[len(segments)-2], segments[len(segments)-1]
			}
		}
	}

	// HTTP(S) format
	parts := strings.Split(u, "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2], parts[len(parts)-1]
	}

	return "", ""
}

// CreatePR creates a pull request.
func (c *Client) CreatePR(owner, repo, title, head, base, body string) (*PRResponse, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls", c.baseURL, owner, repo)
	payload := PRRequest{
		Title: title,
		Head:  head,
		Base:  base,
		Body:  body,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal PR request: %w", err)
	}

	resp, err := c.doRequest("POST", url, data)
	if err != nil {
		return nil, fmt.Errorf("create PR: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("create PR: status %d: %s", resp.StatusCode, string(respBody))
	}

	var pr PRResponse
	if err := json.Unmarshal(respBody, &pr); err != nil {
		return nil, fmt.Errorf("parse PR response: %w", err)
	}
	return &pr, nil
}

// GetPR retrieves a pull request by number.
func (c *Client) GetPR(owner, repo string, number int) (*PRResponse, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, number)
	resp, err := c.doRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("get PR: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get PR: status %d: %s", resp.StatusCode, string(respBody))
	}

	var pr PRResponse
	if err := json.Unmarshal(respBody, &pr); err != nil {
		return nil, fmt.Errorf("parse PR response: %w", err)
	}
	return &pr, nil
}

// GetCheckRuns lists check runs for a given ref (SHA).
func (c *Client) GetCheckRuns(owner, repo, ref string) (*CheckRunsResponse, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs", c.baseURL, owner, repo, ref)
	resp, err := c.doRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("get check runs: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get check runs: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result CheckRunsResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse check runs response: %w", err)
	}
	return &result, nil
}

// MergePR merges a pull request using merge commit strategy.
func (c *Client) MergePR(owner, repo string, number int) (*MergeResponse, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/merge", c.baseURL, owner, repo, number)
	payload := MergeRequest{MergeMethod: "merge"}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal merge request: %w", err)
	}

	resp, err := c.doRequest("PUT", url, data)
	if err != nil {
		return nil, fmt.Errorf("merge PR: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("merge PR: status %d: %s", resp.StatusCode, string(respBody))
	}

	var result MergeResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse merge response: %w", err)
	}
	return &result, nil
}

// ClosePR closes a pull request without merging.
func (c *Client) ClosePR(owner, repo string, number int) error {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, number)
	payload := map[string]string{"state": "closed"}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal close request: %w", err)
	}

	resp, err := c.doRequest("PATCH", url, data)
	if err != nil {
		return fmt.Errorf("close PR: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("close PR: status %d: %s", resp.StatusCode, string(respBody))
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

	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Add auth token if available
	token := os.Getenv("GITHUB_TOKEN")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	return c.httpClient.Do(req)
}
