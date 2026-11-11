//go:build !windows

package gui

func selectFolder() (string, error) {
	return "", ErrFolderSelectionUnsupported
}
