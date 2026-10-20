package realsense

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
)

// Server 把 Source 的三路帧以 rawvideo over TCP 分发。
//
// 每个流一个端口：base+0=color、base+1=depth、base+2=ir。
// 每端口同一时刻只服务一个客户端（本项目的消费方是每路一个 ffmpeg 进程，
// 多观众由内嵌 RTSP 服务器扇出，这里不需要多播）；新客户端接入时顶掉旧连接。
// 无客户端时帧直接丢弃——采集设备不能因为没人看就停转（RS 独占语义下
// 也无法按流单独启停）。
//
// ffmpeg 端读取方式（帧格式为定长 rgb24 裸流，无需任何容器头）：
//
//	-f rawvideo -pixel_format rgb24 -video_size {w}x{h} -framerate {fps} -i tcp://127.0.0.1:{port}
type Server struct {
	src    Source
	logger *slog.Logger

	mu       sync.Mutex
	clients  map[StreamKind]net.Conn // 每流当前客户端
	listen   map[StreamKind]net.Listener
	closed   bool
	closedCh chan struct{} // Close 信号：Serve 主循环据此退出
}

// NewServer 创建分发服务器。logger 为 nil 时用 slog.Default()。
func NewServer(src Source, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{src: src, logger: logger, clients: map[StreamKind]net.Conn{},
		listen: map[StreamKind]net.Listener{}, closedCh: make(chan struct{})}
}

// Serve 在 base 端口起监听并开始取帧分发，直到出错或 Close。
// 只监听 Source 声明的流（缺流的设备少占端口）。
func (s *Server) Serve(base int) error {
	kinds := s.src.Streams()
	if len(kinds) == 0 {
		return errors.New("realsense: source provides no streams")
	}
	for i, k := range kinds {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
		if err != nil {
			s.Close()
			return fmt.Errorf("realsense: listen %s on %d: %w", k, base+i, err)
		}
		s.mu.Lock()
		s.listen[k] = ln
		s.mu.Unlock()
		go s.acceptLoop(k, ln)
	}
	s.logger.Info("rs-pipe serving", "base", base, "kinds", kinds)

	for {
		set, err := s.src.Poll()
		if err != nil {
			s.Close()
			return err
		}
		select {
		case <-s.closedCh:
			return nil
		default:
		}
		for k, f := range set {
			if f == nil || len(f.Data) == 0 {
				continue
			}
			s.mu.Lock()
			c := s.clients[k]
			s.mu.Unlock()
			if c == nil {
				continue // 无人观看，丢帧
			}
			if _, err := c.Write(f.Data); err != nil {
				// 客户端断开/写阻塞：关闭并等待新客户端。
				s.logger.Info("rs client gone", "kind", k, "err", err.Error())
				s.dropClient(k, c)
			}
		}
	}
}

// acceptLoop 每流接受客户端；新连接顶掉旧连接。
func (s *Server) acceptLoop(k StreamKind, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener 已关（Close 或出错）
		}
		s.mu.Lock()
		old := s.clients[k]
		s.clients[k] = conn
		closed := s.closed
		s.mu.Unlock()
		s.logger.Info("rs client connected", "kind", k, "remote", conn.RemoteAddr().String())
		if old != nil {
			_ = old.Close()
		}
		if closed {
			_ = conn.Close()
			return
		}
	}
}

// dropClient 关闭指定客户端连接；若仍是当前客户端则清空槽位。
func (s *Server) dropClient(k StreamKind, c net.Conn) {
	s.mu.Lock()
	if s.clients[k] == c {
		delete(s.clients, k)
	}
	s.mu.Unlock()
	_ = c.Close()
}

// Port 返回指定流实际监听的端口（Serve 启动后才有效）。
func (s *Server) Port(k StreamKind) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ln, ok := s.listen[k]
	if !ok {
		return 0, false
	}
	return ln.Addr().(*net.TCPAddr).Port, true
}

// Close 停止监听并断开全部客户端（幂等）。Source 的停止由调用方负责。
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.closedCh)
	listeners := s.listen
	clients := s.clients
	s.listen = map[StreamKind]net.Listener{}
	s.clients = map[StreamKind]net.Conn{}
	s.mu.Unlock()
	for _, ln := range listeners {
		_ = ln.Close()
	}
	for _, c := range clients {
		_ = c.Close()
	}
	return nil
}

// 确保实现 io.Closer 语义完整（Close 不关闭 Source，资源各自管理）。
var _ io.Closer = (*Server)(nil)
