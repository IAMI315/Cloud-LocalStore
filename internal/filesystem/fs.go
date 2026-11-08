package filesystem

import (
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

type FileInfo struct {
	Path       string
	Size       int64
	ModTime    time.Time
	Mode       os.FileMode
	IsDir      bool
	PlatformID string
}

type FileSystem interface {
	Walk(root string, fn func(FileInfo) error) error
	Stat(path string) (FileInfo, error)
	Open(path string) (io.ReadCloser, error)
	CreateTemp(dir, pattern string) (*os.File, error)
	Replace(tempPath, targetPath string) error
	Remove(path string) error
}

type OSFileSystem struct{}

// platformID is a best-effort stable identity hint. Unix systems generally
// expose device/inode values; Windows exposes volume serial and file index.
// An empty value means the platform did not expose a supported identity.
func platformID(info os.FileInfo) string {
	sys := info.Sys()
	if sys == nil { return "" }
	v := reflect.Indirect(reflect.ValueOf(sys))
	if v.Kind() != reflect.Struct { return "" }
	values := make([]string, 0, 3)
	for _, name := range []string{"Dev", "Ino", "VolumeSerialNumber", "FileIndexHigh", "FileIndexLow"} {
		field := v.FieldByName(name)
		if field.IsValid() && field.CanInterface() { values = append(values, fmt.Sprint(field.Interface())) }
	}
	return strings.Join(values, ":")
}

func (OSFileSystem) Walk(root string, fn func(FileInfo) error) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return fn(FileInfo{Path: path, Size: info.Size(), ModTime: info.ModTime(), Mode: info.Mode(), IsDir: info.IsDir(), PlatformID: platformID(info)})
	})
}

func (OSFileSystem) Stat(path string) (FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{Path: path, Size: info.Size(), ModTime: info.ModTime(), Mode: info.Mode(), IsDir: info.IsDir(), PlatformID: platformID(info)}, nil
}

func (OSFileSystem) Open(path string) (io.ReadCloser, error) { return os.Open(path) }
func (OSFileSystem) CreateTemp(dir, pattern string) (*os.File, error) { return os.CreateTemp(dir, pattern) }
func (OSFileSystem) Replace(tempPath, targetPath string) error { return os.Rename(tempPath, targetPath) }
func (OSFileSystem) Remove(path string) error { return os.Remove(path) }

func NormalizeRelativePath(path string) (string, error) {
	// Persist paths with slash separators regardless of the host OS. This also
	// lets a Linux client reject Windows-style absolute paths when validating
	// data received from another platform.
	path = strings.ReplaceAll(path, `\`, "/")
	if filepath.IsAbs(path) || filepath.VolumeName(path) != "" || isWindowsAbsolute(path) {
		return "", errorsInvalidPath{}
	}
	path = pathpkg.Clean(path)
	path = strings.TrimPrefix(path, "./")
	if path == "." || path == "" || path == ".." || strings.HasPrefix(path, "../") || strings.HasPrefix(path, "/") {
		return "", errorsInvalidPath{}
	}
	return path, nil
}

func isWindowsAbsolute(path string) bool {
	return len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && path[2] == '/'
}

type errorsInvalidPath struct{}
func (errorsInvalidPath) Error() string { return "invalid relative path" }
