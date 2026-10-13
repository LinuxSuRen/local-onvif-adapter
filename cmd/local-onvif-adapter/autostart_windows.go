//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

// registerAutostart 写入 HKCU Run 键（当前用户，免管理员）。
func registerAutostart(value string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(autostartRunName, value)
}

// unregisterAutostart 删除 Run 键值；值不存在视为成功（幂等）。
func unregisterAutostart() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(autostartRunName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// autostartRegistered 读取当前注册的启动命令（未注册时第二个返回值为 false）。
func autostartRegistered() (string, bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKey, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(autostartRunName)
	if err != nil {
		return "", false
	}
	return v, true
}
