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

	// Mosaics 融合流管理器（main 注入；nil 时融合布局 API 返回未启用）。
	Mosaics *stream.MosaicManager

	// RTSPBaseURL 是取流服务基址（如 rtsp://127.0.0.1:8554），
	// 启用中的摄像头优先从该服务的流抓帧（设备独占问题，见 captureSnapshot）。
	RTSPBaseURL string

	// DiscoverySwitch 用于运行时启停 WS-Discovery（main 注入）。
	DiscoverySwitch DiscoverySwitcher
}

// DiscoverySwitcher 支持运行时启停 WS-Discovery 的组件。
type DiscoverySwitcher interface {
	Start()
	Stop()
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
	mux.HandleFunc("PUT /api/discovery", s.handleToggleDiscovery)
	mux.HandleFunc("PUT /api/rtsp-port", s.handleUpdateRTSPPort)
	mux.HandleFunc("PUT /api/auth", s.handleUpdateAuth)

	mux.HandleFunc("GET /api/cameras", s.handleListCameras)
	mux.HandleFunc("POST /api/cameras", s.handleCreateCamera)
	mux.HandleFunc("PUT /api/cameras/{id}", s.handleUpdateCamera)
	mux.HandleFunc("DELETE /api/cameras/{id}", s.handleDeleteCamera)
	mux.HandleFunc("GET /api/cameras/{id}/snapshot", s.handleCameraSnapshot)
	mux.HandleFunc("GET /api/cameras/{id}/ptz", s.handleGetPTZ)
	mux.HandleFunc("POST /api/cameras/{id}/ptz", s.handlePostPTZ)

	mux.HandleFunc("GET /api/mosaics", s.handleListMosaics)
	mux.HandleFunc("POST /api/mosaics", s.handleCreateMosaic)
	mux.HandleFunc("PUT /api/mosaics/{id}", s.handleUpdateMosaic)
	mux.HandleFunc("DELETE /api/mosaics/{id}", s.handleDeleteMosaic)
	mux.HandleFunc("POST /api/mosaics/{id}/push", s.handleStartMosaicPush)
	mux.HandleFunc("DELETE /api/mosaics/{id}/push/{token}", s.handleStopMosaicPush)

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
		"version":              s.version,
		"advertise_ip":         uris.AdvertiseIP,
		"http_port":            uris.HTTPPort,
		"rtsp_port":            uris.RTSPPort,
		"rtsp_port_configured": root.Server.RTSPPort,
		"onvif_endpoint":       uris.XAddr(),
		"discovery_enabled":    root.Server.Discovery,
		"auth_enabled":         root.Server.AuthEnabled,
		"camera_count":         len(root.Cameras),
		"profile_count":        enabled,
		"mosaic_count":         len(root.Mosaics),
	})
}

// handleUpdateRTSPPort 修改 RTSP 端口：持久化到配置文件，重启服务后生效。
// 当前实际监听端口（含端口漂移）不受影响；两者不一致时 UI 提示待重启。
func (s *Server) handleUpdateRTSPPort(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RTSPPort int `json:"rtsp_port"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.RTSPPort < 1 || req.RTSPPort > 65535 {
		writeError(w, http.StatusBadRequest, "invalid_argument", "RTSP 端口取值范围 1-65535")
		return
	}
	if err := s.store.Update(func(root *config.Root) error {
		root.Server.RTSPPort = req.RTSPPort
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "save_failed", "保存配置失败")
		return
	}
	restartRequired := req.RTSPPort != s.onvif.URIs().RTSPPort
	s.logger.Info("rtsp port updated", "port", req.RTSPPort, "restart_required", restartRequired)
	writeData(w, http.StatusOK, map[string]any{
		"rtsp_port":        req.RTSPPort,
		"restart_required": restartRequired,
	})
}

// handleUpdateAuth 更新设备面认证（ONVIF SOAP + RTSP 共用账号）。
// 变更后重启全部取流进程：ffmpeg 推流地址需要按新凭证重建 userinfo。
func (s *Server) handleUpdateAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled  bool   `json:"enabled"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Enabled && (req.Username == "" || req.Password == "") {
		writeError(w, http.StatusBadRequest, "invalid_args", "开启认证需要填写用户名和密码")
		return
	}
	if err := s.store.Update(func(root *config.Root) error {
		root.Server.AuthEnabled = req.Enabled
		root.Server.AuthUser = req.Username
		root.Server.AuthPass = req.Password
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "save_failed", "保存配置失败")
		return
	}
	// 推流地址携带的 userinfo 随凭证变化，重启取流与融合流进程以新地址重推
	//（融合流的画面中继同样从本地回环流拉取）。
	s.syncStreams()
	s.logger.Info("device auth updated", "enabled", req.Enabled)
	writeData(w, http.StatusOK, map[string]any{"enabled": req.Enabled})
}

func (s *Server) handleToggleDiscovery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := s.store.Update(func(root *config.Root) error {
		root.Server.Discovery = req.Enabled
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "save_failed", "保存配置失败")
		return
	}
	if s.DiscoverySwitch != nil {
		if req.Enabled {
			s.DiscoverySwitch.Start()
		} else {
			s.DiscoverySwitch.Stop()
		}
	}
	s.logger.Info("ws-discovery toggled", "enabled", req.Enabled)
	writeData(w, http.StatusOK, map[string]bool{"enabled": req.Enabled})
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
	// 融合布局虚拟通道：运行状态来自融合流管理器（取流管理器不接管该类型）。
	if cam.Type == config.TypeMosaic && s.Mosaics != nil {
		if run, ok := s.Mosaics.Status(cam.ID); ok {
			st.Running = run.Running
			st.LastError = run.LastError
			if run.Running && !run.StartedAt.IsZero() {
				st.UptimeSeconds = int64(time.Since(run.StartedAt).Seconds())
			}
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
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	Source       string  `json:"source"`
	Width        *int    `json:"width"`
	Height       *int    `json:"height"`
	Framerate    *int    `json:"framerate"`
	BitrateKBPS  *int    `json:"bitrate_kbps"`
	Infrared     *bool   `json:"infrared"`
	AudioEnabled *bool   `json:"audio_enabled"`
	AudioSource  *string `json:"audio_source"`
	Enabled      *bool   `json:"enabled"`
}

func (in *cameraInput) validate() error {
	if strings.TrimSpace(in.Name) == "" {
		return badRequest("名称不能为空")
	}
	t := config.CameraType(in.Type)
	if !t.Valid() {
		return badRequest("不支持的摄像头类型：" + in.Type)
	}
	// 融合布局虚拟通道由 /api/mosaics 派生维护，不允许经摄像头接口手工创建。
	if t == config.TypeMosaic {
		return badRequest("融合布局通道由融合布局页面管理，请使用 /api/mosaics")
	}
	if t == config.TypeRTSP && !strings.HasPrefix(in.Source, "rtsp://") {
		return badRequest("RTSP 源必须以 rtsp:// 开头")
	}
	// RealSense 源是 rs-pipe 分发的 rawvideo TCP 流（默认端口 18020=color/18021=depth/18022=ir）。
	if t == config.TypeRealSense && !strings.HasPrefix(in.Source, "tcp://") {
		return badRequest("RealSense 源必须以 tcp:// 开头（rs-pipe 分发地址，如 tcp://127.0.0.1:18020）")
	}
	// testsrc 与 rpicam 的源可空（rpicam 空表示 libcamera 默认摄像头）。
	if t != config.TypeTestSrc && t != config.TypeRpicam && strings.TrimSpace(in.Source) == "" {
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
			if root.Cameras[i].Type == config.TypeMosaic {
				return badRequest("融合布局通道请在融合布局页面编辑")
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
	if in.AudioEnabled != nil {
		cam.AudioEnabled = *in.AudioEnabled
	}
	if in.AudioSource != nil {
		cam.AudioSource = strings.TrimSpace(*in.AudioSource)
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
				if root.Cameras[i].Type == config.TypeMosaic {
					return badRequest("融合布局通道请在融合布局页面删除")
				}
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

// syncStreams 将取流与融合流进程对齐到最新配置。
func (s *Server) syncStreams() {
	root := s.store.Root()
	s.streams.Sync(root.Cameras)
	if s.Mosaics != nil {
		s.Mosaics.Sync(root.Mosaics, root.Cameras)
	}
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
	audioDevices := stream.ListLocalAudioDevices(bin)
	if audioDevices == nil {
		audioDevices = []stream.AudioDevice{}
	}
	writeData(w, http.StatusOK, map[string]any{
		"devices":       devices,
		"audio_devices": audioDevices,
		"default_type":  stream.DefaultLocalType(),
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

// captureSnapshot 抓取一帧 JPEG：
// 启用中的摄像头优先从取流服务拉帧 —— Windows(dshow)/Linux(v4l2) 的采集设备
// 通常只能被一个进程打开，取流进程持有设备时无法再开第二个 ffmpeg 直连采集；
// 拉流失败（如进程尚未就绪）再回退直连，禁用的摄像头直接直连。
// 融合布局虚拟通道没有直连采集源：合成进程未就绪时明确报错，不做无意义的直连。
func (s *Server) captureSnapshot(cam config.Camera) ([]byte, error) {
	if cam.Enabled && s.RTSPBaseURL != "" {
		url := strings.TrimSuffix(s.RTSPBaseURL, "/") + "/cam/" + cam.ID
		data, err := s.snaps.JPEGFromStream(cam.ID, url)
		if err == nil {
			return data, nil
		}
		s.logger.Warn("snapshot from stream failed, fallback to direct capture",
			"camera", cam.ID, "err", err.Error())
	}
	if cam.Type == config.TypeMosaic {
		return nil, errors.New("融合流尚未就绪，请稍后重试")
	}
	return s.snaps.JPEG(cam)
}

func (s *Server) writeSnapshot(w http.ResponseWriter, _ *http.Request, cam config.Camera) {
	data, err := s.captureSnapshot(cam)
	if err != nil {
		s.logger.Warn("snapshot failed", "camera", cam.ID, "err", err.Error())
		msg := "抓取画面失败，请检查摄像头源是否可用（ ffmpeg 是否安装、设备是否被占用 ）"
		if cam.Type == config.TypeMosaic {
			msg = "融合流尚未就绪，请稍后重试（合成进程启动中或已停止）"
		}
		writeError(w, http.StatusBadGateway, "snapshot_failed", msg)
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

// ---- 融合布局 CRUD ----

// 融合布局数量与画面格上限（多路解码 + 单路编码的资源边界）。
const (
	maxMosaics    = 8
	maxMosaicCell = 16
)

type mosaicStatusResp struct {
	Running       bool                     `json:"running"`
	Restarts      int                      `json:"restarts"`
	UptimeSeconds int64                    `json:"uptime_seconds"`
	LastError     string                   `json:"last_error"`
	StreamURL     string                   `json:"stream_url"`
	Cells         []stream.MosaicCellStatus `json:"cells,omitempty"`
	Pushes        []stream.MosaicPushInfo  `json:"pushes,omitempty"`
}

type mosaicResp struct {
	config.Mosaic
	Status *mosaicStatusResp `json:"status"`
}

func (s *Server) mosaicResponse(m config.Mosaic) mosaicResp {
	resp := mosaicResp{Mosaic: m}
	if !m.Enabled {
		return resp
	}
	st := &mosaicStatusResp{StreamURL: s.onvif.URIs().StreamURI(m.ID)}
	if s.Mosaics != nil {
		if run, ok := s.Mosaics.Status(m.ID); ok {
			st.Running = run.Running
			st.Restarts = run.Restarts
			st.LastError = run.LastError
			st.Cells = run.Cells
			if run.Running && !run.StartedAt.IsZero() {
				st.UptimeSeconds = int64(time.Since(run.StartedAt).Seconds())
			}
		}
		st.Pushes = s.Mosaics.MosaicPushes(m.ID)
	}
	resp.Status = st
	return resp
}

func (s *Server) handleListMosaics(w http.ResponseWriter, _ *http.Request) {
	root := s.store.Root()
	out := make([]mosaicResp, 0, len(root.Mosaics))
	for _, m := range root.Mosaics {
		out = append(out, s.mosaicResponse(m))
	}
	writeData(w, http.StatusOK, out)
}

type mosaicCellInput struct {
	CameraID string `json:"camera_id"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	W        int    `json:"w"`
	H        int    `json:"h"`
}

type mosaicInput struct {
	Name          string            `json:"name"`
	Width         *int              `json:"width"`
	Height        *int              `json:"height"`
	Framerate     *int              `json:"framerate"`
	BitrateKBPS   *int              `json:"bitrate_kbps"`
	LossPolicy    *string           `json:"loss_policy"`
	Cells         []mosaicCellInput `json:"cells"`
	AudioCameraID *string           `json:"audio_camera_id"`
	Enabled       *bool             `json:"enabled"`
}

// applyMosaicInput 校验并把输入应用到融合布局（校验依赖画布尺寸与摄像头列表，
// 必须在 store.Update 的事务草稿上执行）。坐标统一向下偶数对齐
//（yuv420 色度半采样要求），对齐后校验边界与最小格尺寸。
func applyMosaicInput(m *config.Mosaic, in *mosaicInput, root *config.Root) error {
	if strings.TrimSpace(in.Name) == "" {
		return badRequest("名称不能为空")
	}
	m.Name = strings.TrimSpace(in.Name)

	w, h := 1920, 1080
	if in.Width != nil {
		w = *in.Width
	}
	if in.Height != nil {
		h = *in.Height
	}
	if w < 64 || w > 3840 || h < 64 || h > 2160 {
		return badRequest("画布尺寸范围 64x64 至 3840x2160")
	}
	m.Width, m.Height = w&^1, h&^1

	fps := 15
	if in.Framerate != nil {
		fps = *in.Framerate
	}
	if fps < 1 || fps > 30 {
		return badRequest("帧率取值范围 1-30")
	}
	m.Framerate = fps

	br := 4096
	if in.BitrateKBPS != nil {
		br = *in.BitrateKBPS
	}
	if br < 256 || br > 8192 {
		return badRequest("码率取值范围 256-8192 kbps")
	}
	m.BitrateKBPS = br

	policy := ""
	if in.LossPolicy != nil {
		policy = strings.TrimSpace(*in.LossPolicy)
	}
	if !config.ValidLossPolicy(policy) {
		return badRequest("断流策略仅支持 blackframe 或 restart")
	}
	m.LossPolicy = policy

	if in.AudioCameraID != nil {
		id := strings.TrimSpace(*in.AudioCameraID)
		if id != "" {
			cam, ok := root.FindCamera(id)
			if !ok || cam.Type == config.TypeMosaic {
				return badRequest("音频源摄像头不存在或不可用：" + id)
			}
		}
		m.AudioCameraID = id
	}

	if len(in.Cells) > maxMosaicCell {
		return badRequest("画面格最多 16 个")
	}
	cells := make([]config.MosaicCell, 0, len(in.Cells))
	for i, c := range in.Cells {
		cam, ok := root.FindCamera(c.CameraID)
		if !ok || cam.Type == config.TypeMosaic {
			return badRequest(fmt.Sprintf("第 %d 格的摄像头不存在，融合通道不能嵌套引用", i+1))
		}
		x, y, cw, ch := c.X&^1, c.Y&^1, c.W&^1, c.H&^1
		if cw < 16 || ch < 16 {
			return badRequest(fmt.Sprintf("第 %d 格尺寸过小，最小 16x16", i+1))
		}
		if x < 0 || y < 0 || x+cw > m.Width || y+ch > m.Height {
			return badRequest(fmt.Sprintf("第 %d 格超出画布范围", i+1))
		}
		cells = append(cells, config.MosaicCell{CameraID: c.CameraID, X: x, Y: y, W: cw, H: ch})
	}
	m.Cells = cells

	if in.Enabled != nil {
		m.Enabled = *in.Enabled
	}
	return nil
}

func (s *Server) handleCreateMosaic(w http.ResponseWriter, r *http.Request) {
	var in mosaicInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	var created config.Mosaic
	err := s.store.Update(func(root *config.Root) error {
		if len(root.Mosaics) >= maxMosaics {
			return badRequest(fmt.Sprintf("最多支持 %d 个融合布局", maxMosaics))
		}
		id := fmt.Sprintf("mosaic%d", root.NextID)
		root.NextID++
		created = config.Mosaic{ID: id, Enabled: true}
		if err := applyMosaicInput(&created, &in, root); err != nil {
			return err
		}
		root.Mosaics = append(root.Mosaics, created)
		// 虚拟通道：ONVIF profile、RTSP 地址、快照全套复用摄像头机制，
		// 生命周期随融合布局维护（摄像头接口禁止手工改动）。
		root.Cameras = append(root.Cameras, created.VirtualCamera())
		return nil
	})
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	s.logger.Info("mosaic created", "id", created.ID, "name", created.Name, "cells", len(created.Cells))
	s.syncStreams()
	writeData(w, http.StatusOK, s.mosaicResponse(created))
}

func (s *Server) handleUpdateMosaic(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in mosaicInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	var updated config.Mosaic
	err := s.store.Update(func(root *config.Root) error {
		for i := range root.Mosaics {
			if root.Mosaics[i].ID != id {
				continue
			}
			if err := applyMosaicInput(&root.Mosaics[i], &in, root); err != nil {
				return err
			}
			updated = root.Mosaics[i]
			// 同步虚拟通道投影（配置被手工编辑导致缺失时补建）。
			found := false
			for j := range root.Cameras {
				if root.Cameras[j].ID == id {
					root.Cameras[j] = updated.VirtualCamera()
					found = true
					break
				}
			}
			if !found {
				root.Cameras = append(root.Cameras, updated.VirtualCamera())
			}
			return nil
		}
		return &apiError{status: http.StatusNotFound, code: "not_found", message: "融合布局不存在"}
	})
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	s.logger.Info("mosaic updated", "id", updated.ID, "cells", len(updated.Cells))
	s.syncStreams()
	writeData(w, http.StatusOK, s.mosaicResponse(updated))
}

func (s *Server) handleDeleteMosaic(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.store.Update(func(root *config.Root) error {
		for i := range root.Mosaics {
			if root.Mosaics[i].ID != id {
				continue
			}
			root.Mosaics = append(root.Mosaics[:i], root.Mosaics[i+1:]...)
			for j := range root.Cameras {
				if root.Cameras[j].ID == id && root.Cameras[j].Type == config.TypeMosaic {
					root.Cameras = append(root.Cameras[:j], root.Cameras[j+1:]...)
					break
				}
			}
			return nil
		}
		return &apiError{status: http.StatusNotFound, code: "not_found", message: "融合布局不存在"}
	})
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	if s.Mosaics != nil {
		s.Mosaics.Forget(id)
	}
	s.ptzReg.Remove(id)
	s.logger.Info("mosaic deleted", "id", id)
	s.syncStreams()
	writeData(w, http.StatusOK, true)
}

// handleStartMosaicPush 创建一路融合流直推会话（rtsp/rtmp/srt 目标）。
func (s *Server) handleStartMosaicPush(w http.ResponseWriter, r *http.Request) {
	if s.Mosaics == nil {
		writeError(w, http.StatusServiceUnavailable, "mosaic_disabled", "融合流管理器未启用")
		return
	}
	var req struct {
		Target string `json:"target"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	token, _, err := s.Mosaics.StartMosaicPush(r.PathValue("id"), req.Target)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	writeData(w, http.StatusOK, map[string]string{"token": token})
}

func (s *Server) handleStopMosaicPush(w http.ResponseWriter, r *http.Request) {
	if s.Mosaics == nil {
		writeError(w, http.StatusServiceUnavailable, "mosaic_disabled", "融合流管理器未启用")
		return
	}
	if err := s.Mosaics.StopMosaicPush(r.PathValue("token")); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "推流会话不存在")
		return
	}
	writeData(w, http.StatusOK, true)
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
// 缓存策略：index.html 每次回源校验（它引用的带内容哈希的资产文件名会随构建变化），
// assets/ 长缓存不可变——否则浏览器驻留旧 bundle，服务升级后页面迟迟不更新，
// 表现为"新版功能不见了"（实测踩坑：融合布局版块在新版中已存在，用户浏览器
// 仍在跑旧代码）。
func spaHandler() http.Handler {
	fileServer := http.FileServerFS(web.Dist())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(web.Dist(), path); err != nil {
			// 旧构建的资产已不存在时明确 404（回退成 index.html 会让浏览器
			// 把 HTML 当 JS 解析，整页白屏）；SPA 路由仍回退 index.html。
			if strings.HasPrefix(path, "assets/") {
				http.NotFound(w, r)
				return
			}
			r.URL.Path = "/"
			path = "index.html"
		}
		if strings.HasPrefix(path, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
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
