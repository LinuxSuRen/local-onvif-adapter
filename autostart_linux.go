//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// autostartSupported Linux 支持子命令注册（systemd 用户单元）。
func autostartSupported() bool { return true }

// systemdUserDir 返回 systemd 用户单元目录（$XDG_CONFIG_HOME/systemd/user，
// 未设置时为 ~/.config/systemd/user）。
func systemdUserDir() (string, error) {
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("定位用户配置目录失败: %w", err)
	}
	return filepath.Join(cfgDir, "systemd", "user"), nil
}

// runSystemctl 封装 systemctl --user 调用，输出随错误一并返回便于诊断。
func runSystemctl(args ...string) error {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %w\n%s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// installAutostart 写入 systemd 用户单元并 enable --now；
// 再尝试 loginctl enable-linger，让服务在开机后无需登录即启动
//（v4l2/rtsp 源不依赖图形会话，无头场景下即真正的开机自启）。
func installAutostart(exe, dataDir string, extraArgs []string, dryRun bool) error {
	unitDir, err := systemdUserDir()
	if err != nil {
		return err
	}
	unitPath := filepath.Join(unitDir, autostartUnitName)
	content := systemdUnitContent(exe, dataDir, extraArgs)

	fmt.Printf("单元文件 : %s\n", unitPath)
	if dryRun {
		fmt.Println("（dry-run，未落盘）")
		return nil
	}

	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return fmt.Errorf("创建单元目录失败: %w", err)
	}
	if err := os.WriteFile(unitPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写入单元文件失败: %w", err)
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		fmt.Fprintf(os.Stderr, "重载 systemd 失败: %v\n", err)
		fmt.Fprintf(os.Stderr, "常见原因：当前在容器/SSH 中执行（无 systemd 用户会话）。\n"+
			"单元文件已写入 %s；请在本机登录会话中执行：\n"+
			"  systemctl --user daemon-reload && systemctl --user enable --now %s\n", unitPath, autostartUnitName)
		return err
	}
	if err := runSystemctl("enable", "--now", autostartUnitName); err != nil {
		fmt.Fprintf(os.Stderr, "启用服务失败: %v\n", err)
		return err
	}
	// linger 非致命：失败只提示，不影响注册结果。
	if u, uerr := user.Current(); uerr == nil {
		if out, lerr := exec.Command("loginctl", "enable-linger", u.Username).CombinedOutput(); lerr != nil {
			fmt.Printf("提示：启用 linger 失败（%v），服务将在首次登录后启动；"+
				"如需开机即启（无头场景）请手动执行: loginctl enable-linger %s\n", lerr, u.Username)
		} else {
			fmt.Println("已启用 linger：开机后无需登录即自动启动。")
			_ = out
		}
	}
	fmt.Println("已注册开机自启（systemd 用户服务）。")
	fmt.Println("卸载：local-onvif-adapter uninstall")
	fmt.Println("注意：移动可执行文件或数据目录后需重新执行 install。")
	return nil
}

// uninstallAutostart 停用并删除 systemd 用户单元；未注册时幂等成功。
func uninstallAutostart(dataDir string) error {
	// disable --now 对未注册单元会报错（如首次 uninstall 或无用户会话），只提示不失败。
	if err := runSystemctl("disable", "--now", autostartUnitName); err != nil {
		fmt.Printf("提示：disable 服务失败（未注册或无用户会话时属正常）: %v\n", err)
	}
	unitDir, err := systemdUserDir()
	if err != nil {
		return err
	}
	unitPath := filepath.Join(unitDir, autostartUnitName)
	if err := os.Remove(unitPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("删除单元文件失败: %w", err)
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		fmt.Printf("提示：daemon-reload 失败（无用户会话时属正常）: %v\n", err)
	}
	fmt.Println("已移除开机自启注册。")
	return nil
}
