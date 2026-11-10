# Cloud-LocalStore 本地原型

这是项目的第一个可运行雏形。它把一个普通本地目录建立为本地版本库：文件按固定 4 MiB 分块，使用 BLAKE3 校验，数据保存在本地 `.clouddrive` 目录。当前版本不连接 VPS，后续同步协议将在此基础上加入。

## 构建

```powershell
$env:Path = "D:\Develop\go-toolchain\go\bin;$env:Path"
$env:GOCACHE = "D:\Develop\Temp\Cloud-LocalStore-commit-checkout\go-cache"
$env:GOMODCACHE = "D:\Develop\Temp\Cloud-LocalStore-commit-checkout\go-mod-cache"
$env:GOTMPDIR = "D:\Develop\Temp\Cloud-LocalStore-commit-checkout\go-tmp"
go build -o build-temp\clouddrive-agent.exe .\cmd\clouddrive-agent
```

## 演示流程

启动本地 Web 控制台（页面和 CSS/JS 已嵌入可执行文件，不需要 Node.js）。控制台没有登录层，只允许绑定 `127.0.0.1`、`localhost` 或 `::1` 等回环地址，不能通过局域网访问；不接受 `0.0.0.0` 或外部地址：

```powershell
& .\build-temp\clouddrive-agent.exe gui
# 不自动打开浏览器时：
& .\build-temp\clouddrive-agent.exe gui --addr 127.0.0.1:8765 --no-browser
```

浏览器打开控制台后，输入同步目录和可选数据目录，点击“初始化并打开”或“打开本地库”。页面支持手动扫描、状态和文件列表、查看版本、恢复历史版本，以及启动/停止本地目录监控。网络状态会显示“未连接（本地原型）”；GUI API 只接受本地目录和同步目录内的相对文件路径。

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

`init` 会写入 `config.json`、`metadata.db` 和 `objects/`。元数据目录会从同步扫描中排除。`status` 中的 `Network: not connected (local prototype)` 表示 VPS 同步尚未接入。
