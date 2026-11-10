const $ = (id) => document.getElementById(id);
let selectedPath = "";
let monitoring = false;

async function request(url, options = {}) {
  const response = await fetch(url, { headers: { "Content-Type": "application/json" }, ...options });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `请求失败（${response.status}）`);
  return body;
}
function message(text, ok = false) { $("message").textContent = text || ""; $("message").className = ok ? "message ok" : "message"; }
function formatSize(size) { if (size < 1024) return `${size} B`; if (size < 1048576) return `${(size / 1024).toFixed(1)} KB`; return `${(size / 1048576).toFixed(1)} MB`; }
function renderFiles(files) {
  const body = $("files"); body.innerHTML = "";
  if (!files || !files.length) { body.innerHTML = '<tr><td colspan="5" class="muted">暂无文件。先添加文件后执行扫描。</td></tr>'; return; }
  for (const file of files) {
    const row = document.createElement("tr");
    row.innerHTML = `<td>${escapeHTML(file.path)}</td><td>${formatSize(file.size)}</td><td>${file.tombstone ? "已删除" : "活跃"}</td><td><code>${escapeHTML(file.version || "-")}</code></td><td><button class="secondary versions" data-path="${escapeHTML(file.path)}">查看版本</button></td>`;
    body.appendChild(row);
  }
  document.querySelectorAll(".versions").forEach((button) => button.addEventListener("click", () => showVersions(button.dataset.path)));
}
function escapeHTML(value) { const node = document.createElement("div"); node.textContent = value ?? ""; return node.innerHTML; }
function renderState(state) {
  $("network").textContent = `网络：${state.network || "未连接（本地原型）"}`;
  $("monitoring").textContent = state.monitoring ? "运行中" : "未运行"; monitoring = !!state.monitoring;
  if (!state.configured) return;
  $("root").value = state.root || ""; $("data-dir").value = state.data_dir || "";
  const status = state.status || {}; $("active-files").textContent = status.ActiveFiles ?? 0; $("versions").textContent = status.Versions ?? 0; $("tombstones").textContent = status.Tombstones ?? 0; renderFiles(state.files || []);
}
async function refresh() { try { renderState(await request("/api/state")); } catch (error) { message(error.message); } }
async function openLibrary(initialize) {
  const root = $("root").value.trim(), dataDir = $("data-dir").value.trim();
  try { await request("/api/library", { method: "POST", body: JSON.stringify({ root, data_dir: dataDir, initialize }) }); message(initialize ? "本地库已初始化并打开" : "本地库已打开", true); await refresh(); } catch (error) { message(error.message); }
}
$("library-form").addEventListener("submit", (event) => { event.preventDefault(); openLibrary(false); });
$("init").addEventListener("click", () => openLibrary(true));
$("scan").addEventListener("click", async () => { try { const result = await request("/api/scan", { method: "POST", body: "{}" }); message(`扫描完成：${result.Files} 个文件，新增 ${result.NewVersions} 个版本`, true); await refresh(); } catch (error) { message(error.message); } });
$("monitor").addEventListener("click", async () => { try { await request("/api/monitor", { method: "POST", body: JSON.stringify({ action: monitoring ? "stop" : "start" }) }); await refresh(); } catch (error) { message(error.message); } });
async function showVersions(path) { selectedPath = path; try { const versions = await request(`/api/versions?path=${encodeURIComponent(path)}`); $("selected-path").textContent = path; $("versions-panel").classList.remove("hidden"); const body = $("versions-list"); body.innerHTML = ""; for (const version of versions) { const row = document.createElement("tr"); row.innerHTML = `<td><code>${escapeHTML(version.ID)}</code></td><td>${escapeHTML(version.CreatedAt)}</td><td><code>${escapeHTML(version.Manifest)}</code></td><td><button class="restore secondary" data-version="${escapeHTML(version.ID)}">恢复</button></td>`; body.appendChild(row); } document.querySelectorAll(".restore").forEach((button) => button.addEventListener("click", () => restore(button.dataset.version))); } catch (error) { message(error.message); } }
async function restore(version) { if (!selectedPath) return; try { await request("/api/restore", { method: "POST", body: JSON.stringify({ path: selectedPath, version }) }); message("历史版本已恢复，并记录为新的当前版本", true); await refresh(); await showVersions(selectedPath); } catch (error) { message(error.message); } }
$("close-versions").addEventListener("click", () => $("versions-panel").classList.add("hidden"));
refresh();
