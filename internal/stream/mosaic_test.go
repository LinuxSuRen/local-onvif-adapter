package stream

import (
	"bytes"
	"strings"
	"testing"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

func TestMosaicSpec(t *testing.T) {
	cams := []config.Camera{{ID: "cam1", Type: config.TypeTestSrc, Enabled: true}}
	base := config.Mosaic{
		ID: "mosaic1", Width: 1920, Height: 1080, Framerate: 15,
		Cells: []config.MosaicCell{
			{CameraID: "cam1", X: 0, Y: 0, W: 960, H: 540},
			{CameraID: "cam2", X: 960, Y: 0, W: 960, H: 540},
		},
	}
	s1 := mosaicSpec(base, []string{"rtsp://a", "rtsp://b"}, "rtsp://127.0.0.1:8554", cams)
	// 目标顺序无关（内部排序）。
	if s2 := mosaicSpec(base, []string{"rtsp://b", "rtsp://a"}, "rtsp://127.0.0.1:8554", cams); s1 != s2 {
		t.Fatalf("target order should not affect spec:\n%s\n%s", s1, s2)
	}
	// 画面格顺序即叠放顺序，交换必须改变指纹。
	swapped := base
	swapped.Cells = []config.MosaicCell{base.Cells[1], base.Cells[0]}
	if s3 := mosaicSpec(swapped, nil, "rtsp://127.0.0.1:8554", cams); s3 == mosaicSpec(base, nil, "rtsp://127.0.0.1:8554", cams) {
		t.Fatal("cell order (z-order) must be part of spec")
	}
	// 摄像头可用性变化（禁用/删除）必须改变指纹：命令行形态随之变化。
	disabled := []config.Camera{{ID: "cam1", Type: config.TypeTestSrc, Enabled: false}}
	if s4 := mosaicSpec(base, nil, "rtsp://127.0.0.1:8554", disabled); s4 == mosaicSpec(base, nil, "rtsp://127.0.0.1:8554", cams) {
		t.Fatal("camera availability must be part of spec")
	}
	// 名称不参与指纹（改名不应重启进程）。
	renamed := base
	renamed.Name = "大厅"
	if s5 := mosaicSpec(renamed, nil, "rtsp://127.0.0.1:8554", cams); s5 != mosaicSpec(base, nil, "rtsp://127.0.0.1:8554", cams) {
		t.Fatal("name should not affect spec")
	}
}

func TestBuildMosaicArgs(t *testing.T) {
	m := config.Mosaic{
		ID: "mosaic1", Width: 1920, Height: 1080, Framerate: 15,
		Cells: []config.MosaicCell{
			{CameraID: "cam1", X: 0, Y: 0, W: 960, H: 540},
			{CameraID: "gone", X: 960, Y: 0, W: 960, H: 540},
		},
		AudioCameraID: "cam2",
	}
	cams := []config.Camera{
		{ID: "cam1", Type: config.TypeTestSrc, Enabled: true},
		{ID: "cam2", Type: config.TypeRTSP, Source: "rtsp://x", Enabled: true},
		{ID: "cam3", Type: config.TypeTestSrc, Enabled: false}, // 禁用 → 不接入
	}
	args := strings.Join(buildMosaicArgs(m, cams, "rtsp://127.0.0.1:8554",
		[]string{"rtsp://cloud:8554/live/m1"}, "ffmpeg", testLogger()), " ")

	for _, want := range []string{
		"-i rtsp://127.0.0.1:8554/cam/cam1",        // 画面输入
		"-i rtsp://127.0.0.1:8554/cam/cam2",        // 音频独立输入
		"color=c=black:s=960x540:r=15",             // 缺失格 black 兜底源
		"color=c=black:s=1920x1080:r=15[base]",     // 黑底画布
		"[0:v]scale=960:540,fps=15,setsar=1[c0]",   // 格归一
		"[1:v]scale=960:540,fps=15,setsar=1[c1]",   // 兜底源（输入 1）同样归一
		"[base][c0]overlay=x=0:y=0",                // 叠放链
		"[t0][c1]overlay=x=960:y=0",
		"-map [vout]",
		"-map 2:a?",                                 // 音频取输入 2（画面/兜底之后，可选）
		"-c:a aac",
		"-f tee",                                    // 本地 + 远端 tee 输出
		"[f=rtsp:rtsp_transport=tcp]rtsp://127.0.0.1:8554/cam/mosaic1",
		"[f=rtsp:rtsp_transport=tcp]rtsp://cloud:8554/live/m1",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "cam/cam3") {
		t.Errorf("disabled camera should not be wired: %s", args)
	}
	if strings.Contains(args, "cam/gone") {
		t.Errorf("missing camera should fall back to black source: %s", args)
	}
}

func TestBuildMosaicArgsNoAudio(t *testing.T) {
	m := config.Mosaic{ID: "mosaic1", Width: 640, Height: 480,
		Cells: []config.MosaicCell{{CameraID: "cam1", X: 0, Y: 0, W: 640, H: 480}}}
	cams := []config.Camera{{ID: "cam1", Type: config.TypeTestSrc, Enabled: true}}
	args := strings.Join(buildMosaicArgs(m, cams, "rtsp://127.0.0.1:8554", nil, "ffmpeg", testLogger()), " ")
	if !strings.Contains(args, "-an") {
		t.Fatalf("no audio camera should disable audio: %s", args)
	}
	if !strings.Contains(args, "-f rtsp -rtsp_transport tcp rtsp://127.0.0.1:8554/cam/mosaic1") {
		t.Fatalf("no targets should keep legacy single output: %s", args)
	}
	// 空画面格也应产出合法 graph（纯黑画布）。
	empty := config.Mosaic{ID: "mosaic2", Width: 640, Height: 480}
	args = strings.Join(buildMosaicArgs(empty, nil, "rtsp://127.0.0.1:8554", nil, "ffmpeg", testLogger()), " ")
	if !strings.Contains(args, "[base]format=yuv420p[vout]") {
		t.Fatalf("empty layout should compose black canvas: %s", args)
	}
}

func TestMosaicRelayAndEncoderArgs(t *testing.T) {
	relay := strings.Join(mosaicRelayArgs("rtsp://127.0.0.1:8554/cam/cam1", 960, 540, 15), " ")
	for _, want := range []string{
		"-rtsp_transport tcp -i rtsp://127.0.0.1:8554/cam/cam1",
		"-vf scale=960:540,fps=15,setsar=1,format=yuv420p",
		"-f rawvideo -pix_fmt yuv420p pipe:1",
	} {
		if !strings.Contains(relay, want) {
			t.Errorf("relay args missing %q: %s", want, relay)
		}
	}

	m := config.Mosaic{ID: "mosaic1", Width: 1920, Height: 1080, Framerate: 15, BitrateKBPS: 4096}
	enc := strings.Join(mosaicEncoderArgs(m, "", "rtsp://127.0.0.1:8554/cam/mosaic1", nil, "ffmpeg", testLogger()), " ")
	for _, want := range []string{
		"-f rawvideo -pix_fmt yuv420p -video_size 1920x1080 -framerate 15 -i pipe:0",
		"-map 0:v", "-an", "-f rtsp -rtsp_transport tcp rtsp://127.0.0.1:8554/cam/mosaic1",
	} {
		if !strings.Contains(enc, want) {
			t.Errorf("encoder args missing %q: %s", want, enc)
		}
	}
	// 音频路径：第二输入拉音频摄像头流，可选映射。
	enc = strings.Join(mosaicEncoderArgs(m, "rtsp://127.0.0.1:8554/cam/cam2",
		"rtsp://127.0.0.1:8554/cam/mosaic1", nil, "ffmpeg", testLogger()), " ")
	for _, want := range []string{
		"-rtsp_transport tcp -i rtsp://127.0.0.1:8554/cam/cam2",
		"-map 1:a?", "-c:a aac",
	} {
		if !strings.Contains(enc, want) {
			t.Errorf("encoder audio args missing %q: %s", want, enc)
		}
	}
}

func TestBlackYUV420PAndBlit(t *testing.T) {
	black := blackYUV420P(4, 2)
	if len(black) != 12 {
		t.Fatalf("black frame size: %d", len(black))
	}
	for i, b := range black {
		want := byte(128)
		if i < 8 {
			want = 16
		}
		if b != want {
			t.Fatalf("black[%d]=%d want %d", i, b, want)
		}
	}

	// 8x4 画布，4x2 帧贴到 (2,2)。
	canvas := blackYUV420P(8, 4)
	frame := make([]byte, 4*2*3/2)
	copy(frame[0:8], []byte{1, 2, 3, 4, 5, 6, 7, 8})       // Y 4x2
	copy(frame[8:10], []byte{21, 22})                      // U 2x1
	copy(frame[10:12], []byte{31, 32})                     // V 2x1
	blitYUV420P(canvas, frame, 8, 4, 2, 2, 4, 2)

	// Y 平面：第 2/3 行、列 2-5。
	wantRow2 := []byte{1, 2, 3, 4}
	wantRow3 := []byte{5, 6, 7, 8}
	if got := canvas[2*8+2 : 2*8+6]; !bytes.Equal(got, wantRow2) {
		t.Fatalf("Y row2: %v", got)
	}
	if got := canvas[3*8+2 : 3*8+6]; !bytes.Equal(got, wantRow3) {
		t.Fatalf("Y row3: %v", got)
	}
	// U 平面（画布半宽 4、半高 2）：(1,1) 起两个字节。
	ySize := 8 * 4
	if got := canvas[ySize+1*4+1 : ySize+1*4+3]; !bytes.Equal(got, []byte{21, 22}) {
		t.Fatalf("U plane: %v", got)
	}
	// V 平面：紧随 U 平面之后。
	if got := canvas[ySize+2*4+1*4+1 : ySize+2*4+1*4+3]; !bytes.Equal(got, []byte{31, 32}) {
		t.Fatalf("V plane: %v", got)
	}
	// 其余区域保持黑帧原值。
	if canvas[0] != 16 || canvas[ySize] != 128 {
		t.Fatalf("untouched area changed: %d %d", canvas[0], canvas[ySize])
	}
}

func TestManagerSyncSkipsMosaicCameras(t *testing.T) {
	mgr := NewManager(func() string { return "rtsp://127.0.0.1:8554" }, "false", testLogger())
	defer mgr.StopAll()
	mgr.Sync([]config.Camera{
		{ID: "mosaic1", Type: config.TypeMosaic, Enabled: true, Width: 640, Height: 480},
		{ID: "cam1", Type: config.TypeTestSrc, Enabled: true},
	})
	if _, ok := mgr.Status("mosaic1"); ok {
		t.Fatal("mosaic 虚拟通道不应由摄像头取流管理器接管")
	}
	if _, ok := mgr.Status("cam1"); !ok {
		t.Fatal("普通摄像头应由取流管理器接管")
	}
}

func TestMosaicRunnerFailsFastWithBadBin(t *testing.T) {
	cams := []config.Camera{{ID: "cam1", Type: config.TypeTestSrc, Enabled: true}}
	m := config.Mosaic{ID: "mosaic1", Width: 640, Height: 480, Enabled: true,
		Cells: []config.MosaicCell{{CameraID: "cam1", X: 0, Y: 0, W: 640, H: 480}}}
	// bin 不可执行：两种模式都应把启动错误带回 Run 循环（而不是 panic/挂死）。
	r := newMosaicRunner(m, cams, "rtsp://127.0.0.1:8554", nil, "false", testLogger())
	if msg := r.runRestartOnce(); msg == "" {
		t.Fatal("restart 模式：bin 启动失败应返回错误")
	}
	if msg := r.runBlackframeOnce(); msg == "" {
		t.Fatal("blackframe 模式：bin 启动失败应返回错误")
	}
}

func TestMosaicPushSessions(t *testing.T) {
	m := NewMosaicManager(func() string { return "rtsp://127.0.0.1:8554" }, "false", testLogger())
	defer m.StopAll()
	cams := []config.Camera{{ID: "cam1", Type: config.TypeTestSrc, Enabled: true}}
	mos := []config.Mosaic{{ID: "mosaic1", Name: "四分屏", Width: 1920, Height: 1080, Enabled: true,
		Cells: []config.MosaicCell{{CameraID: "cam1", X: 0, Y: 0, W: 960, H: 540}}}}
	m.Sync(mos, cams)

	// 非法目标与不存在的布局。
	if _, _, err := m.StartMosaicPush("mosaic1", "http://x/y"); err == nil {
		t.Fatal("http 目标应被拒绝")
	}
	if _, _, err := m.StartMosaicPush("mosaic9", "rtsp://cloud:8554/live/x"); err == nil {
		t.Fatal("不存在的布局应报错")
	}

	target := "rtsp://u:secret@cloud:8554/live/m1"
	tok, reused, err := m.StartMosaicPush("mosaic1", target)
	if err != nil || reused {
		t.Fatalf("create: %v reused=%v", err, reused)
	}
	// 幂等复用。
	tok2, reused2, _ := m.StartMosaicPush("mosaic1", target)
	if tok2 != tok || !reused2 {
		t.Fatalf("同 (mosaic,target) 应复用会话: %q vs %q reused=%v", tok, tok2, reused2)
	}
	// 目标进入运行指纹。
	targets := m.targetsForLocked("mosaic1")
	if len(targets) != 1 || targets[0] != target {
		t.Fatalf("targets: %v", targets)
	}
	// 快照脱敏。
	pushes := m.MosaicPushes("mosaic1")
	if len(pushes) != 1 || strings.Contains(pushes[0].Target, "secret") {
		t.Fatalf("push snapshot should be redacted: %+v", pushes)
	}
	if !strings.Contains(pushes[0].Target, ":***@") {
		t.Fatalf("push snapshot should mask password: %+v", pushes)
	}
	// 停止与重复停止。
	if err := m.StopMosaicPush(tok); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := m.StopMosaicPush(tok); err == nil {
		t.Fatal("重复停止应报错")
	}
	if got := m.targetsForLocked("mosaic1"); len(got) != 0 {
		t.Fatalf("targets after stop: %v", got)
	}
}

func TestMosaicManagerSyncLifecycle(t *testing.T) {
	m := NewMosaicManager(func() string { return "rtsp://127.0.0.1:8554" }, "false", testLogger())
	defer m.StopAll()
	cams := []config.Camera{{ID: "cam1", Type: config.TypeTestSrc, Enabled: true}}
	mos := config.Mosaic{ID: "mosaic1", Width: 640, Height: 480, Enabled: true,
		Cells: []config.MosaicCell{{CameraID: "cam1", X: 0, Y: 0, W: 640, H: 480}}}

	// 禁用 → 不启动。
	m.Sync([]config.Mosaic{{ID: mos.ID, Width: 640, Height: 480}}, cams)
	if _, ok := m.Status("mosaic1"); ok {
		t.Fatal("禁用的布局不应启动合成进程")
	}
	// 启用 → 启动。
	m.Sync([]config.Mosaic{mos}, cams)
	if _, ok := m.Status("mosaic1"); !ok {
		t.Fatal("启用的布局应启动合成进程")
	}
	// 删除 → 停止。
	m.Sync(nil, cams)
	if _, ok := m.Status("mosaic1"); ok {
		t.Fatal("删除的布局应停止合成进程")
	}
}

func TestRedactArgs(t *testing.T) {
	got := redactArgs([]string{"-i", "rtsp://u:secret@host:8554/cam/1", "libx264"})
	if strings.Contains(got, "secret") || !strings.Contains(got, ":***@") {
		t.Fatalf("args should be redacted: %s", got)
	}
}
