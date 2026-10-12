// Package rtspserver 提供内嵌的 RTSP 服务器（基于 gortsplib，mediamtx 同款底层库）：
// 接收 ffmpeg 的推流（ANNOUNCE/RECORD），并向多个读者（DESCRIBE/SETUP/PLAY）分发，
// 用于替代独立部署的 mediamtx，实现纯单二进制部署。
package rtspserver

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/pion/rtp"
)

// DefaultPublisherWait 读者等待发布者就绪的默认时长。
const DefaultPublisherWait = 5 * time.Second

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

	// Creds 按请求实时返回生效凭证（返回 ok=false 表示认证关闭）；
	// 设置后 DESCRIBE/SETUP/PLAY 与 ANNOUNCE 均要求 Basic/Digest 认证，
	// ffmpeg 推流端需在推流地址注入 userinfo（见 main.go）。
	Creds func() (user, pass string, ok bool)

	mu     sync.Mutex
	paths  map[string]*pathEntry
	closed bool
}

// New 创建内嵌 RTSP 服务器。
// rtspAddr 形如 ":8554"；udpRTPAddr/udpRTCPAddr 为空表示仅支持 TCP 传输。
func New(rtspAddr, udpRTPAddr, udpRTCPAddr string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		logger:        logger,
		PublisherWait: DefaultPublisherWait,
		paths:         map[string]*pathEntry{},
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
