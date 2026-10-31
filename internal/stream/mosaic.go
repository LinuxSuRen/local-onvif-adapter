// mosaic.go 实现多摄像头融合布局（mosaic）的合成流管理：
// 每个启用中的布局由一个 mosaicRunner 监督，把多路画面按 cells 拼到一张画布，
// 编码为单路 H.264 后经 tee 同时输出本地 RTSP（/cam/<mosaicID>，作为虚拟通道
// 暴露给 ONVIF/NVR）与按需直推目标（rtsp/rtmp/srt，会话机制见下）。
//
// 断流策略（loss_policy，见 config.LossPolicy*）：
//   - blackframe（默认）：每路画面由独立中继进程拉流成 rawvideo 定长帧，
//     Go 侧拼画布喂编码进程；单路断流只黑对应格，整流不断（运行时见 mosaicpipe.go）。
//   - restart：单 ffmpeg filter_complex 一次成型；任一路断流进程退出整体重启，
//     简单可靠但整流会中断数秒。
//
// 推流会话与 push.go 的摄像头会话语义对齐（内存态、凭证不落盘、失败预算），
// 区别是会话由管理台显式创建/停止，不设 TTL 租约——远端平台是长期观看者，
// 没有"无人观看自动停流"的本地信号可依赖。
package stream

import (
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
	"github.com/linuxsuren/local-onvif-adapter/internal/onvifserver"
)

// mosaicFrameStale blackframe 模式判定画面格「断流」的帧龄上限：
// 超过该时长没有新帧即铺黑帧（中继重启期间画布保持整流不断）。
const mosaicFrameStale = 5 * time.Second

// MosaicCellStatus 单个画面格的运行健康度（blackframe 模式有效）。
type MosaicCellStatus struct {
	CameraID     string `json:"camera_id"`
	Fresh        bool   `json:"fresh"`         // 近期有帧
	RelayRunning bool   `json:"relay_running"` // 中继进程存活
}

// MosaicStatus 一路融合流的运行状态。
type MosaicStatus struct {
	Running   bool               `json:"running"`
	LastError string             `json:"last_error"`
	StartedAt time.Time          `json:"-"`
	Restarts  int                `json:"restarts"`
	Cells     []MosaicCellStatus `json:"cells,omitempty"`
}

// MosaicPushInfo 推流会话快照（Target 已脱敏）。
type MosaicPushInfo struct {
	Token     string    `json:"token"`
	Target    string    `json:"target"`
	Status    string    `json:"status"`
	LastError string    `json:"last_error"`
	CreatedAt time.Time `json:"created_at"`
}

// MosaicManager 管理全部融合流进程与推流会话。
type MosaicManager struct {
	mu       sync.Mutex
	pushBase func() string
	bin      string
	logger   *slog.Logger

	runners  map[string]*mosaicRunner
	sessions map[string]*mosaicPushSession // token → 会话
	fails    map[string]int                // mosaicID → 连续短命运行计数（失败预算）
	lastAddr string
	// 最近一次 Sync 的期望状态：会话增删时据此重新对齐运行态。
	lastMosaics []config.Mosaic
	lastCameras []config.Camera
}

// mosaicPushSession 一路融合流直推会话（仅存内存，Target 凭证不落盘）。
type mosaicPushSession struct {
	Token     string
	MosaicID  string
	Target    string
	Status    string
	LastError string
	CreatedAt time.Time
}

// NewMosaicManager 创建融合流管理器。pushBase 语义与 NewManager 相同
//（认证启用时携带 userinfo，拉流与推流同账号）。
func NewMosaicManager(pushBase func() string, bin string, logger *slog.Logger) *MosaicManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &MosaicManager{
		pushBase: pushBase,
		bin:      bin,
		logger:   logger,
		runners:  map[string]*mosaicRunner{},
		sessions: map[string]*mosaicPushSession{},
		fails:    map[string]int{},
	}
}

// Sync 将运行态对齐到期望的布局列表：
// 新增的启动、删除/禁用的停止、指纹变化的重启（ffmpeg 命令行无法运行中变更）。
func (m *MosaicManager) Sync(mosaics []config.Mosaic, cameras []config.Camera) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncLocked(mosaics, cameras)
}

func (m *MosaicManager) syncLocked(mosaics []config.Mosaic, cameras []config.Camera) {
	m.lastMosaics = mosaics
	m.lastCameras = cameras

	// 推流地址变化（认证开关切换注入/移除 userinfo）：停掉全部进程按新地址重建，
	// 避免旧进程以失效凭证反复 401（与 Manager 同款处理）。
	addr := m.pushBase()
	if addr != m.lastAddr {
		for id, r := range m.runners {
			r.Stop()
			delete(m.runners, id)
		}
		m.lastAddr = addr
	}

	want := map[string]config.Mosaic{}
	for _, mo := range mosaics {
		if mo.Enabled {
			want[mo.ID] = mo
		}
	}
	for id, r := range m.runners {
		if _, ok := want[id]; !ok {
			r.Stop()
			delete(m.runners, id)
			continue
		}
		if r.Spec() != mosaicSpec(want[id], m.targetsForLocked(id), addr, cameras) {
			r.Stop()
			delete(m.runners, id)
		}
	}
	for id, mo := range want {
		if _, ok := m.runners[id]; ok {
			continue
		}
		r := newMosaicRunner(mo, cameras, addr, m.targetsForLocked(id), m.bin, m.logger)
		r.onStart = m.noteRunStart
		r.onExit = m.noteRunExit
		m.runners[id] = r
		go r.Run()
	}
}

// StopAll 停止全部融合流进程（进程退出时随会话一起清理）。
func (m *MosaicManager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.runners {
		r.Stop()
		delete(m.runners, id)
	}
}

// Status 返回指定融合流的运行状态。
func (m *MosaicManager) Status(mosaicID string) (MosaicStatus, bool) {
	m.mu.Lock()
	r, ok := m.runners[mosaicID]
	m.mu.Unlock()
	if !ok {
		return MosaicStatus{}, false
	}
	return r.statusSnapshot(), true
}

// StartMosaicPush 创建（或幂等复用）一路融合流直推会话；目标列表进入
// Spec 指纹，Sync 据此重启合成进程把目标并入 tee 输出。
func (m *MosaicManager) StartMosaicPush(mosaicID, target string) (string, bool, error) {
	if err := validatePushTarget(target); err != nil {
		return "", false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	enabled := false
	for _, mo := range m.lastMosaics {
		if mo.ID == mosaicID && mo.Enabled {
			enabled = true
			break
		}
	}
	if !enabled {
		return "", false, fmt.Errorf("融合布局 %q 不存在或未启用", mosaicID)
	}
	for _, s := range m.sessions {
		if s.MosaicID == mosaicID && s.Target == target {
			m.logger.Info("mosaic push session reused", "mosaic", mosaicID,
				"target", onvifserver.RedactURL(target), "token", s.Token)
			return s.Token, true, nil
		}
	}
	s := &mosaicPushSession{
		Token:     newPushToken(),
		MosaicID:  mosaicID,
		Target:    target,
		Status:    pushStatusStarting,
		CreatedAt: time.Now(),
	}
	m.sessions[s.Token] = s
	m.syncLocked(m.lastMosaics, m.lastCameras)
	m.logger.Info("mosaic push session created", "mosaic", mosaicID,
		"target", onvifserver.RedactURL(target), "token", s.Token)
	return s.Token, false, nil
}

// StopMosaicPush 显式停止并移除一路会话。
func (m *MosaicManager) StopMosaicPush(token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[token]
	if !ok {
		return fmt.Errorf("push session %q not found", token)
	}
	delete(m.sessions, token)
	m.logger.Info("mosaic push session removed", "mosaic", s.MosaicID,
		"token", token, "reason", "explicit")
	m.syncLocked(m.lastMosaics, m.lastCameras)
	return nil
}

// MosaicPushes 返回指定融合流的会话快照（Target 已脱敏），按创建时间稳定排序。
func (m *MosaicManager) MosaicPushes(mosaicID string) []MosaicPushInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MosaicPushInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		if s.MosaicID != mosaicID {
			continue
		}
		out = append(out, MosaicPushInfo{
			Token:     s.Token,
			Target:    onvifserver.RedactURL(s.Target),
			Status:    s.Status,
			LastError: s.LastError,
			CreatedAt: s.CreatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// targetsForLocked 返回指定融合流的去重目标列表（排序保证 Spec 指纹稳定）。
func (m *MosaicManager) targetsForLocked(mosaicID string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range m.sessions {
		if s.MosaicID == mosaicID && !seen[s.Target] {
			seen[s.Target] = true
			out = append(out, s.Target)
		}
	}
	sort.Strings(out)
	return out
}

// Forget 布局被删除时调用：清空其全部推流会话（运行进程由随后的 Sync 回收）。
func (m *MosaicManager) Forget(mosaicID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for tok, s := range m.sessions {
		if s.MosaicID == mosaicID {
			delete(m.sessions, tok)
			m.logger.Info("mosaic push session removed", "mosaic", mosaicID,
				"token", tok, "reason", "mosaic_deleted")
		}
	}
}

// dropSessionsLocked 摘除指定融合流的全部会话（失败预算耗尽，failed 终态）。
func (m *MosaicManager) dropSessionsLocked(mosaicID, errMsg string) {
	for _, s := range m.sessions {
		if s.MosaicID != mosaicID {
			continue
		}
		s.Status = pushStatusFailed
		s.LastError = trimEvent(errMsg, 160)
		delete(m.sessions, s.Token)
		m.logger.Warn("mosaic push session failed, budget exhausted",
			"mosaic", mosaicID, "token", s.Token, "err", s.LastError)
		onvifserver.PublishEvent("lns:Push/Failed",
			[]onvifserver.EventItem{
				{Name: "Source", Value: "Mosaic_" + mosaicID},
				{Name: "Mosaic", Value: mosaicID},
				{Name: "PushToken", Value: s.Token},
				{Name: "Target", Value: onvifserver.RedactURL(s.Target)},
			},
			[]onvifserver.EventItem{{Name: "State", Value: "failed"}, {Name: "Error", Value: s.LastError}})
	}
}

// noteRunExit 合成进程意外退出的回调（正常 Stop 不计）：短命运行计一次失败，
// 连续达预算且存在推流会话时摘除全部会话（failed 终态），进程回到本地-only
// 形态继续退避重试。长命运行视为成功，失败计数清零。
func (m *MosaicManager) noteRunExit(mosaicID string, uptime time.Duration, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if uptime >= pushFailWindow {
		m.fails[mosaicID] = 0
		return
	}
	m.fails[mosaicID]++
	if m.fails[mosaicID] < pushFailBudget {
		return
	}
	delete(m.fails, mosaicID)
	m.dropSessionsLocked(mosaicID, errMsg)
	m.syncLocked(m.lastMosaics, m.lastCameras) // 摘除目标 → 重启回本地-only
}

// noteRunStartLocked 语义对齐 push.go 的 noteRunStart：进程成功启动时
// 该融合流的会话进入 running。由 mosaicRunner 在 Run 循环内调用。
func (m *MosaicManager) noteRunStart(mosaicID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.MosaicID == mosaicID {
			s.Status = pushStatusRunning
			s.LastError = ""
		}
	}
}

// ---- mosaicRunner：单路融合流的进程监督器 ----

type mosaicRunner struct {
	m       config.Mosaic
	cameras []config.Camera // Sync 时刻的摄像头快照（判断画面格可用性）
	addr    string          // 推流基址（含 userinfo，拉流与本地输出共用）
	targets []string
	bin     string
	logger  *slog.Logger

	onStart func(mosaicID string)
	onExit  func(mosaicID string, uptime time.Duration, errMsg string)

	mu      sync.Mutex
	status  MosaicStatus
	spec    string
	stopCh  chan struct{}
	stopped bool
	starts  int
	cmd     *exec.Cmd     // restart 模式：唯一 ffmpeg；blackframe 模式：编码进程
	relays  []*cellRelay  // blackframe 模式：当前一代的画面中继（Stop 时一并终止）
}

func newMosaicRunner(m config.Mosaic, cameras []config.Camera, addr string,
	targets []string, bin string, logger *slog.Logger) *mosaicRunner {
	return &mosaicRunner{
		m:       m,
		cameras: cameras,
		addr:    addr,
		targets: targets,
		bin:     bin,
		logger:  logger,
		spec:    mosaicSpec(m, targets, addr, cameras),
		stopCh:  make(chan struct{}),
	}
}

// Spec 返回创建时的参数指纹。
func (r *mosaicRunner) Spec() string { return r.spec }

// Stop 请求停止（幂等）：置位停止标志并强制结束当前进程树
//（blackframe 模式含编码进程与全部画面中继，防止进程泄漏）。
func (r *mosaicRunner) Stop() {
	r.mu.Lock()
	cmd := r.cmd
	r.cmd = nil
	relays := r.relays
	r.relays = nil
	if !r.stopped {
		r.stopped = true
		close(r.stopCh)
	}
	r.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	for _, rl := range relays {
		rl.Stop()
	}
}

func (r *mosaicRunner) isStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

func (r *mosaicRunner) statusSnapshot() MosaicStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.status
	if r.m.EffectiveLossPolicy() == config.LossPolicyBlackframe {
		cells := make([]MosaicCellStatus, len(r.status.Cells))
		copy(cells, r.status.Cells)
		st.Cells = cells
	}
	return st
}

func (r *mosaicRunner) setRunning(v bool, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Running = v
	r.status.LastError = errMsg
	if v {
		r.starts++
		r.status.StartedAt = time.Now()
		r.status.Restarts = r.starts - 1
	}
}

// setCells 更新画面格健康度快照（blackframe 模式的合成循环与中继回调调用）。
func (r *mosaicRunner) setCells(cells []MosaicCellStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Cells = cells
}

// Run 阻塞监督融合流进程，异常退出后指数退避重启。
func (r *mosaicRunner) Run() {
	backoff := time.Second
	const maxBackoff = 15 * time.Second
	for !r.isStopped() {
		started := time.Now()
		var msg string
		if r.m.EffectiveLossPolicy() == config.LossPolicyRestart {
			msg = r.runRestartOnce()
		} else {
			msg = r.runBlackframeOnce()
		}
		if r.isStopped() {
			r.setRunning(false, "")
			r.publishLifecycle("Stopped", "")
			return
		}
		r.setRunning(false, msg)
		r.logger.Warn("mosaic compositor exited, restarting",
			"mosaic", r.m.ID, "err", msg,
			"uptime", time.Since(started).Round(time.Second).String())
		r.publishLifecycle("Restarting", msg)
		uptime := time.Since(started)
		if uptime >= pushFailWindow {
			backoff = time.Second
		}
		if r.onExit != nil {
			r.onExit(r.m.ID, uptime, msg)
		}
		select {
		case <-r.stopCh:
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// publishLifecycle 发布 lns:Source/Lifecycle 事件（Camera 侧带 mosaic 标识，
// NVR 侧与普通摄像头通道的生命周期信号一致）。
func (r *mosaicRunner) publishLifecycle(phase, errMsg string) {
	items := []onvifserver.EventItem{
		{Name: "Source", Value: "Mosaic_" + r.m.ID},
		{Name: "Mosaic", Value: r.m.ID},
		{Name: "Phase", Value: phase},
	}
	if errMsg != "" {
		items = append(items, onvifserver.EventItem{Name: "Error", Value: trimEvent(errMsg, 120)})
	}
	onvifserver.PublishEvent("lns:Source/Lifecycle", items, nil)
}

// publishStarted 进程成功启动事件（Run 各模式在 Start 成功后调用）。
func (r *mosaicRunner) publishStarted() {
	onvifserver.PublishEvent("lns:Source/Lifecycle",
		[]onvifserver.EventItem{
			{Name: "Source", Value: "Mosaic_" + r.m.ID},
			{Name: "Mosaic", Value: r.m.ID},
			{Name: "Phase", Value: "Started"},
		}, nil)
}

// redactArgs 对命令行参数逐项脱敏后拼接（地址可能携带凭证，日志统一打码）。
func redactArgs(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = onvifserver.RedactURL(a)
	}
	return strings.Join(parts, " ")
}

// runRestartOnce 执行一代 restart 模式合成（单 ffmpeg filter_complex），
// 阻塞到进程退出，返回错误描述（含 stderr 尾部）。
func (r *mosaicRunner) runRestartOnce() string {
	args := buildMosaicArgs(r.m, r.cameras, r.addr, r.targets, r.bin, r.logger)
	cmd := exec.Command(r.bin, args...) //nolint:gosec // 二进制路径来自受控配置
	var stderr tailBuffer
	cmd.Stderr = &stderr
	r.logger.Debug("mosaic ffmpeg starting", "mosaic", r.m.ID, "args", redactArgs(args))
	if err := cmd.Start(); err != nil {
		return "ffmpeg start: " + err.Error()
	}
	r.mu.Lock()
	r.cmd = cmd
	r.mu.Unlock()
	r.setRunning(true, "")
	r.publishStarted()
	if r.onStart != nil {
		r.onStart(r.m.ID)
	}
	err := cmd.Wait()
	r.mu.Lock()
	if r.cmd == cmd {
		r.cmd = nil
	}
	r.mu.Unlock()
	if err == nil {
		return ""
	}
	msg := err.Error()
	if tail := stderr.Tail(); tail != "" {
		msg += "; ffmpeg: " + tail
	}
	return msg
}

// ---- 参数与指纹构造 ----

// availableCameras 返回可作为画面源的摄像头（启用且非融合通道；
// 融合通道不能嵌套引用，避免无限递归的布局依赖）。
func availableCameras(cameras []config.Camera) map[string]config.Camera {
	out := map[string]config.Camera{}
	for _, c := range cameras {
		if c.Enabled && c.Type != config.TypeMosaic {
			out[c.ID] = c
		}
	}
	return out
}

// mosaicCamURL 拼接本地摄像头流的拉流地址。
func mosaicCamURL(base, camID string) string {
	return strings.TrimSuffix(base, "/") + "/cam/" + camID
}

// mosaicSpec 构造融合流的参数指纹：画布/帧率/码率/断流策略/画面格（含
// 各格摄像头可用性——摄像头删除、禁用会改变命令行形态，须重启）/
// 音频源可用性/推流目标。画面格顺序即叠放顺序，不排序；目标排序保证顺序无关。
// 名称不影响进程形态，不纳入指纹（改名不重启）。
func mosaicSpec(m config.Mosaic, targets []string, addr string, cameras []config.Camera) string {
	avail := availableCameras(cameras)
	cellParts := make([]string, 0, len(m.Cells))
	for _, cell := range m.Cells {
		a := 0
		if _, ok := avail[cell.CameraID]; ok {
			a = 1
		}
		cellParts = append(cellParts,
			fmt.Sprintf("%s@%d,%d,%dx%d:%d", cell.CameraID, cell.X, cell.Y, cell.W, cell.H, a))
	}
	audioAvail := 0
	if m.AudioCameraID != "" {
		if _, ok := avail[m.AudioCameraID]; ok {
			audioAvail = 1
		}
	}
	sorted := append([]string(nil), targets...)
	sort.Strings(sorted)
	return fmt.Sprintf("%s|%d|%d|%d|%d|%s|%s|%s:%d|%s|push:%s",
		m.ID, m.Width, m.Height, m.FramerateOrDefault(), m.BitrateOrDefault(),
		m.EffectiveLossPolicy(), strings.Join(cellParts, ";"),
		m.AudioCameraID, audioAvail, addr, strings.Join(sorted, ","))
}

// buildMosaicArgs 构造 restart 模式（单 ffmpeg filter_complex）的完整命令参数：
// 黑底画布 + 每格 scale/fps 归一后 overlay 链式叠放。画面缺失（摄像头被删/禁用）
// 的格用 black color 兜底源，布局仍然成立；音频取 AudioCameraID 的源流音轨
//（可选映射，源无音轨时优雅退化为纯视频）。
func buildMosaicArgs(m config.Mosaic, cameras []config.Camera, base string,
	targets []string, bin string, logger *slog.Logger) []string {
	fps := m.FramerateOrDefault()
	avail := availableCameras(cameras)
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}

	// 输入：可用摄像头去重复用；缺失格每格一个 black 兜底源。
	// 兜底源带 -re 按帧率实时供给，防止全缺失时 lavfi 全速编码。
	cellSrc := make([]int, len(m.Cells))
	inputIdx := map[string]int{}
	for i, cell := range m.Cells {
		if _, alive := avail[cell.CameraID]; alive {
			if idx, ok := inputIdx[cell.CameraID]; ok {
				cellSrc[i] = idx
				continue
			}
			inputIdx[cell.CameraID] = len(inputIdx)
			cellSrc[i] = inputIdx[cell.CameraID]
			args = append(args, "-rtsp_transport", "tcp", "-i", mosaicCamURL(base, cell.CameraID))
			continue
		}
		cellSrc[i] = -1 - i // 负数标记兜底源，稍后统一追加输入
	}
	// 兜底输入追加在真实输入之后，重排 cellSrc 指向。
	off := len(inputIdx)
	for i, cell := range m.Cells {
		if cellSrc[i] < 0 {
			cellSrc[i] = off
			off++
			args = append(args, "-re", "-f", "lavfi", "-i",
				fmt.Sprintf("color=c=black:s=%dx%d:r=%d", cell.W, cell.H, fps))
		}
	}
	// 音频源：未在画面输入中出现时补一个独立输入（仅可用摄像头）。
	audioIdx := -1
	if m.AudioCameraID != "" {
		if _, alive := avail[m.AudioCameraID]; alive {
			if idx, ok := inputIdx[m.AudioCameraID]; ok {
				audioIdx = idx
			} else {
				audioIdx = off
				off++
				args = append(args, "-rtsp_transport", "tcp", "-i", mosaicCamURL(base, m.AudioCameraID))
			}
		}
	}

	// filter graph：黑底画布 → 每格归一 → overlay 链（cells 顺序即 z 序）。
	var graph strings.Builder
	fmt.Fprintf(&graph, "color=c=black:s=%dx%d:r=%d[base];", m.Width, m.Height, fps)
	for i, cell := range m.Cells {
		fmt.Fprintf(&graph, "[%d:v]scale=%d:%d,fps=%d,setsar=1[c%d];",
			cellSrc[i], cell.W, cell.H, fps, i)
	}
	prev := "base"
	for i, cell := range m.Cells {
		out := fmt.Sprintf("t%d", i)
		fmt.Fprintf(&graph, "[%s][c%d]overlay=x=%d:y=%d:shortest=0:eof_action=repeat[%s];",
			prev, i, cell.X, cell.Y, out)
		prev = out
	}
	fmt.Fprintf(&graph, "[%s]format=yuv420p[vout]", prev)

	args = append(args, "-filter_complex", graph.String(), "-map", "[vout]")
	if audioIdx >= 0 {
		args = append(args, "-map", fmt.Sprintf("%d:a?", audioIdx))
	} else {
		args = append(args, "-an")
	}
	gov := fps * 2
	if gov < 10 {
		gov = 10
	}
	args = append(args, videoEncodeArgs(resolveEncoder(bin, logger), gov, m.BitrateOrDefault())...)
	if audioIdx >= 0 {
		args = append(args, "-c:a", "aac", "-b:a", "64k", "-ar", "44100")
	}
	args = append(args, outputArgs(mosaicCamURL(base, m.ID), targets)...)
	return args
}
