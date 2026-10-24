// push.go 实现按需直推会话管理：
// 会话由 lns:StartPushStream 等 ONVIF 扩展动作创建/续租/停止（见 onvifserver.PushController），
// 每个会话携带一个远端推流目标（rtsp/rtmp/srt URL），目标列表纳入取流进程的 Spec 指纹——
// 目标变化即重启进程，由同一 ffmpeg 以 tee 多输出同时写本地 RTSP 与远端目标。
// 生命周期保护：TTL 租约（janitor 定期回收，防 controller 崩溃后白占上行带宽）
// 与失败预算（短命运行连续达上限即摘除会话，绝无限重启）。
package stream

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/linuxsuren/local-onvif-adapter/internal/onvifserver"
)

// 推流会话默认参数（均可在测试中覆盖 Manager 的对应字段）。
const (
	defaultPushTTL    = 10 * time.Minute // 租约时长：controller 以 1/3 周期续租
	pushJanitorPeriod = 15 * time.Second // 过期回收巡检周期
	pushFailBudget    = 10               // 连续短命运行达该次数即摘除全部推流会话
	pushFailWindow    = 30 * time.Second // 单次运行不足该时长计一次失败（超时视为成功并清零）
)

// 推流会话状态。
const (
	pushStatusStarting = "starting" // 已创建，等待取流进程带推流目标重启
	pushStatusRunning  = "running"  // 取流进程已启动（含推流输出）
	pushStatusFailed   = "failed"   // 失败预算耗尽被摘除（终态）
)

// PushSession 一路按需直推会话（仅存内存，TargetURL 凭证不落盘）。
type PushSession struct {
	Token        string
	CameraID     string
	ProfileToken string // 创建会话时使用的 ONVIF profile（信息性）
	Target       string // 远端推流目标（原始 URL，含凭证）
	Status       string
	CreatedAt    time.Time
	ExpiresAt    time.Time // 租约到期时刻，Renew 顺延
	LastError    string
	startedEvent bool // lns:Push/Started 每会话只发一次（进程反复重启不刷屏）
}

// supportedPushSchemes 目标 URL scheme → ffmpeg tee 子输出封装格式。
var supportedPushSchemes = map[string]string{
	"rtsp":  "rtsp", // ANNOUNCE/RECORD 推入 mediamtx 等 RTSP 入口（v1 主路径）
	"rtmp":  "flv",  // RTMP 入口
	"rtmps": "flv",
	"srt":   "mpegts", // SRT 入口
}

// validatePushTarget 校验推流目标：scheme 受支持，且不含 tee 规格
// 的保留字符（| 与 [ ]，会破坏子输出分隔语法）。
func validatePushTarget(target string) error {
	if strings.ContainsAny(target, "|[]") {
		return fmt.Errorf("target URL contains reserved characters (| or [ ]): %s", onvifserver.RedactURL(target))
	}
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("parse target URL: %w", err)
	}
	if _, ok := supportedPushSchemes[u.Scheme]; !ok {
		return fmt.Errorf("unsupported target scheme %q (supported: rtsp/rtmp/rtmps/srt)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("target URL missing host")
	}
	return nil
}

// teeEntry 构造单个目标的 tee 子输出规格（如 [f=rtsp:rtsp_transport=tcp]rtsp://...）。
func teeEntry(target string) string {
	u, _ := url.Parse(target)
	format := supportedPushSchemes[u.Scheme]
	if format == "rtsp" {
		// 与本地输出一致：RTSP 推流走 TCP interleaved，穿 NAT/防火墙最稳。
		return "[f=rtsp:rtsp_transport=tcp]" + target
	}
	return "[f=" + format + "]" + target
}

// outputArgs 构造 ffmpeg 输出参数：
//   - 无推流目标：保持历史形态（单输出 -f rtsp）；
//   - 有推流目标：tee 多路输出——编码只做一次，各子输出独立封装，
//     任一子输出失败 ffmpeg 即退出（交由 Runner 退避重启整体重试）。
func outputArgs(pushURL string, targets []string) []string {
	if len(targets) == 0 {
		return []string{"-f", "rtsp", "-rtsp_transport", "tcp", pushURL}
	}
	parts := make([]string, 0, len(targets)+1)
	parts = append(parts, "[f=rtsp:rtsp_transport=tcp]"+pushURL)
	for _, t := range targets {
		parts = append(parts, teeEntry(t))
	}
	return []string{"-f", "tee", strings.Join(parts, "|")}
}

// newPushToken 生成会话标识（随机 8 字节十六进制，带 push_ 前缀便于日志辨认）。
func newPushToken() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("push_%d", time.Now().UnixNano())
	}
	return "push_" + hex.EncodeToString(b)
}

// ---- onvifserver.PushController 实现（Manager 上的方法） ----

// StartPush 创建（或幂等复用）一路直推会话：同 (camera, target) 去重，
// 重复 Start 视为续租并返回同一 token；目标列表变化经 Sync 重启取流进程。
func (m *Manager) StartPush(camID, profileToken, target string) (string, bool, error) {
	if err := validatePushTarget(target); err != nil {
		return "", false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.pushSessions {
		if s.CameraID == camID && s.Target == target {
			s.ExpiresAt = time.Now().Add(m.pushTTL)
			m.logger.Info("push session reused", "camera", camID,
				"target", onvifserver.RedactURL(target), "token", s.Token)
			return s.Token, true, nil
		}
	}
	s := &PushSession{
		Token:        newPushToken(),
		CameraID:     camID,
		ProfileToken: profileToken,
		Target:       target,
		Status:       pushStatusStarting,
		CreatedAt:    time.Now(),
		ExpiresAt:    time.Now().Add(m.pushTTL),
	}
	m.pushSessions[s.Token] = s
	m.syncLocked(m.lastCameras) // targets 进入 Spec → 重启该路取流进程
	m.logger.Info("push session created", "camera", camID,
		"target", onvifserver.RedactURL(target), "token", s.Token,
		"ttl", m.pushTTL.String())
	return s.Token, false, nil
}

// RenewPush 续租（TTL 顺延）；会话不存在或已失败时返回错误，
// controller 据此感知设备重启/会话丢失并重新 Start。
func (m *Manager) RenewPush(token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.pushSessions[token]
	if !ok {
		return fmt.Errorf("push session %q not found", token)
	}
	if s.Status == pushStatusFailed {
		return fmt.Errorf("push session %q has failed", token)
	}
	s.ExpiresAt = time.Now().Add(m.pushTTL)
	return nil
}

// StopPush 显式停止一路会话（controller 侧观看者引用清零时调用）。
func (m *Manager) StopPush(token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.pushSessions[token]
	if !ok {
		return fmt.Errorf("push session %q not found", token)
	}
	m.removeSessionLocked(s, "explicit")
	m.syncLocked(m.lastCameras)
	return nil
}

// PushSessions 返回全部会话快照（Target 已脱敏），按 CameraID/创建时间稳定排序。
func (m *Manager) PushSessions() []onvifserver.PushSessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]onvifserver.PushSessionInfo, 0, len(m.pushSessions))
	for _, s := range m.pushSessions {
		out = append(out, onvifserver.PushSessionInfo{
			Token:        s.Token,
			CameraID:     s.CameraID,
			ProfileToken: s.ProfileToken,
			Target:       onvifserver.RedactURL(s.Target),
			Status:       s.Status,
			CreatedAt:    s.CreatedAt,
			ExpiresAt:    s.ExpiresAt,
			LastError:    s.LastError,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CameraID != out[j].CameraID {
			return out[i].CameraID < out[j].CameraID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// ---- 内部生命周期 ----

// targetsForLocked 返回指定摄像头的去重目标列表（排序保证 Spec 指纹稳定）。
func (m *Manager) targetsForLocked(camID string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range m.pushSessions {
		if s.CameraID == camID && !seen[s.Target] {
			seen[s.Target] = true
			out = append(out, s.Target)
		}
	}
	sort.Strings(out)
	return out
}

// removeSessionLocked 删除会话并发布 lns:Push/Stopped 事件（调用方持锁）。
func (m *Manager) removeSessionLocked(s *PushSession, reason string) {
	delete(m.pushSessions, s.Token)
	m.logger.Info("push session removed", "camera", s.CameraID,
		"token", s.Token, "reason", reason)
	onvifserver.PublishEvent("lns:Push/Stopped",
		pushSourceItems(s),
		[]onvifserver.EventItem{{Name: "State", Value: "stopped"}, {Name: "Reason", Value: reason}})
}

// expirePushes 回收租约到期的会话（janitor 周期调用；测试直接调用以缩短周期）。
func (m *Manager) expirePushes() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	changed := false
	for _, s := range m.pushSessions {
		if now.After(s.ExpiresAt) {
			m.removeSessionLocked(s, "lease_expired")
			changed = true
		}
	}
	if changed {
		m.syncLocked(m.lastCameras)
	}
}

// noteRunStart 取流进程成功启动的回调（Runner goroutine 调用）：
// 该摄像头的会话进入 running 并发布一次 lns:Push/Started。
func (m *Manager) noteRunStart(camID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.pushSessions {
		if s.CameraID != camID {
			continue
		}
		s.Status = pushStatusRunning
		if !s.startedEvent {
			s.startedEvent = true
			m.logger.Info("push session running", "camera", camID, "token", s.Token)
			onvifserver.PublishEvent("lns:Push/Started",
				pushSourceItems(s),
				[]onvifserver.EventItem{{Name: "State", Value: "started"}})
		}
	}
}

// noteRunExit 取流进程意外退出的回调（Runner goroutine 调用；正常 Stop 不计）：
// 短命运行（< pushFailWindow）计一次失败；连续达 pushFailBudget 且该摄像头
// 存在推流会话时，摘除全部会话（failed 终态 + 事件），进程回到本地-only 形态，
// 绝无限重启。长命运行视为成功，失败计数清零。
func (m *Manager) noteRunExit(camID string, uptime time.Duration, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if uptime >= pushFailWindow {
		m.pushFails[camID] = 0
		return
	}
	m.pushFails[camID]++
	if m.pushFails[camID] < pushFailBudget {
		return
	}
	delete(m.pushFails, camID)
	var dropped []*PushSession
	for _, s := range m.pushSessions {
		if s.CameraID == camID {
			dropped = append(dropped, s)
		}
	}
	if len(dropped) == 0 {
		return
	}
	for _, s := range dropped {
		s.Status = pushStatusFailed
		s.LastError = trimEvent(errMsg, 160)
		delete(m.pushSessions, s.Token)
		m.logger.Warn("push session failed, budget exhausted", "camera", camID,
			"token", s.Token, "err", s.LastError)
		onvifserver.PublishEvent("lns:Push/Failed",
			pushSourceItems(s),
			[]onvifserver.EventItem{{Name: "State", Value: "failed"},
				{Name: "Error", Value: s.LastError}})
	}
	m.syncLocked(m.lastCameras) // 摘除目标 → 重启回本地-only
}

// janitorLoop 周期回收过期会话，直到 StopAll 关闭 stop 通道。
func (m *Manager) janitorLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(m.pushJanitorPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			m.expirePushes()
		}
	}
}

// pushSourceItems 构造事件 Source 侧条目（Target 永远脱敏）。
func pushSourceItems(s *PushSession) []onvifserver.EventItem {
	return []onvifserver.EventItem{
		{Name: "Source", Value: "Camera_" + s.CameraID},
		{Name: "Camera", Value: s.CameraID},
		{Name: "PushToken", Value: s.Token},
		{Name: "ProfileToken", Value: s.ProfileToken},
		{Name: "Target", Value: onvifserver.RedactURL(s.Target)},
	}
}
