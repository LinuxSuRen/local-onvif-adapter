// Package api 提供管理 REST API、快照下载与前端静态资源托管。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
	"github.com/linuxsuren/local-onvif-adapter/internal/onvifserver"
	"github.com/linuxsuren/local-onvif-adapter/internal/ptzmock"
	"github.com/linuxsuren/local-onvif-adapter/internal/snapshot"
	"github.com/linuxsuren/local-onvif-adapter/internal/stream"
	"github.com/linuxsuren/local-onvif-adapter/web"
)

// Server 聚合管理面的全部依赖。
type Server struct {
	store   *config.Store
	ptzReg  *ptzmock.Registry
	streams *stream.Manager
	snaps   *snapshot.Generator
	onvif   *onvifserver.Service
	version string
	logger  *slog.Logger
}

// NewServer 创建管理服务。
func NewServer(store *config.Store, ptzReg *ptzmock.Registry, streams *stream.Manager,
	snaps *snapshot.Generator, onvif *onvifserver.Service, version string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		store: store, ptzReg: ptzReg, streams: streams, snaps: snaps,
		onvif: onvif, version: version, logger: logger,
	}
}

// Handler 返回完整 HTTP 处理器（API + 快照 + UI）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeData(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /api/system", s.handleSystem)

	mux.HandleFunc("GET /api/cameras", s.handleListCameras)
	mux.HandleFunc("POST /api/cameras", s.handleCreateCamera)
	mux.HandleFunc("PUT /api/cameras/{id}", s.handleUpdateCamera)
	mux.HandleFunc("DELETE /api/cameras/{id}", s.handleDeleteCamera)
	mux.HandleFunc("GET /api/cameras/{id}/snapshot", s.handleCameraSnapshot)
	mux.HandleFunc("GET /api/cameras/{id}/ptz", s.handleGetPTZ)
	mux.HandleFunc("POST /api/cameras/{id}/ptz", s.handlePostPTZ)

	// ONVIF 快照路由由根 mux 直接注册（HandleOnvifSnapshot），
	// 以获得比 /onvif/ SOAP handler 更高的匹配优先级。

	// 本地摄像头设备枚举（跨平台：内置/USB 摄像头）。
	mux.HandleFunc("GET /api/devices", s.handleListDevices)

	// 前端静态资源。
	staticHandler := spaHandler()
	mux.Handle("/", staticHandler)
	mux.Handle("/assets/", staticHandler)

	return logMiddleware(mux, s.logger)
}

// ---- 系统信息 ----

func (s *Server) handleSystem(w http.ResponseWriter, _ *http.Request) {
	root := s.store.Root()
	uris := s.onvif.URIs()
	enabled := 0
	for _, c := range root.Cameras {
		if c.Enabled {
			enabled++
		}
	}
	writeData(w, http.StatusOK, map[string]any{
		"version":           s.version,
		"advertise_ip":      uris.AdvertiseIP,
		"http_port":         uris.HTTPPort,
		"rtsp_port":         uris.RTSPPort,
		"onvif_endpoint":    uris.XAddr(),
		"discovery_enabled": root.Server.Discovery,
		"camera_count":      len(root.Cameras),
		"profile_count":     enabled,
	})
}

// ---- 摄像头 CRUD ----

type cameraStatus struct {
	Running       bool   `json:"running"`
	ProfileToken  string `json:"profile_token"`
	RestreamURL   string `json:"restream_url"`
	SnapshotURL   string `json:"snapshot_url"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	LastError     string `json:"last_error"`
}

type cameraResp struct {
	config.Camera
	Status *cameraStatus `json:"status"`
}

func (s *Server) cameraResponse(cam config.Camera) cameraResp {
	resp := cameraResp{Camera: cam}
	if !cam.Enabled {
		return resp
	}
	uris := s.onvif.URIs()
	st := &cameraStatus{
		ProfileToken: "profile_" + cam.ID,
		RestreamURL:  uris.StreamURI(cam.ID),
		SnapshotURL:  "/api/cameras/" + cam.ID + "/snapshot",
	}
	if run, ok := s.streams.Status(cam.ID); ok {
		st.Running = run.Running
		st.LastError = run.LastError
		if run.Running && !run.StartedAt.IsZero() {
			st.UptimeSeconds = int64(time.Since(run.StartedAt).Seconds())
		}
	}
	resp.Status = st
	return resp
}

func (s *Server) handleListCameras(w http.ResponseWriter, _ *http.Request) {
	root := s.store.Root()
	out := make([]cameraResp, 0, len(root.Cameras))
	for _, cam := range root.Cameras {
		out = append(out, s.cameraResponse(cam))
	}
	writeData(w, http.StatusOK, out)
}

type cameraInput struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Source      string `json:"source"`
	Width       *int   `json:"width"`
	Height      *int   `json:"height"`
	Framerate   *int   `json:"framerate"`
	BitrateKBPS *int   `json:"bitrate_kbps"`
	Infrared    *bool  `json:"infrared"`
	Enabled     *bool  `json:"enabled"`
}

func (in *cameraInput) validate() error {
	if strings.TrimSpace(in.Name) == "" {
		return badRequest("名称不能为空")
	}
	t := config.CameraType(in.Type)
	if !t.Valid() {
		return badRequest("不支持的摄像头类型：" + in.Type)
	}
	if t == config.TypeRTSP && !strings.HasPrefix(in.Source, "rtsp://") {
		return badRequest("RTSP 源必须以 rtsp:// 开头")
	}
	if t != config.TypeTestSrc && strings.TrimSpace(in.Source) == "" {
		return badRequest("取流源不能为空")
	}
	checkRange := func(name string, v, min, max int) error {
		if v < min || v > max {
			return badRequest(fmt.Sprintf("%s取值范围 %d-%d", name, min, max))
		}
		return nil
	}
	if in.Width != nil {
		if err := checkRange("宽度", *in.Width, 0, 3840); err != nil {
			return err
		}
	}
	if in.Height != nil {
		if err := checkRange("高度", *in.Height, 0, 2160); err != nil {
			return err
		}
	}
	if in.Framerate != nil {
		if err := checkRange("帧率", *in.Framerate, 1, 60); err != nil {
			return err
		}
	}
	if in.BitrateKBPS != nil {
		if err := checkRange("码率", *in.BitrateKBPS, 256, 8192); err != nil {
			return err
		}
	}
	return nil
}

func badRequest(msg string) error {
	return &apiError{status: http.StatusBadRequest, code: "invalid_argument", message: msg}
}

type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func (s *Server) handleCreateCamera(w http.ResponseWriter, r *http.Request) {
	var in cameraInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := in.validate(); err != nil {
		s.writeAPIError(w, err)
		return
	}
	var created config.Camera
	err := s.store.Update(func(root *config.Root) error {
		if len(root.Cameras) >= 16 {
			return badRequest("最多支持 16 个摄像头")
		}
		id := fmt.Sprintf("cam%d", root.NextID)
		root.NextID++
		created = config.Camera{
			ID: id, Name: strings.TrimSpace(in.Name), Type: config.CameraType(in.Type),
			Source:  strings.TrimSpace(in.Source),
			Enabled: true,
		}
		applyInput(&created, &in)
		root.Cameras = append(root.Cameras, created)
		return nil
	})
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	s.logger.Info("camera created", "id", created.ID, "name", created.Name, "type", created.Type)
	s.syncStreams()
	writeData(w, http.StatusOK, s.cameraResponse(created))
}

func (s *Server) handleUpdateCamera(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in cameraInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := in.validate(); err != nil {
		s.writeAPIError(w, err)
		return
	}
	var updated config.Camera
	err := s.store.Update(func(root *config.Root) error {
		for i := range root.Cameras {
			if root.Cameras[i].ID != id {
				continue
			}
			root.Cameras[i].Name = strings.TrimSpace(in.Name)
			root.Cameras[i].Type = config.CameraType(in.Type)
			root.Cameras[i].Source = strings.TrimSpace(in.Source)
			applyInput(&root.Cameras[i], &in)
			updated = root.Cameras[i]
			return nil
		}
		return &apiError{status: http.StatusNotFound, code: "not_found", message: "摄像头不存在"}
	})
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	s.logger.Info("camera updated", "id", updated.ID)
	s.syncStreams()
	writeData(w, http.StatusOK, s.cameraResponse(updated))
}

func applyInput(cam *config.Camera, in *cameraInput) {
	if in.Width != nil {
		cam.Width = *in.Width
	}
	if in.Height != nil {
		cam.Height = *in.Height
	}
	if in.Framerate != nil {
		cam.Framerate = *in.Framerate
	}
	if in.BitrateKBPS != nil {
		cam.BitrateKBPS = *in.BitrateKBPS
	}
	if in.Infrared != nil {
		cam.Infrared = *in.Infrared
	}
	if in.Enabled != nil {
		cam.Enabled = *in.Enabled
	}
}

func (s *Server) handleDeleteCamera(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.store.Update(func(root *config.Root) error {
		for i := range root.Cameras {
			if root.Cameras[i].ID == id {
				root.Cameras = append(root.Cameras[:i], root.Cameras[i+1:]...)
				return nil
			}
		}
		return &apiError{status: http.StatusNotFound, code: "not_found", message: "摄像头不存在"}
	})
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	s.ptzReg.Remove(id)
	s.logger.Info("camera deleted", "id", id)
	s.syncStreams()
	writeData(w, http.StatusOK, true)
}

// syncStreams 将取流进程对齐到最新配置。
func (s *Server) syncStreams() {
	root := s.store.Root()
	s.streams.Sync(root.Cameras)
}

// handleListDevices 枚举本机视频设备（内置/USB 摄像头，跨平台）。
// 响应附带当前平台的本地摄像头类型，供 UI 手动输入时兜底。
func (s *Server) handleListDevices(w http.ResponseWriter, _ *http.Request) {
	bin := s.store.Root().Server.FFmpegBin
	devices, err := stream.ListLocalVideoDevices(bin)
	if err != nil {
		if errors.Is(err, stream.ErrUnsupportedPlatform) {
			writeError(w, http.StatusBadRequest, "unsupported_platform",
				"当前平台不支持本地设备枚举，请使用 RTSP 拉流或测试彩条")
			return
		}
		s.logger.Warn("device enumerate failed", "err", err.Error())
		writeError(w, http.StatusBadGateway, "device_enum_failed",
			"枚举设备失败，请确认 ffmpeg 已安装且可正常执行")
		return
	}
	if devices == nil {
		devices = []stream.Device{}
	}
	writeData(w, http.StatusOK, map[string]any{
		"devices":      devices,
		"default_type": stream.DefaultLocalType(),
	})
}

// ---- 快照 ----

func (s *Server) handleCameraSnapshot(w http.ResponseWriter, r *http.Request) {
	cam, ok := s.lookupCamera(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "摄像头不存在")
		return
	}
	s.writeSnapshot(w, r, cam)
}

// HandleOnvifSnapshot 处理 GetSnapshotUri 返回地址的 JPEG 下载。
// 供根 mux 以 /onvif/snapshot/ 前缀注册，匹配优先级高于 SOAP handler。
func (s *Server) HandleOnvifSnapshot(w http.ResponseWriter, r *http.Request) {
	s.handleOnvifSnapshot(w, r)
}

// handleOnvifSnapshot 处理 GetSnapshotUri 返回的地址（按 profile token 定位）。
func (s *Server) handleOnvifSnapshot(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	root := s.store.Root()
	for _, cam := range root.Cameras {
		if cam.Enabled && "profile_"+cam.ID == token {
			s.writeSnapshot(w, r, cam)
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "profile 不存在或未启用")
}

func (s *Server) writeSnapshot(w http.ResponseWriter, _ *http.Request, cam config.Camera) {
	data, err := s.snaps.JPEG(cam)
	if err != nil {
		s.logger.Warn("snapshot failed", "camera", cam.ID, "err", err.Error())
		writeError(w, http.StatusBadGateway, "snapshot_failed",
			"抓取画面失败，请检查摄像头源是否可用（ ffmpeg 是否安装、设备是否被占用 ）")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) lookupCamera(id string) (config.Camera, bool) {
	root := s.store.Root()
	for _, cam := range root.Cameras {
		if cam.ID == id {
			return cam, true
		}
	}
	return config.Camera{}, false
}

// ---- PTZ 试控 ----

type ptzStatusResp struct {
	Pan     float64          `json:"pan"`
	Tilt    float64          `json:"tilt"`
	Zoom    float64          `json:"zoom"`
	Moving  bool             `json:"moving"`
	Presets []ptzmock.Preset `json:"presets"`
}

func (s *Server) ptzStatus(camID string) ptzStatusResp {
	node := s.ptzReg.Get(camID)
	pan, tilt, zoom, moving := node.Status()
	return ptzStatusResp{Pan: pan, Tilt: tilt, Zoom: zoom, Moving: moving, Presets: node.Presets()}
}

func (s *Server) handleGetPTZ(w http.ResponseWriter, r *http.Request) {
	cam, ok := s.lookupCamera(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "摄像头不存在")
		return
	}
	writeData(w, http.StatusOK, s.ptzStatus(cam.ID))
}

type ptzInput struct {
	Op          string   `json:"op"`
	Pan         *float64 `json:"pan"`
	Tilt        *float64 `json:"tilt"`
	Zoom        *float64 `json:"zoom"`
	PresetToken string   `json:"preset_token"`
	PresetName  string   `json:"preset_name"`
}

func (in *ptzInput) vec() (pan, tilt, zoom float64) {
	if in.Pan != nil {
		pan = *in.Pan
	}
	if in.Tilt != nil {
		tilt = *in.Tilt
	}
	if in.Zoom != nil {
		zoom = *in.Zoom
	}
	return
}

func (s *Server) handlePostPTZ(w http.ResponseWriter, r *http.Request) {
	cam, ok := s.lookupCamera(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "摄像头不存在")
		return
	}
	var in ptzInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	node := s.ptzReg.Get(cam.ID)
	pan, tilt, zoom := in.vec()
	switch in.Op {
	case "continuous":
		node.ContinuousMove(pan, tilt, zoom)
	case "stop":
		node.Stop(true, true)
	case "absolute":
		node.AbsoluteMove(pan, tilt, zoom)
	case "preset_set":
		name := in.PresetName
		if name == "" {
			name = time.Now().Format("预置位 15:04:05")
		}
		writeData(w, http.StatusOK, map[string]string{"preset_token": node.SetPreset(name)})
		return
	case "preset_goto":
		if !node.GotoPreset(in.PresetToken) {
			writeError(w, http.StatusNotFound, "preset_not_found", "预置位不存在")
			return
		}
	case "preset_remove":
		if !node.RemovePreset(in.PresetToken) {
			writeError(w, http.StatusNotFound, "preset_not_found", "预置位不存在")
			return
		}
	case "home_set":
		node.SetHomePosition()
	case "home_goto":
		node.GotoHomePosition()
	default:
		writeError(w, http.StatusBadRequest, "invalid_op", "不支持的云台操作："+in.Op)
		return
	}
	writeData(w, http.StatusOK, s.ptzStatus(cam.ID))
}

// ---- 通用 ----

func decodeJSON(r *http.Request, v interface{}) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(body) == 0 {
		return errors.New("请求体不能为空")
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("解析请求体: %w", err)
	}
	return nil
}

func writeData(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": v})
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func (s *Server) writeAPIError(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeError(w, ae.status, ae.code, ae.message)
		return
	}
	s.logger.Error("api internal error", "err", err.Error())
	writeError(w, http.StatusInternalServerError, "internal", "服务内部错误，请查看日志")
}

// spaHandler 托管前端静态资源；未命中的路径回退到 index.html（SPA 单页）。
func spaHandler() http.Handler {
	fileServer := http.FileServerFS(web.Dist())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(web.Dist(), path); err != nil {
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})
}

func logMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		// 管理面与快照请求频率较高，统一 debug 级别。
		logger.Debug("http", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start).String())
	})
}
