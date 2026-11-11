const $ = (id) => document.getElementById(id);
let monitoring = false;
let browserPath = "";
let browserEntries = [];
let browserSort = "name";
let browserOrder = "asc";
let selectedPaths = new Set();
let historyBack = [];
let historyForward = [];
let viewMode = "details";
let refreshInFlight = false;
let libraryDraftDirty = false;
let connectionDraftDirty = false;

async function request(url, options = {}) {
  const response = await fetch(url, { headers: { "Content-Type": "application/json" }, ...options });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `请求失败（${response.status}）`);
  return body;
}
function message(text, ok = false) { $("message").textContent = text || ""; $("message").className = ok ? "message ok" : "message"; }
function formatSize(size) { if (size < 1024) return `${size} B`; if (size < 1048576) return `${(size / 1024).toFixed(1)} KB`; if (size < 1073741824) return `${(size / 1048576).toFixed(1)} MB`; return `${(size / 1073741824).toFixed(1)} GB`; }
function formatDate(value) { try { return new Date(value).toLocaleString(); } catch (_) { return value || "-"; } }
function escapeHTML(value) { const node = document.createElement("div"); node.textContent = value ?? ""; return node.innerHTML; }
function iconFor(entry) { if (entry.directory) return "📁"; if (entry.kind === "image") return "🖼️"; if (entry.kind === "text") return "📄"; return "📦"; }

function renderState(state) {
  $("network").textContent = `网络：${state.network || "未配置"}`;
  $("monitoring").textContent = state.monitoring ? "运行中" : "未运行"; monitoring = !!state.monitoring; $("monitor").textContent = monitoring ? "停止监控" : "启动监控";
  if (!state.configured) return;
  // The state poll runs while the settings form is open. Do not replace a
  // draft that the user is typing with the last server response.
  if (!libraryDraftDirty) { $("root").value = state.root || ""; $("data-dir").value = state.data_dir || ""; }
  if (!connectionDraftDirty) { $("server-url").value = state.server_url || ""; $("space-id").value = state.space_id || ""; $("device-id").value = state.device_id || ""; }
  const status = state.status || {}; $("active-files").textContent = status.ActiveFiles ?? 0; $("versions").textContent = status.Versions ?? 0; $("tombstones").textContent = status.Tombstones ?? 0; $("active-files-small").textContent = status.ActiveFiles ?? 0;
  loadBrowser(); loadConflicts();
}
async function refresh() {
  if (refreshInFlight) return;
  refreshInFlight = true;
  try { renderState(await request("/api/state")); } catch (error) { message(error.message); } finally { refreshInFlight = false; }
}
async function chooseFolder() { try { const result = await request("/api/select-folder", { method: "POST", body: "{}" }); if (result.cancelled || !result.path) { message("已取消选择同步目录"); return false; } $("root").value = result.path; libraryDraftDirty = true; return true; } catch (error) { message(error.message); return false; } }
async function openLibrary(initialize) { if (!$("root").value.trim() && !(await chooseFolder())) return; const root = $("root").value.trim(), dataDir = $("data-dir").value.trim(); try { await request("/api/library", { method: "POST", body: JSON.stringify({ root, data_dir: dataDir, initialize }) }); libraryDraftDirty = false; message(initialize ? "本地库已初始化并打开" : "本地库已打开", true); browserPath = ""; historyBack = []; historyForward = []; await refresh(); } catch (error) { message(error.message); } }

function renderBreadcrumbs() {
  const nav = $("breadcrumbs"); nav.innerHTML = ""; const root = document.createElement("button"); root.className = "crumb"; root.textContent = "同步文件夹"; root.addEventListener("click", () => openBrowser("", true)); nav.appendChild(root); let current = "";
  for (const part of (browserPath ? browserPath.split("/") : [])) { current = current ? `${current}/${part}` : part; const button = document.createElement("button"); button.className = "crumb"; button.textContent = part; const target = current; button.addEventListener("click", () => openBrowser(target, true)); nav.appendChild(button); }
}
function renderTree(entries) {
  const tree = $("directory-tree"); tree.innerHTML = ""; const root = document.createElement("button"); root.className = `tree-item ${browserPath === "" ? "active" : ""}`; root.innerHTML = "<span>☁</span> 同步文件夹"; root.addEventListener("click", () => openBrowser("", true)); tree.appendChild(root);
  for (const entry of entries.filter((item) => item.directory)) { const item = document.createElement("button"); item.className = `tree-item ${browserPath === entry.path ? "active" : ""}`; item.style.paddingLeft = "28px"; item.innerHTML = `<span>📁</span> ${escapeHTML(entry.name)}`; item.addEventListener("click", () => openBrowser(entry.path, true)); tree.appendChild(item); }
}
function renderEntry(entry, mode) {
  const selected = selectedPaths.has(entry.path); const type = entry.directory ? "文件夹" : entry.kind === "image" ? "图片" : entry.kind === "text" ? "文本" : "文件";
  const conflict = entry.conflict ? '<span class="conflict-badge">冲突副本</span>' : "";
  if (mode === "icons") return `<button class="icon-card ${selected ? "selected" : ""}" data-path="${escapeHTML(entry.path)}"><span class="large-file-icon">${iconFor(entry)}</span><span>${escapeHTML(entry.name)}</span>${conflict}</button>`;
  return `<tr class="file-row ${selected ? "selected" : ""}" data-path="${escapeHTML(entry.path)}"><td><span class="file-icon">${iconFor(entry)}</span><span>${escapeHTML(entry.name)}</span>${conflict}</td><td>${formatDate(entry.modified)}</td><td>${type}</td><td>${entry.directory ? "-" : formatSize(entry.size)}</td></tr>`;
}
function bindEntryEvents(container) { container.querySelectorAll("[data-path]").forEach((node) => { const path = node.dataset.path, entry = browserEntries.find((item) => item.path === path); node.addEventListener("click", (event) => { if (event.ctrlKey || event.metaKey) { if (selectedPaths.has(path)) selectedPaths.delete(path); else selectedPaths.add(path); } else if (event.shiftKey && selectedPaths.size) { selectedPaths.add(path); } else { selectedPaths = new Set([path]); } renderBrowser(browserEntries); }); node.addEventListener("dblclick", () => entry.directory ? openBrowser(path, true) : showPreview(entry)); }); }
function renderBrowser(entries) { browserEntries = entries || []; const details = $("browser-files"), icons = $("icons-view"); details.innerHTML = ""; icons.innerHTML = ""; if (!browserEntries.length) { details.innerHTML = '<tr><td colspan="4" class="muted">当前文件夹为空</td></tr>'; icons.innerHTML = '<div class="muted">当前文件夹为空</div>'; } else { details.innerHTML = browserEntries.map((entry) => renderEntry(entry, "details")).join(""); icons.innerHTML = browserEntries.map((entry) => renderEntry(entry, "icons")).join(""); } bindEntryEvents(details); bindEntryEvents(icons); $("selection-hint").textContent = selectedPaths.size ? `已选择 ${selectedPaths.size} 项` : "未选择项目"; }
async function loadBrowser() { if (!$("root") || !$("root").value.trim()) return; try { const query = encodeURIComponent($("file-search").value.trim()); const result = await request(`/api/browser?path=${encodeURIComponent(browserPath)}&sort=${browserSort}&order=${browserOrder}&query=${query}`); browserPath = result.path || ""; renderBreadcrumbs(); renderTree(result.entries || []); renderBrowser(result.entries); $("go-up").disabled = !result.parent; } catch (error) { message(error.message); } }
async function loadConflicts() {
  if (!$("root") || !$("root").value.trim()) return;
  try {
    const conflicts = await request("/api/conflicts"); const panel = $("conflicts-panel"); const list = $("conflicts-list");
    panel.classList.toggle("hidden", !conflicts.length);
    list.innerHTML = conflicts.map((item) => `<tr><td>${escapeHTML(item.path)}</td><td>${escapeHTML(item.original_path)}</td><td>${formatDate(item.modified)}</td><td class="conflict-actions"><button class="keep-local" data-conflict-path="${escapeHTML(item.path)}">保留本地</button><button class="secondary keep-remote" data-conflict-path="${escapeHTML(item.path)}">保留云端</button></td></tr>`).join("");
    list.querySelectorAll("[data-conflict-path]").forEach((button) => button.addEventListener("click", () => resolveConflict(button.dataset.conflictPath, button.classList.contains("keep-local") ? "keep_local" : "keep_remote")));
  } catch (error) { message(error.message); }
}
async function resolveConflict(path, action) { try { await request("/api/conflicts/resolve", { method: "POST", body: JSON.stringify({ path, action }) }); message(action === "keep_local" ? "已保留本地版本" : "已保留云端版本", true); await refresh(); } catch (error) { message(error.message); } }
async function openBrowser(path, recordHistory) { if (recordHistory && path !== browserPath) { historyBack.push(browserPath); historyForward = []; } browserPath = path || ""; selectedPaths.clear(); await loadBrowser(); }
function selectedEntries() { return browserEntries.filter((entry) => selectedPaths.has(entry.path)); }
async function fileAction(action, path, name = "", paths = []) { await request("/api/file", { method: "POST", body: JSON.stringify({ action, path, name, paths }) }); await loadBrowser(); await refresh(); }
async function showPreview(entry) { $("preview-title").textContent = entry.name; const content = $("preview-content"); content.innerHTML = ""; if (entry.kind === "image") { const image = document.createElement("img"); image.className = "preview-image"; image.src = `/api/preview?path=${encodeURIComponent(entry.path)}`; content.appendChild(image); } else if (entry.kind === "text") { const pre = document.createElement("pre"); pre.className = "preview-text"; pre.textContent = await fetch(`/api/preview?path=${encodeURIComponent(entry.path)}`).then((response) => response.text()); content.appendChild(pre); } else { content.innerHTML = `<p class="muted">该文件类型不支持内置预览。</p><a class="download-link" href="/api/preview?path=${encodeURIComponent(entry.path)}" target="_blank">打开文件</a>`; } $("preview-panel").classList.remove("hidden"); }

$("settings").addEventListener("click", () => $("settings-panel").classList.remove("hidden")); $("close-settings").addEventListener("click", () => $("settings-panel").classList.add("hidden"));
$("library-form").addEventListener("input", () => { libraryDraftDirty = true; });
$("library-form").addEventListener("submit", (event) => { event.preventDefault(); openLibrary(false); }); $("init").addEventListener("click", () => openLibrary(true)); $("browse").addEventListener("click", chooseFolder);
$("scan").addEventListener("click", async () => { try { const result = await request("/api/scan", { method: "POST", body: "{}" }); message(`扫描完成：${result.Files} 个文件，新增 ${result.NewVersions} 个版本`, true); await refresh(); } catch (error) { message(error.message); } });
$("monitor").addEventListener("click", async () => { try { await request("/api/monitor", { method: "POST", body: JSON.stringify({ action: monitoring ? "stop" : "start" }) }); await refresh(); } catch (error) { message(error.message); } });
$("connection-form").addEventListener("input", () => { connectionDraftDirty = true; });
$("connection-form").addEventListener("submit", async (event) => { event.preventDefault(); try { await request("/api/connection", { method: "POST", body: JSON.stringify({ server_url: $("server-url").value, space_id: $("space-id").value, device_id: $("device-id").value, device_token: $("device-token").value, pairing_token: $("pairing-token").value }) }); connectionDraftDirty = false; message("连接配置已保存", true); $("settings-panel").classList.add("hidden"); await refresh(); } catch (error) { message(error.message); } });
$("sync").addEventListener("click", async () => { try { await request("/api/sync", { method: "POST", body: "{}" }); message("同步完成", true); await refresh(); } catch (error) { message(error.message); } });
$("go-back").addEventListener("click", async () => { if (!historyBack.length) return; historyForward.push(browserPath); browserPath = historyBack.pop(); await loadBrowser(); }); $("go-forward").addEventListener("click", async () => { if (!historyForward.length) return; historyBack.push(browserPath); browserPath = historyForward.pop(); await loadBrowser(); }); $("go-up").addEventListener("click", async () => { const parts = browserPath ? browserPath.split("/") : []; parts.pop(); await openBrowser(parts.join("/"), true); }); $("refresh-browser").addEventListener("click", loadBrowser);
$("file-search").addEventListener("input", () => loadBrowser()); $("new-folder").addEventListener("click", async () => { const name = prompt("文件夹名称"); if (name) try { await fileAction("mkdir", browserPath, name); message("文件夹已创建", true); } catch (error) { message(error.message); } }); $("new-file").addEventListener("click", async () => { const name = prompt("文件名"); if (name) try { await fileAction("create", browserPath, name); message("文件已创建", true); } catch (error) { message(error.message); } });
$("rename-file").addEventListener("click", async () => { const entries = selectedEntries(); if (entries.length !== 1) { message("请选择一个文件或文件夹"); return; } const name = prompt("新名称", entries[0].name); if (name && name !== entries[0].name) try { await fileAction("rename", entries[0].path, name); message("已重命名", true); } catch (error) { message(error.message); } }); $("delete-file").addEventListener("click", async () => { const entries = selectedEntries(); if (!entries.length) { message("请先选择文件或文件夹"); return; } if (!confirm(`确定删除选中的 ${entries.length} 项吗？`)) return; try { await fileAction("delete", "", "", entries.map((entry) => entry.path)); selectedPaths.clear(); message("已删除", true); } catch (error) { message(error.message); } });
document.querySelectorAll(".sortable").forEach((header) => header.addEventListener("click", () => { const sort = header.dataset.sort; if (browserSort === sort) browserOrder = browserOrder === "asc" ? "desc" : "asc"; else { browserSort = sort; browserOrder = "asc"; } loadBrowser(); })); $("view-details").addEventListener("click", () => { viewMode = "details"; $("details-view").classList.remove("hidden"); $("icons-view").classList.add("hidden"); $("view-details").classList.add("active"); $("view-icons").classList.remove("active"); }); $("view-icons").addEventListener("click", () => { viewMode = "icons"; $("details-view").classList.add("hidden"); $("icons-view").classList.remove("hidden"); $("view-details").classList.remove("active"); $("view-icons").classList.add("active"); });
$("close-preview").addEventListener("click", () => $("preview-panel").classList.add("hidden")); $("close-versions").addEventListener("click", () => $("versions-panel").classList.add("hidden"));
document.addEventListener("keydown", (event) => { if (event.key === "F2") { event.preventDefault(); $("rename-file").click(); } if (event.key === "Delete") { event.preventDefault(); $("delete-file").click(); } });
refresh();
setInterval(() => { if (!document.hidden) refresh(); }, 2000);
