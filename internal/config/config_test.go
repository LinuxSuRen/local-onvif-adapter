package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadStoreCreatesDefault(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadStore(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	root := s.Root()
	if root.Model != "local-onvif-adapter" || root.Serial == "" || root.Server.HTTPAddr != ":8080" {
		t.Fatalf("default root: %+v", root)
	}
	// 文件已落盘。
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("config file: %v", err)
	}
}

func TestUpdatePersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadStore(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	err = s.Update(func(r *Root) error {
		r.Cameras = append(r.Cameras, Camera{ID: "cam1", Name: "n", Type: TypeV4L2, Source: "/dev/video0", Enabled: true})
		return nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	s2, err := LoadStore(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	cam, ok := s2.Root().FindCamera("cam1")
	if !ok || cam.Source != "/dev/video0" {
		t.Fatalf("reloaded camera: %+v", cam)
	}
	// 序列号保持稳定。
	if s2.Root().Serial != s.Root().Serial {
		t.Fatal("serial should persist")
	}
}

func TestUpdateErrorRollsBack(t *testing.T) {
	dir := t.TempDir()
	s, _ := LoadStore(dir)
	err := s.Update(func(r *Root) error { r.NextID = 99; return os.ErrPermission })
	if err == nil {
		t.Fatal("want error")
	}
	if s.Root().NextID == 99 {
		t.Fatal("update should roll back on error")
	}
}

func TestRootCloneIsolation(t *testing.T) {
	root := Default()
	root.Cameras = append(root.Cameras, Camera{ID: "cam1"})
	clone := root.clone()
	clone.Cameras[0].Name = "changed"
	if root.Cameras[0].Name == "changed" {
		t.Fatal("clone should not alias cameras slice")
	}
}

func TestCameraDefaults(t *testing.T) {
	c := Camera{Type: TypeTestSrc}
	if c.FramerateOrDefault() != 15 {
		t.Fatalf("framerate default: %d", c.FramerateOrDefault())
	}
	if c.BitrateOrDefault() != 2048 {
		t.Fatalf("bitrate default: %d", c.BitrateOrDefault())
	}
}

func TestCameraTypeValid(t *testing.T) {
	for _, ok := range []CameraType{TypeV4L2, TypeAVFoundation, TypeRTSP, TypeTestSrc, TypeMosaic} {
		if !ok.Valid() {
			t.Fatalf("%s should be valid", ok)
		}
	}
	if CameraType("bogus").Valid() {
		t.Fatal("bogus should be invalid")
	}
}

func TestMosaicDefaultsAndPolicy(t *testing.T) {
	m := Mosaic{ID: "mosaic1"}
	if m.FramerateOrDefault() != 15 {
		t.Fatalf("framerate default: %d", m.FramerateOrDefault())
	}
	if m.BitrateOrDefault() != 4096 {
		t.Fatalf("bitrate default: %d", m.BitrateOrDefault())
	}
	if m.EffectiveLossPolicy() != LossPolicyBlackframe {
		t.Fatalf("empty policy should default to blackframe: %q", m.EffectiveLossPolicy())
	}
	m.LossPolicy = LossPolicyRestart
	if m.EffectiveLossPolicy() != LossPolicyRestart {
		t.Fatalf("restart policy: %q", m.EffectiveLossPolicy())
	}
	if !ValidLossPolicy("") || !ValidLossPolicy(LossPolicyBlackframe) || ValidLossPolicy("bogus") {
		t.Fatal("ValidLossPolicy 分类错误")
	}
}

func TestMosaicCloneIsolation(t *testing.T) {
	root := Default()
	root.Mosaics = append(root.Mosaics, Mosaic{ID: "mosaic1",
		Cells: []MosaicCell{{CameraID: "cam1", X: 0, Y: 0, W: 640, H: 480}}})
	clone := root.clone()
	clone.Mosaics[0].Cells[0].CameraID = "cam2"
	if root.Mosaics[0].Cells[0].CameraID != "cam1" {
		t.Fatal("clone should deep-copy mosaic cells")
	}
	// 手工构造的旧配置（无 mosaics 字段）加载后归一为空列表。
	root.Mosaics = nil
	root.normalize()
	if root.Mosaics == nil {
		t.Fatal("normalize should init mosaics to empty slice")
	}
}

func TestMosaicVirtualCamera(t *testing.T) {
	m := Mosaic{ID: "mosaic1", Name: "四分屏", Width: 1920, Height: 1080, Framerate: 15, Enabled: true}
	cam := m.VirtualCamera()
	if cam.ID != m.ID || cam.Type != TypeMosaic || cam.Width != 1920 || !cam.Enabled {
		t.Fatalf("virtual camera: %+v", cam)
	}
	// 融合通道本身不能作为画面源（防递归嵌套）——由 stream 层
	// availableCameras 过滤，这里保证类型标记正确。
	root := Root{Cameras: []Camera{cam}}
	if _, ok := root.FindMosaic("mosaic1"); ok {
		t.Fatal("FindMosaic 不应把摄像头当融合布局")
	}
}
