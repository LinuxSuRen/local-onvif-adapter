//go:build !windows

package main

import "errors"

// 非 Windows 平台暂未实现 install/uninstall 子命令；
// Linux（systemd 用户单元）与 macOS（launchd）的手动配置见 README「开机自启」。
func registerAutostart(string) error {
	return errors.New("install is not implemented on this platform yet, see README for manual setup")
}

func unregisterAutostart() error {
	return errors.New("uninstall is not implemented on this platform yet")
}

func autostartRegistered() (string, bool) { return "", false }

func autostartSupported() bool { return false }
