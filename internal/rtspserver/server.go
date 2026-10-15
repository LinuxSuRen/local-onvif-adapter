// Package rtspserver 提供内嵌的 RTSP 服务器（基于 gortsplib，mediamtx 同款底层库）：
// 接收 ffmpeg 的推流（ANNOUNCE/RECORD），并向多个读者（DESCRIBE/SETUP/PLAY）分发，
// 用于替代独立部署的 mediamtx，实现纯单二进制部署。
package rtspserver

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/liberrors"
	"github.com/linuxsuren/local-onvif-adapter/internal/onvifserver"
	"github.com/pion/rtp"
)

// DefaultPublisherWait 读者等待发布者就绪的默认时长。
const DefaultPublisherWait = 5 * time.Second

// DefaultSlowLogInterval 慢读者日志节流的默认间隔。
const DefaultSlowLogInterval = 5 * time.Second

// pathEntry 一个路径（/cam/<id>）对应的流与发布者。
type pathEntry struct {
	stream    *gortsplib.ServerStream
	publisher *gortsplib.ServerSession
}

// Server 内嵌 RTSP 服务器。生命周期：Start 后阻塞于 Wait；Close 停止一切。
type Server struct {
	srv    *gortsplib.Server
	logger *slog.Logger
	authz  *authorizer

	// PublisherWait 控制读者在发布者未就绪时的等待时长。
	PublisherWait time.Duration

	// SlowLogInterval 同一会话慢读者（写队列满、背压丢帧）日志的最小间隔，
	// 避免慢客户端把日志刷屏。测试可调短。
	SlowLogInterval time.Duration

	// Creds 按请求实时返回生效凭证（返回 ok=false 表示认证关闭）；
	// 设置后 DESCRIBE/SETUP/PLAY 与 ANNOUNCE 均要求 Basic/Digest 认证，
	// ffmpeg 推流端需在推流地址注入 userinfo（见 main.go）。
	Creds func() (user, pass string, ok bool)

	mu       sync.Mutex
	paths    map[string]*pathEntry
	closed   bool
	slowMu   sync.Mutex
	slowLast map[*gortsplib.ServerSession]time.Time
}

// New 创建内嵌 RTSP 服务器。
// rtspAddr 形如 ":8554"；udpRTPAddr/udpRTCPAddr 为空表示仅支持 TCP 传输。
func New(rtspAddr, udpRTPAddr, udpRTCPAddr string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		logger:          logger,
		PublisherWait:   DefaultPublisherWait,
		SlowLogInterval: DefaultSlowLogInterval,
		paths:           map[string]*pathEntry{},
		slowLast:        map[*gortsplib.ServerSession]time.Time{},
	}
	s.authz = newAuthorizer(func() (string, string, bool) {
		if s.Creds == nil {
			return "", "", false
		}
		return s.Creds()
	})
	s.srv = &gortsplib.Server{
		Handler:     s,
		RTSPAddress: rtspAddr,
	}
	if udpRTPAddr != "" {
		s.srv.UDPRTPAddress = udpRTPAddr
		s.srv.UDPRTCPAddress = udpRTCPAddr
	}
	return s
}

// Start 启动监听。
func (s *Server) Start() error { return s.srv.Start() }

// Wait 阻塞直到服务器出错或被 Close。
func (s *Server) Wait() error { return s.srv.Wait() }

// Close 停止服务器并断开全部会话。
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	for path, e := range s.paths {
		if e.stream != nil {
			e.stream.Close()
		}
		delete(s.paths, path)
	}
	s.mu.Unlock()
	s.srv.Close()
}

// lookup 等待指定路径的流就绪，超时返回 nil。
func (s *Server) lookup(path string, timeout time.Duration) *pathEntry {
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		e, ok := s.paths[path]
		closed := s.closed
		s.mu.Unlock()
		if ok {
			return e
		}
		if closed || time.Now().After(deadline) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ---- ServerHandler 实现 ----

// OnStreamWriteError 读者写失败回调。gortsplib 在 handler 未实现本接口时会用
// log.Println 兜底（经 slog.SetDefault 桥接后表现为无上下文的 INFO 刷屏）。
// 写队列满是慢读者的正常背压丢帧：降为 Debug 并按会话节流；
// 其他写错误（如连接损坏）保持 Warn 且不节流——它们通常紧接着会话关闭。
func (s *Server) OnStreamWriteError(ctx *gortsplib.ServerHandlerOnStreamWriteErrorCtx) {
	if errors.Is(ctx.Error, liberrors.ErrServerWriteQueueFull{}) {
		if s.allowSlowLog(ctx.Session) {
			s.logger.Debug("rtsp reader slow, packets dropped",
				"path", ctx.Session.SetuppedPath())
		}
		return
	}
	s.logger.Warn("rtsp reader write failed",
		"path", ctx.Session.SetuppedPath(), "err", ctx.Error.Error())
}

// allowSlowLog 同一会话在 SlowLogInterval 内最多放行一条慢读者日志。
func (s *Server) allowSlowLog(sess *gortsplib.ServerSession) bool {
	s.slowMu.Lock()
	defer s.slowMu.Unlock()
	now := time.Now()
	if last, ok := s.slowLast[sess]; ok && now.Sub(last) < s.SlowLogInterval {
		return false
	}
	s.slowLast[sess] = now
	return true
}

// OnConnOpen 连接建立。
func (s *Server) OnConnOpen(_ *gortsplib.ServerHandlerOnConnOpenCtx) {}

// OnConnClose 连接关闭。
func (s *Server) OnConnClose(ctx *gortsplib.ServerHandlerOnConnCloseCtx) {
	s.logger.Debug("rtsp conn closed", "err", fmt.Sprint(ctx.Error))
}

// OnSessionOpen 会话建立。
func (s *Server) OnSessionOpen(_ *gortsplib.ServerHandlerOnSessionOpenCtx) {}

// OnSessionClose 会话关闭；发布者离开时释放对应路径（读者随之断开，
// 客户端会自动重连，配合 ffmpeg 退避重推实现秒级恢复，与 mediamtx 语义一致）。
func (s *Server) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	s.slowMu.Lock()
	delete(s.slowLast, ctx.Session)
	s.slowMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	for path, e := range s.paths {
		if e.publisher == ctx.Session {
			if e.stream != nil {
				e.stream.Close()
			}
			delete(s.paths, path)
			s.logger.Info("rtsp publisher gone", "path", path)
		}
	}
}

// OnDescribe 读者描述请求：返回该路径的流；发布者未就绪时等待。
func (s *Server) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if !s.authz.verify(ctx.Request) {
		s.logger.Info("rtsp describe unauthorized", "remote", fmt.Sprintf("%v", ctx.Conn.NetConn().RemoteAddr()))
		onvifserver.PublishEvent("lns:Security/AuthFailure",
			[]onvifserver.EventItem{{Name: "Service", Value: "RTSP"}}, []onvifserver.EventItem{{Name: "State", Value: "Failed"}})
		return s.authz.challenge(), nil, nil
	}
	e := s.lookup(ctx.Path, s.PublisherWait)
	if e == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	s.logger.Debug("rtsp describe", "path", ctx.Path)
	return &base.Response{StatusCode: base.StatusOK}, e.stream, nil
}

// OnAnnounce 发布者通告：为路径创建流；已有发布者则踢旧接管（ffmpeg 重启场景）。
func (s *Server) OnAnnounce(ctx *gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	if !s.authz.verify(ctx.Request) {
		// 不发 AuthFailure 事件：announce 是自家 ffmpeg 推流端的 401 重试握手
		//（带凭证 URL 首次不带 Authorization 被挑战属正常流程），发布事件只会
		// 造成每次启动一条自伤噪音；读者路径（describe/setup）的拒绝才是安全信号
		s.logger.Info("rtsp announce unauthorized", "remote", fmt.Sprintf("%v", ctx.Conn.NetConn().RemoteAddr()))
		return s.authz.challenge(), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("server is closing")
	}
	if old, ok := s.paths[ctx.Path]; ok {
		if old.publisher != nil && old.publisher != ctx.Session {
			old.publisher.Close()
		}
		if old.stream != nil {
			old.stream.Close()
		}
		delete(s.paths, ctx.Path)
	}
	stream := &gortsplib.ServerStream{
		Server: s.srv,
		Desc:   ctx.Description,
	}
	if err := stream.Initialize(); err != nil {
		return nil, fmt.Errorf("initialize stream: %w", err)
	}
	s.paths[ctx.Path] = &pathEntry{stream: stream, publisher: ctx.Session}
	s.logger.Info("rtsp publisher announced",
		"path", ctx.Path, "medias", len(ctx.Description.Medias))
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// OnSetup 会话参数协商：发布者直接放行；读者返回对应路径的流。
func (s *Server) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if !s.authz.verify(ctx.Request) {
		s.logger.Info("rtsp setup unauthorized", "remote", fmt.Sprintf("%v", ctx.Conn.NetConn().RemoteAddr()))
		onvifserver.PublishEvent("lns:Security/AuthFailure",
			[]onvifserver.EventItem{{Name: "Service", Value: "RTSP"}}, []onvifserver.EventItem{{Name: "State", Value: "Failed"}})
		return s.authz.challenge(), nil, nil
	}
	if ctx.Session.State() == gortsplib.ServerSessionStatePreRecord {
		return &base.Response{StatusCode: base.StatusOK}, nil, nil
	}
	e := s.lookup(ctx.Path, s.PublisherWait)
	if e == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, e.stream, nil
}

// OnPlay 读者开始播放。
func (s *Server) OnPlay(_ *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// OnRecord 发布者开始录制：把发布者的 RTP 包广播给全部读者。
func (s *Server) OnRecord(ctx *gortsplib.ServerHandlerOnRecordCtx) (*base.Response, error) {
	s.mu.Lock()
	e, ok := s.paths[ctx.Path]
	s.mu.Unlock()
	if !ok || e.publisher != ctx.Session {
		return nil, fmt.Errorf("no announce for path %q", ctx.Path)
	}
	ctx.Session.OnPacketRTPAny(func(medi *description.Media, _ format.Format, pkt *rtp.Packet) {
		if err := e.stream.WritePacketRTP(medi, pkt); err != nil {
			s.logger.Warn("rtsp distribute failed", "err", err.Error())
		}
	})
	s.logger.Info("rtsp publisher recording", "path", ctx.Path)
	return &base.Response{StatusCode: base.StatusOK}, nil
}
