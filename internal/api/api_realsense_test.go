package api

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

// TestRealsenseDevicesEndpoint 设备枚举：正常解析、二进制缺失不报错（引导配置）。
func TestRealsenseDevicesEndpoint(t *testing.T) {
	_, ts := newTestServer(t)

	orig := listRealsenseDevicesFn
	defer func() { listRealsenseDevicesFn = orig }()

	// 正常路径：rs-pipe list 输出 "serial\tmodel"。
	listRealsenseDevicesFn = func(string) ([]byte, error) {
		return []byte("348522076218\tRealSense D435\n"), nil
	}
	code, out := doJSON(t, "GET", ts.URL+"/api/realsense/devices", "")
	if code != 200 {
		t.Fatalf("status: %d", code)
	}
	data := out["data"].(map[string]any)
	if data["bin_available"] != true {
		t.Fatalf("bin_available: %v", data["bin_available"])
	}
	devs := data["devices"].([]any)
	if len(devs) != 1 {
		t.Fatalf("devices: %v", devs)
	}
	d := devs[0].(map[string]any)
	if d["serial"] != "348522076218" || d["name"] != "RealSense D435" {
		t.Fatalf("device: %v", d)
	}

	// 二进制缺失：仍 200，bin_available=false + hint（前端引导配置而非报错）。
	listRealsenseDevicesFn = func(string) ([]byte, error) {
		return nil, &exec.Error{Name: "rs-pipe", Err: exec.ErrNotFound}
	}
	code, out = doJSON(t, "GET", ts.URL+"/api/realsense/devices", "")
	if code != 200 {
		t.Fatalf("missing-bin status: %d", code)
	}
	data = out["data"].(map[string]any)
	if data["bin_available"] != false {
		t.Fatalf("missing bin_available: %v", data)
	}
	if hint, _ := data["hint"].(string); !strings.Contains(hint, "rs-pipe") {
		t.Fatalf("hint: %v", data["hint"])
	}
}

// TestRealsenseSettingsRoundtrip 设置读写：保存路径后 GET 能读回。
func TestRealsenseSettingsRoundtrip(t *testing.T) {
	srv, ts := newTestServer(t)
	code, out := doJSON(t, "PUT", ts.URL+"/api/realsense/settings",
		`{"realsense_bin":"D:/tools/rs-pipe.exe"}`)
	if code != 200 {
		t.Fatalf("put status: %d body: %v", code, out)
	}
	if got := srv.store.Root().Server.RealsenseBin; got != "D:/tools/rs-pipe.exe" {
		t.Fatalf("config bin: %q", got)
	}
	code, out = doJSON(t, "GET", ts.URL+"/api/realsense/settings", "")
	if code != 200 {
		t.Fatalf("get status: %d", code)
	}
	data := out["data"].(map[string]any)
	if data["realsense_bin"] != "D:/tools/rs-pipe.exe" {
		t.Fatalf("get bin: %v", data["realsense_bin"])
	}
}

// TestCreateRealsenseTrio 一键创建三路：命名/源端口/几何一致、槽位校验。
func TestCreateRealsenseTrio(t *testing.T) {
	srv, ts := newTestServer(t)

	code, out := doJSON(t, "POST", ts.URL+"/api/realsense/cameras",
		`{"name_prefix":"D435","width":848,"height":480,"framerate":30}`)
	if code != 201 {
		t.Fatalf("status: %d body: %v", code, out)
	}
	root := srv.store.Root()
	if len(root.Cameras) != 3 {
		t.Fatalf("cameras: %d", len(root.Cameras))
	}
	want := []struct{ name, source string }{
		{"D435-彩色", "tcp://127.0.0.1:18020"},
		{"D435-深度", "tcp://127.0.0.1:18021"},
		{"D435-红外", "tcp://127.0.0.1:18022"},
	}
	for i, w := range want {
		c := root.Cameras[i]
		if c.Name != w.name || c.Source != w.source {
			t.Fatalf("cam[%d] = %s/%s, want %s/%s", i, c.Name, c.Source, w.name, w.source)
		}
		if c.Width != 848 || c.Height != 480 || c.Framerate != 30 || !c.Enabled {
			t.Fatalf("cam[%d] geometry: %dx%d@%d enabled=%v", i, c.Width, c.Height, c.Framerate, c.Enabled)
		}
	}

	// 参数校验：宽度越界拒绝。
	if code, _ = doJSON(t, "POST", ts.URL+"/api/realsense/cameras", `{"width":4096}`); code != 400 {
		t.Fatalf("oversize width status: %d", code)
	}
}

// TestCreateRealsenseTrioSlotLimit 槽位不足时拒绝且不产生半套摄像头。
func TestCreateRealsenseTrioSlotLimit(t *testing.T) {
	srv, ts := newTestServer(t)
	if err := srv.store.Update(func(r *config.Root) error {
		for i := 0; i < 14; i++ {
			r.Cameras = append(r.Cameras, config.Camera{ID: "x", Type: config.TypeTestSrc, Enabled: false})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code, out := doJSON(t, "POST", ts.URL+"/api/realsense/cameras", `{}`)
	if code != 400 {
		t.Fatalf("status: %d body: %v", code, out)
	}
	if n := len(srv.store.Root().Cameras); n != 14 {
		t.Fatalf("cameras must stay 14, got %d", n)
	}
}
