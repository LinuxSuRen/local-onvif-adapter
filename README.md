# local-onvif-adapter

把本地摄像头暴露为标准 ONVIF 设备。

- **后端**：Go（ONVIF SOAP 服务 + WS-Discovery + 内嵌 RTSP 服务器 + ffmpeg 取流 + 管理 REST API）
- **前端**：Vue 3 + Element Plus 管理台（内嵌进单二进制）
- **媒体链路**：ffmpeg 拉源转 H264 → 推流给内嵌 RTSP 服务器（gortsplib）→ ONVIF `GetStreamUri` 返回 RTSP 地址；无需部署 mediamtx
- **PTZ**：纯 mock（虚拟位置状态机），所有操作只返回成功、绝不报错

设计参考 [ysc6c-onvif-adapter](https://github.com/linuxsuren/ysc6c-onvif-adapter)，
目标是满足 [device-camera-onvif](../../ws/feishu/device-camera-onvif) 这类 ONVIF 客户端的核心诉求：
发现设备、读设备信息、多 profile 取流、抓拍、PTZ/预置位、编码参数读写。

## 架构

```
┌──────────────────────────── host (Linux/macOS) ────────────────────────────┐
│                                                                             │
│  ┌──────────────────────────────────────┐                                  │
│  │ local-onvif-adapter (Go, 单二进制)   │                                  │
│  │  :8080 HTTP                          │                                  │
│  │   ├ /            管理 UI             │   ffmpeg 按需抓帧 → JPEG          │
│  │   ├ /api/...     REST API            │                                  │
│  │   └ /onvif/device_service (SOAP)     │◀── ONVIF 客户端                  │
│  │  :8554 内嵌 RTSP 服务器 (gortsplib)  │◀── rtsp://host:8554/cam/<id>     │
│  │  UDP 3702 WS-Discovery               │    （多客户端并发拉流）           │
│  │  UDP 8000/8001 RTSP-over-UDP（可选） │                                  │
│  └──────────────────────────────────────┘                                  │
│     ffmpeg 拉本地源: v4l2 / avfoundation / rtsp / testsrc → H264 → 推 :8554 │
└─────────────────────────────────────────────────────────────────────────────┘
```

- **多摄像头 = 单设备多 profile**（双光云台模式）：每个启用的摄像头对应一个 media
  profile，顺序与配置一致；客户端 `channel N → profile N-1` 的索引映射直接可用
- 在 UI 里把摄像头标记为"红外"后，profile 名称会带 `infrared` 关键词，
  device-camera-onvif 会按名称自动识别红外通道
- **鉴权（可选）**：管理台一键开启设备面认证——ONVIF SOAP 校验 WS-Security UsernameToken
  （PasswordDigest，`Base64(SHA1(nonce+Created+口令))`，Created ±5 分钟；兼容 PasswordText），
  RTSP 走 Digest(MD5)+Basic 401 挑战；单一账号两端共用，`GetSystemDateAndTime/GetCapabilities/GetServices`
  免认证（对时/能力发现前置），管理台与 REST API 不设防；默认关闭，升级零破坏

## 访问认证

管理台「系统信息 → 设备认证」配置（落盘 `data/config.json`，`auth_enabled/auth_user/auth_pass`）：

```bash
# RTSP 拉流（ffmpeg/VLC/gortsplib 凭 URL userinfo 自动完成 401 重试）
ffplay rtsp://admin:密码@<本机IP>:8554/cam/<摄像头ID>
```

- 开启后 ONVIF 客户端（NVR / onvif-ai / device-camera-onvif）在同一处填该账号
- ffmpeg 推流端由服务自动注入凭证（取流进程随认证变更自动重启）
- 口令明文存于本地配置文件：RTSP Digest 与 WS-PasswordDigest 的服务端校验
  都要求明文参与哈希，无法只存摘要

## 快速开始

### 容器一键启动（推荐）

```bash
make up        # docker compose up -d --build（host 网络，单容器，无需 mediamtx）
```

启动后：

- 管理台：<http://localhost:8080/>
- ONVIF 接入地址：`http://<本机IP>:8080/onvif/device_service`（UI 首页可复制）
- RTSP：`rtsp://<本机IP>:8554/cam/<摄像头ID>`

Linux 下接入 USB 摄像头，在 `docker-compose.yml` 中取消对应设备映射注释：

```yaml
    volumes:
      - /dev/video0:/dev/video0
```

然后到管理台添加摄像头（类型选 `v4l2`，源填 `/dev/video0`）。

compose 使用 **host 网络模式**：WS-Discovery 的 UDP 3702 组播与 RTSP 取流都最顺畅。
macOS Docker Desktop 需较新版本（支持 host networking）；组播发现不可用的环境，
在客户端里手动填 ONVIF 接入地址即可。

### 本地开发

```bash
make setup     # 前端依赖（首次）
make run       # 构建并本地运行（需本机 ffmpeg）
make test      # go test -race
make lint      # go vet
go run .       # 或者直接跑（同样需本机 ffmpeg）
```

前端单独开发调试：`cd web && npm run dev`（/api、/onvif 代理到 127.0.0.1:8080）。

## 摄像头源类型

管理台添加摄像头时**自动枚举本机摄像头**（内置、USB，名称与连接方式直接列出），
用户从列表中选择即可，无需关心平台差异；跨平台适配由服务端完成：

| 内部类型 | 平台 | 枚举方式 | 源形式 |
|---|---|---|---|
| `v4l2` | Linux | 扫描 `/dev/video*` + sysfs 读取名称与连接方式（过滤 UVC 元数据节点） | `/dev/video0` |
| `avfoundation` | macOS | `ffmpeg -list_devices`（过滤屏幕采集） | 设备索引 `0` |
| `dshow` | Windows | `ffmpeg -list_devices` | 设备名 |
| `rtsp` | 任意 | 手动填写 | `rtsp://user:pass@ip:554/stream` |
| `testsrc` | 任意 | 无需设备 | 留空 |

每个摄像头可配置分辨率（0 表示自动）、帧率、码率、是否红外、是否启用。
枚举列表也支持手动输入设备源（高级场景）。

### 音频采集

本地摄像头与 RTSP 源可开启**音频**，推流携带 AAC 音轨（44.1kHz / 64kbps，
ONVIF profile 同步声明 `AudioEncoderConfiguration`）：

| 来源 | 音频输入 | 说明 |
|---|---|---|
| Windows dshow | 下拉选择麦克风（自动枚举） | `video=摄像头:audio=麦克风` 联合输入，设备需未被其他应用占用 |
| macOS avfoundation | 下拉选择麦克风（自动枚举） | `视频索引:音频索引` 联合输入 |
| Linux v4l2 | 填写 pulse 源名（默认 `default`） | 视频/音频双输入分别采集 |
| RTSP 源 | 无需选择 | 直接透传源自带音轨（统一转码 AAC）；源无音轨时自动退化为纯视频 |

麦克风被占用或源缺音轨时**不会导致整路失败**，自动降级为纯视频并保留配置。

### Windows 说明

- 添加摄像头时直接从枚举列表选择设备（也可手填，或使用 `video=@device_cm_{...}` 别名形式）
- ffmpeg 需为含 dshow 支持的完整版（[gyan.dev](https://www.gyan.dev/ffmpeg/builds/)
  full 版或 `winget install ffmpeg` 均可）
- 防火墙首启放行：UDP 3702（WS-Discovery 自动发现）与 UDP 8000/8001
  （RTSP over UDP，不放行也不影响 TCP 拉流）
- 老摄像头只输出 MJPEG/YUYV 原始格式也没关系，统一转码 H264

## ONVIF 覆盖清单

| 服务 | 操作 |
|---|---|
| Device (tds) | GetServices、GetServiceCapabilities、GetCapabilities、GetDeviceInformation、GetSystemDateAndTime、GetHostname、GetScopes、GetNetworkInterfaces、GetDNS、GetNTP、GetUsers 等 |
| Media (trt) | GetProfiles、GetProfile、GetVideoSources、GetVideo/Encoder/SourceConfiguration(s)、SetVideoEncoderConfiguration、GetVideoEncoderConfigurationOptions、GetStreamUri、GetSnapshotUri、GetAudioSources |
| PTZ (tptz) | GetNodes、GetNode、GetConfigurations、GetConfigurationOptions、GetStatus、ContinuousMove、AbsoluteMove、RelativeMove、Stop、Goto/SetHomePosition、Get/Set/Goto/RemovePreset（全部 mock，含虚拟位置积分） |
| Imaging (timg) | Get/SetImagingSettings、GetOptions（内存态 mock） |

未实现的操作返回标准 SOAP Fault（`ter:ActionNotSupported`）。

## 管理 API

响应统一 envelope：成功 `{"data": ...}`，失败 `{"error":{"code","message"}}`，字段 snake_case。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/system` | 系统/ONVIF 端点信息（`rtsp_port` 为实际监听端口，`rtsp_port_configured` 为配置端口） |
| PUT | `/api/rtsp-port` | 修改 RTSP 端口（body `{"rtsp_port": 8554}`，持久化到配置，重启服务后生效） |
| GET / POST | `/api/cameras` | 列表 / 新增 |
| PUT / DELETE | `/api/cameras/{id}` | 更新 / 删除 |
| GET | `/api/cameras/{id}/snapshot` | 抓拍 JPEG |
| GET / POST | `/api/cameras/{id}/ptz` | 云台状态 / 试控（continuous、stop、absolute、preset_set、preset_goto、preset_remove、home_set、home_goto） |
| GET | `/onvif/snapshot/{profileToken}` | `GetSnapshotUri` 指向的公开快照地址 |
| GET | `/healthz` | 健康检查 |

## 与 device-camera-onvif 对接

```bash
go run ./cmd/device-camera-onvif --rtsp-forward-enabled=false
```

调用 gRPC 接口时在 `DeviceConnection.endpoint`（或 `x-onvif-endpoint` metadata）传入：

```
http://<local-onvif-adapter 所在机器IP>:8080/onvif/device_service
```

用户名/密码任意（服务端不校验）。抓拍、取流、PTZ、预置位、分辨率调整即可直接使用；
多摄像头时通过 channel 选择 profile（channel 1 → 第一个摄像头）。

## 目录结构

```
├── main.go 等                 # 服务入口（flag/env 装配、install/uninstall 子命令，go run . 即可启动）
├── internal/
│   ├── api/                  # 管理 REST API + 静态 UI 托管 + 快照路由
│   ├── config/               # 配置与摄像头列表持久化（data/config.json）
│   ├── discovery/            # WS-Discovery 应答器（UDP 3702）
│   ├── onvifserver/          # ONVIF SOAP 服务（device/media/ptz/imaging）
│   ├── ptzmock/              # 虚拟云台状态机
│   ├── rtspserver/           # 内嵌 RTSP 服务器（gortsplib，多读者分发）
│   ├── snapshot/             # ffmpeg 抓帧（带短缓存）
│   └── stream/               # ffmpeg 取流进程监督（退避重启）
├── web/                      # Vue3 管理台（构建产物内嵌）
├── Dockerfile                # 多阶段：node 构建前端 → go 构建后端 → alpine+ffmpeg
├── docker-compose.yml        # 单容器（host 网络）
└── Makefile
```

## 运行参数

| 参数 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `--http-addr` | `HTTP_ADDR` | `:8080` | HTTP 监听（UI/API/SOAP） |
| `--advertise-ip` | `ADVERTISE_IP` | 自动探测 | 对外宣告 IP |
| `--rtsp-server` | `RTSP_SERVER` | `true` | 启用内嵌 RTSP 服务器（gortsplib） |
| `--rtsp-push` | `RTSP_PUSH` | `rtsp://127.0.0.1:8554` | ffmpeg 推流目标（仅 `--rtsp-server=false` 时使用，指向外部 mediamtx） |
| `--rtsp-port` | `RTSP_PORT` | `8554` | RTSP 端口（内嵌服务器监听 + 对外宣告）；仅显式指定时覆盖配置文件中的值，详见 [修改 RTSP 端口](#修改-rtsp-端口) |
| `--data-dir` | `DATA_DIR` | `./data` | 配置持久化目录 |
| `--ffmpeg-bin` | `FFMPEG_BIN` | `ffmpeg` | ffmpeg 路径 |
| `--discovery` | `DISCOVERY` | `true` | 是否开启 WS-Discovery |
| `--log-level` | `LOG_LEVEL` | `info` | 日志级别 |

## 开机自启

Windows 下提供子命令一键注册/卸载开机自启（用户登录时静默启动，无控制台闪窗）：

```powershell
# 注册：记录当前可执行文件与 data-dir，并从既有配置重建完整启动参数
local-onvif-adapter.exe install -data-dir D:\path\to\data
# 预览将注册的内容（不落盘）
local-onvif-adapter.exe install -data-dir D:\path\to\data -dry-run
# 卸载（幂等）
local-onvif-adapter.exe uninstall -data-dir D:\path\to\data
```

实现：HKCU `Run` 注册表键 + data-dir 下的 VBS 隐藏窗口启动器；免管理员权限。
**为什么是"登录时启动"而不是系统服务**：采集桌面（gdigrab）与摄像头（dshow）
要求进程运行在交互式用户会话中，SYSTEM 服务位于 Session 0 采不到画面。
注意：移动可执行文件或数据目录后需重新执行 `install`；主程序每次启动会把
命令行参数写回配置文件，因此 `install` 会显式携带当前配置的完整参数，
避免自启动把用户配置重置为默认值。

### 其他平台（手动配置）

Linux（systemd 用户单元）：

```bash
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/local-onvif-adapter.service <<'EOF'
[Unit]
Description=local ONVIF adapter
After=graphical-session.target

[Service]
ExecStart=/usr/local/bin/local-onvif-adapter -data-dir %h/.local-onvif-adapter
Restart=on-failure

[Install]
WantedBy=default.target
EOF
systemctl --user enable --now local-onvif-adapter
```

macOS（launchd，登录时启动）：

```bash
cat > ~/Library/LaunchAgents/com.linuxsuren.local-onvif-adapter.plist <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.linuxsuren.local-onvif-adapter</string>
  <key>ProgramArguments</key><array>
    <string>/usr/local/bin/local-onvif-adapter</string>
    <string>-data-dir</string><string>/Users/USERNAME/.local-onvif-adapter</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><false/>
</dict></plist>
EOF
launchctl load ~/Library/LaunchAgents/com.linuxsuren.local-onvif-adapter.plist
```

### 修改 RTSP 端口

RTSP 端口支持三种修改途径，优先级从高到低：

1. **命令行** `--rtsp-port=9554`（仅显式指定时覆盖配置）
2. **环境变量** `RTSP_PORT=9554`
3. **配置文件 / 管理 API**：UI「系统信息 → RTSP 端口 → 修改」或 `PUT /api/rtsp-port`，
   持久化到 `data/config.json` 的 `server.rtsp_port`

通过 API/UI 修改后**需重启服务生效**：运行中的 RTSP 监听（含漂移后的实际端口）不会变化，
`GET /api/system` 会同时返回实际端口（`rtsp_port`）与配置端口（`rtsp_port_configured`），
两者不一致时 UI 会提示"已配置 xx，重启后生效"。未显式指定 `--rtsp-port`/`RTSP_PORT` 的
重启不会覆盖配置文件中的端口。

### 端口漂移

HTTP 与 RTSP 端口被占用时（`EADDRINUSE`），启动会自动向后尝试下一个端口
（8080 → 8081 → …；8554 → 8555 → …，各最多 20 个候选），直到能监听为止；对外
宣告的 ONVIF 地址、快照地址、`GetStreamUri` 的 RTSP 地址、UI 展示与 WS-Discovery
XAddr 都会使用**实际端口**，客户端不会拿到漂移前的地址。漂移结果不写入配置：下次
启动仍从配置端口开始探测，端口空闲时会回到首选端口。连续 20 个端口都被占用时启动
失败并报错；权限不足等其它绑定错误不做漂移。内嵌 RTSP 服务器的 UDP 端口
（8000/8001）被占用时会降级为仅 TCP 模式，不影响拉流。

## 发布

发布通过 GitHub Release 驱动：

1. 打 tag 并推送：`git tag v0.1.0 && git push origin v0.1.0`
2. 在 GitHub [Releases](https://github.com/LinuxSuRen/local-onvif-adapter/releases) 页面基于该
   tag 创建并发布 Release（`release` workflow 自动触发）
3. workflow 自动完成：构建前端 → 六平台交叉编译（linux/darwin/windows × amd64/arm64）→
   打包 tar.gz / zip（含 LICENSE、README）→ 生成 `checksums.txt` → 上传到该 Release
4. 产物缺失时可在 Actions 页面手动 `workflow_dispatch` 指定 tag 补传（`--clobber` 覆盖）

本地验证发布配置：`make snapshot`（需要 [goreleaser](https://goreleaser.com)，不打
tag、不上传）；正式本地发布用 `make release`。推送到 master / PR 会触发 `build`
workflow 跑 `go vet` + `go test -race`（CI 会先构建前端再测试）。

## 依赖

- Go 1.24+、Node 20+（构建前端）
- ffmpeg：唯一的运行时依赖（采集 + H264 转码）；容器镜像内置，本地运行需自行安装（`brew install ffmpeg`）
- 无需 mediamtx：RTSP 服务由 gortsplib 内嵌提供；如需 mediamtx 的高级能力（WebRTC/HLS/多播），
  可 `--rtsp-server=false --rtsp-push=rtsp://<mediamtx>:8554` 切回外部 mediamtx
