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
	if root.Model != "onvif-local" || root.Serial == "" || root.Server.HTTPAddr != ":8080" {
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
	for _, ok := range []CameraType{TypeV4L2, TypeAVFoundation, TypeRTSP, TypeTestSrc} {
		if !ok.Valid() {
			t.Fatalf("%s should be valid", ok)
		}
	}
	if CameraType("bogus").Valid() {
		t.Fatal("bogus should be invalid")
	}
}
