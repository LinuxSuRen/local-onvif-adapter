package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

func TestVBSContent(t *testing.T) {
	got := vbsContent(`C:\Program Files\adapter\local-onvif-adapter.exe`, `D:\my data`, []string{"-http-addr", ":1235", "-advertise-ip", "192.168.1.10"})
	// 路径含空格必须正确加引号（VBS 内双写双引号）。
	wantRun := `CreateObject("WScript.Shell").Run """C:\Program Files\adapter\local-onvif-adapter.exe"" -data-dir ""D:\my data"" -http-addr :1235 -advertise-ip 192.168.1.10", 0, False`
	if !strings.Contains(got, wantRun) {
		t.Fatalf("vbs run line mismatch:\n got: %s\nwant substring: %s", got, wantRun)
	}
	if !strings.HasSuffix(got, "\r\n") {
		t.Fatalf("vbs should use CRLF line endings: %q", got)
	}
	// wscript 按本地编码解析，内容必须纯 ASCII。
	for _, r := range got {
		if r > 0x7F {
			t.Fatalf("vbs must be ASCII-only, found %q", r)
		}
	}
	// 参数含空格时加引号。
	got = vbsContent(`/opt/a`, `/data`, []string{"-ffmpeg-bin", "/opt/my tools/ffmpeg"})
	if !strings.Contains(got, `""/opt/my tools/ffmpeg""`) {
		t.Fatalf("spaced arg not quoted: %s", got)
	}
}

func TestLaunchArgsFromConfig(t *testing.T) {
	dir := t.TempDir()
	args, err := launchArgsFromConfig(dir)
	if err != nil {
		t.Fatalf("launchArgsFromConfig: %v", err)
	}
	join := strings.Join(args, " ")
	for _, want := range []string{"-http-addr :8080", "-rtsp-server true", "-ffmpeg-bin ffmpeg"} {
		if !strings.Contains(join, want) {
			t.Fatalf("default config args missing %q in %q", want, join)
		}
	}
	if strings.Contains(join, "-advertise-ip") || strings.Contains(join, "-discovery") {
		t.Fatalf("default config should not carry optional flags: %q", join)
	}
	// RTSP 端口不得携带：#15 语义下不携带该 flag 时配置文件即权威，
	// 携带反而会把 install 时刻的端口冻结住，回滚用户后续修改。
	if strings.Contains(join, "-rtsp-port") {
		t.Fatalf("args should not freeze -rtsp-port: %q", join)
	}

	// 定制配置：非默认端口/发现关闭/固定宣告 IP/外部 mediamtx。
	dir2 := t.TempDir()
	store, err := config.LoadStore(dir2)
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	if err := store.Update(func(r *config.Root) error {
		r.Server.HTTPAddr = "127.0.0.1:1235"
		r.Server.RTSPPort = 18655
		r.Server.AdvertiseIP = "192.168.18.165"
		r.Server.Discovery = false
		r.Server.RTSPEmbedded = false
		r.Server.RTSPPushAddr = "rtsp://127.0.0.1:1932"
		return nil
	}); err != nil {
		t.Fatalf("update store: %v", err)
	}
	args, err = launchArgsFromConfig(dir2)
	if err != nil {
		t.Fatalf("launchArgsFromConfig(dir2): %v", err)
	}
	join = strings.Join(args, " ")
	for _, want := range []string{"-http-addr 127.0.0.1:1235", "-advertise-ip 192.168.18.165", "-discovery=false", "-rtsp-server false", "-rtsp-push rtsp://127.0.0.1:1932"} {
		if !strings.Contains(join, want) {
			t.Fatalf("custom config args missing %q in %q", want, join)
		}
	}
	if strings.Contains(join, "-rtsp-port") {
		t.Fatalf("args should not freeze -rtsp-port: %q", join)
	}
}

func TestRunInstallDryRun(t *testing.T) {
	dir := t.TempDir()
	code := runInstall([]string{"-data-dir", dir, "-dry-run"})
	if runtime.GOOS == "windows" {
		if code != 0 {
			t.Fatalf("install -dry-run exit = %d, want 0", code)
		}
		// dry-run 不落任何文件。
		if _, err := os.Stat(filepath.Join(dir, autostartVBSName)); !os.IsNotExist(err) {
			t.Fatalf("dry-run should not write vbs: %v", err)
		}
		return
	}
	// 非 Windows：入口即判平台支持，dry-run 也应失败退出（无误导性预览）。
	if code != 1 {
		t.Fatalf("install -dry-run exit = %d on %s, want 1", code, runtime.GOOS)
	}
}
