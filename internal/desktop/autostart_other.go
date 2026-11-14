//go:build !windows

package desktop

import "errors"

var errAutoStartUnsupported = errors.New("当前平台不支持开机自动启动")

func autoStartSupported() bool    { return false }
func getAutoStart() (bool, error) { return false, errAutoStartUnsupported }
func setAutoStart(bool) error     { return errAutoStartUnsupported }
