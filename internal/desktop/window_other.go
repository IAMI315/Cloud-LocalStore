//go:build !windows

package desktop

import "errors"

func showWindow(string, func()) error {
	return errors.New("native desktop window is only available on Windows")
}
func showWindowError(error) {}
