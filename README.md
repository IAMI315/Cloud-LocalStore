# Cloud-LocalStore 本地原型

这是一个可运行的本地优先同步原型：文件按固定 4 MiB 分块，使用 BLAKE3 校验，客户端保存本地版本和对象，VPS 保存元数据、操作日志和对象副本。当前版本已具备客户端配对、设备令牌鉴权、双向文件同步和桌面 GUI 闭环；HTTPS、冲突解决策略和后台常驻同步仍属于后续增强项。

## 阶段 3A 服务端

服务端默认使用 `data/metadata.db`，监听回环地址 `127.0.0.1:8080`。只有显式配置 `listen_addr` 或 `--addr` 时才会监听其他地址；合法的 IPv4、IPv6 和 `0.0.0.0`/`[::]` 均可使用。开发启动：

```powershell
$env:GOCACHE = "$PWD\go-cache"
$env:GOMODCACHE = "$PWD\go-mod-cache"
$env:GOTMPDIR = "$PWD\go-tmp"
go run .\cmd\clouddrive-server --addr 127.0.0.1:8080
```

可用配置文件（简单 `key: value` 格式）：

```text
data_dir: data
database_path: data/metadata.db
listen_addr: 127.0.0.1:8080
# 可选：不填写时首次启动自动生成并持久化到 metadata.db
# pairing_token: 在受控环境中也可以显式指定
# dev_mode: false
```

需要让其他设备访问时，必须显式绑定目标地址，例如 `--addr 0.0.0.0:8080` 或在配置文件中设置
`listen_addr: 192.0.2.10:8080`。当前服务使用 HTTP；业务接口要求已配对设备的 bearer 令牌，首次注册还要求配对码。生产环境仍应通过防火墙限制来源，或放在 TLS 反向代理后。

### 客户端首次启动与配对

Linux 服务端建议固定部署在 `/opt/cloud-localstore`。服务首次启动时会在服务端数据库中生成并持久化配对码。管理员在服务端执行下面的命令查看配对码，命令只输出到当前终端：

```bash
/opt/cloud-localstore/bin/clouddrive-server \
  --config /opt/cloud-localstore/server.conf --show-pairing
```

桌面客户端 `clouddrive-desktop.exe` 首次启动时，会在 exe 同级创建 `CloudLocalStore-Sync`（同步文件夹）和 `CloudLocalStore-State`（本地数据库、对象缓存及连接配置）。打开窗口的“连接配置”区域，填写服务地址（例如 `http://186.244.241.96:18080`）、空间 ID、设备 ID 和服务端配对码，点击“保存连接”。客户端会调用设备注册接口，保存服务端返回的设备 bearer 令牌；之后配对码和设备令牌会持久化在 `CloudLocalStore-State/config.json` 中，后续启动会直接加载，不需要重复配置。密码框不会回显，重新保存时留空会保留已有凭证。

点击“立即同步”即可执行一次双向同步：先拉取服务端操作、下载并校验远端文件，再扫描本地同步文件夹、上传缺失分块、提交本地变更并确认 ACK。保存有效连接后，桌面客户端会自动启动文件监控；在 `CloudLocalStore-Sync` 中新建或修改文件会触发自动双向同步，连接暂时失败时会在后续文件事件或定时周期重试。同步完成后，文件会出现在窗口的“文件”列表中，也可以直接在同步文件夹中查看。多个客户端使用同一个空间 ID、各自不同的设备 ID 和同一配对码即可分别注册，之后所有业务请求都使用各自的设备令牌。

服务端不会通过匿名 `/v1/pairing/start` 接口返回配对码；正式模式下该接口关闭。请不要把配对码或设备令牌写入日志、截图或公开仓库。当前 HTTP 部署没有传输加密，只适合已限制访问的测试环境。

接口示例：

```powershell
Invoke-RestMethod http://127.0.0.1:8080/healthz
Invoke-RestMethod http://127.0.0.1:8080/readyz
$body = @{space_id="demo"; device_id="laptop"; name="开发机"} | ConvertTo-Json
Invoke-RestMethod -Method Post -ContentType application/json -Uri http://127.0.0.1:8080/v1/devices/register -Body $body
Invoke-RestMethod -Headers @{"X-Device-ID"="laptop"} http://127.0.0.1:8080/v1/spaces/demo/state
```

等价的 curl 请求：

```bash
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/readyz
curl -H 'Content-Type: application/json' -d '{"space_id":"demo","device_id":"laptop","name":"dev","pairing_token":"<配对码>"}' http://127.0.0.1:8080/v1/devices/register
curl -H 'X-Device-ID: laptop' -H 'Authorization: Bearer <设备令牌>' http://127.0.0.1:8080/v1/spaces/demo/state
```

### 阶段 3B revision 操作日志

设备注册后，客户端使用 `X-Device-ID` 加 `Authorization: Bearer <device_token>` 访问工作区的最小操作日志接口。操作提交分为预提交和正式提交两步；`parent_revision` 必须等于当前工作区 revision，重复使用同一个 `operation_id` 会返回原结果，复用同一 ID 但提交不同元数据会返回冲突。

```bash
curl -H 'Content-Type: application/json' -H 'X-Device-ID: laptop' \
	-H 'Authorization: Bearer <设备令牌>' \
  -d '{"operation_id":"op-1","parent_revision":0,"path_id":"hello.txt","operation_type":"update","device_local_sequence":1}' \
  http://127.0.0.1:8080/v1/spaces/demo/operations/prepare
curl -H 'Content-Type: application/json' -H 'X-Device-ID: laptop' \
	-H 'Authorization: Bearer <设备令牌>' \
  -d '{"operation_id":"op-1"}' \
  http://127.0.0.1:8080/v1/spaces/demo/operations/commit
curl -H 'X-Device-ID: laptop' -H 'Authorization: Bearer <设备令牌>' \
  'http://127.0.0.1:8080/v1/spaces/demo/operations?after_revision=0'
curl -H 'Content-Type: application/json' -H 'X-Device-ID: laptop' \
	-H 'Authorization: Bearer <设备令牌>' \
  -d '{"ack_revision":1,"last_seen_revision":1,"last_applied_revision":1}' \
  http://127.0.0.1:8080/v1/spaces/demo/ack
```

设备撤销后，预提交、提交、日志查询和 ACK 都会立即拒绝。对象 API 已支持对象存在性查询、分块上传/下载和提交前的 manifest 对象校验，并由客户端同步引擎串联为双设备文件同步。

### 对象 API（阶段 4 起）

对象使用小写 BLAKE3-256 哈希寻址。对象接口要求 `X-Device-ID` 和 bearer 令牌，也可通过 `X-Space-ID` 指明客户端当前工作区；服务端会拒绝非活动设备。单个对象最大 4 MiB，上传前客户端应先用 `/v1/objects/exists` 批量查询缺失对象。

```bash
curl -H 'Content-Type: application/json' -H 'X-Device-ID: laptop' -H 'X-Space-ID: demo' \
  -d '{"hashes":["<64位小写BLAKE3哈希>"]}' \
  http://127.0.0.1:8080/v1/objects/exists
curl -X POST -H 'X-Device-ID: laptop' -H 'X-Space-ID: demo' \
  --data-binary @chunk.bin \
  'http://127.0.0.1:8080/v1/objects/upload?hash=<64位小写BLAKE3哈希>'
curl -H 'X-Device-ID: laptop' -H 'X-Space-ID: demo' \
  -o chunk.out \
  http://127.0.0.1:8080/v1/objects/<64位小写BLAKE3哈希>
```

对象上传已使用临时文件、`fsync` 和原子重命名，重复上传同一哈希是幂等的。以 64 位十六进制哈希表示的 manifest 在 operation commit 时会被读取并逐块检查；旧版非哈希占位 manifest 仍保持兼容。当前客户端同步引擎已经支持最小的上传、revision 提交、远端下载、校验、原子应用和 ACK 闭环；watcher 集成、离线队列、冲突处理和 P2P 仍在后续阶段实现。

`/healthz` 只表示进程能够响应；`/readyz` 还会检查 SQLite 可写以及数据目录可创建临时文件。服务端当前使用 HTTP，没有 TLS 加密；显式绑定非回环地址前请确认这是有意配置，并通过防火墙或 TLS 反向代理限制访问。

## 构建

```powershell
$env:Path = "D:\Develop\go-toolchain\go\bin;$env:Path"
$env:GOCACHE = "$PWD\go-cache"
$env:GOMODCACHE = "$PWD\go-mod-cache"
$env:GOTMPDIR = "$PWD\go-tmp"
go build -o build-temp\clouddrive-agent.exe .\cmd\clouddrive-agent
# Windows 双击入口：隐藏控制台窗口，页面资源随 exe 嵌入
go build -ldflags "-H=windowsgui" -o build-temp\clouddrive-desktop.exe .\cmd\clouddrive-desktop
```

构建后直接双击 `build-temp\clouddrive-desktop.exe` 即可启动本地 GUI。程序会绑定
`127.0.0.1:8765`，并在 Cloud LocalStore 自己的窗口中显示嵌入式 HTTP 控制台，不需要先输入
`gui` 命令；关闭窗口时会停止 HTTP 服务、目录监控并关闭本地数据库。页面、CSS 和
JavaScript 通过 Go `embed.FS` 编入 exe，不依赖 Node.js 或外部静态文件。隐藏控制台后，
启动失败和运行错误写入 exe 同目录的 `clouddrive-desktop.log`（例如
`build-temp\clouddrive-desktop.log`）。需要在终端诊断时可以不使用 `-H=windowsgui`，或
运行 `clouddrive-desktop.exe --addr 127.0.0.1:8765`，GUI 服务仍会拒绝非 loopback 地址。

Windows 桌面程序支持通知区域托盘：点击窗口最小化按钮时窗口会隐藏到托盘，后台同步和目录监控继续运行；右键托盘图标可以选择“显示窗口”或“退出程序”。关闭窗口仍表示退出程序，退出前会停止监控并关闭本地数据库。
程序同时使用 Windows 单实例锁；如果程序已经最小化到托盘，再次双击 exe 会立即退出，请从托盘图标选择“显示窗口”。
退出时会先取消后台同步，再关闭本地服务和数据库；网络异常不会无限阻塞桌面窗口关闭。

桌面窗口使用纯 Go 的 WebView2 绑定作为窗口外壳，复用嵌入式 Web GUI，不包含额外浏览器引擎；
Windows 需要已安装 Microsoft Edge WebView2 Runtime（多数 Windows 设备已有 Evergreen Runtime）。
若运行时缺失，程序会显示错误提示并将诊断写入日志。GUI 还提供服务地址、空间、设备、设备令牌和配对码配置；“立即同步”执行一次完整双向同步。设备令牌不会回显到页面，空密码框保存连接时会保留已保存令牌。

## 演示流程

启动本地 Web 控制台（页面和 CSS/JS 已嵌入可执行文件，不需要 Node.js）。控制台没有登录层，只允许绑定 `127.0.0.1`、`localhost` 或 `::1` 等回环地址，不能通过局域网访问；不接受 `0.0.0.0` 或外部地址：

```powershell
& .\build-temp\clouddrive-agent.exe gui
# 不自动打开浏览器时：
& .\build-temp\clouddrive-agent.exe gui --addr 127.0.0.1:8765 --no-browser
```

浏览器打开控制台后，输入同步目录和可选数据目录，点击“初始化并打开”或“打开本地库”。同步目录为空时，页面会先弹出系统文件夹选择窗口；也可以随时点击“浏览”重新选择。Windows 使用进程内的原生系统文件夹选择对话框，不启动 PowerShell、cmd 或其他命令行窗口；取消选择会停止本次操作。非 Windows 平台不提供系统选择窗口，需要手动填写路径。页面支持手动扫描、状态和文件列表、查看版本、恢复历史版本，以及启动/停止本地目录监控。连接配置支持 HTTP 服务地址、空间和设备配对，立即同步会刷新本地文件列表。GUI API 只接受本地目录和同步目录内的相对文件路径。

```powershell
$root = "D:\Develop\CloudDemo"
New-Item -ItemType Directory -Force $root | Out-Null
& .\build-temp\clouddrive-agent.exe init $root
Set-Content "$root\hello.txt" "version one"
& .\build-temp\clouddrive-agent.exe scan $root
& .\build-temp\clouddrive-agent.exe status $root
& .\build-temp\clouddrive-agent.exe versions $root hello.txt
```

修改文件后再次执行 `scan` 会产生新的本地版本。`versions` 输出版本 ID，例如 `v-...`。恢复历史版本：

```powershell
& .\build-temp\clouddrive-agent.exe restore $root hello.txt v-版本ID
Get-Content "$root\hello.txt"
```

持续监控目录：

```powershell
& .\build-temp\clouddrive-agent.exe run $root --interval 2m
```

自定义状态目录：

```powershell
& .\build-temp\clouddrive-agent.exe init $root --data-dir D:\Develop\CloudDemoState
```

`init` 会写入 `config.json`、`metadata.db` 和 `objects/`。元数据目录会从同步扫描中排除。命令行 Agent 主要用于本地扫描、版本和恢复；服务端连接、配对和双向同步从桌面 GUI 的连接配置区域完成。

客户端同步最小闭环可通过库 API 接入 Agent：创建 `objectstore.LocalFS`、`store.ClientStore` 和 `objectstore.RemoteClient` 后调用 `sync.NewEngine(...).Sync(ctx)`。一次调用会拉取并校验远端 operations，下载对象后原子替换同步根目录内的文件并提交 ACK，再扫描本地文件、上传缺失对象并执行 prepare/commit。调用可被 context 取消，也可在失败后安全重试；同步根目录外的路径会被拒绝，下载或校验失败不会推进 ACK。watcher 只应触发新的 `Sync` 调用，不应在回调中直接执行网络请求。
