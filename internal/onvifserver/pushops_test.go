package onvifserver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

// fakePushCtrl 记录调用并返回可控结果的 PushController 桩。
type fakePushCtrl struct {
	startErr error
	renewErr error
	stopErr  error
	sessions []PushSessionInfo
	started  []string // 每次成功 StartPush 记录 "camID|target"
}

func (f *fakePushCtrl) StartPush(camID, _, target string) (string, bool, error) {
	if f.startErr != nil {
		return "", false, f.startErr
	}
	f.started = append(f.started, camID+"|"+target)
	return "push_test01", false, nil
}

func (f *fakePushCtrl) RenewPush(string) error          { return f.renewErr }
func (f *fakePushCtrl) StopPush(string) error           { return f.stopErr }
func (f *fakePushCtrl) PushSessions() []PushSessionInfo { return f.sessions }

// callPush 以 lns: 命名空间调用扩展动作。
func callPush(t *testing.T, s *Service, op, inner string) (int, string) {
	t.Helper()
	body := `<lns:` + op + ` xmlns:lns="` + nsPush + `">` + inner + `</lns:` + op + `>`
	return call(t, s, body)
}

func pushCam() config.Camera {
	return config.Camera{ID: "cam1", Type: config.TypeTestSrc, Enabled: true}
}

func TestPushOpsStart(t *testing.T) {
	s := newTestService(t, pushCam())
	ctrl := &fakePushCtrl{}
	s.PushCtrl = ctrl

	code, body := callPush(t, s, "StartPushStream",
		`<lns:ProfileToken>profile_cam1</lns:ProfileToken>`+
			`<lns:TargetURL>rtsp://user:pass@cloud:8554/robot/r1/cam1</lns:TargetURL>`)
	if code != 200 || !strings.Contains(body, "<lns:PushToken>push_test01</lns:PushToken>") {
		t.Fatalf("StartPushStream: code=%d body=%s", code, body)
	}
	if len(ctrl.started) != 1 || ctrl.started[0] != "cam1|rtsp://user:pass@cloud:8554/robot/r1/cam1" {
		t.Fatalf("controller got %v", ctrl.started)
	}

	// 未知 profile / 缺 TargetURL / 媒体层不可用 → Fault。
	code, body = callPush(t, s, "StartPushStream",
		`<lns:ProfileToken>profile_nope</lns:ProfileToken><lns:TargetURL>rtsp://cloud</lns:TargetURL>`)
	if code != 200 || !strings.Contains(body, "Fault") || !strings.Contains(body, "not found") {
		t.Fatalf("unknown profile should fault: code=%d body=%s", code, body)
	}
	code, body = callPush(t, s, "StartPushStream", `<lns:ProfileToken>profile_cam1</lns:ProfileToken>`)
	if !strings.Contains(body, "Fault") || !strings.Contains(body, "TargetURL is required") {
		t.Fatalf("missing TargetURL should fault: %s", body)
	}
	s.PushCtrl = nil
	code, body = callPush(t, s, "StartPushStream",
		`<lns:ProfileToken>profile_cam1</lns:ProfileToken><lns:TargetURL>rtsp://cloud</lns:TargetURL>`)
	if !strings.Contains(body, "Fault") || !strings.Contains(body, "not available") {
		t.Fatalf("nil controller should fault: %s", body)
	}
}

func TestPushOpsRenewStop(t *testing.T) {
	s := newTestService(t, pushCam())
	s.PushCtrl = &fakePushCtrl{}

	code, body := callPush(t, s, "RenewPushStream", `<lns:PushToken>push_x</lns:PushToken>`)
	if code != 200 || !strings.Contains(body, "<lns:RenewPushStreamResponse/>") {
		t.Fatalf("RenewPushStream: code=%d body=%s", code, body)
	}
	code, body = callPush(t, s, "StopPushStream", `<lns:PushToken>push_x</lns:PushToken>`)
	if code != 200 || !strings.Contains(body, "<lns:StopPushStreamResponse/>") {
		t.Fatalf("StopPushStream: code=%d body=%s", code, body)
	}

	// 媒体层报错（会话不存在/已失败）→ Fault，controller 据此重新 Start。
	s.PushCtrl = &fakePushCtrl{renewErr: errors.New("push session \"push_x\" not found")}
	_, body = callPush(t, s, "RenewPushStream", `<lns:PushToken>push_x</lns:PushToken>`)
	if !strings.Contains(body, "Fault") || !strings.Contains(body, "not found") {
		t.Fatalf("renew missing session should fault: %s", body)
	}
	s.PushCtrl = &fakePushCtrl{stopErr: errors.New("push session \"push_x\" not found")}
	_, body = callPush(t, s, "StopPushStream", `<lns:PushToken>push_x</lns:PushToken>`)
	if !strings.Contains(body, "Fault") {
		t.Fatalf("stop missing session should fault: %s", body)
	}
}

func TestPushOpsList(t *testing.T) {
	s := newTestService(t, pushCam())
	now := time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)
	s.PushCtrl = &fakePushCtrl{sessions: []PushSessionInfo{{
		Token:        "push_test01",
		CameraID:     "cam1",
		ProfileToken: "profile_cam1",
		Target:       "rtsp://user:***@cloud:8554/robot/r1/cam1",
		Status:       "running",
		CreatedAt:    now,
		ExpiresAt:    now.Add(10 * time.Minute),
	}}}
	code, body := callPush(t, s, "GetPushStreams", "")
	if code != 200 {
		t.Fatalf("GetPushStreams code=%d", code)
	}
	for _, want := range []string{
		`Token="push_test01"`, `Status="running"`,
		"<lns:CameraID>cam1</lns:CameraID>",
		"<lns:Target>rtsp://user:***@cloud:8554/robot/r1/cam1</lns:Target>",
		"<lns:CreatedAt>2025-07-01T12:00:00Z</lns:CreatedAt>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("GetPushStreams missing %q in %s", want, body)
		}
	}
	// 无控制器时返回空列表而非 Fault（能力探测友好）。
	s.PushCtrl = nil
	code, body = callPush(t, s, "GetPushStreams", "")
	if code != 200 || strings.Contains(body, "PushStreams Token=") {
		t.Fatalf("nil controller should list nothing: code=%d body=%s", code, body)
	}
}

// TestPushOpsRequireAuth 推流动作不在 pre-auth 白名单：认证开启时必须过 UsernameToken。
func TestPushOpsRequireAuth(t *testing.T) {
	s := newTestService(t, pushCam())
	s.PushCtrl = &fakePushCtrl{}
	if err := s.store.Update(func(r *config.Root) error {
		r.Server.AuthEnabled, r.Server.AuthUser, r.Server.AuthPass = true, "admin", "secret"
		return nil
	}); err != nil {
		t.Fatalf("enable auth: %v", err)
	}
	// 401 应答为空 body（设备惯例），不能用期待 SOAP 信封的 call() 助手。
	req := httptest.NewRequest("POST", "/onvif/device_service",
		strings.NewReader(`<e:Envelope xmlns:e="http://www.w3.org/2003/05/soap-envelope"><e:Body>`+
			`<lns:GetPushStreams xmlns:lns="`+nsPush+`"/></e:Body></e:Envelope>`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("push op without credentials = %d, want 401", rec.Code)
	}
}

func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"rtsp://user:pass@cloud:8554/live/x": "rtsp://user:***@cloud:8554/live/x",
		"rtmp://k@ingest/live/key":           "rtmp://k@ingest/live/key", // 无口令不打码
		"rtsp://cloud:8554/live/x":           "rtsp://cloud:8554/live/x",
		"srt://cloud:9000?streamid=a/b":      "srt://cloud:9000?streamid=a/b",
		"rtsp://u:p@h:1/a?b=c":               "rtsp://u:***@h:1/a?b=c",
		"://invalid":                         "://invalid",
	}
	for in, want := range cases {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}
