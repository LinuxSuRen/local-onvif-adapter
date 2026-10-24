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
	switch runtime.GOOS {
	case "linux":
		// 单元目录跟随 XDG_CONFIG_HOME，指向临时目录便于断言未落盘。
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
	case "darwin":
		t.Setenv("HOME", filepath.Join(dir, "home"))
	}
	code := runInstall([]string{"-data-dir", dir, "-dry-run"})
	if !autostartSupported() {
		// 不支持的平台入口即失败退出（无误导性预览）。
		if code != 1 {
			t.Fatalf("install -dry-run exit = %d on %s, want 1", code, runtime.GOOS)
		}
		return
	}
	if code != 0 {
		t.Fatalf("install -dry-run exit = %d, want 0", code)
	}
	// dry-run 不落任何文件。
	switch runtime.GOOS {
	case "windows":
		if _, err := os.Stat(filepath.Join(dir, autostartVBSName)); !os.IsNotExist(err) {
			t.Fatalf("dry-run should not write vbs: %v", err)
		}
	case "linux":
		unit := filepath.Join(dir, "xdg", "systemd", "user", autostartUnitName)
		if _, err := os.Stat(unit); !os.IsNotExist(err) {
			t.Fatalf("dry-run should not write unit: %v", err)
		}
	case "darwin":
		plist := filepath.Join(dir, "home", "Library", "LaunchAgents", autostartPlistName)
		if _, err := os.Stat(plist); !os.IsNotExist(err) {
			t.Fatalf("dry-run should not write plist: %v", err)
		}
	}
}

func TestSystemdUnitContent(t *testing.T) {
	got := systemdUnitContent("/usr/local/bin/local-onvif-adapter", "/home/rick/.local-onvif-adapter",
		[]string{"-http-addr", ":1235", "-advertise-ip", "192.168.1.10"})
	for _, want := range []string{
		"ExecStart=/usr/local/bin/local-onvif-adapter -data-dir /home/rick/.local-onvif-adapter -http-addr :1235 -advertise-ip 192.168.1.10",
		"Restart=on-failure",
		"WantedBy=default.target",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("unit content missing %q:\n%s", want, got)
		}
	}
	// 参数含空格/引号必须按 systemd 规则转义。
	got = systemdUnitContent(`/opt/my app/adapter`, `/data dir`, []string{"-ffmpeg-bin", `/opt/my tools/ffmpeg`})
	if !strings.Contains(got, `ExecStart="/opt/my app/adapter" -data-dir "/data dir" -ffmpeg-bin "/opt/my tools/ffmpeg"`) {
		t.Fatalf("spaced args not quoted:\n%s", got)
	}
}

func TestQuoteSystemdArg(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"", `""`},
		{"has space", `"has space"`},
		{`has"quote`, `"has\"quote"`},
	}
	for _, c := range cases {
		if got := quoteSystemdArg(c.in); got != c.want {
			t.Fatalf("quoteSystemdArg(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLaunchdPlistContent(t *testing.T) {
	got := launchdPlistContent("/usr/local/bin/local-onvif-adapter", "/Users/rick/.local-onvif-adapter",
		[]string{"-http-addr", ":1235"})
	for _, want := range []string{
		"<string>" + autostartLabel + "</string>",
		"<string>/usr/local/bin/local-onvif-adapter</string>",
		"<string>-data-dir</string>",
		"<string>/Users/rick/.local-onvif-adapter</string>",
		"<string>-http-addr</string>",
		"<string>:1235</string>",
		"<key>RunAtLoad</key>",
		"<key>SuccessfulExit</key>",
		"autostart.out.log",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("plist content missing %q:\n%s", want, got)
		}
	}
	// XML 保留字符必须转义（路径含 & 时 plist 才能被 launchd 解析）。
	got = launchdPlistContent("/opt/a&b/adapter", "/data", nil)
	if !strings.Contains(got, "<string>/opt/a&amp;b/adapter</string>") {
		t.Fatalf("xml chars not escaped:\n%s", got)
	}
}
