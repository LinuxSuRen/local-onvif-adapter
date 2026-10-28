// RealSense 管理面 API：设备枚举、一键创建三路摄像头、rs-pipe 配置。
//
// 设计要点：UI 不应暴露 rs-pipe 与 tcp:// 源细节——用户只需确认检测到的
// 设备、选几何参数，一键创建"彩色/深度/红外"三路摄像头条目；源端口由
// 服务端按 RealSenseBasePort 固定推导。rs-pipe 二进制路径可在设置里改。
package api

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
	"github.com/linuxsuren/local-onvif-adapter/internal/stream"
)

// listRealsenseDevicesFn 执行 rs-pipe list（测试注入用）。
var listRealsenseDevicesFn = func(bin string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, bin, "list").Output() //nolint:gosec // 路径来自受控配置
}

// realsenseDevice 枚举结果条目。
type realsenseDevice struct {
	Serial string `json:"serial"`
	Name   string `json:"name"`
}

// handleListRealsenseDevices GET /api/realsense/devices
// 始终 200：二进制缺失时 bin_available=false，前端据此引导配置而非报错。
// 并发安全：librealsense 允许枚举与流式传输并存（serve 持有设备时 list 仍可用）。
func (s *Server) handleListRealsenseDevices(w http.ResponseWriter, r *http.Request) {
	bin := strings.TrimSpace(s.store.Root().Server.RealsenseBin)
	type devResp struct {
		RealsenseBin string            `json:"realsense_bin"`
		BinAvailable bool              `json:"bin_available"`
		Devices      []realsenseDevice `json:"devices"`
		Hint         string            `json:"hint,omitempty"`
	}
	out := devResp{RealsenseBin: bin, BinAvailable: true, Devices: []realsenseDevice{}}
	if bin == "" {
		bin = "rs-pipe"
		out.RealsenseBin = bin
	}
	raw, err := listRealsenseDevicesFn(bin)
	if err != nil {
		// 常见两类：二进制不存在（需配置路径）；fake-only 构建未带 SDK。
		out.BinAvailable = false
		out.Hint = "rs-pipe 不可用：请在设置里配置采集器路径，或按 README 编译真机版（需 librealsense SDK）"
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			out.Hint = fmt.Sprintf("rs-pipe list 失败：%s", strings.TrimSpace(string(ee.Stderr)))
		}
		writeData(w, http.StatusOK, out)
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) >= 2 && fields[0] != "" {
			out.Devices = append(out.Devices, realsenseDevice{Serial: fields[0], Name: fields[1]})
		}
	}
	writeData(w, http.StatusOK, out)
}

// realsenseSettingsInput PUT /api/realsense/settings
type realsenseSettingsInput struct {
	RealsenseBin *string `json:"realsense_bin"`
}

// handleRealsenseSettings GET/PUT /api/realsense/settings
// PUT 保存采集器路径并即时生效（重配 Manager + Sync，无需重启服务）。
func (s *Server) handleRealsenseSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		srv := s.store.Root().Server
		writeData(w, http.StatusOK, map[string]any{
			"realsense_bin": srv.RealsenseBin,
			"realsense_fake": srv.RealsenseFake,
		})
		return
	}
	var in realsenseSettingsInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	var bin string
	err := s.store.Update(func(root *config.Root) error {
		if in.RealsenseBin != nil {
			root.Server.RealsenseBin = strings.TrimSpace(*in.RealsenseBin)
		}
		bin = root.Server.RealsenseBin
		return nil
	})
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	// 路径变更即时生效：Manager 重配后按现有摄像头列表重启 rs-pipe。
	s.streams.ConfigureRealsense(bin, s.store.Root().Server.RealsenseFake)
	s.streams.Sync(s.store.Root().Cameras)
	s.logger.Info("realsense settings updated", "bin", bin)
	writeData(w, http.StatusOK, map[string]any{"realsense_bin": bin})
}

// realsenseCamerasInput POST /api/realsense/cameras —— 一键创建三路。
type realsenseCamerasInput struct {
	NamePrefix *string `json:"name_prefix"`
	Width      *int    `json:"width"`
	Height     *int    `json:"height"`
	Framerate  *int    `json:"framerate"`
}

func (in *realsenseCamerasInput) validate() error {
	if in.Width != nil && (*in.Width < 320 || *in.Width > 1920) {
		return badRequest("宽度取值范围 320-1920（RealSense 支持的档位）")
	}
	if in.Height != nil && (*in.Height < 240 || *in.Height > 1080) {
		return badRequest("高度取值范围 240-1080（RealSense 支持的档位）")
	}
	if in.Framerate != nil && (*in.Framerate < 1 || *in.Framerate > 60) {
		return badRequest("帧率取值范围 1-60")
	}
	return nil
}

// handleCreateRealsenseCameras POST /api/realsense/cameras
// 创建三路摄像头（彩色/深度伪彩色/红外），源端口固定：
// base+0=彩色、base+1=深度、base+2=红外。三路几何一致（rs-pipe 统一启用）。
func (s *Server) handleCreateRealsenseCameras(w http.ResponseWriter, r *http.Request) {
	var in realsenseCamerasInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := in.validate(); err != nil {
		s.writeAPIError(w, err)
		return
	}
	prefix := "RealSense"
	if in.NamePrefix != nil && strings.TrimSpace(*in.NamePrefix) != "" {
		prefix = strings.TrimSpace(*in.NamePrefix)
	}
	width, height, fps := 640, 480, 15
	if in.Width != nil {
		width = *in.Width
	}
	if in.Height != nil {
		height = *in.Height
	}
	if in.Framerate != nil {
		fps = *in.Framerate
	}
	kinds := []struct{ suffix, source string }{
		{"彩色", fmt.Sprintf("tcp://127.0.0.1:%d", stream.RealSenseBasePort)},
		{"深度", fmt.Sprintf("tcp://127.0.0.1:%d", stream.RealSenseBasePort+1)},
		{"红外", fmt.Sprintf("tcp://127.0.0.1:%d", stream.RealSenseBasePort+2)},
	}
	var created []config.Camera
	err := s.store.Update(func(root *config.Root) error {
		if n := len(root.Cameras); n+3 > 16 {
			return badRequest(fmt.Sprintf("添加三路需要 3 个槽位，当前已有 %d/16 个摄像头", n))
		}
		for _, k := range kinds {
			id := fmt.Sprintf("cam%d", root.NextID)
			root.NextID++
			cam := config.Camera{
				ID: id, Name: prefix + "-" + k.suffix, Type: config.TypeRealSense,
				Source: k.source, Width: width, Height: height, Framerate: fps, Enabled: true,
			}
			root.Cameras = append(root.Cameras, cam)
			created = append(created, cam)
		}
		return nil
	})
	if err != nil {
		s.writeAPIError(w, err)
		return
	}
	s.streams.Sync(s.store.Root().Cameras)
	s.logger.Info("realsense trio created", "prefix", prefix, "geometry", fmt.Sprintf("%dx%d@%d", width, height, fps))
	writeData(w, http.StatusCreated, map[string]any{"cameras": created})
}
