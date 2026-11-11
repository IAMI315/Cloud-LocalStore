//go:build !windows

package desktop

func acquireSingleInstance() (func(), error) { return func() {}, nil }
