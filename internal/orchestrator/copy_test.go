package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyDirExcludingRootOnly(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Create structure:
	// src/.git/config
	// src/.claude/CLAUDE.md
	// src/sub/.git/nested  (should NOT be excluded - not at root)
	// src/main.go
	os.MkdirAll(filepath.Join(src, ".git"), 0755)
	os.WriteFile(filepath.Join(src, ".git", "config"), []byte("git config"), 0644)
	os.MkdirAll(filepath.Join(src, ".claude"), 0755)
	os.WriteFile(filepath.Join(src, ".claude", "CLAUDE.md"), []byte("claude md"), 0644)
	os.MkdirAll(filepath.Join(src, "sub", ".git"), 0755)
	os.WriteFile(filepath.Join(src, "sub", ".git", "nested"), []byte("nested git"), 0644)
	os.WriteFile(filepath.Join(src, "main.go"), []byte("package main"), 0644)

	err := copyDirExcluding(src, dst, []string{".git", ".claude"})
	if err != nil {
		t.Fatalf("copyDirExcluding failed: %v", err)
	}

	// .git at root should be excluded
	if _, err := os.Stat(filepath.Join(dst, ".git", "config")); !os.IsNotExist(err) {
		t.Error("root .git should be excluded")
	}

	// .claude at root should be excluded
	if _, err := os.Stat(filepath.Join(dst, ".claude", "CLAUDE.md")); !os.IsNotExist(err) {
		t.Error("root .claude should be excluded")
	}

	// sub/.git should NOT be excluded (not at root level)
	if _, err := os.Stat(filepath.Join(dst, "sub", ".git", "nested")); os.IsNotExist(err) {
		t.Error("nested .git should NOT be excluded")
	}

	// main.go should be copied
	data, err := os.ReadFile(filepath.Join(dst, "main.go"))
	if err != nil {
		t.Fatal("main.go should be copied")
	}
	if string(data) != "package main" {
		t.Errorf("main.go content = %q, want %q", string(data), "package main")
	}
}

func TestCopyDirNoExclusions(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	os.MkdirAll(filepath.Join(src, ".git"), 0755)
	os.WriteFile(filepath.Join(src, ".git", "config"), []byte("git config"), 0644)
	os.WriteFile(filepath.Join(src, "file.txt"), []byte("hello"), 0644)

	err := CopyDir(src, dst)
	if err != nil {
		t.Fatalf("CopyDir failed: %v", err)
	}

	// Everything should be copied when no exclusions
	if _, err := os.Stat(filepath.Join(dst, ".git", "config")); os.IsNotExist(err) {
		t.Error(".git should be included with no exclusions")
	}
	if _, err := os.Stat(filepath.Join(dst, "file.txt")); os.IsNotExist(err) {
		t.Error("file.txt should be copied")
	}
}

func TestCopyArchitectureContract(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	os.MkdirAll(filepath.Join(src, ".git"), 0755)
	os.WriteFile(filepath.Join(src, ".git", "HEAD"), []byte("ref"), 0644)
	os.MkdirAll(filepath.Join(src, ".claude"), 0755)
	os.WriteFile(filepath.Join(src, ".claude", "CLAUDE.md"), []byte("md"), 0644)
	os.WriteFile(filepath.Join(src, "CONTRACT.md"), []byte("contract"), 0644)

	err := CopyArchitectureContract(src, dst)
	if err != nil {
		t.Fatalf("CopyArchitectureContract failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, ".git")); !os.IsNotExist(err) {
		t.Error(".git should be excluded")
	}
	if _, err := os.Stat(filepath.Join(dst, ".claude")); !os.IsNotExist(err) {
		t.Error(".claude should be excluded")
	}

	data, _ := os.ReadFile(filepath.Join(dst, "CONTRACT.md"))
	if string(data) != "contract" {
		t.Errorf("CONTRACT.md content = %q", string(data))
	}
}

func TestCopySolutionSource(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	os.MkdirAll(filepath.Join(src, ".git"), 0755)
	os.MkdirAll(filepath.Join(src, "architecture"), 0755)
	os.MkdirAll(filepath.Join(src, "solution-deliverable"), 0755)
	os.MkdirAll(filepath.Join(src, ".claude"), 0755)
	os.WriteFile(filepath.Join(src, ".git", "HEAD"), []byte("ref"), 0644)
	os.WriteFile(filepath.Join(src, "architecture", "spec.md"), []byte("spec"), 0644)
	os.WriteFile(filepath.Join(src, "solution-deliverable", "app"), []byte("binary"), 0644)
	os.WriteFile(filepath.Join(src, ".claude", "CLAUDE.md"), []byte("md"), 0644)
	os.WriteFile(filepath.Join(src, "main.go"), []byte("package main"), 0644)

	err := CopySolutionSource(src, dst)
	if err != nil {
		t.Fatalf("CopySolutionSource failed: %v", err)
	}

	// All four should be excluded
	for _, name := range []string{".git", "architecture", "solution-deliverable", ".claude"} {
		if _, err := os.Stat(filepath.Join(dst, name)); !os.IsNotExist(err) {
			t.Errorf("%s should be excluded", name)
		}
	}

	// Source code should be copied
	data, _ := os.ReadFile(filepath.Join(dst, "main.go"))
	if string(data) != "package main" {
		t.Errorf("main.go content = %q", string(data))
	}
}

func TestCopyFileOverwrite(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Create source and existing destination
	os.WriteFile(filepath.Join(src, "file.txt"), []byte("new content"), 0644)
	os.WriteFile(filepath.Join(dst, "file.txt"), []byte("old content"), 0644)

	err := CopyDir(src, dst)
	if err != nil {
		t.Fatalf("CopyDir failed: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dst, "file.txt"))
	if string(data) != "new content" {
		t.Errorf("file should be overwritten, got %q", string(data))
	}
}

func TestCopyDirPreservesNestedStructure(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	os.MkdirAll(filepath.Join(src, "a", "b", "c"), 0755)
	os.WriteFile(filepath.Join(src, "a", "b", "c", "deep.txt"), []byte("deep"), 0644)

	err := CopyDir(src, dst)
	if err != nil {
		t.Fatalf("CopyDir failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dst, "a", "b", "c", "deep.txt"))
	if err != nil {
		t.Fatal("deep nested file should be copied")
	}
	if string(data) != "deep" {
		t.Errorf("content = %q, want %q", string(data), "deep")
	}
}

func TestCopyDirExcludingRootFileExclusion(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Exclude a file (not directory) at root level
	os.WriteFile(filepath.Join(src, "exclude-me.txt"), []byte("excluded"), 0644)
	os.WriteFile(filepath.Join(src, "keep-me.txt"), []byte("kept"), 0644)
	os.MkdirAll(filepath.Join(src, "sub"), 0755)
	os.WriteFile(filepath.Join(src, "sub", "exclude-me.txt"), []byte("nested"), 0644)

	err := copyDirExcluding(src, dst, []string{"exclude-me.txt"})
	if err != nil {
		t.Fatalf("copyDirExcluding failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, "exclude-me.txt")); !os.IsNotExist(err) {
		t.Error("root exclude-me.txt should be excluded")
	}
	if _, err := os.Stat(filepath.Join(dst, "keep-me.txt")); os.IsNotExist(err) {
		t.Error("keep-me.txt should be copied")
	}
	// Nested file with same name should NOT be excluded
	if _, err := os.Stat(filepath.Join(dst, "sub", "exclude-me.txt")); os.IsNotExist(err) {
		t.Error("nested exclude-me.txt should NOT be excluded")
	}
}
