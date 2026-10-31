package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
	"github.com/linuxsuren/local-onvif-adapter/internal/onvifserver"
	"github.com/linuxsuren/local-onvif-adapter/internal/ptzmock"
	"github.com/linuxsuren/local-onvif-adapter/internal/snapshot"
	"github.com/linuxsuren/local-onvif-adapter/internal/stream"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	store, err := config.LoadStore(t.TempDir())
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ptzReg := ptzmock.NewRegistry()
	streams := stream.NewManager(func() string { return "rtsp://127.0.0.1:8554" }, "false", logger)
	t.Cleanup(streams.StopAll)
	mosaics := stream.NewMosaicManager(func() string { return "rtsp://127.0.0.1:8554" }, "false", logger)
	t.Cleanup(mosaics.StopAll)
	snaps := snapshot.New("false", logger)
	onvif := onvifserver.NewService(store, ptzReg,
		onvifserver.URIs{AdvertiseIP: "192.0.2.10", HTTPPort: 8080, RTSPPort: 8554}, logger)
	srv := NewServer(store, ptzReg, streams, snaps, onvif, "test", logger)
	srv.Mosaics = mosaics
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

func doJSON(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

func TestSystemEndpoint(t *testing.T) {
	_, ts := newTestServer(t)
	code, out := doJSON(t, "GET", ts.URL+"/api/system", "")
	if code != 200 {
		t.Fatalf("status: %d", code)
	}
	data := out["data"].(map[string]any)
	if data["onvif_endpoint"] != "http://192.0.2.10:8080/onvif/device_service" {
		t.Fatalf("endpoint: %v", data["onvif_endpoint"])
	}
	if data["rtsp_port"].(float64) != 8554 {
		t.Fatalf("rtsp port: %v", data["rtsp_port"])
	}
	if data["rtsp_port_configured"].(float64) != 8554 {
		t.Fatalf("rtsp port configured: %v", data["rtsp_port_configured"])
	}
}

func TestUpdateRTSPPort(t *testing.T) {
	_, ts := newTestServer(t)
	// 合法修改：与实际监听端口不同 → 需重启生效。
	code, out := doJSON(t, "PUT", ts.URL+"/api/rtsp-port", `{"rtsp_port":9554}`)
	if code != 200 {
		t.Fatalf("update: %d %v", code, out)
	}
	data := out["data"].(map[string]any)
	if data["rtsp_port"].(float64) != 9554 {
		t.Fatalf("port: %v", data["rtsp_port"])
	}
	if !data["restart_required"].(bool) {
		t.Fatal("端口变化应提示重启生效")
	}
	// /api/system 应返回配置端口与实际端口的区分。
	code, out = doJSON(t, "GET", ts.URL+"/api/system", "")
	if code != 200 {
		t.Fatalf("system: %d", code)
	}
	data = out["data"].(map[string]any)
	if data["rtsp_port"].(float64) != 8554 {
		t.Fatalf("实际端口不应变化: %v", data["rtsp_port"])
	}
	if data["rtsp_port_configured"].(float64) != 9554 {
		t.Fatalf("配置端口: %v", data["rtsp_port_configured"])
	}
	// 改回实际端口 → 无需重启。
	code, out = doJSON(t, "PUT", ts.URL+"/api/rtsp-port", `{"rtsp_port":8554}`)
	if code != 200 {
		t.Fatalf("revert: %d %v", code, out)
	}
	if out["data"].(map[string]any)["restart_required"].(bool) {
		t.Fatal("与实际端口一致不应提示重启")
	}
	// 非法端口。
	for _, bad := range []string{`{"rtsp_port":0}`, `{"rtsp_port":-1}`, `{"rtsp_port":65536}`, `{"rtsp_port":"x"}`} {
		code, _ = doJSON(t, "PUT", ts.URL+"/api/rtsp-port", bad)
		if code != 400 {
			t.Fatalf("非法端口 %s 应返回 400，实际 %d", bad, code)
		}
	}
	// 空请求体。
	code, _ = doJSON(t, "PUT", ts.URL+"/api/rtsp-port", "")
	if code != 400 {
		t.Fatalf("空请求体应返回 400，实际 %d", code)
	}
}

func TestCameraCRUD(t *testing.T) {
	_, ts := newTestServer(t)
	// 创建。
	code, out := doJSON(t, "POST", ts.URL+"/api/cameras",
		`{"name":"前门","type":"v4l2","source":"/dev/video0","width":1280,"height":720,"framerate":15,"infrared":false,"enabled":true}`)
	if code != 200 {
		t.Fatalf("create: %d %v", code, out)
	}
	created := out["data"].(map[string]any)
	if created["id"] != "cam1" {
		t.Fatalf("id: %v", created["id"])
	}
	// 列表。
	code, out = doJSON(t, "GET", ts.URL+"/api/cameras", "")
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
	list := out["data"].([]any)
	if len(list) != 1 {
		t.Fatalf("list len: %d", len(list))
	}
	item := list[0].(map[string]any)
	if item["status"] == nil {
		t.Fatal("enabled camera should have status")
	}
	st := item["status"].(map[string]any)
	if st["restream_url"] != "rtsp://192.0.2.10:8554/cam/cam1" {
		t.Fatalf("restream url: %v", st["restream_url"])
	}
	// 更新（禁用）。
	code, out = doJSON(t, "PUT", ts.URL+"/api/cameras/cam1",
		`{"name":"前门","type":"v4l2","source":"/dev/video0","enabled":false}`)
	if code != 200 {
		t.Fatalf("update: %d %v", code, out)
	}
	if out["data"].(map[string]any)["status"] != nil {
		t.Fatal("disabled camera should have null status")
	}
	// 校验失败：rtsp 源必须 rtsp:// 开头。
	code, out = doJSON(t, "POST", ts.URL+"/api/cameras", `{"name":"x","type":"rtsp","source":"http://a"}`)
	if code != 400 {
		t.Fatalf("rtsp validation: %d %v", code, out)
	}
	// 校验失败：非法类型。
	code, _ = doJSON(t, "POST", ts.URL+"/api/cameras", `{"name":"x","type":"bogus","source":"s"}`)
	if code != 400 {
		t.Fatalf("type validation: %d", code)
	}
	// 删除。
	code, _ = doJSON(t, "DELETE", ts.URL+"/api/cameras/cam1", "")
	if code != 200 {
		t.Fatalf("delete: %d", code)
	}
	code, _ = doJSON(t, "DELETE", ts.URL+"/api/cameras/cam1", "")
	if code != 404 {
		t.Fatalf("double delete: %d", code)
	}
}

func TestPTZEndpoints(t *testing.T) {
	_, ts := newTestServer(t)
	doJSON(t, "POST", ts.URL+"/api/cameras", `{"name":"t","type":"testsrc","enabled":true}`)
	code, out := doJSON(t, "POST", ts.URL+"/api/cameras/cam1/ptz", `{"op":"continuous","pan":0.5}`)
	if code != 200 {
		t.Fatalf("continuous: %d %v", code, out)
	}
	st := out["data"].(map[string]any)
	if !st["moving"].(bool) {
		t.Fatal("should be moving")
	}
	code, out = doJSON(t, "POST", ts.URL+"/api/cameras/cam1/ptz", `{"op":"stop"}`)
	if code != 200 || out["data"].(map[string]any)["moving"].(bool) {
		t.Fatalf("stop: %d %v", code, out)
	}
	code, out = doJSON(t, "POST", ts.URL+"/api/cameras/cam1/ptz", `{"op":"absolute","pan":30,"tilt":-5,"zoom":0.2}`)
	if code != 200 {
		t.Fatalf("absolute: %d", code)
	}
	st = out["data"].(map[string]any)
	if st["pan"].(float64) != 30 {
		t.Fatalf("pan: %v", st["pan"])
	}
	// 预置位。
	code, out = doJSON(t, "POST", ts.URL+"/api/cameras/cam1/ptz", `{"op":"preset_set","preset_name":"door"}`)
	if code != 200 {
		t.Fatalf("preset set: %d %v", code, out)
	}
	token := out["data"].(map[string]any)["preset_token"].(string)
	code, out = doJSON(t, "POST", ts.URL+"/api/cameras/cam1/ptz", `{"op":"preset_goto","preset_token":"`+token+`"}`)
	if code != 200 {
		t.Fatalf("preset goto: %d", code)
	}
	code, out = doJSON(t, "GET", ts.URL+"/api/cameras/cam1/ptz", "")
	presets := out["data"].(map[string]any)["presets"].([]any)
	if len(presets) != 1 {
		t.Fatalf("presets: %v", presets)
	}
	code, _ = doJSON(t, "POST", ts.URL+"/api/cameras/cam1/ptz", `{"op":"bogus"}`)
	if code != 400 {
		t.Fatalf("bogus op: %d", code)
	}
}

func TestSnapshotEndpointFailsGracefully(t *testing.T) {
	_, ts := newTestServer(t)
	doJSON(t, "POST", ts.URL+"/api/cameras", `{"name":"t","type":"testsrc","enabled":true}`)
	resp, err := http.Get(ts.URL + "/api/cameras/cam1/snapshot")
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("snapshot with broken ffmpeg should 502, got %d", resp.StatusCode)
	}
}

func TestOnvifSnapshotRoute(t *testing.T) {
	srv, ts := newTestServer(t)
	// 按 main 的方式注册外层路由，验证优先级。
	root := http.NewServeMux()
	root.Handle("GET /onvif/snapshot/{token}", http.HandlerFunc(srv.HandleOnvifSnapshot))
	root.Handle("/onvif/", http.NotFoundHandler())
	wrapped := httptest.NewServer(root)
	defer wrapped.Close()
	doJSON(t, "POST", ts.URL+"/api/cameras", `{"name":"t","type":"testsrc","enabled":true}`)
	resp, err := http.Get(wrapped.URL + "/onvif/snapshot/profile_cam1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// ffmpeg 为 false，必然失败，但不应是 404/405（路由应命中）。
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		t.Fatalf("route should match: %d", resp.StatusCode)
	}
	// 未启用/不存在的 profile → 404。
	resp2, err := http.Get(wrapped.URL + "/onvif/snapshot/profile_none")
	if err != nil {
		t.Fatalf("get none: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown profile: %d", resp2.StatusCode)
	}
}

func TestUIStaticServed(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get ui: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("ui status: %d", resp.StatusCode)
	}
	// index.html 必须每次回源校验：否则浏览器驻留旧 bundle，升级后页面不更新。
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index.html cache-control: %q", cc)
	}
	// 不存在的资产明确 404（回退成 HTML 会被当 JS 解析导致白屏）。
	resp2, err := http.Get(ts.URL + "/assets/index-notexist.js")
	if err != nil {
		t.Fatalf("get missing asset: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("missing asset should 404, got %d", resp2.StatusCode)
	}
	// 裸克隆时 web/dist 只有占位文件（返回目录列表），构建前端后才是真实 UI；
	// 这里只保证静态托管路由本身可用，不校验 UI 内容。
}

func TestListDevicesEndpoint(t *testing.T) {
	_, ts := newTestServer(t)
	code, out := doJSON(t, "GET", ts.URL+"/api/devices", "")
	if code != http.StatusOK {
		t.Fatalf("devices endpoint: %d %v", code, out)
	}
	data := out["data"].(map[string]any)
	if _, ok := data["devices"].([]any); !ok {
		t.Fatalf("devices should be an array: %v", data)
	}
	if data["default_type"] == "" {
		t.Fatal("default_type should be non-empty")
	}
}

func TestSnapshotPrefersStreamAndFallsBack(t *testing.T) {
	srv, ts := newTestServer(t)
	// RTSP 基址指向不可达端口：流抓帧失败应回退直连（直连 ffmpeg 为 false 也失败）→ 502。
	srv.RTSPBaseURL = "rtsp://127.0.0.1:1"
	doJSON(t, "POST", ts.URL+"/api/cameras", `{"name":"t","type":"testsrc","enabled":true}`)
	resp, err := http.Get(ts.URL + "/api/cameras/cam1/snapshot")
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502 when both paths fail, got %d", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["error"].(map[string]any)["code"] != "snapshot_failed" {
		t.Fatalf("error envelope: %v", out)
	}
}

func TestHealthz(t *testing.T) {
	_, ts := newTestServer(t)
	code, out := doJSON(t, "GET", ts.URL+"/healthz", "")
	if code != 200 || out["data"].(map[string]any)["status"] != "ok" {
		t.Fatalf("healthz: %d %v", code, out)
	}
}

func TestMosaicCRUD(t *testing.T) {
	_, ts := newTestServer(t)
	// 前置：一路测试摄像头。
	doJSON(t, "POST", ts.URL+"/api/cameras", `{"name":"门口","type":"testsrc","enabled":true}`)

	// 创建融合布局（坐标奇数自动向下偶数对齐）。
	body := `{"name":"四分屏","width":1920,"height":1080,"framerate":15,"bitrate_kbps":4096,` +
		`"loss_policy":"blackframe","audio_camera_id":"cam1","enabled":true,` +
		`"cells":[{"camera_id":"cam1","x":0,"y":0,"w":961,"h":541}]}`
	code, out := doJSON(t, "POST", ts.URL+"/api/mosaics", body)
	if code != 200 {
		t.Fatalf("create mosaic: %d %v", code, out)
	}
	created := out["data"].(map[string]any)
	if created["id"] != "mosaic2" {
		t.Fatalf("mosaic id: %v", created["id"])
	}
	cells := created["cells"].([]any)
	cell := cells[0].(map[string]any)
	if cell["w"].(float64) != 960 || cell["h"].(float64) != 540 {
		t.Fatalf("cell size should be even-aligned: %v", cell)
	}
	st := created["status"].(map[string]any)
	if st["stream_url"] != "rtsp://192.0.2.10:8554/cam/mosaic2" {
		t.Fatalf("stream url: %v", st["stream_url"])
	}

	// 虚拟通道出现在摄像头列表（普通通道视角，RTSP 地址/快照全套复用）。
	code, out = doJSON(t, "GET", ts.URL+"/api/cameras", "")
	var virt map[string]any
	for _, item := range out["data"].([]any) {
		c := item.(map[string]any)
		if c["id"] == "mosaic2" {
			virt = c
		}
	}
	if virt == nil || virt["type"] != "mosaic" {
		t.Fatalf("virtual camera missing: %v", virt)
	}
	if vst := virt["status"].(map[string]any); vst["restream_url"] != "rtsp://192.0.2.10:8554/cam/mosaic2" {
		t.Fatalf("virtual camera restream url: %v", vst["restream_url"])
	}

	// 摄像头接口守卫：融合通道不能经 /api/cameras 创建/修改/删除。
	code, _ = doJSON(t, "POST", ts.URL+"/api/cameras", `{"name":"x","type":"mosaic"}`)
	if code != 400 {
		t.Fatalf("create mosaic camera should 400: %d", code)
	}
	code, _ = doJSON(t, "PUT", ts.URL+"/api/cameras/mosaic2", `{"name":"x","type":"testsrc","source":""}`)
	if code != 400 {
		t.Fatalf("update virtual camera should 400: %d", code)
	}
	code, _ = doJSON(t, "DELETE", ts.URL+"/api/cameras/mosaic2", "")
	if code != 400 {
		t.Fatalf("delete virtual camera should 400: %d", code)
	}

	// 校验：画面格引用不存在的摄像头 / 超出画布 / 非法策略。
	for _, bad := range []string{
		`{"name":"x","cells":[{"camera_id":"none","x":0,"y":0,"w":64,"h":64}]}`,
		`{"name":"x","cells":[{"camera_id":"cam1","x":1900,"y":0,"w":64,"h":64}]}`,
		`{"name":"x","cells":[{"camera_id":"cam1","x":0,"y":0,"w":8,"h":8}]}`,
		`{"name":"x","loss_policy":"bogus"}`,
		`{"name":"x","audio_camera_id":"none"}`,
		`{"name":" "}`,
	} {
		code, _ = doJSON(t, "PUT", ts.URL+"/api/mosaics/mosaic2", bad)
		if code != 400 {
			t.Fatalf("invalid mosaic body %s should 400: %d", bad, code)
		}
	}

	// 更新：改名并禁用 → status 为空，虚拟通道同步禁用。
	code, out = doJSON(t, "PUT", ts.URL+"/api/mosaics/mosaic2",
		`{"name":"大厅","width":1920,"height":1080,"enabled":false,"cells":[]}`)
	if code != 200 {
		t.Fatalf("update mosaic: %d %v", code, out)
	}
	if out["data"].(map[string]any)["status"] != nil {
		t.Fatal("禁用后 status 应为空")
	}

	// 删除 → 虚拟通道一并移除。
	code, _ = doJSON(t, "DELETE", ts.URL+"/api/mosaics/mosaic2", "")
	if code != 200 {
		t.Fatalf("delete mosaic: %d", code)
	}
	code, out = doJSON(t, "GET", ts.URL+"/api/cameras", "")
	for _, item := range out["data"].([]any) {
		if item.(map[string]any)["id"] == "mosaic2" {
			t.Fatal("virtual camera should be removed with mosaic")
		}
	}
	code, _ = doJSON(t, "DELETE", ts.URL+"/api/mosaics/mosaic2", "")
	if code != 404 {
		t.Fatalf("double delete: %d", code)
	}
}

// 无画面格的布局 cells 必须序列化为 [] 而不是 null：
// 前端模板直接读 cells.length，null 会抛 TypeError 炸掉整个面板（实测踩坑）。
func TestMosaicEmptyCellsNotNull(t *testing.T) {
	_, ts := newTestServer(t)
	code, out := doJSON(t, "POST", ts.URL+"/api/mosaics", `{"name":"空格","width":640,"height":480}`)
	if code != 200 {
		t.Fatalf("create: %d %v", code, out)
	}
	created := out["data"].(map[string]any)
	cells, ok := created["cells"].([]any)
	if !ok {
		t.Fatalf("cells 应为空数组而非 null: %v", created["cells"])
	}
	if len(cells) != 0 {
		t.Fatalf("cells 应为空: %v", cells)
	}
	// 列表接口同样不得返回 null。
	code, out = doJSON(t, "GET", ts.URL+"/api/mosaics", "")
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
	list := out["data"].([]any)
	if _, ok := list[0].(map[string]any)["cells"].([]any); !ok {
		t.Fatalf("列表 cells 应为数组: %v", list[0])
	}
}

func TestMosaicPushAPI(t *testing.T) {
	_, ts := newTestServer(t)
	doJSON(t, "POST", ts.URL+"/api/cameras", `{"name":"t","type":"testsrc","enabled":true}`)
	code, _ := doJSON(t, "POST", ts.URL+"/api/mosaics", `{"name":"融合","width":1280,"height":720}`)
	if code != 200 {
		t.Fatalf("create mosaic: %d", code)
	}
	// 非法目标。
	code, _ = doJSON(t, "POST", ts.URL+"/api/mosaics/mosaic2/push", `{"target":"http://a/b"}`)
	if code != 400 {
		t.Fatalf("http target should 400: %d", code)
	}
	// 合法目标 → token；会话出现在列表快照（脱敏）。
	code, out := doJSON(t, "POST", ts.URL+"/api/mosaics/mosaic2/push",
		`{"target":"rtsp://u:secret@cloud:8554/live/m"}`)
	if code != 200 {
		t.Fatalf("start push: %d %v", code, out)
	}
	token := out["data"].(map[string]any)["token"].(string)
	code, out = doJSON(t, "GET", ts.URL+"/api/mosaics", "")
	pushes := out["data"].([]any)[0].(map[string]any)["status"].(map[string]any)["pushes"].([]any)
	if len(pushes) != 1 {
		t.Fatalf("pushes: %v", pushes)
	}
	if tgt := pushes[0].(map[string]any)["target"].(string); strings.Contains(tgt, "secret") {
		t.Fatalf("push target should be redacted: %s", tgt)
	}
	// 停止与重复停止。
	code, _ = doJSON(t, "DELETE", ts.URL+"/api/mosaics/mosaic2/push/"+token, "")
	if code != 200 {
		t.Fatalf("stop push: %d", code)
	}
	code, _ = doJSON(t, "DELETE", ts.URL+"/api/mosaics/mosaic2/push/"+token, "")
	if code != 404 {
		t.Fatalf("double stop push: %d", code)
	}
}
