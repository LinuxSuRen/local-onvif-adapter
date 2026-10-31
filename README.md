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

## 按需直推（lns: 扩展动作）

面向树莓派等资源受限设备的「一次推流」能力：云端观看时，由本服务把流**直接推到
远端媒体入口**（RTSP ANNOUNCE 到 mediamtx 等，也支持 RTMP/RTMPS/SRT），
取流端控制器不再需要拉流再转发——采集、编码、上送各只发生一次。
标准 ONVIF 客户端不识别这些动作，行为完全不受影响。

在标准 `device_service` SOAP 端点上提供 4 个厂商命名空间
（`xmlns:lns="http://www.linuxsuren.com/onvif/ext"`）扩展动作
（设备面认证开启时与其他操作同策略，不在免认证白名单）：

| 动作 | 请求 | 应答 |
|---|---|---|
| `lns:StartPushStream` | `ProfileToken`、`TargetURL`（含 userinfo 凭证） | `PushToken` |
| `lns:RenewPushStream` | `PushToken` | 租约顺延 |
| `lns:StopPushStream` | `PushToken` | 停推 |
| `lns:GetPushStreams` | 空 | 会话列表（Target 已脱敏） |

```xml
<s:Body>
  <lns:StartPushStream>
    <lns:ProfileToken>profile_cam1</lns:ProfileToken>
    <lns:TargetURL>rtsp://user:pass@cloud:8554/robot/r1/cam1</lns:TargetURL>
  </lns:StartPushStream>
</s:Body>
```

生命周期语义（对 controller 侧的契约）：

- **幂等**：同 `(ProfileToken, TargetURL)` 重复 Start 返回同一 `PushToken`，不重启进程
- **租约**：默认 TTL 10 分钟，以约 1/3 周期 `RenewPushStream` 续租；到期未续自动停推
  （防 controller 崩溃后白占上行带宽），事件 `lns:Push/Stopped(Reason=lease_expired)`
- **失败预算**：远端不可达连续失败 10 次即摘除会话并发布 `lns:Push/Failed`，
  进程回到仅本地 RTSP 形态，绝无限重启；controller 收到 Failed 可回退拉流模式
- **状态事件**（经标准 PullPoint 订阅，话题命名空间 `http://www.linuxsuren.com/onvif/topics`）：
  `lns:Push/Started`、`lns:Push/Failed`、`lns:Push/Stopped(Reason=explicit|lease_expired|failure_budget|source_gone)`
- **凭证**：TargetURL 仅会话期内存持有、不落盘；日志与事件中一律 `user:***@` 脱敏
- **能力宣告**：WS-Discovery 与 `GetScopes` 均携带
  `onvif://www.linuxsuren.com/feature/push-stream`，controller 发现阶段即可免探测
  识别直推能力（无此能力的相机自动回退拉流，双模式自适应见设计文档）

媒体面实现：目标列表纳入取流进程指纹，ffmpeg 单进程 `-f tee` 多路输出
（本地 RTSP 常开 + 会话期远端直推），编码只做一次；起停推流会重启该路进程，
冷启动 1~3 秒。详细设计与 controller 侧双模式自适应方案见
[docs/push-stream-design.md](docs/push-stream-design.md)。

## 融合布局（多画面合成与整流推送）

把多个摄像头的画面按自定义布局拼到一张画布，**合成单路 H.264 流**对外输出：
本地 `rtsp://host:8554/cam/<布局ID>`（作为普通通道被 NVR/ONVIF 客户端拉流），
并可同时 `tee` 直推多个远端目标（rtsp/rtmp/srt，复用按需直推的目标校验与脱敏）。
管理台「融合布局」面板可视化编辑：从摄像头列表添加画面，画布上**拖拽移动、
拖角缩放**（网格吸附 + 像素级微调），快照轮询预览，保存后即生效。

```
画面格(cell) ──ffmpeg 中继──▶ rawvideo 定长帧 ──┐
画面格(cell) ──ffmpeg 中继──▶ rawvideo 定长帧 ──┼─▶ Go 拼画布 ──▶ 编码 ffmpeg ──▶ tee 输出
（断流格自动铺黑帧）                            ┘   (stdin 管道)     ├ /cam/<布局ID> 本地
                                                                     └ rtsp/rtmp/srt 远端
```

- **断流策略**（创建时可选）：
  - `blackframe`（默认）：每格独立中继拉本地回环流，断流只黑对应格、整流不断；
    格级健康度（画面正常/中断）在 UI 与 `/api/mosaics` 状态里可见
  - `restart`：单 ffmpeg `filter_complex` 一次成型（黑底画布 + overlay 链），
    进程少但任一路断流整流重启（中断数秒）
- **虚拟通道**：每个布局自动在摄像头列表生成一条 `mosaic` 类型通道——
  ONVIF profile、`GetStreamUri`、快照、RTSP 地址全套复用，NVR 直接拉合成流；
  该通道只能经融合布局面板管理，摄像头接口对它只读
- **画面格**：坐标为画布绝对像素（服务端统一向下偶数对齐，yuv420 要求），
  最多 16 格、格序即叠放顺序；引用的摄像头被删除/禁用时该格自动铺黑帧，布局不破坏
- **音频**：可选单源（取某路摄像头的音轨转 AAC），不做混音
- **布局变更即重启**：ffmpeg 命令行无法运行中变更，布局保存后合成流重启
  （前端 800ms 防抖，拖拽结束才落一次）；推流会话在内存中持有（凭证不落盘），
  合成进程连续短命失败达预算即摘除会话并发布 `lns:Push/Failed`，回到本地-only
- **能力宣告**：WS-Discovery 携带 `onvif://www.linuxsuren.com/feature/mosaic`

资源提示：合成需像素域解码 N 路再编码 1 路，画面格按格尺寸配置低分辨率源
（如四分屏用子码流），总像素不超过画布即可在中等 CPU 上稳定运行。

## 快速开始

### 容器运行（Windows / macOS / Linux）

镜像是多阶段构建的 Linux 容器（node 构建前端 → go 构建后端 → alpine + ffmpeg 运行），
三种宿主机上跑的都是同一份镜像；平台差异集中在**网络模式**与**本地摄像头透传**两点：

| 宿主机 | 推荐方式 | UI/API/RTSP | WS-Discovery 自动发现 | 本地摄像头采集 |
|---|---|---|---|---|
| Linux | host 网络（默认） | ✅ | ✅ | ✅ 映射 `/dev/video*` |
| Windows / macOS（默认安装） | bridge 服务（端口映射） | ✅ | ❌ 手填接入地址 | ❌ 用 rtsp/testsrc 源 |
| Windows / macOS（Desktop ≥ 4.34 开启 host networking） | host 网络 | ✅ | ⚠️ 组播不保证 | ❌ 用 rtsp/testsrc 源 |

> **为什么 Windows/macOS 容器采不到本地摄像头**：Docker Desktop 的容器运行在内置
> Linux 虚拟机里，宿主机的 dshow/avfoundation 设备无法透传进 VM（设备映射只对
> Linux 的 `/dev/video*` 有效）。容器内可正常使用 `rtsp`（网络相机）与 `testsrc`
> 源；要采集宿主机 USB 摄像头，请下载对应平台原生二进制运行（见[发布](#发布)），
> 并用 `install` 子命令注册开机自启（见[开机自启](#开机自启)）。容器自身的
> "开机自启"由 `restart: unless-stopped` 承担：Docker 服务随宿主机启动后容器自动拉起。

#### Linux（host 网络，全功能）

```bash
make up        # docker compose up -d --build（host 网络，单容器，无需 mediamtx）
```

- WS-Discovery 的 UDP 3702 组播与 RTSP 取流都最顺畅
- 接入 USB 摄像头：在 `docker-compose.yml` 中取消对应设备映射注释，然后到管理台
  添加摄像头（类型选 `v4l2`，源填 `/dev/video0`）：

  ```yaml
      volumes:
        - /dev/video0:/dev/video0
  ```

#### Windows / macOS（bridge 模式，开箱即用）

```bash
ADVERTISE_IP=<宿主机局域网IP> docker compose up -d --build local-onvif-adapter-bridge
```

- **`ADVERTISE_IP` 必填**（bridge 模式下的头号坑）：桥接网络里自动探测拿到的是
  容器内网 IP（172.x），而 `GetStreamUri`/快照地址/XAddr 宣告的都是这个 IP，局域网
  客户端将无法访问。环境变量每次启动都会写回配置，宿主机 IP 变化后改这里重启即可
- 端口映射：`8080`（UI/API/SOAP）、`8554`（RTSP over TCP，推荐）；RTSP over UDP
  需再放开 `8000-8001/udp` 映射，或客户端直接走 TCP：
  `ffplay -rtsp_transport tcp rtsp://<宿主机IP>:8554/cam/<摄像头ID>`
- **宿主机端口被占用时**：映射端口须与容器内端口保持一致（服务对外宣告的是
  容器内实际端口），换端口要同时调整环境变量并映射同号端口，例如
  `-e HTTP_ADDR=:18080 -p 18080:18080`、`-e RTSP_PORT=18554 -p 18554:18554`
- **WS-Discovery 组播无法穿越 bridge 网络**：ONVIF 客户端里手动填接入地址
  `http://<宿主机IP>:8080/onvif/device_service`（UI 首页可复制）
- Windows 首次使用需在防火墙放行入站 TCP 8080/8554
- 管理台添加摄像头时选 `rtsp` / `testsrc` 类型（容器枚举不到宿主机设备）

#### Windows / macOS（Docker Desktop host networking）

Docker Desktop 4.34+ 可在 Settings → Resources → Network 勾选 *Enable host
networking*（Windows 需 WSL2 后端），之后与 Linux 一样直接 `docker compose up -d
--build`：端口直接绑定宿主机，无需 ports 映射。但该特性下 UDP 组播（WS-Discovery
自动发现）不保证可用，客户端发现不了时用手填接入地址兜底。

#### 启动后

- 管理台：<http://localhost:8080/>
- ONVIF 接入地址：`http://<本机IP>:8080/onvif/device_service`（UI 首页可复制）
- RTSP：`rtsp://<本机IP>:8554/cam/<摄像头ID>`

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

**启动时自动发现**（默认开启，`--auto-cameras=false` / `--auto-screens=false` 关闭）：
启动阶段枚举本机设备，把尚未配置的摄像头/显示器自动创建为已启用的摄像头。
按 Type+Source 匹配去重——已有条目（包括手动禁用的）不受影响，参数定制保留；
屏幕只逐显示器添加（"整桌面拼接"项跳过）。Windows 同型号多台摄像头按 PnP
别名精确定位。枚举失败（如无 ffmpeg）只告警不阻断启动。RealSense 深度相机
家族设备跳过——整机由 rs-pipe 独占托管，请走管理台"深度相机"一键添加。

| 内部类型 | 平台 | 枚举方式 | 源形式 |
|---|---|---|---|
| `v4l2` | Linux | 扫描 `/dev/video*` + sysfs 读取名称与连接方式（过滤 UVC 元数据节点） | `/dev/video0` |
| `avfoundation` | macOS | `ffmpeg -list_devices`（过滤屏幕采集） | 设备索引 `0` |
| `dshow` | Windows | `ffmpeg -list_devices` | 设备名 |
| `rtsp` | 任意 | 手动填写 | `rtsp://user:pass@ip:554/stream` |
| `rpicam` | 树莓派 | 无需选择（libcamera 默认摄像头） | 留空；多摄像头填 libcamera 索引 |
| `realsense` | 任意（设备需 USB 直连） | 无需选择（rs-pipe 统一取三路） | `tcp://127.0.0.1:1802{0,1,2}` |
| `testsrc` | 任意 | 无需设备 | 留空 |

`rpicam` 类型走 **libcamera 管线**（`rpicam-vid`）：新版树莓派系统的 CSI 摄像头
无法通过 `/dev/video0` 直接 v4l2 采集（unicam 媒体管线需 libcamera 配置）。
该类型由 rpicam-vid 完成**硬件 H264 编码**（`--inline` 保证 SPS/PPS 随关键帧
重发，RTSP 读者中途加入可解码），ffmpeg 仅 `-c copy` 直通封装——**零转码**，
分辨率/帧率/码率由 libcamera 强制执行（无驱动静默回退问题）。快照走
`rpicam-still` 原生输出。

### RealSense 深度相机（RGB / 伪彩色深度 / IR 三路）

RealSense（D435/D455 等）设备 **USB 独占**——同一时刻只允许一个进程访问，
因此由本仓库自带的 `rs-pipe` 采集分发器统一打开设备、一次取齐三路帧，
再以 rawvideo TCP 裸流（rgb24 定长帧）分发，每路各起一个 ffmpeg 编码推流：

```
cmd/rs-pipe（独占设备，默认端口 18020/18021/18022）
  ├─ color  RGB 彩色  ── tcp://127.0.0.1:18020 ─→ ffmpeg → H264 → RTSP /cam/<id>
  ├─ depth  Turbo 伪彩色（Z16 毫米 → 固定范围映射）── :18021 ─→ ffmpeg → RTSP
  └─ ir     红外灰度（Y8）── :18022 ─→ ffmpeg → RTSP
```

使用步骤：

1. 编译采集器并放到 adapter 可寻址位置（默认 `realsense_bin=rs-pipe`），
   按平台安装依赖后构建（见下方「rs-pipe 真机构建」）。
2. 管理台「添加摄像头 → 深度相机」一键添加：自动检测设备并列出型号与
   序列号，选择常用档位（640×480@15 / 848×480@30 / 1280×720@30）后一次
   创建三路摄像头（`前缀-彩色` / `-深度` / `-红外`），源端口由服务端固定
   推导，无需手工填写；采集器路径也可在该流程或「系统信息 → 深度相机 →
   采集器配置」里在线修改，保存即时生效。
3. 有任一 `realsense` 摄像头启用时 adapter 自动拉起 rs-pipe，全部停用后自动回收。

#### rs-pipe 真机构建

librealsense 依赖以构建标签 `realsense` 隔离：默认构建（含 CI / goreleaser /
交叉编译）零 cgo 依赖；真机版需系统安装 librealsense2 后带标签编译。
无 SDK 环境直接 `go build -o rs-pipe ./cmd/rs-pipe` 得到 fake-only 版本，
配合 `--fake` 即可离线验证全链路。

**Linux（Ubuntu/Debian）**：

```bash
sudo apt install librealsense2-dev        # 或按 librealsense 文档从源码仓库安装
CGO_ENABLED=1 go build -tags realsense -o rs-pipe ./cmd/rs-pipe
```

**Windows**：

1. 装 [librealsense SDK](https://github.com/IntelRealSense/librealsense/releases)
   Windows 安装包（自带 USB 驱动），默认路径
   `C:\Program Files (x86)\Intel RealSense SDK 2.0`；
   可先用其自带的 RealSense Viewer 确认设备出图正常。
2. 装 cgo 所需的 gcc（三选一）：MSYS2 的
   `pacman -S mingw-w64-ucrt-x86_64-gcc`，或 [WinLibs](https://winlibs.com/)
    解压后加入 PATH，或 [w64devkit](https://github.com/skeeto/w64devkit)
    单文件自解压版（实测 gcc 16.2 + SDK 2.59.1 可用）。
3. 编译（PowerShell，SDK 装在非默认搜索路径，需环境变量补充头文件/库路径）：

   ```powershell
   $env:CGO_ENABLED = "1"
    # 注意：路径必须用正斜杠且不能含空格（cgo 按空格切分标志，反斜杠/空格都会导致
    # "could not determine what C.rs2_* refers to"）；默认安装路径的空格用 8.3 短路径
    # 规避（cmd /c dir /x "C:\Program Files (x86)" 查短名，通常 Intel RealSense SDK 2.0 → INTELR~1.0）：
    $env:CGO_CFLAGS = '-IC:/PROGRA~2/INTELR~1.0/include'
    $env:CGO_LDFLAGS = '-LC:/PROGRA~2/INTELR~1.0/lib/x64 -lrealsense2'
    go build -tags realsense -o rs-pipe.exe .\cmd\rs-pipe
   ```

4. 把 SDK `bin\x64\realsense2.dll` 复制到 `rs-pipe.exe` 同目录（或将该目录
   加入 PATH），否则启动报找不到 DLL；首次运行 Windows 防火墙弹窗时放行
   rs-pipe 的本地监听。

**macOS**：`brew install librealsense`（注意：官方对 macOS 支持有限，
D4 系列需 USB3 且部分功能不可用，建议优先 Linux/Windows）。

**验证**（三步都通再到管理台加摄像头）：

```bash
rs-pipe list        # 应列出序列号与型号
rs-pipe serve       # 监听 18020/18021/18022
ffplay -f rawvideo -pixel_format rgb24 -video_size 640x480 -framerate 15 \
  tcp://127.0.0.1:18021    # 直接看伪彩色深度画面
```

深度伪彩色采用固定范围 Turbo 映射（默认 300–8000mm，`--depth-min/--depth-max`
可调），颜色可复现、可用于目测距离；无效深度点（0）显示为纯黑。
`realsense_fake=true`（配置文件）或 `rs-pipe serve --fake` 以合成帧运行——
无硬件也能全链路验证（彩条移动/深度斜面+移动方块/灰度同步）。

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
- **多屏采集与显示拓扑变化**：屏幕摄像头按配置时刻的虚拟桌面坐标定位显示器；
  RDP 接管、扩展坞/显示器断开等使坐标越出当前虚拟桌面时，该路采集自动**挂起等待**
  （日志与 UI 状态给出"等待显示恢复"与具体坐标差异，不再无限重启刷屏），
  显示器恢复后数秒内自动续采。远程桌面场景可在客户端勾选"使用所有监视器"

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
| GET / POST | `/api/mosaics` | 融合布局列表（含运行状态/格健康度/推流会话）/ 新增（body `{"name","width","height","framerate","bitrate_kbps","loss_policy","cells":[{camera_id,x,y,w,h}],"audio_camera_id","enabled"}`） |
| PUT / DELETE | `/api/mosaics/{id}` | 更新布局（整量替换 cells）/ 删除（虚拟通道一并移除） |
| POST | `/api/mosaics/{id}/push` | 开始整流直推（body `{"target":"rtsp://..."}`，返回 token；会话内存态，凭证不落盘） |
| DELETE | `/api/mosaics/{id}/push/{token}` | 停止整流直推 |
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
│   └── stream/               # ffmpeg 取流进程监督（退避重启）+ 融合流合成
│                             #  （mosaic.go 管理与 restart 模式 / mosaicpipe.go blackframe 兜底）
├── web/                      # Vue3 管理台（构建产物内嵌）
├── Dockerfile                # 多阶段：node 构建前端 → go 构建后端 → alpine+ffmpeg
├── docker-compose.yml        # 单容器双模式：host 网络（默认）/ bridge（--profile bridge，Win/mac）
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
| `--video-encoder` | `VIDEO_ENCODER` | `auto` | 视频编码器：`auto`/`libx264` 均为软编码；`h264_v4l2m2m` 为 V4L2 硬件编码（实验性，见下） |
| `--discovery` | `DISCOVERY` | `true` | 是否开启 WS-Discovery |
| `--auto-cameras` | `AUTO_CAMERAS` | `true` | 启动时自动发现并启用本机摄像头（未配置的设备自动创建） |
| `--auto-screens` | `AUTO_SCREENS` | `true` | 启动时自动发现并启用本机显示器（未配置的屏幕自动创建） |
| `--log-level` | `LOG_LEVEL` | `info` | 日志级别 |

## 帧率与硬件编码（树莓派等 arm 设备）

- **帧率配置即生效**：设备采集源（v4l2/dshow/avfoundation）的输入帧率请求可能被驱动
  静默忽略（实测配置 10fps 回退 30fps），程序在输出侧用 `-r` 强制对齐配置帧率——
  高于配置抽帧、低于配置补帧。rtsp 源按源节奏转码、testsrc 自带节奏，均不强制。
- **硬件编码（实验性，opt-in）**：`--video-encoder=h264_v4l2m2m` 在带 V4L2 M2M
  硬件编码器的设备（树莓派 3/4）上用硬件编码，可将 armv7 上约 200% 的编码 CPU
  降到零头。**默认不启用**——实测树莓派 3B（Raspbian trixie / ffmpeg 7.1 /
  bcm2835-codec）上本地编码正常，但经 RTSP 分发后中途加入的读者报
  `non-existing PPS`：bcm2835 编码器不随 GOP 周期重复 SPS/PPS，
  `dump_extra=freq=keyframe`、去掉 `-g` 等变体均无效；树莓派 5 已移除硬件
  编码器。待驱动/上游修复后再评估默认启用。注意硬件编码不支持 x264 的
  `-preset`/`-tune`，参数自动分派。

## 开机自启

三平台（Windows / Linux / macOS）统一提供 `install` / `uninstall` 子命令，一键注册/卸载
开机自启；`install -dry-run` 只打印将注册的内容，不落盘：

```bash
# 注册：记录当前可执行文件与 data-dir，并从既有配置重建完整启动参数
local-onvif-adapter install -data-dir /path/to/data
# 预览将注册的内容（不落盘）
local-onvif-adapter install -data-dir /path/to/data -dry-run
# 卸载（幂等）
local-onvif-adapter uninstall -data-dir /path/to/data
```

各平台实现：

| 平台 | 机制 | 触发时机 |
|---|---|---|
| Windows | HKCU `Run` 注册表 + data-dir 下 VBS 隐藏窗口启动器（免管理员） | 用户登录（静默无闪窗） |
| Linux | systemd 用户单元 `~/.config/systemd/user/local-onvif-adapter.service` + `enable --now`，并尝试 `loginctl enable-linger` | 开机即启（linger 成功时），否则首次登录 |
| macOS | launchd `~/Library/LaunchAgents/com.linuxsuren.local-onvif-adapter.plist` + `launchctl load` | 用户登录 |

通用说明：

- **登录会话而非系统服务**：采集桌面（gdigrab/x11grab/avfoundation）与摄像头
  （dshow/avfoundation）都要求进程运行在交互式用户会话中；Linux 的 v4l2/rtsp 源
  不依赖图形会话，`enable-linger` 成功后即为真正的开机即启（无头场景）
- Linux 下在容器/SSH 中执行 `install`（无 systemd 用户会话）会写入单元文件但
  `systemctl --user` 失败，按提示在本机登录会话中补执行 `daemon-reload` + `enable`
  即可；**容器场景不要用 systemd**，用 compose 的 `restart: unless-stopped`——
  Docker 服务随宿主机启动后容器自动拉起，即容器版的"开机自启"
- macOS 的 launchd 作业日志落在 `data-dir/autostart.out.log` / `autostart.err.log`
- 注意：移动可执行文件或数据目录后需重新执行 `install`；主程序每次启动会把
  命令行参数写回配置文件，因此 `install` 会显式携带当前配置的完整参数，
  避免自启动把用户配置重置为默认值

### 手动配置（兜底）

`install` 生成的单元/plist 与以下手工写法等价，特殊定制时参考：

Linux（systemd 用户单元）：

```bash
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/local-onvif-adapter.service <<'EOF'
[Unit]
Description=local ONVIF adapter
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/local-onvif-adapter -data-dir %h/.local-onvif-adapter
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload && systemctl --user enable --now local-onvif-adapter
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
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>StandardOutPath</key><string>/Users/USERNAME/.local-onvif-adapter/autostart.out.log</string>
  <key>StandardErrorPath</key><string>/Users/USERNAME/.local-onvif-adapter/autostart.err.log</string>
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
