package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

type RequestData struct {
	Type              string `yaml:"type"`
	From              string `yaml:"from,omitempty"`
	AdditionalContext string `yaml:"additional_context,omitempty"`
}

type Request struct {
	Timestamp time.Time   `yaml:"timestamp"`
	SessionID string      `yaml:"session_id,omitempty"`
	Attempts  int         `yaml:"attempts,omitempty"`
	Request   RequestData `yaml:"request"`
}

func WriteRequest(queuesDir string, reqType, from, additionalContext string) (string, error) {
	if err := os.MkdirAll(queuesDir, 0755); err != nil {
		return "", fmt.Errorf("create queues dir: %w", err)
	}

	now := time.Now().UTC()
	req := Request{
		Timestamp: now,
		Request: RequestData{
			Type:              reqType,
			From:              from,
			AdditionalContext: additionalContext,
		},
	}

	data, err := yaml.Marshal(&req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	// Use exact timestamp format per contract. Increment nanosecond on collision.
	ts := now
	var path string
	for i := 0; i < 1000; i++ {
		filename := fmt.Sprintf("request-%s.yaml", ts.Format("20060102T150405.000000000"))
		path = filepath.Join(queuesDir, filename)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			if os.IsExist(err) {
				ts = ts.Add(time.Nanosecond)
				// Re-marshal so the YAML timestamp matches the filename
				req.Timestamp = ts
				data, err = yaml.Marshal(&req)
				if err != nil {
					return "", fmt.Errorf("marshal request: %w", err)
				}
				continue
			}
			return "", fmt.Errorf("create request file: %w", err)
		}
		_, writeErr := f.Write(data)
		closeErr := f.Close()
		if writeErr != nil {
			os.Remove(path)
			return "", fmt.Errorf("write request: %w", writeErr)
		}
		if closeErr != nil {
			os.Remove(path)
			return "", fmt.Errorf("close request file: %w", closeErr)
		}
		return path, nil
	}
	return "", fmt.Errorf("could not create unique request file after 1000 attempts")
}

// LogFunc is an optional logging callback for ReadRequests.
type LogFunc func(format string, args ...interface{})

func ReadRequests(queuesDir string, logFn LogFunc) ([]Request, []string, error) {
	entries, err := os.ReadDir(queuesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("read queues dir: %w", err)
	}

	var requests []Request
	var paths []string

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(queuesDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			if logFn != nil {
				logFn("[queue] failed to read %s: %v", path, err)
			}
			continue
		}
		var req Request
		if err := yaml.Unmarshal(data, &req); err != nil {
			if logFn != nil {
				logFn("[queue] failed to parse %s: %v", path, err)
			}
			continue
		}
		requests = append(requests, req)
		paths = append(paths, path)
	}

	// Sort by timestamp (co-sort paths alongside requests)
	type pair struct {
		req  Request
		path string
	}
	pairs := make([]pair, len(requests))
	for i := range requests {
		pairs[i] = pair{requests[i], paths[i]}
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		return pairs[i].req.Timestamp.Before(pairs[j].req.Timestamp)
	})
	for i := range pairs {
		requests[i] = pairs[i].req
		paths[i] = pairs[i].path
	}

	return requests, paths, nil
}

// UpdateSessionID reads the request YAML file at the given path, sets the
// session_id field, and writes it back in place. If the file no longer exists
// (e.g., already moved to completed), the update is skipped silently.
func UpdateSessionID(requestPath, sessionID string) error {
	data, err := os.ReadFile(requestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // file already moved, skip silently
		}
		return fmt.Errorf("read request file: %w", err)
	}
	var req Request
	if err := yaml.Unmarshal(data, &req); err != nil {
		return fmt.Errorf("parse request file: %w", err)
	}
	req.SessionID = sessionID
	out, err := yaml.Marshal(&req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	if err := os.WriteFile(requestPath, out, 0644); err != nil {
		if os.IsNotExist(err) {
			return nil // file moved between read and write, skip silently
		}
		return fmt.Errorf("write request file: %w", err)
	}
	return nil
}

// SetAttempts updates the attempts field in a request YAML file and clears
// session_id so it can be re-captured from the new agent session.
// If the file no longer exists (e.g., already moved), the update is skipped silently.
func SetAttempts(requestPath string, attempts int) error {
	data, err := os.ReadFile(requestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read request file: %w", err)
	}
	var req Request
	if err := yaml.Unmarshal(data, &req); err != nil {
		return fmt.Errorf("parse request file: %w", err)
	}
	req.Attempts = attempts
	// Note: session_id is not cleared here. The OnSessionID callback
	// will overwrite it when the new session starts streaming. If the
	// agent crashes before producing output, the previous session_id
	// is preserved (still useful for debugging).
	out, err := yaml.Marshal(&req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	if err := os.WriteFile(requestPath, out, 0644); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("write request file: %w", err)
	}
	return nil
}

// MarkFailed moves a request file from queues/ to the failed directory.
func MarkFailed(requestPath, failedDir string) error {
	if err := os.MkdirAll(failedDir, 0755); err != nil {
		return fmt.Errorf("create failed dir: %w", err)
	}

	filename := filepath.Base(requestPath)
	destPath := filepath.Join(failedDir, filename)

	if err := os.Rename(requestPath, destPath); err != nil {
		return fmt.Errorf("move request to failed: %w", err)
	}

	return nil
}

func MarkCompleted(requestPath, completedDir string) error {
	if err := os.MkdirAll(completedDir, 0755); err != nil {
		return fmt.Errorf("create completed dir: %w", err)
	}

	filename := filepath.Base(requestPath)
	destPath := filepath.Join(completedDir, filename)

	if err := os.Rename(requestPath, destPath); err != nil {
		return fmt.Errorf("move request to completed: %w", err)
	}

	return nil
}
