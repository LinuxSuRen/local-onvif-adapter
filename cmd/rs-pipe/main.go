// rs-pipe：RealSense 深度相机采集分发器。
//
// 独占打开一台 RealSense 设备，把 RGB / 深度（伪彩色）/ IR 三路帧以
// rawvideo over TCP 分发（默认端口 18020/18021/18022），local-onvif-adapter
// 的每路 ffmpeg 以 `-f rawvideo -i tcp://127.0.0.1:<port>` 读取后编码推流。
//
// 设备独占语义：RealSense 同一时刻只允许一个进程访问，因此必须由本进程
// 统一取齐三路再扇出，不能每路各开一个采集进程。
//
// 构建形态：
//
//	默认（无 cgo）：仅支持 --fake 合成帧与 --list 提示，用于无 SDK 环境
//	                与离线链路验证；
//	-tags realsense：链接 librealsense2（需系统安装 librealsense），
//	                支持真实设备。
//
// 用法：
//
//	rs-pipe serve [--fake] [--port 18020] [--width 640] [--height 480]
//	              [--framerate 15] [--depth-min 300] [--depth-max 8000]
//	              [--serial <SN>]
//	rs-pipe list
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/linuxsuren/local-onvif-adapter/internal/realsense"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	fs := flag.NewFlagSet("rs-pipe "+cmd, flag.ExitOnError)
	switch cmd {
	case "list":
		_ = fs.Parse(os.Args[2:])
		devices, err := listDevices()
		if err != nil {
			log.Error("list failed", "err", err.Error())
			os.Exit(1)
		}
		if len(devices) == 0 {
			fmt.Println("no realsense device found")
			return
		}
		for _, d := range devices {
			fmt.Printf("%s\t%s\n", d.serial, d.name)
		}
	case "serve":
		var cfg realsense.Config
		var fake bool
		fs.StringVar(&cfg.Serial, "serial", "", "设备序列号，空取第一台")
		fs.IntVar(&cfg.Width, "width", 640, "输出宽度（三路一致）")
		fs.IntVar(&cfg.Height, "height", 480, "输出高度（三路一致）")
		fs.IntVar(&cfg.Framerate, "framerate", 15, "帧率")
		fs.IntVar(&cfg.DepthMinMM, "depth-min", 300, "深度伪彩色映射最近距离（毫米）")
		fs.IntVar(&cfg.DepthMaxMM, "depth-max", 8000, "深度伪彩色映射最远距离（毫米）")
		fs.BoolVar(&fake, "fake", false, "合成帧模式（无需硬件）")
		port := fs.Int("port", 18020, "监听起始端口：base+0=color base+1=depth base+2=ir")
		_ = fs.Parse(os.Args[2:])
		cfg = cfg.WithDefaults()

		var src realsense.Source
		if fake {
			src = realsense.NewFakeSource(cfg)
		} else {
			s, err := openDevice(cfg)
			if err != nil {
				log.Error("open realsense failed", "err", err.Error())
				os.Exit(1)
			}
			src = s
		}
		srv := realsense.NewServer(src, log)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		go func() {
			<-ctx.Done()
			srv.Close()
		}()
		if err := srv.Serve(*port); err != nil {
			log.Error("serve exited", "err", err.Error())
			if f, ok := src.(*realsense.FakeSource); ok {
				f.Stop()
			}
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q; usage: rs-pipe serve|list\n", cmd)
		os.Exit(2)
	}
}
