package rtspserver

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/liberrors"
)

// newRecordingLogger 返回写入 buffer 的 Debug 级 logger 与 buffer 本身。
func newRecordingLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// TestOnStreamWriteErrorThrottle 写队列满（慢读者背压丢帧）应降为 Debug 且按会话节流；
// 其他写错误保持 Warn 且不节流。
func TestOnStreamWriteErrorThrottle(t *testing.T) {
	logger, buf := newRecordingLogger()
	s := New("127.0.0.1:0", "", "", logger)
	s.SlowLogInterval = 30 * time.Millisecond
	sess := &gortsplib.ServerSession{}

	full := &gortsplib.ServerHandlerOnStreamWriteErrorCtx{
		Session: sess, Error: liberrors.ErrServerWriteQueueFull{},
	}
	// 间隔内连续 3 次只记 1 条。
	s.OnStreamWriteError(full)
	s.OnStreamWriteError(full)
	s.OnStreamWriteError(full)
	if got := strings.Count(buf.String(), "rtsp reader slow"); got != 1 {
		t.Fatalf("throttled logs = %d, want 1:\n%s", got, buf.String())
	}
	if strings.Contains(buf.String(), "level=INFO") {
		t.Fatalf("slow reader log should be Debug, not INFO:\n%s", buf.String())
	}
	// 超过间隔后再记 1 条。
	time.Sleep(40 * time.Millisecond)
	s.OnStreamWriteError(full)
	if got := strings.Count(buf.String(), "rtsp reader slow"); got != 2 {
		t.Fatalf("logs after interval = %d, want 2:\n%s", got, buf.String())
	}

	// 其他写错误：Warn 且每次都记。
	boom := &gortsplib.ServerHandlerOnStreamWriteErrorCtx{
		Session: sess, Error: errors.New("connection reset"),
	}
	s.OnStreamWriteError(boom)
	s.OnStreamWriteError(boom)
	if got := strings.Count(buf.String(), "rtsp reader write failed"); got != 2 {
		t.Fatalf("non-throttle logs = %d, want 2:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("write failure should be WARN:\n%s", buf.String())
	}
}

// TestOnStreamWriteErrorSessionCleanup 会话关闭后应清理节流状态。
func TestOnStreamWriteErrorSessionCleanup(t *testing.T) {
	logger, buf := newRecordingLogger()
	s := New("127.0.0.1:0", "", "", logger)
	s.SlowLogInterval = time.Hour // 长间隔下验证清理后可再记
	sess := &gortsplib.ServerSession{}

	s.OnStreamWriteError(&gortsplib.ServerHandlerOnStreamWriteErrorCtx{
		Session: sess, Error: liberrors.ErrServerWriteQueueFull{},
	})
	s.OnSessionClose(&gortsplib.ServerHandlerOnSessionCloseCtx{Session: sess})
	s.OnStreamWriteError(&gortsplib.ServerHandlerOnStreamWriteErrorCtx{
		Session: sess, Error: liberrors.ErrServerWriteQueueFull{},
	})
	if got := strings.Count(buf.String(), "rtsp reader slow"); got != 2 {
		t.Fatalf("logs after session close = %d, want 2 (state cleaned):\n%s", got, buf.String())
	}
}
