package orchestrator

import (
	"fmt"
	"io"
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
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Get relative path
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			for _, ex := range excludeDirs {
				if info.Name() == ex {
					return filepath.SkipDir
				}
			}
		}

		destPath := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(destPath, info.Mode())
		}

		return copyFile(path, destPath, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}
