package securefile

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteAtomic replaces path with data written to a new, unpredictable file in
// the same directory. Creating a fresh file prevents stale permissions or a
// predictable symlink from carrying into the replacement.
func WriteAtomic(path string, perm fs.FileMode, write func(io.Writer) error) error {
	directory := filepath.Dir(path)
	if err := os.Remove(path + ".tmp"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := file.Name()
	defer os.Remove(temporaryPath)

	if err := file.Chmod(perm); err != nil {
		_ = file.Close()
		return err
	}
	if err := write(file); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	return WriteAtomic(path, perm, func(writer io.Writer) error {
		_, err := writer.Write(data)
		return err
	})
}
