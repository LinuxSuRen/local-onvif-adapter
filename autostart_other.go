//go:build !windows && !linux && !darwin

package main

import "errors"

// 其余平台（BSD 等）暂未实现 install/uninstall 子命令；参考 README「开机自启」手动配置。
func autostartSupported() bool { return false }

func installAutostart(exe, dataDir string, extraArgs []string, dryRun bool) error {
	return errors.New("install is not implemented on this platform yet, see README for manual setup")
}

func uninstallAutostart(dataDir string) error {
	return errors.New("uninstall is not implemented on this platform yet")
}
