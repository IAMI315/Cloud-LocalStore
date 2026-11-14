package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/IAMI315/Cloud-LocalStore/internal/server"
)

const consoleGracePeriod = 48 * time.Hour

func runInteractive(app *server.App, rt *serverRuntime, configPath string) error {
	reader := bufio.NewReader(os.Stdin)
	if rt.AttachIfHealthy() {
		fmt.Fprintln(os.Stderr, "检测到已有服务正在运行，管理控制台已附着；不会重复启动。")
	}
	for {
		rt.Refresh()
		printDashboard(app, rt, configPath)
		fmt.Println("\n请选择操作：")
		fmt.Println("  1. 服务管理")
		fmt.Println("  2. 查看同步文件")
		fmt.Println("  3. 文件管理")
		fmt.Println("  4. 存储空间管理")
		fmt.Println("  5. 设备与配对管理")
		fmt.Println("  6. 数据备份与恢复")
		fmt.Println("  7. 检查更新")
		fmt.Println("  8. 执行升级")
		fmt.Println("  9. 查看运行错误")
		fmt.Println("  0. 退出管理菜单")
		choice := readLine(reader, "请输入选项：")
		if choice == "" {
			fmt.Println("管理菜单已退出，服务状态保持不变。")
			return nil
		}
		switch choice {
		case "1":
			serviceMenu(reader, rt, app, configPath)
		case "2":
			fileListMenu(reader, app)
		case "3":
			fileManageMenu(reader, app)
		case "4":
			storageMenu(reader, app, rt)
		case "5":
			deviceMenu(reader, app)
		case "6":
			backupMenu(reader, app, rt, configPath)
		case "7":
			printVersionInfo(reader)
		case "8":
			upgradeMenu(reader)
		case "9":
			showRuntimeError(reader, rt)
		case "0":
			fmt.Println("管理菜单已退出，服务状态保持不变。")
			return nil
		default:
			fmt.Println("无效选项。")
			pause(reader)
		}
	}
}

func printDashboard(app *server.App, rt *serverRuntime, configPath string) {
	fmt.Println("\n========================================")
	fmt.Println("       Cloud LocalStore Server")
	fmt.Println("========================================")
	status := "已停止"
	if rt.Running() {
		status = "运行中"
		if rt.External() {
			status = "运行中（当前控制台已接管）"
		}
	}
	fmt.Printf("服务状态：%s\n", status)
	fmt.Printf("监听地址：http://%s\n", app.Config().ListenAddr)
	fmt.Printf("配置文件：%s\n", configPath)
	usage, err := app.AdminUsage(context.Background())
	if err != nil {
		fmt.Printf("状态统计：读取失败（%v）\n", err)
		return
	}
	fmt.Printf("工作空间：%d  设备：%d  操作记录：%d\n", usage.WorkspaceCount, usage.DeviceCount, usage.OperationCount)
	fmt.Printf("对象文件：%d 个，%s  临时文件：%d 个，%s\n", usage.ObjectCount, formatBytes(usage.ObjectBytes), usage.TemporaryCount, formatBytes(usage.TemporaryBytes))
}

func serviceMenu(reader *bufio.Reader, rt *serverRuntime, app *server.App, configPath string) {
	for {
		fmt.Println("\n--- 服务管理 ---")
		fmt.Println("  1. 查看服务状态")
		fmt.Println("  2. 启动服务")
		fmt.Println("  3. 停止服务")
		fmt.Println("  4. 重启服务")
		fmt.Println("  5. 配置开机自动启动")
		fmt.Println("  6. 取消开机自动启动")
		fmt.Println("  0. 返回")
		switch readLine(reader, "请选择：") {
		case "1":
			if rt.Running() {
				if rt.External() {
					fmt.Printf("服务已由当前控制台接管，启动时间：%s\n", rt.StartedAt().Format(time.RFC3339))
				} else {
					fmt.Printf("服务运行中，启动时间：%s\n", rt.StartedAt().Format(time.RFC3339))
				}
			} else {
				fmt.Println("服务未运行。")
			}
			pause(reader)
		case "2":
			if rt.Running() {
				fmt.Println("服务已经运行。")
				pause(reader)
				continue
			}
			if err := rt.StartBackground(configPath); err != nil {
				fmt.Printf("启动失败：%v\n", err)
			} else {
				fmt.Println("服务已启动。")
			}
			pause(reader)
		case "3":
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := rt.Stop(ctx)
			cancel()
			if err != nil {
				fmt.Printf("停止失败：%v\n", err)
			} else {
				fmt.Println("服务已停止。")
			}
			pause(reader)
		case "4":
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = rt.Stop(ctx)
			cancel()
			err := rt.StartBackground(configPath)
			if err != nil {
				fmt.Printf("重启失败：%v\n", err)
			} else {
				fmt.Println("服务已重启。")
			}
			pause(reader)
		case "5":
			if err := configureAutostart(app, configPath); err != nil {
				fmt.Printf("配置开机自启失败：%v\n", err)
			} else {
				fmt.Println("开机自启已配置；重启系统后会自动启动服务。")
			}
			pause(reader)
		case "6":
			if err := disableAutostart(); err != nil {
				fmt.Printf("取消开机自启失败：%v\n", err)
			} else {
				fmt.Println("开机自启已取消。")
			}
			pause(reader)
		case "0":
			return
		default:
			fmt.Println("无效选项。")
		}
	}
}

func fileListMenu(reader *bufio.Reader, app *server.App) {
	space := readLine(reader, "空间 ID（留空显示全部）：")
	files, err := app.ListAdminFiles(context.Background(), space)
	if err != nil {
		fmt.Printf("读取文件列表失败：%v\n", err)
		pause(reader)
		return
	}
	if len(files) == 0 {
		fmt.Println("没有同步文件记录。")
		pause(reader)
		return
	}
	fmt.Printf("\n%-18s %-46s %-10s %-8s %-20s\n", "空间", "路径", "状态", "版本", "时间")
	fmt.Println(strings.Repeat("-", 110))
	for _, f := range files {
		state := "已同步"
		if f.OperationType == "delete" {
			state = "已删除"
		}
		fmt.Printf("%-18s %-46s %-10s %-8d %-20s\n", truncate(f.SpaceID, 18), truncate(f.Path, 46), state, f.Revision, f.CreatedAt)
	}
	fmt.Printf("\n共 %d 条路径记录。\n", len(files))
	pause(reader)
}

func fileManageMenu(reader *bufio.Reader, app *server.App) {
	fmt.Println("\n--- 文件管理 ---")
	fmt.Println("当前版本只允许通过同步操作管理文件；服务端删除和恢复将在客户端管理接口启用后执行。")
	fmt.Println("可用操作：查看删除记录、查看版本路径、导出文件清单。")
	files, err := app.ListAdminFiles(context.Background(), "")
	if err == nil {
		deleted := 0
		for _, f := range files {
			if f.OperationType == "delete" {
				deleted++
			}
		}
		fmt.Printf("当前路径：%d，删除记录：%d\n", len(files)-deleted, deleted)
	}
	pause(reader)
}

func storageMenu(reader *bufio.Reader, app *server.App, rt *serverRuntime) {
	for {
		fmt.Println("\n--- 存储空间管理 ---")
		fmt.Println("  1. 查看存储使用情况")
		fmt.Println("  2. 清理上传临时文件")
		fmt.Println("  3. 垃圾回收并显示释放空间")
		fmt.Println("  4. 设置空间容量")
		fmt.Println("  5. 重建空间用量账本")
		fmt.Println("  0. 返回")
		switch readLine(reader, "请选择：") {
		case "1":
			usage, err := app.AdminUsage(context.Background())
			if err != nil {
				fmt.Printf("读取失败：%v\n", err)
			} else {
				fmt.Printf("数据库：%s\n对象：%s（%d 个）\n临时文件：%s（%d 个）\n", formatBytes(usage.DatabaseBytes), formatBytes(usage.ObjectBytes), usage.ObjectCount, formatBytes(usage.TemporaryBytes), usage.TemporaryCount)
				space := readLine(reader, "空间 ID（留空跳过）：")
				if space != "" {
					quota, used, quotaErr := app.Store().SpaceQuota(context.Background(), space)
					if quotaErr != nil {
						fmt.Printf("读取空间配额失败：%v\n", quotaErr)
					} else {
						fmt.Printf("空间 %s：已用 %s / 配额 %s\n", space, formatBytes(used), formatBytes(quota))
					}
				}
			}
			pause(reader)
		case "2":
			if err := runMaintenance(reader, rt, func() error { return app.CleanupUploadTemps(context.Background()) }); err != nil {
				fmt.Printf("清理失败：%v\n", err)
			} else {
				fmt.Println("上传临时文件已清理。")
			}
			pause(reader)
		case "3":
			fmt.Println("垃圾回收只会删除超过宽限期且没有被任何同步操作引用的对象。")
			if readLine(reader, "输入 GC 确认执行：") != "GC" {
				fmt.Println("已取消。")
				continue
			}
			var report server.GCReport
			err := runMaintenance(reader, rt, func() error {
				var err error
				report, err = app.GarbageCollect(context.Background(), consoleGracePeriod)
				return err
			})
			if err != nil {
				fmt.Printf("垃圾回收失败：%v\n", err)
			} else {
				fmt.Printf("已删除 %d 个对象，释放 %s；保留 %d 个对象。\n", report.RemovedFiles, formatBytes(report.RemovedBytes), report.KeptFiles)
			}
			pause(reader)
		case "4":
			space := readLine(reader, "空间 ID：")
			if space == "" {
				fmt.Println("空间 ID 不能为空。")
				continue
			}
			value := readLine(reader, "容量（GB，输入 0 恢复服务端默认）：")
			gb, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || gb < 0 {
				fmt.Println("容量必须是非负数字。")
				continue
			}
			quotaBytes := int64(gb * (1 << 30))
			quota, used, err := app.Store().SetSpaceQuota(context.Background(), space, quotaBytes)
			if err != nil {
				fmt.Printf("设置空间容量失败：%v\n", err)
			} else {
				fmt.Printf("空间容量已更新：已用 %s / 配额 %s\n", formatBytes(used), formatBytes(quota))
			}
			pause(reader)
		case "5":
			space := readLine(reader, "空间 ID：")
			if space == "" {
				fmt.Println("空间 ID 不能为空。")
				continue
			}
			if err := app.Store().RebuildSpaceUsage(context.Background(), space); err != nil {
				fmt.Printf("重建失败：%v\n", err)
			} else if quota, used, quotaErr := app.Store().SpaceQuota(context.Background(), space); quotaErr != nil {
				fmt.Printf("读取重建结果失败：%v\n", quotaErr)
			} else {
				fmt.Printf("重建完成：已用 %s / 配额 %s\n", formatBytes(used), formatBytes(quota))
			}
			pause(reader)
		case "0":
			return
		default:
			fmt.Println("无效选项。")
		}
	}
}

func runMaintenance(reader *bufio.Reader, rt *serverRuntime, task func() error) error {
	wasRunning := rt.Running()
	if wasRunning {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := rt.Stop(ctx); err != nil {
			cancel()
			return err
		}
		cancel()
	}
	err := task()
	if wasRunning {
		restartErr := rt.StartBackground("")
		if restartErr != nil && err == nil {
			err = restartErr
		}
	}
	return err
}

func configureAutostart(app *server.App, configPath string) error {
	if runtime.GOOS != "linux" {
		return errors.New("开机自启目前只支持 Linux systemd")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	userName := ""
	if current, err := user.Current(); err == nil {
		userName = current.Username
	}
	unit := fmt.Sprintf(`[Unit]
Description=Cloud LocalStore Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s --serve --config %s
WorkingDirectory=%s
Restart=on-failure
RestartSec=3
%s

[Install]
WantedBy=multi-user.target
`, systemdQuote(executable), systemdQuote(configPath), systemdQuote(filepath.Dir(executable)), systemdUser(userName))
	const unitPath = "/etc/systemd/system/cloud-localstore.service"
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return err
	}
	if output, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	if output, err := exec.Command("systemctl", "enable", "cloud-localstore.service").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func disableAutostart() error {
	if runtime.GOOS != "linux" {
		return errors.New("开机自启目前只支持 Linux systemd")
	}
	if output, err := exec.Command("systemctl", "disable", "cloud-localstore.service").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl disable: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func systemdQuote(value string) string { return value }

func systemdUser(name string) string {
	if name == "" || name == "root" {
		return ""
	}
	return "User=" + name
}

func deviceMenu(reader *bufio.Reader, app *server.App) {
	for {
		fmt.Println("\n--- 设备与配对管理 ---")
		fmt.Println("  1. 显示配对码")
		fmt.Println("  2. 查看设备")
		fmt.Println("  0. 返回")
		switch readLine(reader, "请选择：") {
		case "1":
			fmt.Printf("配对码：%s\n", app.PairingToken())
			pause(reader)
		case "2":
			devices, err := app.ListAdminDevices(context.Background())
			if err != nil {
				fmt.Printf("读取设备失败：%v\n", err)
			} else if len(devices) == 0 {
				fmt.Println("暂无设备。")
			} else {
				for _, d := range devices {
					fmt.Printf("%s  %s  %s  %s  ack=%d\n", d.SpaceID, d.DeviceID, d.Name, d.Status, d.LastAck)
				}
			}
			pause(reader)
		case "0":
			return
		default:
			fmt.Println("无效选项。")
		}
	}
}

func backupMenu(reader *bufio.Reader, app *server.App, rt *serverRuntime, configPath string) {
	destination := readLine(reader, "备份目录（留空使用当前目录 backup-时间）：")
	if destination == "" {
		destination = filepath.Join(filepath.Dir(configPath), "backup-"+time.Now().Format("20060102-150405"))
	}
	if readLine(reader, "输入 BACKUP 确认备份：") != "BACKUP" {
		fmt.Println("已取消。")
		return
	}
	err := runMaintenance(reader, rt, func() error { return createBackup(app, configPath, destination) })
	if err != nil {
		fmt.Printf("备份失败：%v\n", err)
	} else {
		fmt.Printf("备份完成：%s\n", destination)
	}
	pause(reader)
}

func createBackup(app *server.App, configPath, destination string) error {
	if app.Config().DatabasePath == ":memory:" {
		return errors.New("内存数据库不能备份")
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return err
	}
	dbTarget := filepath.Join(destination, "metadata.db")
	if err := os.RemoveAll(dbTarget); err != nil {
		return err
	}
	if _, err := app.Store().DB.Exec(`VACUUM INTO ?`, dbTarget); err != nil {
		return fmt.Errorf("backup database: %w", err)
	}
	if err := copyDir(app.Store().Objects.Root, filepath.Join(destination, "objects")); err != nil {
		return err
	}
	if configPath != "" {
		if err := copyFile(configPath, filepath.Join(destination, "server.conf")); err != nil {
			return err
		}
	}
	return nil
}

func printVersionInfo(reader *bufio.Reader) {
	fmt.Printf("当前版本：%s\n", serverVersion)
	fmt.Println("远程版本检查需要提供发行版地址；执行升级时可直接输入下载 URL 或本地文件路径。")
	pause(reader)
}

func upgradeMenu(reader *bufio.Reader) {
	executable, err := os.Executable()
	if err != nil {
		fmt.Printf("无法定位当前程序：%v\n", err)
		pause(reader)
		return
	}
	executable, _ = filepath.EvalSymlinks(executable)
	source := readLine(reader, "输入升级文件路径或下载 URL（留空取消）：")
	if source == "" {
		return
	}
	checksum := readLine(reader, "可选：输入 SHA-256（留空跳过）：")
	if readLine(reader, "输入 UPGRADE 确认替换当前程序：") != "UPGRADE" {
		fmt.Println("已取消。")
		return
	}
	tmp := executable + ".new"
	_ = os.Remove(tmp)
	if strings.HasPrefix(strings.ToLower(source), "http://") || strings.HasPrefix(strings.ToLower(source), "https://") {
		if err := downloadFile(source, tmp); err != nil {
			fmt.Printf("下载失败：%v\n", err)
			pause(reader)
			return
		}
	} else if err := copyFile(source, tmp); err != nil {
		fmt.Printf("复制失败：%v\n", err)
		pause(reader)
		return
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		fmt.Printf("设置权限失败：%v\n", err)
		return
	}
	if checksum != "" {
		got, err := fileSHA256(tmp)
		if err != nil || !strings.EqualFold(strings.TrimSpace(checksum), got) {
			_ = os.Remove(tmp)
			fmt.Printf("SHA-256 校验失败，实际值：%s\n", got)
			pause(reader)
			return
		}
	}
	if out, err := exec.Command(tmp, "--version").CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		fmt.Printf("升级文件验证失败：%v (%s)\n", err, strings.TrimSpace(string(out)))
		pause(reader)
		return
	}
	backup := executable + ".bak"
	if err := copyFile(executable, backup); err != nil {
		_ = os.Remove(tmp)
		fmt.Printf("备份当前程序失败：%v\n", err)
		pause(reader)
		return
	}
	if runtime.GOOS == "windows" {
		_ = os.Remove(tmp)
		fmt.Println("服务端升级替换应在 Linux VPS 上执行；Windows 正在运行的程序不能直接替换。")
		pause(reader)
		return
	}
	if err := os.Rename(tmp, executable); err != nil {
		_ = os.Remove(tmp)
		fmt.Printf("替换失败，旧程序仍保留：%v\n", err)
		pause(reader)
		return
	}
	fmt.Printf("升级文件已替换，备份文件：%s\n请退出并重新启动程序。\n", backup)
	pause(reader)
}

func showRuntimeError(reader *bufio.Reader, rt *serverRuntime) {
	select {
	case err := <-rt.Errors():
		fmt.Printf("服务运行错误：%v\n", err)
	default:
		fmt.Println("没有新的运行错误。")
	}
	pause(reader)
}

func readLine(reader *bufio.Reader, prompt string) string {
	fmt.Print(prompt)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	return strings.TrimSpace(line)
}

func pause(reader *bufio.Reader) { _ = readLine(reader, "按回车返回...") }

func formatBytes(value int64) string {
	if value < 1024 {
		return fmt.Sprintf("%d B", value)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	amount := float64(value)
	for _, unit := range units {
		amount /= 1024
		if amount < 1024 || unit == units[len(units)-1] {
			return fmt.Sprintf("%.2f %s", amount, unit)
		}
	}
	return fmt.Sprintf("%d B", value)
}

func truncate(value string, width int) string {
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	return string(runes[:width-3]) + "..."
}

func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return err
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func copyDir(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		return copyFile(path, target)
	})
}

func downloadFile(url, destination string) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download returned HTTP %s", resp.Status)
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
