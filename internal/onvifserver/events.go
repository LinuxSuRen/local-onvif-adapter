package onvifserver

import (
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event 服务（ONVIF Core §Events，PullPoint 订阅模型）：
//   - 订阅：CreatePullPointSubscription 在主端点创建，返回独立 PullPoint 地址
//     （/onvif/sub_N，由 Handler 按路径路由）；客户端向该地址发 PullMessages
//   - 拉取：PullMessages 阻塞至 Timeout 或有消息；按规范把订阅 TTL 顺延到
//     「请求到达时刻 + Timeout」；Renew/Unsubscribe 补齐生命周期
//   - 发布：PublishEvent 供事件源（取流进程监督器/认证失败/配置变更）调用，
//     全局限频（同 topic+首 source 2s 内合并）防抖动刷屏；无人订阅不缓存
const (
	nsEvents    = "http://www.onvif.org/ver10/events/wsdl"
	topicDialect = "http://www.onvif.org/ver10/tev/topicExpression/ConcreteSet"
	subQueueCap  = 100
	pubMinGap    = 2 * time.Second
	subDefaultTTL = 60 * time.Second
)

// EventItem 一条 Source/Data 简单项。
type EventItem struct{ Name, Value string }

type eventMessage struct {
	topic  string
	source []EventItem
	data   []EventItem
	utc    string
}

type subscription struct {
	id       string
	deadline time.Time
	queue    []string
	notify   chan struct{} // cap 1：唤醒阻塞的 pull
}

type eventBus struct {
	mu      sync.Mutex
	subs    map[string]*subscription
	seq     int
	lastPub map[string]pubRecord
	logger  *slog.Logger
}

// pubRecord 限频记录：同 topic+source 的「相同载荷」2s 内只发一次；
// 状态交替（如 SignalLoss false→true）载荷不同，必须放行——快速重启
// 循环里 100ms 间隔的交替若被合并，NVR 只会看到恢复永远看不到丢失。
type pubRecord struct {
	at time.Time
	fp string
}

// bus 包级单例：事件源分散在多个包（stream/rtspserver/config），统一入口。
var bus = &eventBus{subs: map[string]*subscription{}, lastPub: map[string]pubRecord{}}

// PublishEvent 发布一条事件（Changed 语义）。
func PublishEvent(topic string, source, data []EventItem) {
	bus.publish(topic, source, data)
}

func fingerprint(topic string, source, data []EventItem) string {
	var sb strings.Builder
	sb.WriteString(topic)
	for _, it := range source {
		sb.WriteString("|" + it.Name + "=" + it.Value)
	}
	for _, it := range data {
		sb.WriteString("|" + it.Name + "=" + it.Value)
	}
	return sb.String()
}

func (b *eventBus) publish(topic string, source, data []EventItem) {
	key := topic
	if len(source) > 0 {
		key += "|" + source[0].Value
	}
	fp := fingerprint(topic, source, data)
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if last, ok := b.lastPub[key]; ok && now.Sub(last.at) < pubMinGap && last.fp == fp {
		return // 相同载荷短窗内重复：合并
	}
	b.lastPub[key] = pubRecord{at: now, fp: fp}
	if len(b.subs) == 0 {
		return // 无人订阅不缓存：新订阅不补发历史
	}
	msg := &eventMessage{topic: topic, source: source, data: data, utc: now.UTC().Format("2006-01-02T15:04:05Z")}
	rendered := renderNotification(msg)
	for _, sub := range b.subs {
		if len(sub.queue) >= subQueueCap {
			copy(sub.queue, sub.queue[1:])
			sub.queue[len(sub.queue)-1] = rendered
		} else {
			sub.queue = append(sub.queue, rendered)
		}
		select {
		case sub.notify <- struct{}{}:
		default:
		}
	}
	if b.logger != nil {
		b.logger.Debug("event published", "topic", topic)
	}
}

func (b *eventBus) create(ttl time.Duration) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	id := fmt.Sprintf("sub_%d", b.seq)
	if ttl <= 0 {
		ttl = subDefaultTTL
	}
	b.subs[id] = &subscription{id: id, deadline: time.Now().Add(ttl), notify: make(chan struct{}, 1)}
	b.expireLocked()
	return id
}

func (b *eventBus) renew(id string, ttl time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.subs[id]
	if !ok {
		return false
	}
	if ttl <= 0 {
		ttl = subDefaultTTL
	}
	s.deadline = time.Now().Add(ttl)
	return true
}

func (b *eventBus) unsubscribe(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, id)
}

// pull 阻塞至 wait 或有消息，凑满 limit 条返回渲染结果；
// 订阅不存在/中途过期返回 ok=false。按规范顺延 TTL。
func (b *eventBus) pull(id string, wait time.Duration, limit int) (string, bool) {
	if wait < 0 {
		wait = 0
	}
	if wait > time.Minute {
		wait = time.Minute
	}
	if limit <= 0 {
		limit = 5
	}
	deadline := time.Now().Add(wait)
	b.mu.Lock()
	s, ok := b.subs[id]
	if ok {
		s.deadline = time.Now().Add(max(wait, 5*time.Second))
	}
	b.mu.Unlock()
	if !ok {
		return "", false
	}
	var out []string
	for {
		b.mu.Lock()
		if b.subs[id] != s { // 中途被回收
			b.mu.Unlock()
			return "", false
		}
		if len(s.queue) > 0 {
			n := min(limit, len(s.queue))
			out = append(out, s.queue[:n]...)
			s.queue = s.queue[n:]
			b.mu.Unlock()
			return strings.Join(out, ""), true
		}
		b.mu.Unlock()
		remain := time.Until(deadline)
		if remain <= 0 {
			return "", true // 空等超时：合法返回（0 条消息）
		}
		select {
		case <-s.notify:
		case <-time.After(min(remain, 200*time.Millisecond)):
		}
	}
}

func (b *eventBus) expireLocked() {
	now := time.Now()
	for id, s := range b.subs {
		if s.deadline.Before(now) {
			delete(b.subs, id)
		}
	}
}

func renderNotification(m *eventMessage) string {
	var sb strings.Builder
	sb.WriteString(`<wsnt:NotificationMessage>`)
	sb.WriteString(`<wsnt:Topic Dialect="` + topicDialect + `">` + xmlEsc(m.topic) + `</wsnt:Topic>`)
	sb.WriteString(`<wsnt:Message><tt:Message UtcTime="` + m.utc + `" PropertyOperation="Changed">`)
	if len(m.source) > 0 {
		sb.WriteString(`<tt:Source>`)
		for _, it := range m.source {
			sb.WriteString(`<tt:SimpleItem Name="` + xmlEsc(it.Name) + `" Value="` + xmlEsc(it.Value) + `"/>`)
		}
		sb.WriteString(`</tt:Source>`)
	}
	if len(m.data) > 0 {
		sb.WriteString(`<tt:Data>`)
		for _, it := range m.data {
			sb.WriteString(`<tt:SimpleItem Name="` + xmlEsc(it.Name) + `" Value="` + xmlEsc(it.Value) + `"/>`)
		}
		sb.WriteString(`</tt:Data>`)
	}
	sb.WriteString(`</tt:Message></wsnt:Message></wsnt:NotificationMessage>`)
	return sb.String()
}

// registerEventsOps 注册事件服务操作（主端点）。
func registerEventsOps() {
	reg(nsEvents, "CreatePullPointSubscription", opCreatePullPoint)
	reg(nsEvents, "GetEventProperties", opGetEventProperties)
	reg(nsEvents, "GetServiceCapabilities", func(s *Service, _ []byte) (string, error) {
		return `<tev:GetServiceCapabilitiesResponse><tev:Capabilities PullPointSupport="true"/></tev:GetServiceCapabilitiesResponse>`, nil
	})
}

func opCreatePullPoint(s *Service, inner []byte) (string, error) {
	ttl := subDefaultTTL
	if m := reInitialTerm.FindSubmatch(inner); m != nil {
		if d, ok := parseISODuration(string(m[1])); ok && d > 0 {
			ttl = d
		}
	}
	id := bus.create(ttl)
	addr := fmt.Sprintf("http://%s:%d/onvif/%s", s.uris.AdvertiseIP, s.uris.HTTPPort, id)
	return `<tev:CreatePullPointSubscriptionResponse>` +
		`<tev:SubscriptionReference><wsa:Address>` + xmlEsc(addr) + `</wsa:Address></tev:SubscriptionReference>` +
		`<wsnt:CurrentTime>` + time.Now().UTC().Format(time.RFC3339) + `</wsnt:CurrentTime>` +
		`<wsnt:TerminationTime>` + time.Now().Add(ttl).UTC().Format(time.RFC3339) + `</wsnt:TerminationTime>` +
		`</tev:CreatePullPointSubscriptionResponse>`, nil
}

func opGetEventProperties(s *Service, _ []byte) (string, error) {
	return `<tev:GetEventPropertiesResponse>` +
		`<tev:TopicNamespaceLocation>http://www.onvif.org/onvif/ver10/topics/index.html</tev:TopicNamespaceLocation>` +
		`<wsnt:FixedTopicSet>true</wsnt:FixedTopicSet>` +
		`<wsnt:TopicExpressionDialect>` + topicDialect + `</wsnt:TopicExpressionDialect>` +
		`<tev:MessageContentFilterDialect>http://www.onvif.org/ver10/tev/messageContentFilter/ItemFilter</tev:MessageContentFilterDialect>` +
		topicSet() +
		`</tev:GetEventPropertiesResponse>`, nil
}

// topicSet 支持的话题：标准 tns1（SignalLoss/Motion）+ 自定义 lns。
func topicSet() string {
	return `<wstop:TopicSet>` +
		`<wstop:VideoSource><wstop:SignalLoss wstop:topic="true"/></wstop:VideoSource>` +
		`<wstop:RuleEngine><wstop:CellMotion><wstop:Motion wstop:topic="true"/></wstop:CellMotion></wstop:RuleEngine>` +
		`<wstop:Security><wstop:AuthFailure wstop:topic="true"/></wstop:Security>` +
		`<wstop:Source><wstop:Lifecycle wstop:topic="true"/></wstop:Source>` +
		`<wstop:Config><wstop:Changed wstop:topic="true"/></wstop:Config>` +
		`</wstop:TopicSet>`
}

var reInitialTerm = regexp.MustCompile(`InitialTerminationTime>([^<]+)<`)

var (
	rePullTimeout  = regexp.MustCompile(`Timeout>([^<]+)<`)
	rePullLimit    = regexp.MustCompile(`MessageLimit>(\d+)<`)
	reRenewTerm    = regexp.MustCompile(`TerminationTime>([^<]+)<`)
	reSubPath      = regexp.MustCompile(`^/onvif/(sub_[A-Za-z0-9]+)$`)
	reSoapOpEvents = regexp.MustCompile(`(PullMessages|Renew|Unsubscribe)`)
)

// subIDFromPath 提取 PullPoint 订阅地址中的订阅 ID；非订阅地址返回空。
func subIDFromPath(p string) string {
	if m := reSubPath.FindStringSubmatch(strings.TrimSpace(p)); m != nil {
		return m[1]
	}
	return ""
}

// handlePullPoint 订阅端点分发：Pull/Renew/Unsubscribe。
// 返回完整应答（成功体以事件信封包裹，fault 为完整 SOAP Fault 信封）。
// 订阅端点与主端点同样过认证（开启时）——事件订阅是安全旁路高发点。
func (s *Service) handlePullPoint(subID string, body []byte) ([]byte, int, bool) {
	if user, pass, ok := s.store.Root().Server.AuthCreds(); ok &&
		!verifyUsernameToken(body, user, pass) {
		s.logger.Info("onvif event op unauthorized", "sub", subID)
		PublishEvent("lns:Security/AuthFailure",
			[]EventItem{{"Service", "ONVIF"}}, []EventItem{{"State", "Failed"}})
		return nil, 401, false
	}
	op := ""
	if m := reSoapOpEvents.FindSubmatch(body); m != nil {
		op = string(m[1])
	}
	switch op {
	case "PullMessages":
		wait := 5 * time.Second
		if m := rePullTimeout.FindSubmatch(body); m != nil {
			if d, ok := parseISODuration(string(m[1])); ok {
				wait = d
			}
		}
		limit := 10
		if m := rePullLimit.FindSubmatch(body); m != nil {
			if n, err := strconv.Atoi(string(m[1])); err == nil {
				limit = n
			}
		}
		msgs, ok := bus.pull(subID, wait, limit)
		if !ok {
			return soapFault("ter:SubscriptionLost", "subscription expired or unknown"), 200, true
		}
		bodyXML := `<tev:PullMessagesResponse>` +
			`<tev:CurrentTime>` + time.Now().UTC().Format(time.RFC3339) + `</tev:CurrentTime>` +
			`<tev:TerminationTime>` + time.Now().Add(subDefaultTTL).UTC().Format(time.RFC3339) + `</tev:TerminationTime>` +
			msgs +
			`</tev:PullMessagesResponse>`
		return eventsEnvelope(bodyXML), 200, true
	case "Renew":
		ttl := subDefaultTTL
		if m := reRenewTerm.FindSubmatch(body); m != nil {
			if d, ok := parseISODuration(string(m[1])); ok && d > 0 {
				ttl = d
			}
		}
		if !bus.renew(subID, ttl) {
			return soapFault("ter:SubscriptionLost", "subscription expired or unknown"), 200, true
		}
		return eventsEnvelope(`<wsnt:RenewResponse><wsnt:TerminationTime>` +
			time.Now().Add(ttl).UTC().Format(time.RFC3339) +
			`</wsnt:TerminationTime></wsnt:RenewResponse>`), 200, true
	case "Unsubscribe":
		bus.unsubscribe(subID)
		return eventsEnvelope(`<wsnt:UnsubscribeResponse/>`), 200, true
	default:
		return soapFault("ter:ActionNotSupported", "operation not supported on pull point"), 200, true
	}
}

// eventsEnvelope 事件应答信封：声明事件相关命名空间（tev/wsa/wsnt/wstop +
// 话题 tns1/lns，ConcreteSet 方言的 topic 字符串依赖前缀绑定）。
func eventsEnvelope(body string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"` +
		` xmlns:tds="http://www.onvif.org/ver10/device/wsdl"` +
		` xmlns:trt="http://www.onvif.org/ver10/media/wsdl"` +
		` xmlns:tt="http://www.onvif.org/ver10/schema"` +
		` xmlns:tev="` + nsEvents + `"` +
		` xmlns:wsa="http://www.w3.org/2005/08/addressing"` +
		` xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2"` +
		` xmlns:wstop="http://docs.oasis-open.org/wsn/t-1"` +
		` xmlns:tns1="http://www.onvif.org/ver10/topics"` +
		` xmlns:lns="http://www.linuxsuren.com/onvif/topics">` +
		`<s:Body>` + body + `</s:Body></s:Envelope>`)
}

// parseISODuration 解析 ISO 8601 时长（PT#H#M#S，含小数秒）。
func parseISODuration(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "PT") && !strings.HasPrefix(s, "pt") {
		return 0, false
	}
	rest := s[2:]
	if rest == "" {
		return 0, false
	}
	var total time.Duration
	num := ""
	flush := func(unit time.Duration) bool {
		if num == "" {
			return false
		}
		v, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return false
		}
		total += time.Duration(v * float64(unit))
		num = ""
		return true
	}
	for _, c := range rest {
		switch {
		case c >= '0' && c <= '9', c == '.', c == ',':
			num += string(rune(c))
		case c == 'H' || c == 'h':
			if !flush(time.Hour) {
				return 0, false
			}
		case c == 'M' || c == 'm':
			if !flush(time.Minute) {
				return 0, false
			}
		case c == 'S' || c == 's':
			if !flush(time.Second) {
				return 0, false
			}
		default:
			return 0, false
		}
	}
	return total, true
}
