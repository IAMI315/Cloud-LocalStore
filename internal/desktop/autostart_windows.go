//go:build windows

package desktop

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

const autoStartRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const autoStartValueName = "Cloud LocalStore"

func autoStartSupported() bool { return true }

func autoStartCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`"%s"`, exe), nil
}

func getAutoStart() (bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, autoStartRunKey, registry.QUERY_VALUE)
	if err == registry.ErrNotExist {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer key.Close()
	value, _, err := key.GetStringValue(autoStartValueName)
	if err == registry.ErrNotExist {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return value != "", nil
}

func setAutoStart(enabled bool) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, autoStartRunKey, registry.CREATE_SUB_KEY|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if !enabled {
		if err := key.DeleteValue(autoStartValueName); err != nil && err != registry.ErrNotExist {
			return err
		}
		return nil
	}
	command, err := autoStartCommand()
	if err != nil {
		return err
	}
	return key.SetStringValue(autoStartValueName, command)
}
