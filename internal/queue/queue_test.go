package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestWriteRequestRoundTrip(t *testing.T) {
	dir := t.TempDir()

	path, err := WriteRequest(dir, "request_solution_change", "evaluator", "fix the bug")
	if err != nil {
		t.Fatalf("WriteRequest failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	var req Request
	if err := yaml.Unmarshal(data, &req); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if req.Request.Type != "request_solution_change" {
		t.Errorf("got type %q, want %q", req.Request.Type, "request_solution_change")
	}
	if req.Request.From != "evaluator" {
		t.Errorf("got from %q, want %q", req.Request.From, "evaluator")
	}
	if req.Request.AdditionalContext != "fix the bug" {
		t.Errorf("got context %q, want %q", req.Request.AdditionalContext, "fix the bug")
	}
	if req.Timestamp.IsZero() {
		t.Error("timestamp should not be zero")
	}
}

func TestWriteRequestCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "queues")

	_, err := WriteRequest(dir, "start_solver", "user", "")
	if err != nil {
		t.Fatalf("WriteRequest failed: %v", err)
	}

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("expected queues dir to be created")
	}
}

func TestWriteRequestCollisionHandling(t *testing.T) {
	dir := t.TempDir()

	// Write two requests in quick succession — should not collide
	path1, err := WriteRequest(dir, "start_solver", "user", "first")
	if err != nil {
		t.Fatalf("first WriteRequest failed: %v", err)
	}
	path2, err := WriteRequest(dir, "start_solver", "user", "second")
	if err != nil {
		t.Fatalf("second WriteRequest failed: %v", err)
	}

	if path1 == path2 {
		t.Error("expected different file paths for two requests")
	}
}

func TestReadRequestsTimestampOrdering(t *testing.T) {
	dir := t.TempDir()

	// Write requests with known timestamps by creating files directly
	timestamps := []time.Time{
		time.Date(2025, 1, 1, 12, 0, 3, 0, time.UTC),
		time.Date(2025, 1, 1, 12, 0, 1, 0, time.UTC),
		time.Date(2025, 1, 1, 12, 0, 2, 0, time.UTC),
	}

	for i, ts := range timestamps {
		req := Request{
			Timestamp: ts,
			Request: RequestData{
				Type:              "request_solution_change",
				From:              "evaluator",
				AdditionalContext: string(rune('A' + i)),
			},
		}
		data, _ := yaml.Marshal(&req)
		filename := ts.Format("request-20060102T150405.000000000.yaml")
		os.WriteFile(filepath.Join(dir, filename), data, 0644)
	}

	requests, _, err := ReadRequests(dir, nil)
	if err != nil {
		t.Fatalf("ReadRequests failed: %v", err)
	}

	if len(requests) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(requests))
	}

	// Should be sorted by timestamp ascending
	for i := 1; i < len(requests); i++ {
		if requests[i].Timestamp.Before(requests[i-1].Timestamp) {
			t.Errorf("request %d timestamp %v is before request %d timestamp %v",
				i, requests[i].Timestamp, i-1, requests[i-1].Timestamp)
		}
	}

	// Verify order: B (12:00:01), C (12:00:02), A (12:00:03)
	if requests[0].Request.AdditionalContext != "B" {
		t.Errorf("first request context = %q, want %q", requests[0].Request.AdditionalContext, "B")
	}
	if requests[2].Request.AdditionalContext != "A" {
		t.Errorf("last request context = %q, want %q", requests[2].Request.AdditionalContext, "A")
	}
}

func TestReadRequestsEmptyDir(t *testing.T) {
	dir := t.TempDir()

	requests, paths, err := ReadRequests(dir, nil)
	if err != nil {
		t.Fatalf("ReadRequests failed: %v", err)
	}
	if len(requests) != 0 {
		t.Errorf("expected 0 requests, got %d", len(requests))
	}
	if len(paths) != 0 {
		t.Errorf("expected 0 paths, got %d", len(paths))
	}
}

func TestReadRequestsNonExistentDir(t *testing.T) {
	requests, paths, err := ReadRequests(filepath.Join(t.TempDir(), "nonexistent"), nil)
	if err != nil {
		t.Fatalf("ReadRequests should not error for nonexistent dir: %v", err)
	}
	if len(requests) != 0 || len(paths) != 0 {
		t.Error("expected empty results for nonexistent dir")
	}
}

func TestReadRequestsSkipsMalformed(t *testing.T) {
	dir := t.TempDir()

	// Write a valid request
	WriteRequest(dir, "start_solver", "user", "")

	// Write a malformed file
	os.WriteFile(filepath.Join(dir, "request-bad.yaml"), []byte("not: valid: yaml: {{"), 0644)

	requests, _, err := ReadRequests(dir, nil)
	if err != nil {
		t.Fatalf("ReadRequests failed: %v", err)
	}
	if len(requests) != 1 {
		t.Errorf("expected 1 valid request, got %d", len(requests))
	}
}

func TestUpdateSessionID(t *testing.T) {
	dir := t.TempDir()

	path, err := WriteRequest(dir, "start_solver", "user", "")
	if err != nil {
		t.Fatalf("WriteRequest failed: %v", err)
	}

	if err := UpdateSessionID(path, "sess-123"); err != nil {
		t.Fatalf("UpdateSessionID failed: %v", err)
	}

	data, _ := os.ReadFile(path)
	var req Request
	yaml.Unmarshal(data, &req)

	if req.SessionID != "sess-123" {
		t.Errorf("session_id = %q, want %q", req.SessionID, "sess-123")
	}
}

func TestUpdateSessionIDMissingFile(t *testing.T) {
	err := UpdateSessionID(filepath.Join(t.TempDir(), "nonexistent.yaml"), "sess-123")
	if err != nil {
		t.Errorf("should silently skip missing file, got: %v", err)
	}
}

func TestSetAttempts(t *testing.T) {
	dir := t.TempDir()

	path, err := WriteRequest(dir, "start_solver", "user", "")
	if err != nil {
		t.Fatalf("WriteRequest failed: %v", err)
	}

	// Set session ID first
	UpdateSessionID(path, "sess-123")

	if err := SetAttempts(path, 2); err != nil {
		t.Fatalf("SetAttempts failed: %v", err)
	}

	data, _ := os.ReadFile(path)
	var req Request
	yaml.Unmarshal(data, &req)

	if req.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", req.Attempts)
	}
	if req.SessionID != "" {
		t.Errorf("session_id should be cleared, got %q", req.SessionID)
	}
}

func TestMarkCompleted(t *testing.T) {
	dir := t.TempDir()
	completedDir := filepath.Join(dir, "completed")

	path, _ := WriteRequest(filepath.Join(dir, "queues"), "start_solver", "user", "")
	filename := filepath.Base(path)

	if err := MarkCompleted(path, completedDir); err != nil {
		t.Fatalf("MarkCompleted failed: %v", err)
	}

	// Original should not exist
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("original file should be removed")
	}
	// Should exist in completed
	if _, err := os.Stat(filepath.Join(completedDir, filename)); os.IsNotExist(err) {
		t.Error("file should exist in completed dir")
	}
}

func TestMarkFailed(t *testing.T) {
	dir := t.TempDir()
	failedDir := filepath.Join(dir, "failed")

	path, _ := WriteRequest(filepath.Join(dir, "queues"), "start_solver", "user", "")
	filename := filepath.Base(path)

	if err := MarkFailed(path, failedDir); err != nil {
		t.Fatalf("MarkFailed failed: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("original file should be removed")
	}
	if _, err := os.Stat(filepath.Join(failedDir, filename)); os.IsNotExist(err) {
		t.Error("file should exist in failed dir")
	}
}

func TestWriteRequestFilenameFormat(t *testing.T) {
	dir := t.TempDir()

	path, err := WriteRequest(dir, "start_solver", "user", "")
	if err != nil {
		t.Fatalf("WriteRequest failed: %v", err)
	}

	filename := filepath.Base(path)
	if !strings.HasPrefix(filename, "request-") || !strings.HasSuffix(filename, ".yaml") {
		t.Errorf("unexpected filename format: %s", filename)
	}
}
