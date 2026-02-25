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

	// Use os.CreateTemp to avoid filename collisions when multiple calls
	// happen near-simultaneously (e.g., back-to-back WriteRequest calls).
	pattern := fmt.Sprintf("request-%s-*.yaml", now.Format("20060102T150405.000000000"))
	tmpFile, err := os.CreateTemp(queuesDir, pattern)
	if err != nil {
		return "", fmt.Errorf("create request file: %w", err)
	}
	path := tmpFile.Name()

	_, writeErr := tmpFile.Write(data)
	closeErr := tmpFile.Close()
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

func ReadRequests(queuesDir string) ([]Request, []string, error) {
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
			continue
		}
		var req Request
		if err := yaml.Unmarshal(data, &req); err != nil {
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
