# onvif-local

把本地摄像头暴露为标准 ONVIF 设备。

- **后端**：Go（ONVIF SOAP 服务 + WS-Discovery + ffmpeg 取流 + 管理 REST API）
- **前端**：Vue 3 + Element Plus 管理台（内嵌进单二进制）
- **媒体链路**：ffmpeg 拉源转 H264 → 推流 mediamtx → ONVIF `GetStreamUri` 返回 RTSP 地址
- **PTZ**：纯 mock（虚拟位置状态机），所有操作只返回成功、绝不报错

设计参考 [ysc6c-onvif-adapter](https://github.com/linuxsuren/ysc6c-onvif-adapter)，
目标是满足 [device-camera-onvif](../../ws/feishu/device-camera-onvif) 这类 ONVIF 客户端的核心诉求：
发现设备、读设备信息、多 profile 取流、抓拍、PTZ/预置位、编码参数读写。

## 架构

```
┌──────────────────────────── host (Linux/macOS) ────────────────────────────┐
│                                                                             │
│  ┌───────────────────────────┐  RTSP push  ┌─────────────┐                 │
│  │ onvif-local (Go)          │ ──────────▶ │ mediamtx    │                 │
│  │  :8080 HTTP               │             │  :8554 RTSP │◀── ONVIF 客户端 │
│  │   ├ /            管理 UI  │  ffmpeg 按需抓帧 → JPEG      拉流            │
│  │   ├ /api/...     REST API │                                             │
│  │   └ /onvif/device_service │  WS-Discovery UDP 3702 → 局域网自动发现      │
│  │  UDP 3702                 │                                             │
│  └───────────────────────────┘                                             │
│     ffmpeg 拉本地源: v4l2 / avfoundation / rtsp / testsrc → H264           │
└─────────────────────────────────────────────────────────────────────────────┘
```

- **多摄像头 = 单设备多 profile**（双光云台模式）：每个启用的摄像头对应一个 media
  profile，顺序与配置一致；客户端 `channel N → profile N-1` 的索引映射直接可用
- 在 UI 里把摄像头标记为"红外"后，profile 名称会带 `infrared` 关键词，
  device-camera-onvif 会按名称自动识别红外通道
- **鉴权**：ONVIF 侧不校验凭据（digest / WS-Security 头一律放行），避免客户端认证循环

## 快速开始

### 容器一键启动（推荐）

```bash
make up        # docker compose up -d --build（host 网络 + mediamtx）
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
make run       # 构建并本地运行（需本机 ffmpeg；mediamtx 可选）
make test      # go test -race
make lint      # go vet
```

前端单独开发调试：`cd web && npm run dev`（/api、/onvif 代理到 127.0.0.1:8080）。

## 摄像头源类型

| 类型 | 说明 | 源示例 |
|---|---|---|
| `v4l2` | Linux 本地设备（Docker/Linux） | `/dev/video0` |
| `avfoundation` | macOS 摄像头（本地开发） | `0`（设备索引） |
| `rtsp` | 已有网络摄像机的 RTSP 流 | `rtsp://user:pass@ip:554/stream` |
| `testsrc` | ffmpeg 测试彩条（无需真实摄像头） | 留空 |

每个摄像头可配置分辨率（0 表示自动）、帧率、码率、是否红外、是否启用。

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
| GET | `/api/system` | 系统/ONVIF 端点信息 |
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
http://<onvif-local 所在机器IP>:8080/onvif/device_service
```

用户名/密码任意（服务端不校验）。抓拍、取流、PTZ、预置位、分辨率调整即可直接使用；
多摄像头时通过 channel 选择 profile（channel 1 → 第一个摄像头）。

## 目录结构

```
├── cmd/onvif-local/          # 服务入口（flag/env 装配）
├── internal/
│   ├── api/                  # 管理 REST API + 静态 UI 托管 + 快照路由
│   ├── config/               # 配置与摄像头列表持久化（data/config.json）
│   ├── discovery/            # WS-Discovery 应答器（UDP 3702）
│   ├── onvifserver/          # ONVIF SOAP 服务（device/media/ptz/imaging）
│   ├── ptzmock/              # 虚拟云台状态机
│   ├── snapshot/             # ffmpeg 抓帧（带短缓存）
│   └── stream/               # ffmpeg 取流进程监督（退避重启）
├── web/                      # Vue3 管理台（构建产物内嵌）
├── Dockerfile                # 多阶段：node 构建前端 → go 构建后端 → alpine+ffmpeg
├── docker-compose.yml        # onvif-local + mediamtx（host 网络）
├── mediamtx.yml              # mediamtx 最小配置（仅 RTSP 8554）
└── Makefile
```

## 运行参数

| 参数 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `--http-addr` | `HTTP_ADDR` | `:8080` | HTTP 监听（UI/API/SOAP） |
| `--advertise-ip` | `ADVERTISE_IP` | 自动探测 | 对外宣告 IP |
| `--rtsp-push` | `RTSP_PUSH` | `rtsp://127.0.0.1:8554` | ffmpeg 推流目标 |
| `--rtsp-port` | `RTSP_PORT` | `8554` | 对外宣告 RTSP 端口 |
| `--data-dir` | `DATA_DIR` | `./data` | 配置持久化目录 |
| `--ffmpeg-bin` | `FFMPEG_BIN` | `ffmpeg` | ffmpeg 路径 |
| `--discovery` | `DISCOVERY` | `true` | 是否开启 WS-Discovery |
| `--log-level` | `LOG_LEVEL` | `info` | 日志级别 |

### 端口漂移

HTTP 端口被占用时（`EADDRINUSE`），启动会自动向后尝试下一个端口（8080 → 8081 →
8082 …，最多 20 个候选），直到能监听为止；对外宣告的 ONVIF 地址、快照地址、UI
展示与 WS-Discovery XAddr 都会使用**实际端口**，客户端不会拿到漂移前的地址。漂移
结果不写入配置：下次启动仍从配置端口开始探测，端口空闲时会回到首选端口。连续 20
个端口都被占用时启动失败并报错；权限不足等其它绑定错误不做漂移。

## 依赖

- Go 1.24+、Node 20+（构建前端）
- ffmpeg：容器镜像内置；本地运行需自行安装
- mediamtx：compose 中作为独立容器运行；本地运行可 `brew install mediamtx`
