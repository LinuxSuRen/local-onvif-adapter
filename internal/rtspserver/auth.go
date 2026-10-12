package rtspserver

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/auth"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
)

// Realm 是 RTSP 401 挑战的设备域名。
const Realm = "local-onvif"

// nonceLifetime 控制 Digest nonce 的轮换周期；轮换后保留上一枚
// （宽限期内客户端仍可用旧 nonce 完成 challenge-response 往返）。
const nonceLifetime = 5 * time.Minute

// authorizer 实现 RTSP 设备面认证（RFC 2617 复用）：
//   - 凭证由 creds 闭包按请求实时获取（支持运行时开关认证）；
//   - Digest(MD5，无 qop 兼容形态) 与 Basic 同通告，ffmpeg/VLC/
//     gortsplib 全支持；nonce 周期轮换防长期重放。
type authorizer struct {
	creds func() (user, pass string, ok bool)

	mu       sync.Mutex
	current  string
	previous string
	swapped  time.Time
}

func newAuthorizer(creds func() (string, string, bool)) *authorizer {
	return &authorizer{creds: creds}
}

// rotateIfNeeded 按周期轮换 nonce（调用方持锁）。
func (a *authorizer) rotateIfNeeded() {
	if time.Since(a.swapped) < nonceLifetime && a.current != "" {
		return
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		// 随机源不可用属系统级异常,退化为时间熵
		now := time.Now().UnixNano()
		for i := range nonce {
			nonce[i] = byte(now >> (uint(i%8) * 8))
		}
	}
	a.previous = a.current
	a.current = hex.EncodeToString(nonce)
	a.swapped = time.Now()
}

// challengeValue 返回 WWW-Authenticate 头值（Digest + Basic 双通告）。
func (a *authorizer) challengeValue() base.HeaderValue {
	a.mu.Lock()
	a.rotateIfNeeded()
	nonce := a.current
	a.mu.Unlock()
	return base.HeaderValue{
		fmt.Sprintf(`Digest realm="%s", nonce="%s", algorithm=MD5`, Realm, nonce),
		fmt.Sprintf(`Basic realm="%s"`, Realm),
	}
}

// challenge 构造 401 应答。
func (a *authorizer) challenge() *base.Response {
	return &base.Response{
		StatusCode: base.StatusUnauthorized,
		Header:     base.Header{"WWW-Authenticate": a.challengeValue()},
	}
}

// verify 校验请求的 Authorization 头；未启用认证时放行。
func (a *authorizer) verify(req *base.Request) bool {
	user, pass, ok := a.creds()
	if !ok {
		return true
	}
	if req == nil || len(req.Header["Authorization"]) == 0 {
		return false
	}
	a.mu.Lock()
	a.rotateIfNeeded()
	current, previous := a.current, a.previous
	a.mu.Unlock()
	if err := auth.Verify(req, user, pass, nil, Realm, current); err == nil {
		return true
	}
	if previous != "" {
		return auth.Verify(req, user, pass, nil, Realm, previous) == nil
	}
	return false
}
