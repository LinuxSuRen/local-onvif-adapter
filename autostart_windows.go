//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

// autostartSupported Windows 支持子命令注册。
func autostartSupported() bool { return true }

// installAutostart 写 HKCU Run 键 + data-dir 下的 VBS 启动器。
// 先写注册表，成功后再落 VBS；VBS 写失败时回滚注册，
// 避免残留指向不存在文件的注册表项。
func installAutostart(exe, dataDir string, extraArgs []string, dryRun bool) error {
	vbs := vbsContent(exe, dataDir, extraArgs)
	vbsPath := filepath.Join(dataDir, autostartVBSName)
	runValue := `wscript.exe //B //Nologo "` + vbsPath + `"`

	fmt.Printf("启动器文件 : %s\n", vbsPath)
	fmt.Printf("注册表键值 : HKCU\\%s\\%s = %s\n", autostartRunKey, autostartRunName, runValue)
	if dryRun {
		fmt.Println("（dry-run，未落盘）")
		return nil
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	if err := registerAutostart(runValue); err != nil {
		return err
	}
	// VBS 只用 ASCII：wscript 按本地编码解析脚本，避免非 ASCII 注释乱码。
	if err := os.WriteFile(vbsPath, []byte(vbs), 0o644); err != nil {
		_ = unregisterAutostart()
		return err
	}
	fmt.Println("已注册开机自启（用户登录时静默启动）。")
	fmt.Println("卸载：local-onvif-adapter uninstall")
	fmt.Println("注意：移动可执行文件或数据目录后需重新执行 install。")
	return nil
}

// uninstallAutostart 删除 Run 键值与启动器文件；值不存在视为成功（幂等）。
func uninstallAutostart(dataDir string) error {
	if err := unregisterAutostart(); err != nil {
		return err
	}
	fmt.Println("已移除开机自启注册。")
	vbsPath := filepath.Join(dataDir, autostartVBSName)
	if err := os.Remove(vbsPath); err == nil {
		fmt.Printf("已清理启动器文件: %s\n", vbsPath)
	}
	return nil
}

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
