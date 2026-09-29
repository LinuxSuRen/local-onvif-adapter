package onvifserver

import (
	"net/http"
)

// 各服务的 WSDL 命名空间。
const (
	nsDevice  = "http://www.onvif.org/ver10/device/wsdl"
	nsMedia   = "http://www.onvif.org/ver10/media/wsdl"
	nsPTZ     = "http://www.onvif.org/ver20/ptz/wsdl"
	nsImaging = "http://www.onvif.org/ver10/imaging/wsdl"
)

// opHandler 处理一个 ONVIF 操作；返回响应 body 片段，
// err 非 nil 时以 SOAP Fault 形式返回（对客户端表现为此操作不支持或参数错误）。
type opHandler func(s *Service, inner []byte) (string, error)

var (
	// dispatchNS 按「命名空间|操作名」精确分发。
	dispatchNS map[string]opHandler
	// dispatchLocal 仅按操作名分发（无命名空间或未匹配时的兜底），
	// 同名操作（如 tptz:Stop 与 timg:Stop）取先注册者。
	dispatchLocal map[string]opHandler
)

func init() {
	dispatchNS = map[string]opHandler{}
	dispatchLocal = map[string]opHandler{}
	registerDeviceOps()
	registerMediaOps()
	registerPTZOps()
	registerImagingOps()
}

func reg(ns, op string, h opHandler) {
	dispatchNS[ns+"|"+op] = h
	if _, exists := dispatchLocal[op]; !exists {
		dispatchLocal[op] = h
	}
}

// lookupHandler 命名空间精确匹配优先，回退到操作名匹配。
func lookupHandler(ns, op string) (opHandler, bool) {
	if ns != "" {
		if h, ok := dispatchNS[ns+"|"+op]; ok {
			return h, true
		}
	}
	h, ok := dispatchLocal[op]
	return h, ok
}

// Handler 返回 ONVIF SOAP HTTP 处理器。
// 鉴权策略：不校验任何凭据（digest/WS-Security 头一律忽略并直接放行），
// 保证各类客户端不因认证问题报错。
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "ONVIF service accepts POST only", http.StatusMethodNotAllowed)
			return
		}
		req, err := parseSOAPRequest(r)
		if err != nil {
			s.logger.Warn("onvif request parse failed", "err", err.Error(), "remote", r.RemoteAddr)
			writeSOAP(w, soapFault("ter:InvalidArgVal", "malformed SOAP request: "+err.Error()))
			return
		}
		handler, ok := lookupHandler(req.NS, req.Op)
		if !ok {
			s.logger.Info("onvif op not supported", "op", req.Op, "ns", req.NS, "remote", r.RemoteAddr)
			writeSOAP(w, soapFault("ter:ActionNotSupported", "operation "+req.Op+" is not supported"))
			return
		}
		body, err := handler(s, req.Inner)
		if err != nil {
			s.logger.Warn("onvif op failed", "op", req.Op, "err", err.Error())
			writeSOAP(w, soapFault("ter:ActionNotSupported", "operation "+req.Op+" failed: "+err.Error()))
			return
		}
		s.logger.Debug("onvif op handled", "op", req.Op, "remote", r.RemoteAddr)
		writeSOAP(w, soapEnvelope(body))
	})
}

func writeSOAP(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
