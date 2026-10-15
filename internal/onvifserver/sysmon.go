package onvifserver

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/host"
)

// SystemMonitor 周期采集宿主机资源（CPU/内存/磁盘/温度），跨越阈值时发布事件。
// 设计与 pad 侧 Battery 事件一致：滞回防抖（高阈值触发/低阈值恢复）、
// 载荷带具体数值、只发状态翻转不发持续值。
//
// 视角：adapter + 宿主机整体 = 一台带算力的相机，宿主机资源就是相机的
// 内部资源（SoC 负载/缓冲区/存储卡/内部温度）——adapter 是这台机器唯一
// 的 ONVIF 出口，不上报就没人上报。
type SystemMonitor struct {
	logger   *slog.Logger
	dataDir  string // 磁盘余量检测路径（config 目录）
	interval time.Duration

	// 滞回状态（跨越阈值翻转）
	cpuHigh  bool
	memHigh  bool
	diskLow  bool
	tempHigh bool
}

// System thresholds——高于 high 触发事件、低于 low 恢复，中间保持（滞回防抖）。
const (
	cpuHighPercent  = 80.0 // CPU 使用率：>80% 持续告警
	cpuLowPercent   = 60.0 // <60% 恢复
	memHighPercent  = 85.0 // 内存使用率：>85% 告警
	memLowPercent   = 70.0 // <70% 恢复
	diskLowBytes    = 1 << 30 // 磁盘余量：<1GB 告警
	diskRestoreBytes = 2 << 30 // >2GB 恢复
	tempHighCelsius = 70.0 // CPU 温度：>70°C 告警
	tempLowCelsius  = 60.0 // <60°C 恢复
)

// StartSystemMonitor 启动资源监控（后台 goroutine，30s 采集周期）。
func StartSystemMonitor(dataDir string, logger *slog.Logger) *SystemMonitor {
	if logger == nil {
		logger = slog.Default()
	}
	m := &SystemMonitor{
		logger:   logger.With("component", "sysmon"),
		dataDir:  dataDir,
		interval: 30 * time.Second,
	}
	go m.loop()
	return m
}

func (m *SystemMonitor) loop() {
	// 首拍等 5 秒（等 ffmpeg 起来再采，避免把启动风暴当成过载）
	time.Sleep(5 * time.Second)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for range ticker.C {
		m.check()
	}
}

func (m *SystemMonitor) check() {
	m.checkCPU()
	m.checkMemory()
	m.checkDisk()
	m.checkTemperature()
}

func (m *SystemMonitor) checkCPU() {
	percent, err := cpu.Percent(0, false)
	if err != nil || len(percent) == 0 {
		return
	}
	p := percent[0]
	if !m.cpuHigh && p > cpuHighPercent {
		m.cpuHigh = true
		PublishEvent("lns:System/CPU",
			[]EventItem{{Name: "Source", Value: "Host"}},
			[]EventItem{{Name: "State", Value: "High"}, {Name: "Load", Value: fmt.Sprintf("%.0f%%", p)}})
	} else if m.cpuHigh && p < cpuLowPercent {
		m.cpuHigh = false
		PublishEvent("lns:System/CPU",
			[]EventItem{{Name: "Source", Value: "Host"}},
			[]EventItem{{Name: "State", Value: "Normal"}, {Name: "Load", Value: fmt.Sprintf("%.0f%%", p)}})
	}
}

func (m *SystemMonitor) checkMemory() {
	vm, err := mem.VirtualMemory()
	if err != nil {
		return
	}
	used := vm.UsedPercent
	availMB := vm.Available >> 20
	if !m.memHigh && used > memHighPercent {
		m.memHigh = true
		PublishEvent("lns:System/Memory",
			[]EventItem{{Name: "Source", Value: "Host"}},
			[]EventItem{{Name: "State", Value: "High"},
				{Name: "Used", Value: fmt.Sprintf("%.0f%%", used)},
				{Name: "Available", Value: fmt.Sprintf("%dMB", availMB)}})
	} else if m.memHigh && used < memLowPercent {
		m.memHigh = false
		PublishEvent("lns:System/Memory",
			[]EventItem{{Name: "Source", Value: "Host"}},
			[]EventItem{{Name: "State", Value: "Normal"},
				{Name: "Used", Value: fmt.Sprintf("%.0f%%", used)},
				{Name: "Available", Value: fmt.Sprintf("%dMB", availMB)}})
	}
}

func (m *SystemMonitor) checkDisk() {
	du, err := disk.Usage(m.dataDir)
	if err != nil {
		return
	}
	freeMB := du.Free >> 20
	if !m.diskLow && du.Free < diskLowBytes {
		m.diskLow = true
		PublishEvent("lns:System/Disk",
			[]EventItem{{Name: "Source", Value: "Host"}},
			[]EventItem{{Name: "State", Value: "Low"}, {Name: "Free", Value: fmt.Sprintf("%dMB", freeMB)}})
	} else if m.diskLow && du.Free > diskRestoreBytes {
		m.diskLow = false
		PublishEvent("lns:System/Disk",
			[]EventItem{{Name: "Source", Value: "Host"}},
			[]EventItem{{Name: "State", Value: "Normal"}, {Name: "Free", Value: fmt.Sprintf("%dMB", freeMB)}})
	}
}

func (m *SystemMonitor) checkTemperature() {
	// 温度不是所有平台都能读到（macOS 无标准接口、部分 Linux 无传感器模块）
	temps, err := host.SensorsTemperatures()
	if err != nil || len(temps) == 0 {
		return
	}
	// 取最高的传感器读数（通常 coretemp/package 是 CPU 温度）
	var max float64
	for _, t := range temps {
		if t.Temperature > max {
			max = t.Temperature
		}
	}
	if max == 0 {
		return
	}
	if !m.tempHigh && max > tempHighCelsius {
		m.tempHigh = true
		PublishEvent("lns:System/Temperature",
			[]EventItem{{Name: "Source", Value: "Host"}},
			[]EventItem{{Name: "State", Value: "High"}, {Name: "Temp", Value: fmt.Sprintf("%.0f°C", max)}})
	} else if m.tempHigh && max < tempLowCelsius {
		m.tempHigh = false
		PublishEvent("lns:System/Temperature",
			[]EventItem{{Name: "Source", Value: "Host"}},
			[]EventItem{{Name: "State", Value: "Normal"}, {Name: "Temp", Value: fmt.Sprintf("%.0f°C", max)}})
	}
}
