// pushops.go 实现按需直推的厂商扩展 SOAP 动作（lns: 命名空间，
// 挂在标准 device_service 端点上；标准客户端不识别即不调用，互不可见）：
//
//	lns:StartPushStream  {ProfileToken, TargetURL} → {PushToken}
//	lns:RenewPushStream  {PushToken}               → 租约顺延
//	lns:StopPushStream   {PushToken}               → 停推
//	lns:GetPushStreams   {}                        → 会话列表（Target 脱敏）
//
// 动作不在 pre-auth 白名单：设备面认证开启时与其他高价值操作同策略。
package onvifserver

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// nsPush 厂商扩展动作命名空间（与事件话题命名空间相互独立）。
const nsPush = "http://www.linuxsuren.com/onvif/ext"

// PushSessionInfo 推流会话的对外快照（Target 已脱敏）。
type PushSessionInfo struct {
	Token        string
	CameraID     string
	ProfileToken string
	Target       string
	Status       string // starting | running | failed
	CreatedAt    time.Time
	ExpiresAt    time.Time
	LastError    string
}

// PushController 由媒体层（stream.Manager）实现的按需直推会话控制面。
type PushController interface {
	// StartPush 创建或幂等复用一路直推会话；exited 表示命中已有会话。
	StartPush(camID, profileToken, target string) (token string, existed bool, err error)
	// RenewPush 续租；会话不存在/已失败返回错误（调用方应重新 Start）。
	RenewPush(token string) error
	// StopPush 显式停止并移除会话。
	StopPush(token string) error
	// PushSessions 返回全部会话快照。
	PushSessions() []PushSessionInfo
}

// RedactURL 把 URL userinfo 中的口令打码为 ***（日志与事件统一走这里；
// 手工拼接而不用 url.UserPassword 序列化，避免对特殊字符二次百分号转义）。
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, hasPass := u.User.Password(); !hasPass {
		return raw
	}
	rest := u.Host + u.Path
	if u.RawQuery != "" {
		rest += "?" + u.RawQuery
	}
	return u.Scheme + "://" + u.User.Username() + ":***@" + rest
}

func registerPushOps() {
	reg(nsPush, "StartPushStream", opStartPushStream)
	reg(nsPush, "RenewPushStream", opRenewPushStream)
	reg(nsPush, "StopPushStream", opStopPushStream)
	reg(nsPush, "GetPushStreams", opGetPushStreams)
}

type pushTokenReq struct {
	PushToken string `xml:"PushToken"`
}

func opStartPushStream(s *Service, inner []byte) (string, error) {
	var req struct {
		ProfileToken string `xml:"ProfileToken"`
		TargetURL    string `xml:"TargetURL"`
	}
	if err := parseInner(inner, &req); err != nil {
		return "", err
	}
	if s.PushCtrl == nil {
		return "", errors.New("push streaming is not available")
	}
	v, ok := s.findByProfile(req.ProfileToken)
	if !ok {
		return "", fmt.Errorf("profile %q not found", req.ProfileToken)
	}
	if strings.TrimSpace(req.TargetURL) == "" {
		return "", errors.New("TargetURL is required")
	}
	token, _, err := s.PushCtrl.StartPush(v.Camera.ID, req.ProfileToken, req.TargetURL)
	if err != nil {
		return "", err
	}
	s.logger.Info("push stream started via onvif",
		"camera", v.Camera.ID, "profile", req.ProfileToken,
		"target", RedactURL(req.TargetURL), "token", token)
	return `<lns:StartPushStreamResponse><lns:PushToken>` +
		xmlEsc(token) + `</lns:PushToken></lns:StartPushStreamResponse>`, nil
}

func opRenewPushStream(s *Service, inner []byte) (string, error) {
	var req pushTokenReq
	if err := parseInner(inner, &req); err != nil {
		return "", err
	}
	if s.PushCtrl == nil {
		return "", errors.New("push streaming is not available")
	}
	if err := s.PushCtrl.RenewPush(req.PushToken); err != nil {
		return "", err
	}
	return `<lns:RenewPushStreamResponse/>`, nil
}

func opStopPushStream(s *Service, inner []byte) (string, error) {
	var req pushTokenReq
	if err := parseInner(inner, &req); err != nil {
		return "", err
	}
	if s.PushCtrl == nil {
		return "", errors.New("push streaming is not available")
	}
	if err := s.PushCtrl.StopPush(req.PushToken); err != nil {
		return "", err
	}
	s.logger.Info("push stream stopped via onvif", "token", req.PushToken)
	return `<lns:StopPushStreamResponse/>`, nil
}

func opGetPushStreams(s *Service, _ []byte) (string, error) {
	body := `<lns:GetPushStreamsResponse>`
	if s.PushCtrl != nil {
		for _, p := range s.PushCtrl.PushSessions() {
			body += `<lns:PushStreams Token="` + xmlEsc(p.Token) +
				`" Status="` + xmlEsc(p.Status) + `">` +
				`<lns:CameraID>` + xmlEsc(p.CameraID) + `</lns:CameraID>` +
				`<lns:ProfileToken>` + xmlEsc(p.ProfileToken) + `</lns:ProfileToken>` +
				`<lns:Target>` + xmlEsc(p.Target) + `</lns:Target>` +
				`<lns:CreatedAt>` + p.CreatedAt.UTC().Format(time.RFC3339) + `</lns:CreatedAt>` +
				`<lns:ExpiresAt>` + p.ExpiresAt.UTC().Format(time.RFC3339) + `</lns:ExpiresAt>` +
				`<lns:LastError>` + xmlEsc(p.LastError) + `</lns:LastError>` +
				`</lns:PushStreams>`
		}
	}
	return body + `</lns:GetPushStreamsResponse>`, nil
}
