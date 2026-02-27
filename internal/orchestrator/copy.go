package orchestrator

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// CopyDir copies src directory contents to dst with no exclusions.
func CopyDir(src, dst string) error {
	return copyDirExcluding(src, dst, nil)
}

// CopyArchitectureContract copies src directory contents to dst,
// excluding root-level .git/ and .claude/ per CONTRACT.md Section 6.1/6.2.
func CopyArchitectureContract(src, dst string) error {
	return copyDirExcluding(src, dst, []string{".git", ".claude"})
}

// CopySolutionSource copies src directory contents to dst,
// excluding root-level .git/, .claude/, architecture/, solution-deliverable/
// per CONTRACT.md Section 6.1.
func CopySolutionSource(src, dst string) error {
	return copyDirExcluding(src, dst, []string{".git", "architecture", "solution-deliverable", ".claude"})
}

func copyDirExcluding(src, dst string, excludeNames []string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Get relative path
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		// Skip symlinks to avoid potential infinite loops
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}

		// Exclude specified names (files or dirs) at the root level only
		if filepath.Dir(rel) == "." && rel != "." {
			for _, ex := range excludeNames {
				if d.Name() == ex {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			}
		}

		destPath := filepath.Join(dst, rel)

		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(destPath, info.Mode())
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(path, destPath, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	dstDir := filepath.Dir(dst)
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	// Write to a temp file in the same directory, then atomically rename.
	// This prevents leaving a corrupt partial file if the copy is interrupted.
	tmpFile, err := os.CreateTemp(dstDir, ".copyfile-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	_, copyErr := io.Copy(tmpFile, srcFile)
	// Always close before rename; close error matters if copy succeeded.
	closeErr := tmpFile.Close()

	if copyErr != nil {
		os.Remove(tmpPath)
		return copyErr
	}
	if closeErr != nil {
		os.Remove(tmpPath)
		return closeErr
	}

	if err := os.Chmod(tmpPath, mode); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("set file mode: %w", err)
	}

	// On Windows, os.Rename fails if dst already exists. Remove it first.
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		os.Remove(tmpPath)
		return fmt.Errorf("remove existing dst: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp file: %w", err)
	}

	return nil
}
