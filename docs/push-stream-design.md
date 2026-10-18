# 设计：ONVIF 面内的按需直推（single-push）

状态：方案已确认；adapter 侧（第 1 步）已实现于分支 `feat/push-stream`，
controller 侧（第 2 步）待 robot-controller 仓库另行实施。

## 1. 背景与目标

现状链路中，树莓派等资源受限设备承担了两份媒体工作：

```
摄像头 → [adapter: 采集+编码+RTP打包] → loopback → [controller: 拉流+解包+重封装+上送] → 云端
```

采集编码本就只做一次，但媒体数据在设备上被打包/解包各一次、socket 拷贝两次，且 controller
常驻一个完整取流转推进程（Pi 4 实测典型 8~20% 单核 + 60~120MB RSS），该进程不做任何转码，
纯浪费。

**目标**：资源受限设备上媒体面一次采集、一次编码、一次上送；按需推流（云端观看才推）。

**约束**（已确认）：

- ONVIF 协议面零破坏：标准动作行为零改动，任何标准 ONVIF 客户端不受影响
- controller 只面对 ONVIF 协议（SOAP 端点 + WS-Discovery + RTSP），**不对接 adapter 私有 REST**
- **controller 现有功能全部保留**：拉流转推是服务任意标准 ONVIF 相机（真实 IPC、第三方
  adapter）的通用路径，一行不减；直推作为新增模式与之并存，按设备自适应选择（见 §6）
- 目标入口协议：RTSP ANNOUNCE（mediamtx 类）
- 会话带 TTL 租约防泄漏

**为什么需要厂商扩展动作**：ONVIF Profile S 是纯拉模型，标准里没有"设备向指定 URL 主动
推流"的动作（`StartMulticastStreaming` 是组播语义、装不下凭证；事件通道是单向的；云端反向
拉设备流穿不透 NAT）。ONVIF 规范明确允许厂商在自有命名空间扩展动作，这是合规补位，
不是修改标准。

## 2. 拓扑（直推模式）

```
控制面（指令）                                    媒体面（数据，设备出站单向）
───────────────                                  ─────────────────────────────
云端 ──▶ robot-controller ──SOAP: lns:StartPushStream──▶ local-onvif-adapter
              │                                             │
              │ 标准 ONVIF 照常：发现/GetStreamUri/PTZ         │ ffmpeg 单进程
              │ （直推模式不拉流；拉流模式照常）                │ ├─ rtsp://127.0.0.1:8554/cam/x   常开，ONVIF 语义保留
云端播放器 ◀── mediamtx ◀── RTSP ANNOUNCE（会话期直推）───────┘ └─ rtsp://cloud:8554/robot/x/cam/y 按需
```

- controller 形态：现有云控制面 + ONVIF SOAP 客户端（与 PTZ 调用同栈同认证）+ 双模式
  转发器（拉流/直推，见 §6）
- 无新增入站端口，全部出站，NAT 友好
- 拉流模式（现状）不受影响：对无直推能力的设备与降级场景，controller 仍走
  GetStreamUri → 拉 RTSP → 转推

## 3. 信令面：lns: 扩展动作（4 个）+ 既有事件体系

挂在与标准动作相同的 `device_service` SOAP 端点，按 tag 级正则分发（沿用现有模式）。
命名空间：`xmlns:lns="http://www.linuxsuren.com/onvif/ext"`（事件话题沿用既有
`http://www.linuxsuren.com/onvif/topics`）。

| 动作 | 请求 | 应答 |
|---|---|---|
| `lns:StartPushStream` | `ProfileToken`、`TargetURL`（含 userinfo 凭证） | `PushToken` |
| `lns:RenewPushStream` | `PushToken` | 空（续租） |
| `lns:StopPushStream` | `PushToken` | 空 |
| `lns:GetPushStreams` | 空 | 会话列表 + 每路健康 |

报文示例：

```xml
<s:Body>
  <lns:StartPushStream>
    <lns:ProfileToken>profile_0</lns:ProfileToken>
    <lns:TargetURL>rtsp://user:pass@cloud:8554/robot/r1/cam/c0</lns:TargetURL>
  </lns:StartPushStream>
</s:Body>
```

- **认证**：与 PTZ/GetProfiles 同一套 WS-Security UsernameToken（设备面认证开启时必须过认证，
  推流属于高价值操作，不进 pre-auth 白名单）
- **ProfileToken 路由**：沿用 `profile_N` ↔ 摄像头映射，与 `GetStreamUri` 尾号路由一致
- **幂等**：同 `(ProfileToken, TargetURL)` 重复 Start 返回同一 `PushToken`，不重启进程
  （targets 未变化 → Spec 指纹未变化）；controller 超时重试安全
- **引用计数职责在 controller**：多个云端观看者由 controller 归并为一次 Start/Stop；
  adapter 只按 `(profile, target)` 去重
- **controller 重启对账**：`GetPushStreams` 列出现存会话，续租接管即可，无重复进程
- **能力宣告**：WS-Discovery Scopes 追加
  `onvif://www.linuxsuren.com/feature/push-stream`（Scopes 本就是自由扩展的 URI 列表，
  发现报文零成本携带；controller 据此免探测即知设备支持直推）

**状态回报走既有 PullPoint 事件**（controller/云端零轮询）：

| 话题 | 载荷要点 |
|---|---|
| `lns:Push/Started` | Source=`Camera_<id>`、PushToken、Target（脱敏） |
| `lns:Push/Failed` | 含 stderr 尾部摘要与失败次数 |
| `lns:Push/Stopped` | Reason=`explicit` / `lease_expired` / `failure_budget` |

## 4. 会话生命周期

```
Start ──▶ 进程以 (本地RTSP + 远端) 双输出重启 ──▶ Started 事件
  │                                                │
  │  Renew 周期性续租（默认 TTL 10min，1/3 周期续）   │
  ▼                                                ▼
Stop(显式) / TTL 到期 / 连续失败达预算(默认10次) ──▶ 摘除远端输出，回到本地-only，Stopped 事件
```

- **租约**：默认 TTL 10 分钟（可配），controller 以约 1/3 周期调用 Renew；到期未续自动停推，
  防 controller 崩溃后白占上行带宽
- **失败预算**：远端输出失败 → ffmpeg 退出 → 复用现有指数退避（1s→15s）重启重试；连续失败
  达上限（默认 10 次）→ 摘除 target、会话标记 failed、事件上报，绝无限重启
- **凭证**：TargetURL 仅会话期内存持有，不落盘；日志与事件中按 `user:***@` 脱敏

## 5. 媒体面：ffmpeg 双输出（零新增媒体代码）

```
-i <采集> <编码参数（不变）> \
  -f rtsp -rtsp_transport tcp rtsp://127.0.0.1:8554/cam/x \          ← 常开
  -f rtsp -rtsp_transport tcp rtsp://user:***@cloud:8554/robot/x/cam/y ← 会话期
```

- 采集、编码、封装只做一次；增量成本 ≈1~3% CPU（第二路 RTP 打包 + WAN 写）
- 会话起停 = targets 变化纳入 `Spec()` 指纹 → 复用现有 Runner 停旧起新机制
- **冷启动 1~3s**（进程重启 + 设备重开 + GOP 对齐）：与云端播放端"按需冷启动重试"
  语义一致；直推模式下本地 RTSP 已无常态读者，重启闪断影响可忽略
- H264+AAC 对 RTSP/mediamtx 直通；rpicam 路径保持 `-c copy` 零转码
- **二期演进（可选）**：进程内 RTP 级联转发（gortsplib Client ANNOUNCE 后 RTP 原样透传），
  秒级起停无重启；v1 先走 ffmpeg 多输出零代码路径，验证收益后再评估

## 6. controller 双模式与自适应

### 模式定义

- **模式 P（pull，现状全保留）**：GetStreamUri → controller 拉 RTSP → 转推云端。适用于
  任意 ONVIF 相机（真实 IPC、第三方 adapter、不支持扩展的设备）。**controller 对外能力零缩减。**
- **模式 D（direct push，新增）**：`lns:StartPushStream` → 设备直推 → controller 只持
  信令与租约，不过媒体。

### 转发器抽象

```
Forwarder 接口：Start(ctx) / Stop() / Status()
  ├─ PullForwarder        现有拉流转推逻辑原样收编，一个字不改
  └─ DirectPushForwarder  lns:Start/Renew/Stop + lns:Push/* 事件监听 + 降级回调
```

观看会话/引用计数层两种模式共用：云端观看请求入口不变，内部按模式路由到对应 Forwarder；
观看者归并（多观看者一次 Start/Stop）在会话层完成，对 Forwarder 透明。

### 对真实相机（海康等）的边界

- 海康等真实 IPC **不认识 `lns:` 扩展** → 能力发现自动判否 → 恒走模式 P，行为与现状一致
- 海康相机自身的"设备主动推流"只有两条原生路径：
  - **GB28181**（国标，海康广泛支持）：SIP 主动注册 + INVITE 点播时推 PS/RTP——要求云端是
    国标平台（WVP-GB28181-pro + ZLMediaKit 类），**不是** mediamtx 的 RTSP ANNOUNCE
  - **Ehome/ISUP**（私有）：只推海康自家生态平台，第三方不可终结
- 海康的 RTSP 服务端是纯拉模型（DESCRIBE/SETUP/PLAY），不支持 ANNOUNCE/RECORD——
  直推 mediamtx 这条路对海康不存在；RTMP 直推仅个别"直播"型机型支持，非云台机主流
- **演进预留**：Forwarder 抽象天然容纳第三种 `GB28181Forwarder`（模式 G）——若未来云端
  引入 ZLMediaKit（同时收 RTSP ANNOUNCE 与 GB28181），海康相机也能借国标原生推流
  获得"一次推流"收益（本仓库姊妹项目 local-gb28181-adapter 已验证设备侧国标推流全链路）；
  v1 不做，双模式先行

### 三层自适应

**第 1 层：能力发现（每设备缓存）**

- 首选：WS-Discovery Scopes 含 `onvif://www.linuxsuren.com/feature/push-stream`
  → 支持直推，发现阶段即知，零额外交互
- 兜底：静态配置/非发现路径的设备 → 调一次 `lns:GetPushStreams`（只读无副作用），
  标准 ONVIF 相机回 `ActionNotSupported` 类 Fault → 不支持；结果缓存，设备重连时重探

**第 2 层：策略开关**

全局或每设备 `push_mode: auto | direct | pull`（默认 auto）：

- `auto`：能力 + 健康度自动选（默认）
- `pull`：强制拉流——排障、特殊网络（如目标入口不允许设备直连）时用
- `direct`：强制直推——固定部署、明确不想走拉流的设备

**第 3 层：运行时降级与恢复（熔断）**

- 直推会话失败预算耗尽（`lns:Push/Failed`）或 `Started` 等待超时 → 该设备-路标记
  **direct 不健康** → 当前会话即时回退 `PullForwarder` 接管（云端观看中断≈一次冷启动，
  播放端已有重试语义）→ 冷却期（默认 5min）内新会话直接走 P
- 冷却期满 → 半开：下一个新会话再试 D，成功即恢复健康标记
- 降级/恢复对云端完全透明

### 时序（auto 模式，云端观看请求）

```
云端观看 → 会话层(引用计数) → 能力缓存(未知则探测)
  ├─ 支持D ∧ 健康 ∧ 策略允许 → DirectPushForwarder.Start
  │     ├─ lns:Push/Started(超时兜底) → Renew 循环(TTL/3)；运行中 Failed → 降级 P
  │     └─ 失败/超时 → 标记不健康 → PullForwarder.Start（现状路径）
  └─ 不支持/不健康/策略pull → PullForwarder.Start（现状路径）
观看结束(引用清零) → 当前 Forwarder.Stop
```

- adapter 重启导致会话丢失 → Renew 失败 → controller 重新 Start（幂等），无需人工干预
- 未升级的 controller 与已升级 controller 对同一设备的 P 模式行为完全一致（灰度安全的根基）

## 7. ONVIF 兼容性声明

- 全部标准动作行为零改动；扩展动作在自有命名空间，标准客户端不认识即不调用，互不可见
- 内嵌 RTSP 服务器与 `GetStreamUri` 保持常开常可用（≈1~3% CPU），任何 ONVIF 客户端
  随时本地拉流，与直推互不影响

## 8. 改动面与实施切分

| 仓库/模块 | 改动 |
|---|---|
| adapter `onvifserver` | 4 个 `lns:` 动作分发（tag 级正则） |
| adapter `stream` | `buildArgs` 多输出；targets 进 `Spec()`；会话管理（去重/租约/失败预算） |
| adapter `discovery` | Scopes 追加能力宣告 URI |
| adapter 事件 | `lns:Push/*` 三个话题接入既有 PullPoint |
| robot-controller | 拉流转推**原样保留**并收编为 PullForwarder；新增 DirectPushForwarder、能力探测/缓存、策略开关、熔断降级、会话层模式路由 |

切分顺序：

1. **adapter 侧全量**（会话管理 + 多输出 + 动作 + 事件 + 租约/预算 + scopes 宣告）——
   独立可用可测；此时 controller 走 P 模式，行为不变
2. **controller 侧新增**：DirectPushForwarder + 自适应三件套；`push_mode` 策略本身就是
   灰度手段——先对试点设备配 `direct` 验证，稳定后放开默认 `auto`
3. （可选）进程内 RTP 级联演进

## 9. 验收

- 单测：SOAP 动作解析、会话生命周期（去重/续租/过期/预算）、Spec 指纹、多输出参数、凭证脱敏
- 集成（有资源环境）：mediamtx 作云端入口，模拟 controller 发 Start/Renew/Stop，ffplay 从
  mediamtx 拉流端到端验证；TTL 到期与失败预算自动停推
- 双模式：标准 ONVIF 相机（无 lns 能力）自动走拉流且行为与现状一致；`push_mode=pull` 下
  adapter 设备也走拉流；直推失败自动降级拉流、冷却期满半开恢复
- 能力发现：scopes 宣告路径与 `GetPushStreams` Fault 兜底路径均覆盖
- 树莓派实测：改造前后同场景 CPU 对比（预期干掉 controller 转进程的 8~20% 单核 + 60~120MB）、
  冷启动时延、会话起停时延、降级切换时延
- ONVIF 回归：onvif-ai / device-camera-onvif 拉流、PTZ、发现行为与改造前一致
