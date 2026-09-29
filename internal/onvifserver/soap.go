package onvifserver

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
)

const envelopeHeader = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope"` +
	` xmlns:tds="http://www.onvif.org/ver10/device/wsdl"` +
	` xmlns:tt="http://www.onvif.org/ver10/schema"` +
	` xmlns:trt="http://www.onvif.org/ver10/media/wsdl"` +
	` xmlns:tptz="http://www.onvif.org/ver20/ptz/wsdl"` +
	` xmlns:timg="http://www.onvif.org/ver10/imaging/wsdl"` +
	` xmlns:wsa="http://schemas.xmlsoap.org/ws/2004/08/addressing">`

// soapEnvelope 包裹响应体片段。
func soapEnvelope(body string) []byte {
	return []byte(envelopeHeader + "<SOAP-ENV:Body>" + body + "</SOAP-ENV:Body></SOAP-ENV:Envelope>")
}

// soapFault 构造 SOAP 1.2 Fault。
func soapFault(subcode, text string) []byte {
	body := `<SOAP-ENV:Fault><SOAP-ENV:Code><SOAP-ENV:Value>SOAP-ENV:Sender</SOAP-ENV:Value>` +
		`<SOAP-ENV:Subcode><SOAP-ENV:Value>` + xmlEsc(subcode) + `</SOAP-ENV:Value></SOAP-ENV:Subcode></SOAP-ENV:Code>` +
		`<SOAP-ENV:Reason><SOAP-ENV:Text xml:lang="en">` + xmlEsc(text) + `</SOAP-ENV:Text></SOAP-ENV:Reason></SOAP-ENV:Fault>`
	return soapEnvelope(body)
}

// xmlEsc 转义 XML 文本。
func xmlEsc(s string) string {
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(s)); err != nil {
		return ""
	}
	return buf.String()
}

// soapRequest 是从请求体中解析出的操作名、命名空间与操作元素（含命名空间声明）。
type soapRequest struct {
	Op    string
	NS    string
	Inner []byte
}

// parseSOAPRequest 从 HTTP 请求体解析操作名（Body 首个子元素的 local name）
// 与其 innerxml。兼容带/不带命名空间前缀、带/不带 SOAPAction 头的客户端。
func parseSOAPRequest(r *http.Request) (*soapRequest, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var env struct {
		Body struct {
			Inner []byte `xml:",innerxml"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("parse envelope: %w", err)
	}
	if len(bytes.TrimSpace(env.Body.Inner)) == 0 {
		return nil, fmt.Errorf("empty soap body")
	}
	var root struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal(env.Body.Inner, &root); err != nil {
		return nil, fmt.Errorf("parse operation: %w", err)
	}
	// 保留完整操作元素（含 xmlns 声明），请求结构体按其子元素解析。
	return &soapRequest{Op: root.XMLName.Local, NS: root.XMLName.Space, Inner: env.Body.Inner}, nil
}
