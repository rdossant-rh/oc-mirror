package archive

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
)

func addFileToWriter(fi fs.FileInfo, pathToFile, pathInTar string, tarWriter *tar.Writer) error {
	header, err := archiveFileHeader(fi, pathInTar)
	if err != nil {
		return err
	}
	if err := tarWriter.WriteHeader(header); err != nil {
		return err
	}
	// Open the file for reading
	file, err := os.Open(pathToFile)
	if err != nil {
		return err
	}
	defer file.Close()

	// Copy the file contents to the tar archive
	if _, err := io.Copy(tarWriter, file); err != nil {
		return err
	}
	return nil
}

// archiveFileHeader is shared by the actual archive writers.
func archiveFileHeader(info fs.FileInfo, name string) (*tar.Header, error) {
	header, err := tar.FileInfoHeader(info, info.Name())
	if err != nil {
		return nil, fmt.Errorf("create archive entry header: %w", err)
	}
	header.Name = name
	return header, nil
}

func walkArchiveFiles(folder, relativeTo string, add func(fs.FileInfo, string, string) error) error {
	err := filepath.Walk(folder, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		name, err := filepath.Rel(relativeTo, path)
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		return add(info, path, name)
	})
	if err != nil {
		return fmt.Errorf("%w", err) // Preserve the existing callback error messages.
	}
	return nil
}

// AddSize adds nonnegative sizes without overflowing MaxInt64.
func AddSize(a, b int64) int64 {
	a, b = max(a, 0), max(b, 0)
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}
