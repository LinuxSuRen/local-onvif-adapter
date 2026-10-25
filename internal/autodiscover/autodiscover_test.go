package autodiscover

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
	"github.com/linuxsuren/local-onvif-adapter/internal/stream"
)

func fakeList(devices []stream.Device, err error) listFn {
	return func(string) ([]stream.Device, error) { return devices, err }
}

func sampleDevices() []stream.Device {
	return []stream.Device{
		{Name: "Integrated Camera", Source: "Integrated Camera", Type: "dshow", Kind: "builtin"},
		{Name: "WL24A", Source: "WL24A", Type: "dshow", Kind: "usb", AltSource: "@device_pnp_\\\\?\\usb#vid_1234"},
		{Name: "整个桌面（全部显示器拼接）", Source: "desktop", Type: "screen", Kind: "screen"},
		{Name: "显示器 1（1920×1080），主屏", Source: "desktop@0,0,1920x1080", Type: "screen", Kind: "screen"},
		{Name: "显示器 2（2560×1440）", Source: "desktop@-1945,-1480,2560x1440", Type: "screen", Kind: "screen"},
	}
}

// TestDiscoverAddsCamerasAndScreens 默认双开关：摄像头逐台添加、屏幕逐显示器
// 添加，整桌面聚合项跳过；ID 顺延且已启用。
func TestDiscoverAddsCamerasAndScreens(t *testing.T) {
	store := newStore(t)
	added := discover(store, "ffmpeg", true, true, slog.Default(), fakeList(sampleDevices(), nil))
	if len(added) != 4 { // 2 摄像头 + 2 显示器（聚合项跳过）
		t.Fatalf("added = %d, want 4: %+v", len(added), added)
	}
	root := store.Root()
	if len(root.Cameras) != 4 {
		t.Fatalf("cameras = %d, want 4", len(root.Cameras))
	}
	for _, c := range root.Cameras {
		if !c.Enabled {
			t.Fatalf("camera %s should be enabled", c.ID)
		}
	}
	// Windows 摄像头优先使用 PnP 别名精确定位物理设备。
	if root.Cameras[1].Source != "@device_pnp_\\\\?\\usb#vid_1234" {
		t.Fatalf("alt source not used: %s", root.Cameras[1].Source)
	}
	if root.Cameras[2].Source != "desktop@0,0,1920x1080" {
		t.Fatalf("aggregate screen should be skipped, first screen = %+v", root.Cameras[2])
	}
}

// TestDiscoverRespectsSwitches 两个开关独立生效：关摄像头只补屏幕，全关不枚举。
func TestDiscoverRespectsSwitches(t *testing.T) {
	store := newStore(t)
	added := discover(store, "ffmpeg", false, true, slog.Default(), fakeList(sampleDevices(), nil))
	if len(added) != 2 || added[0].Type != config.TypeScreen {
		t.Fatalf("screens-only mismatch: %+v", added)
	}

	store2 := newStore(t)
	if got := discover(store2, "ffmpeg", false, false, slog.Default(), fakeList(sampleDevices(), nil)); got != nil {
		t.Fatalf("both off should add nothing, got %+v", got)
	}
	if len(store2.Root().Cameras) != 0 {
		t.Fatal("both off should not touch cameras")
	}
}

// TestDiscoverIdempotent 幂等：已有条目（含按 PnP 别名配置的）不重复添加，
// 用户禁用/改名的条目保持原样。
func TestDiscoverIdempotent(t *testing.T) {
	store := newStore(t)
	discover(store, "ffmpeg", true, true, slog.Default(), fakeList(sampleDevices(), nil))
	// 用户禁用第一路并改名。
	if err := store.Update(func(r *config.Root) error {
		r.Cameras[0].Enabled = false
		r.Cameras[0].Name = "我的主摄"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	added := discover(store, "ffmpeg", true, true, slog.Default(), fakeList(sampleDevices(), nil))
	if len(added) != 0 {
		t.Fatalf("second run should add nothing, got %+v", added)
	}
	cam := store.Root().Cameras[0]
	if cam.Enabled || cam.Name != "我的主摄" {
		t.Fatalf("existing camera must be untouched: %+v", cam)
	}
}

// TestDiscoverAltSourceMatch 既有配置用了 PnP 别名时按别名去重，
// 不会因枚举的友好名称不同而重复添加同一物理设备。
func TestDiscoverAltSourceMatch(t *testing.T) {
	store := newStore(t)
	if err := store.Update(func(r *config.Root) error {
		r.Cameras = append(r.Cameras, config.Camera{ID: "cam1", Name: "WL24A", Type: config.TypeDShow,
			Source: "@device_pnp_\\\\?\\usb#vid_1234", Enabled: true})
		r.NextID = 2
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	added := discover(store, "ffmpeg", true, true, slog.Default(), fakeList(sampleDevices(), nil))
	integrated := false
	for _, c := range added {
		if c.Type == config.TypeDShow && c.Source == "@device_pnp_\\\\?\\usb#vid_1234" {
			t.Fatalf("usb camera should be deduped by alt source, added %+v", c)
		}
		if c.Name == "Integrated Camera" {
			integrated = true // 另一台设备（不同 PnP 别名）不受影响，应正常添加
		}
	}
	if !integrated {
		t.Fatal("Integrated Camera should still be added")
	}
}

// TestDiscoverEnumerateFailure 枚举失败不阻断：返回空且已有配置不变。
func TestDiscoverEnumerateFailure(t *testing.T) {
	store := newStore(t)
	added := discover(store, "ffmpeg", true, true, slog.Default(), fakeList(nil, errors.New("no ffmpeg")))
	if added != nil {
		t.Fatalf("enumerate failure should add nothing, got %+v", added)
	}
	if len(store.Root().Cameras) != 0 {
		t.Fatal("config must be untouched on enumerate failure")
	}
}

// TestDiscoverCameraLimit 16 路上限：到达后停止补录而不是报错。
func TestDiscoverCameraLimit(t *testing.T) {
	store := newStore(t)
	var devices []stream.Device
	for i := 0; i < 20; i++ {
		devices = append(devices, stream.Device{
			Name: "cam", Source: strings.Repeat("9", i+1), Type: "v4l2", Kind: "usb",
		})
	}
	discover(store, "ffmpeg", true, false, slog.Default(), fakeList(devices, nil))
	if got := len(store.Root().Cameras); got != maxCameras {
		t.Fatalf("cameras = %d, want %d", got, maxCameras)
	}
}

func newStore(t *testing.T) *config.Store {
	t.Helper()
	store, err := config.LoadStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}
