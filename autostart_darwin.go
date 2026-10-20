//go:build darwin

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

// autostartSupported macOS 支持子命令注册（launchd LaunchAgent）。
func autostartSupported() bool { return true }

// launchAgentDir 返回 LaunchAgent 目录（~/Library/LaunchAgents）。
func launchAgentDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("定位用户主目录失败: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

// installAutostart 写入 plist 并 launchctl load（先 unload 忽略旧作业）。
// 采集摄像头（avfoundation）要求进程运行在 GUI 登录会话中，LaunchAgent
// 正好在用户登录时拉起；失败时 plist 已写入，重新登录后仍会加载。
func installAutostart(exe, dataDir string, extraArgs []string, dryRun bool) error {
	agentDir, err := launchAgentDir()
	if err != nil {
		return err
	}
	plistPath := filepath.Join(agentDir, autostartPlistName)
	content := launchdPlistContent(exe, dataDir, extraArgs)

	fmt.Printf("plist 文件 : %s\n", plistPath)
	if dryRun {
		fmt.Println("（dry-run，未落盘）")
		return nil
	}

	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		return fmt.Errorf("创建 LaunchAgents 目录失败: %w", err)
	}
	if err := os.WriteFile(plistPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写入 plist 失败: %w", err)
	}
	// 先卸载可能存在的旧作业（首装时无作业，忽略错误），避免改配置后不生效。
	if out, err := exec.Command("launchctl", "unload", plistPath).CombinedOutput(); err != nil {
		_ = out
	}
	if out, err := exec.Command("launchctl", "load", plistPath).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "launchctl load 失败: %v\n%s\n", err, bytes.TrimSpace(out))
		fmt.Fprintf(os.Stderr, "plist 已写入 %s，重新登录后仍会自动加载。\n", plistPath)
		return err
	}
	fmt.Println("已注册开机自启（launchd，登录时启动）并已加载。")
	fmt.Println("卸载：local-onvif-adapter uninstall")
	fmt.Println("注意：移动可执行文件或数据目录后需重新执行 install。")
	return nil
}

// uninstallAutostart 卸载并删除 LaunchAgent；未注册时幂等成功。
func uninstallAutostart(dataDir string) error {
	agentDir, err := launchAgentDir()
	if err != nil {
		return err
	}
	plistPath := filepath.Join(agentDir, autostartPlistName)
	// 作业未加载时 unload 报错属正常，忽略。
	if out, err := exec.Command("launchctl", "unload", plistPath).CombinedOutput(); err != nil {
		_ = out
	}
	if err := os.Remove(plistPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("删除 plist 失败: %w", err)
	}
	fmt.Println("已移除开机自启注册。")
	return nil
}
