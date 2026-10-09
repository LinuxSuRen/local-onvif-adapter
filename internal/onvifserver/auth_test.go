package onvifserver

import (
	"crypto/rand"
	"encoding/xml"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

const authTestUser = "admin"
const authTestPass = "s3cret"

// enableAuth 在测试服务上开启设备面认证。
func enableAuth(t *testing.T, s *Service) {
	t.Helper()
	if err := s.store.Update(func(r *config.Root) error {
		r.Server.AuthEnabled = true
		r.Server.AuthUser = authTestUser
		r.Server.AuthPass = authTestPass
		return nil
	}); err != nil {
		t.Fatalf("enable auth: %v", err)
	}
}

// authedCall 携带合法 UsernameToken(PasswordDigest) 调用。
func authedCall(t *testing.T, s *Service, inner string) (int, string) {
	t.Helper()
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Format(time.RFC3339)
	header := securityHeaderXML(authTestUser, authTestPass, nonce, created)
	body := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope">` +
		header + `<SOAP-ENV:Body>` + inner + `</SOAP-ENV:Body></SOAP-ENV:Envelope>`
	req := httptest.NewRequest("POST", "/onvif/device_service", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	respBody := rec.Body.String()
	var env struct {
		Body struct {
			Inner []byte `xml:",innerxml"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal([]byte(respBody), &env); err != nil {
		t.Fatalf("response is not valid SOAP envelope: %v\n%s", err, respBody)
	}
	return rec.Code, string(env.Body.Inner)
}

func TestAuthUnauthorizedWithoutToken(t *testing.T) {
	s := newTestService(t, twoCameras()...)
	enableAuth(t, s)
	// 401 应答为空 body(设备惯例),不能用期待 SOAP 信封的 call() 助手
	req := httptest.NewRequest("POST", "/onvif/device_service",
		strings.NewReader(`<e:Envelope xmlns:e="http://www.w3.org/2003/05/soap-envelope"><e:Body><trt:GetProfiles xmlns:trt="http://www.onvif.org/ver10/media/wsdl"/></e:Body></e:Envelope>`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("GetProfiles without token = %d, want 401", rec.Code)
	}
}

func TestAuthValidDigestAccepted(t *testing.T) {
	s := newTestService(t, twoCameras()...)
	enableAuth(t, s)
	code, body := authedCall(t, s, `<trt:GetProfiles/>`)
	if code != 200 {
		t.Fatalf("GetProfiles with valid digest = %d, want 200", code)
	}
	if !strings.Contains(body, "GetProfilesResponse") {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestAuthPreAuthOpsWhitelisted(t *testing.T) {
	s := newTestService(t, twoCameras()...)
	enableAuth(t, s)
	for _, op := range []struct {
		inner string
		want  string
	}{
		{`<tds:GetSystemDateAndTime/>`, "GetSystemDateAndTimeResponse"},
		{`<tds:GetCapabilities><tds:Category>All</tds:Category></tds:GetCapabilities>`, "GetCapabilitiesResponse"},
		{`<tds:GetServices><tds:IncludeCapability>true</tds:IncludeCapability></tds:GetServices>`, "GetServicesResponse"},
	} {
		code, body := call(t, s, op.inner)
		if code != 200 || !strings.Contains(body, op.want) {
			t.Fatalf("pre-auth op %s = %d/%.60s, want 200 with %s", op.inner, code, body, op.want)
		}
	}
}

func TestAuthWrongPasswordRejected(t *testing.T) {
	s := newTestService(t, twoCameras()...)
	enableAuth(t, s)
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Format(time.RFC3339)
	header := securityHeaderXML(authTestUser, "wrong-pass", nonce, created)
	body := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope">` +
		header + `<SOAP-ENV:Body><trt:GetProfiles/></SOAP-ENV:Body></SOAP-ENV:Envelope>`
	req := httptest.NewRequest("POST", "/onvif/device_service", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("GetProfiles with wrong password = %d, want 401", rec.Code)
	}
}

func TestAuthExpiredCreatedRejected(t *testing.T) {
	s := newTestService(t, twoCameras()...)
	enableAuth(t, s)
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	header := securityHeaderXML(authTestUser, authTestPass, nonce, created)
	body := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope">` +
		header + `<SOAP-ENV:Body><trt:GetProfiles/></SOAP-ENV:Body></SOAP-ENV:Envelope>`
	req := httptest.NewRequest("POST", "/onvif/device_service", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("GetProfiles with expired Created = %d, want 401", rec.Code)
	}
}

func TestAuthDisabledKeepsOpen(t *testing.T) {
	s := newTestService(t, twoCameras()...)
	// 不开启认证:无令牌也应放行(向后兼容)
	code, body := call(t, s, `<trt:GetProfiles/>`)
	if code != 200 || !strings.Contains(body, "GetProfilesResponse") {
		t.Fatalf("GetProfiles with auth disabled = %d/%.60s, want 200 with profiles", code, body)
	}
}
