//go:build realsense && cgo

package main

// librealsense C API 驱动：独占打开一台设备，按统一几何同时启用三路流，
// 每次轮询取一组帧并归一为 rgb24：
//
//   - color  RS2_FORMAT_RGB8 直取；
//   - depth  RS2_FORMAT_Z16（16-bit 毫米）→ Go 侧固定范围 Turbo 伪彩色
//     （不用 rs2_colorizer：其直方图均衡导致映射随场景漂移，颜色不可复现）；
//   - ir     RS2_FORMAT_Y8 灰度展开为 rgb24。
//
// 设备节奏即输出节奏（rs2_pipeline_wait_for_frames 阻塞到下一帧组），
// 无需额外节拍器。注意：本文件只在真机环境编译（需 librealsense2
// 头文件与库），无 SDK 的构建/CI 环境不会触及。

/*
#cgo LDFLAGS: -lrealsense2
#include <librealsense2/rs.h>
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/linuxsuren/local-onvif-adapter/internal/realsense"
)

type deviceInfo struct {
	serial string
	name   string
}

func rsErr(e *C.rs2_error) error {
	if e == nil {
		return nil
	}
	return fmt.Errorf("librealsense: %s", C.GoString(C.rs2_get_error_message(e)))
}

// listDevices 枚举全部 RealSense 设备（序列号 + 名称）。
func listDevices() ([]deviceInfo, error) {
	var e *C.rs2_error
	ctx := C.rs2_create_context(C.RS2_API_VERSION, &e)
	if err := rsErr(e); err != nil {
		return nil, err
	}
	defer C.rs2_delete_context(ctx)

	list := C.rs2_query_devices(ctx, &e)
	if err := rsErr(e); err != nil {
		return nil, err
	}
	defer C.rs2_delete_device_list(list)

	n := int(C.rs2_get_device_count(list, &e))
	if err := rsErr(e); err != nil {
		return nil, err
	}
	var out []deviceInfo
	for i := 0; i < n; i++ {
		dev := C.rs2_create_device(list, C.int(i), &e)
		if rsErr(e) != nil {
			continue
		}
		info := deviceInfo{
			serial: C.GoString(C.rs2_get_device_info(dev, C.RS2_CAMERA_INFO_SERIAL_NUMBER, &e)),
			name:   C.GoString(C.rs2_get_device_info(dev, C.RS2_CAMERA_INFO_NAME, &e)),
		}
		C.rs2_delete_device(dev)
		out = append(out, info)
	}
	return out, nil
}

// rsSource librealsense 管线封装。
type rsSource struct {
	cfg realsense.Config

	mu       sync.Mutex
	pipe     C.rs2_pipeline
	prof     C.rs2_pipeline_profile
	colorRGB []byte // 复用缓冲（帧尺寸固定，避免每帧分配）
	depthRGB []byte
	irRGB    []byte
	depthRaw []uint16
}

// openDevice 打开指定（或第一台）RealSense 设备并启动三路流管线。
func openDevice(cfg realsense.Config) (realsense.Source, error) {
	cfg = cfg.WithDefaults()
	devices, err := listDevices()
	if err != nil {
		return nil, err
	}
	if cfg.Serial != "" {
		found := false
		for _, d := range devices {
			if d.serial == cfg.Serial {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("realsense device %s not found (%d devices)", cfg.Serial, len(devices))
		}
	} else if len(devices) == 0 {
		return nil, realsense.ErrNoDevice
	}

	var e *C.rs2_error
	ctx := C.rs2_create_context(C.RS2_API_VERSION, &e)
	if err := rsErr(e); err != nil {
		return nil, err
	}
	defer C.rs2_delete_context(ctx)

	pipe := C.rs2_create_pipeline(ctx, &e)
	if err := rsErr(e); err != nil {
		return nil, err
	}

	conf := C.rs2_create_config(&e)
	if err := rsErr(e); err != nil {
		C.rs2_delete_pipeline(pipe)
		return nil, err
	}
	defer C.rs2_delete_config(conf)

	// 指定序列号（多台设备时）；单台可省。
	if cfg.Serial != "" {
		cstr := C.CString(cfg.Serial)
		defer C.free(unsafe.Pointer(cstr))
		C.rs2_config_enable_device(conf, cstr, &e)
		if err := rsErr(e); err != nil {
			C.rs2_delete_pipeline(pipe)
			return nil, err
		}
	}

	w, h, fps := C.int(cfg.Width), C.int(cfg.Height), C.int(cfg.Framerate)
	// 三路统一几何：rawvideo 传输无格式头，ffmpeg 侧声明必须与这里一致。
	// IR 取 infrared 流索引 1（D4 系左 IR）；分辨率几何与 RGB/深度一致。
	C.rs2_config_enable_stream(conf, C.RS2_STREAM_COLOR, 0, w, h, C.RS2_FORMAT_RGB8, fps, &e)
	if err := rsErr(e); err != nil {
		C.rs2_delete_pipeline(pipe)
		return nil, err
	}
	C.rs2_config_enable_stream(conf, C.RS2_STREAM_DEPTH, 0, w, h, C.RS2_FORMAT_Z16, fps, &e)
	if err := rsErr(e); err != nil {
		C.rs2_delete_pipeline(pipe)
		return nil, err
	}
	C.rs2_config_enable_stream(conf, C.RS2_STREAM_INFRARED, 1, w, h, C.RS2_FORMAT_Y8, fps, &e)
	if err := rsErr(e); err != nil {
		C.rs2_delete_pipeline(pipe)
		return nil, err
	}

	prof := C.rs2_pipeline_start_with_config(pipe, conf, &e)
	if err := rsErr(e); err != nil {
		C.rs2_delete_pipeline(pipe)
		return nil, fmt.Errorf("pipeline start: %w", err)
	}

	n := cfg.Width * cfg.Height
	s := &rsSource{
		cfg:      cfg,
		pipe:     pipe,
		prof:     prof,
		colorRGB: make([]byte, n*3),
		depthRGB: make([]byte, n*3),
		irRGB:    make([]byte, n*3),
		depthRaw: make([]uint16, n),
	}
	// 设备初启首帧常为黑帧/未曝光完成，丢弃前几帧让画面稳定。
	for i := 0; i < 5; i++ {
		_, _ = s.Poll()
	}
	return s, nil
}

// Streams 三路全出。
func (s *rsSource) Streams() []realsense.StreamKind {
	return []realsense.StreamKind{realsense.KindColor, realsense.KindDepth, realsense.KindIR}
}

// Poll 阻塞取下一组帧（5s 超时防设备半死卡死整条链路）。
func (s *rsSource) Poll() (realsense.FrameSet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pipe == nil {
		return nil, realsense.ErrNoDevice
	}
	var e *C.rs2_error
	frames := C.rs2_pipeline_wait_for_frames(s.pipe, C.uint(5000), &e)
	if err := rsErr(e); err != nil {
		return nil, err
	}
	defer C.rs2_release_frame(frames)

	w, h := s.cfg.Width, s.cfg.Height
	n := w * h
	set := realsense.FrameSet{}

	if f, ok := extractStream(frames, C.RS2_STREAM_COLOR); ok {
		data := C.rs2_get_frame_data(f, &e)
		if rsErr(e) == nil && data != nil {
			copy(s.colorRGB, C.GoBytes(data, C.int(n*3)))
			set[realsense.KindColor] = &realsense.Frame{Kind: realsense.KindColor, Width: w, Height: h, Data: s.colorRGB}
		}
		C.rs2_release_frame(f)
	}
	if f, ok := extractStream(frames, C.RS2_STREAM_DEPTH); ok {
		if data := C.rs2_get_frame_data(f, &e); rsErr(e) == nil && data != nil {
			copy(s.depthRaw, (*[1 << 20]uint16)(unsafe.Pointer(data))[:n:n])
			if err := realsense.ApplyColorMapScaled(s.depthRaw, w, h, s.depthRGB, s.cfg.DepthMinMM, s.cfg.DepthMaxMM); err == nil {
				set[realsense.KindDepth] = &realsense.Frame{Kind: realsense.KindDepth, Width: w, Height: h, Data: s.depthRGB}
			}
		}
		C.rs2_release_frame(f)
	}
	if f, ok := extractStream(frames, C.RS2_STREAM_INFRARED); ok {
		if data := C.rs2_get_frame_data(f, &e); rsErr(e) == nil && data != nil {
			gray := C.GoBytes(data, C.int(n))
			if err := realsense.GrayToRGB(gray, w, h, s.irRGB); err == nil {
				set[realsense.KindIR] = &realsense.Frame{Kind: realsense.KindIR, Width: w, Height: h, Data: s.irRGB}
			}
		}
		C.rs2_release_frame(f)
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("realsense: empty frameset")
	}
	return set, nil
}

// extractStream 在帧组中找到指定类型的帧（找不到返回 ok=false）。
func extractStream(frames C.rs2_frame, stream C.rs2_stream) (C.rs2_frame, bool) {
	var e *C.rs2_error
	cnt := int(C.rs2_embedded_frames_count(frames, &e))
	for i := 0; i < cnt; i++ {
		f := C.rs2_extract_frame(frames, C.int(i), &e)
		if rsErr(e) != nil {
			continue
		}
		var ok bool
		prof := C.rs2_get_frame_stream_profile(f, &e)
		if rsErr(e) == nil && prof != nil {
			var st C.rs2_stream
			var fmt_ C.rs2_format
			var idx, uid C.int
			C.rs2_get_stream_profile_data(prof, &st, &fmt_, &idx, &uid, &e)
			if rsErr(e) == nil && st == stream {
				ok = true
			}
		}
		if ok {
			return f, true
		}
		C.rs2_release_frame(f)
	}
	return nil, false
}

// Close 停止管线并释放资源。
func (s *rsSource) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pipe == nil {
		return
	}
	var e *C.rs2_error
	C.rs2_pipeline_stop(s.pipe, &e)
	C.rs2_delete_pipeline_profile(s.prof)
	C.rs2_delete_pipeline(s.pipe)
	s.pipe, s.prof = nil, nil
}
