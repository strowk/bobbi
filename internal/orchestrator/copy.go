package orchestrator

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// CopyDir copies src directory contents to dst, always excluding .git/.
func CopyDir(src, dst string) error {
	return copyDirExcluding(src, dst, nil)
}

// CopySolutionSource copies src directory contents to dst,
// excluding .git/, architecture/, and solution-deliverable/ directories
// as specified by the contract for handoff_solution.
func CopySolutionSource(src, dst string) error {
	return copyDirExcluding(src, dst, []string{"architecture", "solution-deliverable"})
}

func copyDirExcluding(src, dst string, excludeDirs []string) error {
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

		if d.IsDir() {
			// Always exclude .git at any depth
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			// Only exclude specified dirs at the top level (depth 1)
			if filepath.Dir(rel) == "." && rel != "." {
				for _, ex := range excludeDirs {
					if d.Name() == ex {
						return filepath.SkipDir
					}
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

	if err := os.Rename(tmpPath, dst); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp file: %w", err)
	}

	return nil
}
