package onvifserver

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"time"
)

// createdWindow 是 WS-UsernameToken 的时间窗（±5 分钟，ONVIF 惯例），
// 是重放防护的主要时间界。
const createdWindow = 5 * time.Minute

// preAuthOps 免认证操作：对时与能力发现是认证的前置依赖
// （客户端先 GetSystemDateAndTime 对钟才能算出合法 Created）。
var preAuthOps = map[string]bool{
	"GetSystemDateAndTime": true,
	"GetCapabilities":      true,
	"GetServices":          true,
}

// usernameToken 是 SOAP Header 内的 WS-Security 令牌（命名空间前缀无关；
// 路径必须逐层写全 Header>Security>UsernameToken，Go 的 xml 路径不做递归搜索）。
type usernameToken struct {
	Username string `xml:"Header>Security>UsernameToken>Username"`
	Password struct {
		Type  string `xml:"Type,attr"`
		Value string `xml:",chardata"`
	} `xml:"Header>Security>UsernameToken>Password"`
	Nonce   string `xml:"Header>Security>UsernameToken>Nonce"`
	Created string `xml:"Header>Security>UsernameToken>Created"`
}

// verifyUsernameToken 校验 WS-Security UsernameToken（OASIS Profile 1.0）：
//   - PasswordDigest：Base64(SHA1(nonce 原始字节 + Created + 口令)) 比对，
//     Created 须落在时间窗内；
//   - PasswordText：明文比对（兼容能力较弱的客户端）。
//
// 比较使用 hmac.Equal 防时序侧信道。
func verifyUsernameToken(envelope []byte, user, pass string) bool {
	var tok usernameToken
	if err := xml.Unmarshal(envelope, &tok); err != nil || tok.Username == "" {
		return false
	}
	if !hmac.Equal([]byte(tok.Username), []byte(user)) {
		return false
	}
	if tok.Password.Type == "" ||
		tok.Password.Type == "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordText" {
		return hmac.Equal([]byte(tok.Password.Value), []byte(pass))
	}
	if tok.Password.Type != "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest" {
		return false
	}
	if tok.Nonce == "" || tok.Created == "" {
		return false
	}
	created, err := time.Parse(time.RFC3339, tok.Created)
	if err != nil || time.Since(created) > createdWindow {
		return false
	}
	nonce, err := base64.StdEncoding.DecodeString(tok.Nonce)
	if err != nil {
		return false
	}
	sum := sha1.Sum(append(append(append([]byte{}, nonce...),
		[]byte(tok.Created)...), []byte(pass)...))
	expected := base64.StdEncoding.EncodeToString(sum[:])
	return hmac.Equal([]byte(expected), []byte(tok.Password.Value))
}

// tokenDigest 供测试与联调使用：按规范公式计算摘要。
func tokenDigest(nonce []byte, created, pass string) string {
	sum := sha1.Sum(append(append(append([]byte{}, nonce...),
		[]byte(created)...), []byte(pass)...))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// securityHeaderXML 构造标准 UsernameToken SOAP Header（客户端形态，
// 主要用于测试与文档示例）。
func securityHeaderXML(user, pass string, nonce []byte, created string) string {
	return fmt.Sprintf(`<SOAP-ENV:Header><wsse:Security xmlns:wsse="`+
		`http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd"`+
		` xmlns:wsu="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd"`+
		` SOAP-ENV:mustUnderstand="1"><wsse:UsernameToken>`+
		`<wsse:Username>%s</wsse:Username>`+
		`<wsse:Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">%s</wsse:Password>`+
		`<wsse:Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">%s</wsse:Nonce>`+
		`<wsu:Created>%s</wsu:Created>`+
		`</wsse:UsernameToken></wsse:Security></SOAP-ENV:Header>`,
		user, tokenDigest(nonce, created, pass),
		base64.StdEncoding.EncodeToString(nonce), created)
}
