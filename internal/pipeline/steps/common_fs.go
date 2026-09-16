package steps

import (
	"errors"
	"github.com/kunchenguid/no-mistakes/internal/closers"
	"io"
	"os"
	"path/filepath"
)

func copyDirContents(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(srcDir, entry.Name())
		dstPath := filepath.Join(dstDir, entry.Name())
		if err := copyPath(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}

func copyPath(srcPath, dstPath string) error {
	info, err := os.Lstat(srcPath)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(srcPath)
		if err != nil {
			return err
		}
		return os.Symlink(target, dstPath)
	}
	if info.IsDir() {
		if err := os.MkdirAll(dstPath, 0o700); err != nil {
			return err
		}
		if err := copyDirContents(srcPath, dstPath); err != nil {
			return err
		}
		return os.Chmod(dstPath, info.Mode().Perm())
	}
	return copyFile(srcPath, dstPath, info.Mode().Perm())
}

func copyFile(srcPath, dstPath string, perm os.FileMode) (err error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer closers.Quiet(src)

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	// Closing the destination is where a buffered write reaches the disk, so
	// its error is the copy's error. Dropping it reports a truncated file as a
	// complete one.
	defer func() { err = errors.Join(err, dst.Close()) }()

	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	return dst.Chmod(perm)
}
