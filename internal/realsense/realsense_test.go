package realsense

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func TestApplyColorMap(t *testing.T) {
	w, h := 4, 1
	depth := []uint16{300, 4150, 8000, 0} // min / mid / max / 无效点
	dst := make([]byte, w*h*3)
	if err := ApplyColorMap(depth, w, h, dst, 300, 8000); err != nil {
		t.Fatalf("apply: %v", err)
	}
	near := [3]byte{dst[0], dst[1], dst[2]}
	far := [3]byte{dst[6], dst[7], dst[8]}
	invalid := [3]byte{dst[9], dst[10], dst[11]}
	if invalid != [3]byte{} {
		t.Errorf("invalid depth (0) must map to black, got %v", invalid)
	}
	// Turbo 近端偏蓝、远端偏红：远端 R 必须大于近端 R。
	if far[0] <= near[0] {
		t.Errorf("far end should be redder: near=%v far=%v", near, far)
	}
	// 长度不匹配必须报错而非 panic。
	if err := ApplyColorMap([]uint16{1, 2}, 1, 1, make([]byte, 3), 300, 8000); err == nil {
		t.Error("length mismatch should error")
	}
}

func TestTurboRGBClamped(t *testing.T) {
	for _, q := range []float64{-1, 0, 0.5, 1, 2} {
		r, g, b := TurboRGB(q)
		if r > 255 || g > 255 || b > 255 {
			t.Fatalf("q=%v overflow: %v %v %v", q, r, g, b)
		}
	}
}

func TestGrayToRGB(t *testing.T) {
	dst := make([]byte, 6)
	if err := GrayToRGB([]byte{10, 200}, 2, 1, dst); err != nil {
		t.Fatalf("gray: %v", err)
	}
	if dst[0] != 10 || dst[1] != 10 || dst[2] != 10 || dst[3] != 200 {
		t.Errorf("unexpected %v", dst)
	}
}

func TestFakeSourceFrames(t *testing.T) {
	src := NewFakeSource(Config{Width: 64, Height: 48, Framerate: 30})
	defer src.Stop()
	set, err := src.Poll()
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	n := 64 * 48
	for _, k := range []StreamKind{KindColor, KindDepth, KindIR} {
		f, ok := set[k]
		if !ok {
			t.Fatalf("missing stream %s", k)
		}
		if len(f.Data) != n*3 {
			t.Errorf("%s: data len %d want %d", k, len(f.Data), n*3)
		}
	}
	// 帧随时间变化（方块移动）：连续多次取帧，画面必然发生变化。
	changed := false
	prev := string(set[KindColor].Data)
	for i := 0; i < 5 && !changed; i++ {
		next, err := src.Poll()
		if err != nil {
			t.Fatalf("poll2: %v", err)
		}
		changed = string(next[KindColor].Data) != prev
		prev = string(next[KindColor].Data)
	}
	if !changed {
		t.Error("color frames should animate")
	}
}

func TestServerServesRawFrames(t *testing.T) {
	src := NewFakeSource(Config{Width: 32, Height: 24, Framerate: 30})
	defer src.Stop()
	srv := NewServer(src, nil)
	done := make(chan error, 1)
	// 随机高位端口组（0,1,2 是特权/保留端口，直接以 0 为 base 会绑到端口 1 失败）。
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	base := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	go func() { done <- srv.Serve(base) }()
	deadline := time.Now().Add(2 * time.Second)
	var port int
	for time.Now().Before(deadline) {
		if p, ok := srv.Port(KindColor); ok {
			port = p
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if port == 0 {
		t.Fatal("listener not ready")
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(port), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	frame := make([]byte, 32*24*3)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, frame); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	sum := 0
	for _, b := range frame {
		sum += int(b)
	}
	if sum == 0 {
		t.Error("frame is all black")
	}
	srv.Close()
	// Serve 在 Close 后退出（Poll 出错或写帧无客户端均会导致返回错误/终止；
	// 具体返回值不敏感，只要退出即可）。
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("Serve did not exit after Close")
	}
}

func TestKindValid(t *testing.T) {
	for _, k := range Kinds {
		if !k.Valid() {
			t.Errorf("kind %s should be valid", k)
		}
	}
	if StreamKind("nope").Valid() {
		t.Error("unknown kind should be invalid")
	}
}

func itoa(v int) string {
	return fmt.Sprintf("%d", v)
}
