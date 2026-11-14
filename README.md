# Cloud LocalStore

Cloud LocalStore 是一个 Windows 优先的本地优先文件同步系统。文件先落在客户端本地，客户端维护索引、版本和内容寻址对象；服务端保存工作空间元数据、修订记录和对象副本。多个设备通过带设备令牌认证的 HTTP API 同步同一个工作空间。

仓库只包含运行所需的 Go 源码、前端资源、图标和构建辅助脚本。项目不使用 GitHub Actions，编译和发布由维护者在本地完成。

## 功能

- Windows 桌面客户端、通知区域托盘、单实例和开机启动。
- 本地文件扫描、目录监控、版本历史、恢复和冲突副本。
- 双向同步、修订号、确认机制和离线追赶。
- BLAKE3-256 内容寻址分块与原子对象写入。
- 设备配对、Bearer 令牌、设备撤销和工作空间隔离。
- 服务端空间容量设置、使用量统计和超额拒绝。
- 服务端管理控制台、备份、垃圾回收和二进制升级。
- 服务端使用服务器本地时区记录时间，客户端使用客户端本地时区；协议时间使用带偏移量的 RFC3339。

## 项目结构

```text
cmd/clouddrive-agent/       命令行客户端入口
cmd/clouddrive-desktop/     Windows 桌面客户端入口
cmd/clouddrive-server/      服务端入口
internal/agent/              命令行客户端逻辑
internal/desktop/            Windows 窗口、托盘和单实例
internal/filesystem/         文件扫描和目录监控
internal/gui/                本地管理界面和嵌入式前端
internal/localapp/           客户端数据库和本地应用
internal/objectstore/        本地与远端对象存储
internal/protocol/           客户端/服务端协议类型
internal/server/             HTTP 服务端和管理服务
internal/store/              SQLite 存储与迁移
internal/sync/               双向同步引擎
tools/                       Windows 资源辅助工具
```

## 环境要求

- Go 1.24 或兼容的 Go 1.24 工具链。
- Windows 桌面程序需要 Microsoft Edge WebView2 Runtime。
- 构建 Linux 服务端不需要安装 SQLite 或 Node.js。
- Python 仅用于给 Windows 桌面程序写入图标资源。

确认工具链可用：

```powershell
go version
python --version
```

## 本地构建

在项目根目录执行以下命令。构建产物放在 `build-temp/`，该目录已被 Git 忽略。

```powershell
New-Item -ItemType Directory -Force build-temp | Out-Null
go mod download
go vet ./...
go mod verify
node --check internal\gui\web\app.js
```

构建命令行客户端：

```powershell
go build -trimpath -o build-temp\clouddrive-agent.exe .\cmd\clouddrive-agent
```

构建 Windows 桌面客户端：

```powershell
go build -trimpath -ldflags '-H=windowsgui' -o build-temp\clouddrive-desktop.raw.exe .\cmd\clouddrive-desktop
python tools\inject_windows_icon.py build-temp\clouddrive-desktop.raw.exe internal\desktop\app-icon.ico
Copy-Item -Force build-temp\clouddrive-desktop.raw.exe build-temp\clouddrive-desktop.exe
```

构建 Linux amd64 服务端：

```powershell
$env:GOOS = "linux"
$env:GOARCH = "amd64"
$env:CGO_ENABLED = "0"
go build -trimpath -o build-temp\clouddrive-server-linux-amd64 .\cmd\clouddrive-server
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
```

## 客户端使用

命令行客户端的基本流程：

```powershell
.\build-temp\clouddrive-agent.exe init D:\CloudData
.\build-temp\clouddrive-agent.exe scan D:\CloudData
.\build-temp\clouddrive-agent.exe status D:\CloudData
```

默认状态目录是同步目录下的 `.clouddrive`。可以使用 `--data-dir` 将状态目录放到其他位置。

桌面客户端首次启动后，在设置界面填写服务地址、空间 ID、设备 ID 和配对码。每个客户端必须使用不同的设备 ID，设备令牌会保存在本地。桌面客户端支持手动同步、目录监控、文件历史、恢复、搜索、详细/图标视图、文件拖出、框选、空间容量配置和 Windows 开机启动。

## 服务端使用

服务端可以直接在 Linux 目录中运行：

```bash
mkdir -p /opt/cloud-localstore/bin
cp clouddrive-server-linux-amd64 /opt/cloud-localstore/bin/clouddrive-server
chmod +x /opt/cloud-localstore/bin/clouddrive-server
/opt/cloud-localstore/bin/clouddrive-server --config /opt/cloud-localstore/server.conf --serve
```

常用命令：

```bash
clouddrive-server --version
clouddrive-server --show-pairing
clouddrive-server --once
clouddrive-server --menu
```

第一次启动会生成配置文件、SQLite 数据库、对象目录和配对码。管理控制台可以查看设备和空间使用量、执行备份、垃圾回收、服务管理和二进制升级。公网部署前应使用 TLS 反向代理、防火墙和安全的令牌管理；内置 HTTP 服务不提供公网传输加密。

默认配置：

```text
data_dir: data
database_path: data/metadata.db
listen_addr: 0.0.0.0:18080
storage_quota_bytes: 5368709120
```

## API 概览

```text
GET  /healthz
GET  /readyz
POST /v1/devices/register
POST /v1/devices/revoke
GET  /v1/spaces/{space}/state
GET  /v1/spaces/{space}/events
POST /v1/spaces/{space}/operations/prepare
POST /v1/spaces/{space}/operations/commit
GET  /v1/spaces/{space}/operations
POST /v1/spaces/{space}/ack
GET  /v1/spaces/{space}/quota
PUT  /v1/spaces/{space}/quota
POST /v1/objects/exists
POST /v1/objects/upload
GET  /v1/objects/{hash}
```

业务接口需要 `X-Device-ID` 和 `Authorization: Bearer <device-token>`。服务端不接受客户端生成的服务端时间戳，服务端写入同步时间时使用自己的本地时钟。

## 数据与安全

以下内容只在本地生成，禁止提交到仓库：

- `server.conf`、配对码和设备令牌
- `data/`、`CloudLocalStore-State/`、`CloudLocalStore-Sync/`
- SQLite 数据库、WAL 文件、日志和构建缓存
- 本地二进制、临时文件和运行状态

`.gitignore` 已覆盖上述运行数据。对象使用 BLAKE3-256 哈希寻址，数据库迁移由程序启动时自动执行。

## 版本与发布

仓库不包含 GitHub Actions 工作流，也不提交构建产物。发布时在目标平台本地构建对应程序，并通过独立的发布渠道分发二进制文件。
