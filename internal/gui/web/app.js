const $ = (id) => document.getElementById(id);
let monitoring = false;
let browserPath = "";
let currentRootPath = "";
let browserEntries = [];
let allBrowserEntries = [];
let browserSort = "name";
let browserOrder = "asc";
let browserFilter = "all";
let selectedPaths = new Set();
let historyBack = [];
let historyForward = [];
let viewMode = "details";
let refreshInFlight = false;
let browserLoadController = null;
let browserLoadGeneration = 0;
let browserLoaded = false;
let previewController = null;
let libraryDraftDirty = false;
let connectionDraftDirty = false;
let quotaDraftDirty = false;
let lastSyncAt = "刚刚";
let lastIndexUpdatedAt = "";
let lastConflictAt = 0;
let detailsDrawerGeneration = 0;
let detailsDrawerPath = "";
let clipboardPaths = [];
let clipboardMode = "copy";
let contextEntry = null;
let activeMarquee = null;
let pointerPress = null;
let fileDragActive = false;
let suppressingClick = false;
let suppressClickTimer = 0;
let interactionInstalled = false;
let closing = false;
let refreshTimer = 0;
const appPages = ["overview", "tasks", "versions", "devices", "settings"];
let currentPage = "overview";

async function request(url, options = {}) {
  if (closing) throw new Error("应用正在退出");
  const response = await fetch(url, { headers: { "Content-Type": "application/json" }, ...options });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `请求失败（${response.status}）`);
  return body;
}
function message(text, ok = false) {
  const value = String(text || "").includes("quota_exceeded") ? "空间容量不足，无法提交此文件" : (text || "");
  if (closing) return;
  const panelMessage = $("message");
  if (panelMessage) { panelMessage.textContent = value; panelMessage.className = ok ? "message ok" : "message"; }
  const toast = $("toast");
  if (!toast || !value) return;
  toast.textContent = value;
  toast.className = `toast ${ok ? "ok" : "error"} visible`;
  window.clearTimeout(message.toastTimer);
  message.toastTimer = window.setTimeout(() => { toast.className = "toast"; }, 3600);
}
function formatSize(size) { if (size < 1024) return `${size} B`; if (size < 1048576) return `${(size / 1024).toFixed(1)} KB`; if (size < 1073741824) return `${(size / 1048576).toFixed(1)} MB`; return `${(size / 1073741824).toFixed(1)} GB`; }
function formatDate(value) { try { return new Date(value).toLocaleString(); } catch (_) { return value || "-"; } }
function escapeHTML(value) { const node = document.createElement("div"); node.textContent = value ?? ""; return node.innerHTML; }
function escapeAttribute(value) { return escapeHTML(value).replace(/"/g, "&quot;").replace(/'/g, "&#39;"); }
function iconFor(entry) { if (entry.directory) return "📁"; if (entry.kind === "image") return "🖼️"; if (entry.kind === "text") return "📄"; return "📦"; }

function pageFromLocation() {
  const page = (window.location.hash || "#overview").slice(1).toLowerCase();
  return page === "files" || !appPages.includes(page) ? "overview" : page;
}
function setPageVisibility(id, visible) { const node = $(id); if (node) node.classList.toggle("hidden", !visible); }
function navigateTo(page, options = {}) {
  cancelActiveMarquee();
  const target = appPages.includes(page) ? page : "overview";
  if (!options.fromHistory) {
    const url = `#${target}`;
    if (options.replace) window.history.replaceState({}, "", url); else if (window.location.hash !== url) window.history.pushState({}, "", url);
  }
  currentPage = target;
  document.body.dataset.page = target;
  document.querySelectorAll(".nav-item").forEach((item) => item.classList.toggle("active", item.dataset.navTarget === target));
  setPageVisibility("tasks-page", target === "tasks");
  setPageVisibility("conflicts-panel", target === "tasks");
  setPageVisibility("versions-panel", target === "versions");
  setPageVisibility("devices-page", target === "devices");
  setPageVisibility("settings-panel", target === "settings");
  if (target !== "overview") { hideContextMenu(); clearPreview(); closeDetailsDrawer(); $("properties-panel")?.classList.add("hidden"); }
  window.scrollTo(0, 0);
  const workspace = document.querySelector(".workspace"); if (workspace) workspace.scrollTop = 0;
  if (target === "tasks") loadConflicts();
  if (target === "devices") loadDevices();
  if (target === "overview") loadBrowser();
  if (options.focus) window.setTimeout(() => $(options.focus)?.focus(), 0);
}
window.addEventListener("hashchange", () => navigateTo(pageFromLocation(), { fromHistory: true }));
window.addEventListener("popstate", () => navigateTo(pageFromLocation(), { fromHistory: true }));

function setText(id, value) { const node = $(id); if (node) node.textContent = value ?? ""; }
function deviceStatusLabel(value) { return ({ online: "在线", idle: "空闲", offline: "离线", revoked: "已撤销" })[value] || "未知"; }
function deviceStatusClass(value) { return value === "online" ? "online" : value === "idle" ? "idle" : value === "revoked" ? "revoked" : "offline"; }
function renderDevices(payload) {
  const devices = Array.isArray(payload?.devices) ? payload.devices : [];
  const counts = { online: 0, idle: 0, offline: 0 };
  devices.forEach((device) => { if (Object.prototype.hasOwnProperty.call(counts, device.connection_status)) counts[device.connection_status] += 1; });
  setText("devices-total", devices.length); setText("devices-online", counts.online); setText("devices-idle", counts.idle); setText("devices-offline", counts.offline);
  const connection = payload?.connection || {};
  setText("devices-server-status", connectionLabel({ connection }));
  setText("devices-server-url", payload?.server_url || "尚未配置服务端");
  setText("devices-server-meta", payload?.stale ? "状态来自最近一次成功读取，等待服务器恢复" : `空间：${payload?.space_id || "-"} · 修订号：${payload?.current_revision ?? 0}`);
  const serverBadge = $("devices-server-badge"); if (serverBadge) { serverBadge.textContent = connection.status === "ready" ? "正常" : connection.status === "offline" ? "离线" : connection.status === "unauthorized" ? "未授权" : "未知"; serverBadge.className = `online-badge ${deviceStatusClass(connection.status === "ready" ? "online" : connection.status === "offline" ? "offline" : "idle")}`; }
  const list = $("devices-list");
  if (list) list.innerHTML = devices.length ? devices.map((device) => `<tr><td><strong>${escapeHTML(device.name || device.device_id || "未命名设备")}</strong><small class="device-id-text">${escapeHTML(device.device_id || "-")}</small></td><td><span class="device-status-pill ${deviceStatusClass(device.connection_status)}">${deviceStatusLabel(device.connection_status)}</span></td><td>${escapeHTML(device.client_status || "未知")}</td><td>${device.monitoring ? "已启用" : "未启用"}</td><td>${escapeHTML(formatDate(device.last_seen_at || device.last_status_at || ""))}</td></tr>`).join("") : '<tr><td colspan="5" class="muted">暂无已注册设备</td></tr>';
  setText("devices-updated", payload?.server_time ? `服务端时间：${formatDate(payload.server_time)}` : "等待状态");
}
async function loadDevices() { try { renderDevices(await request("/api/devices")); } catch (error) { message(error.message); } }
function renderPageState(state) {
  const configured = !!state.configured;
  const connection = connectionInfo(state);
  const pending = Number(state.pending ?? 0);
  const monitoringLabel = configured && state.monitoring ? "运行中" : "未启用";
  const monitoringDetail = configured ? (state.monitoring ? "监听文件变更并自动同步" : "监控已暂停") : "等待本地库打开";
  $("tasks-pending").textContent = pending;
  $("tasks-pending-detail").textContent = pending ? `${pending} 项待处理` : "无待处理任务";
  $("tasks-monitoring").textContent = monitoringLabel;
  $("tasks-monitoring-detail").textContent = monitoringDetail;
  $("tasks-last-sync").textContent = lastSyncAt;
  $("tasks-service-detail").textContent = configured ? (state.monitoring ? "文件变更会自动进入同步队列并同步到其他设备。" : "实时监控已暂停，可以手动执行同步。") : "请先在设置页打开本地库并配置服务端连接。";
  $("tasks-monitor").textContent = state.monitoring ? "停止监控" : "启动监控";
  renderDevices({ connection, server_url: state.server_url, space_id: state.space_id, devices: state.devices, current_revision: state.current_revision, server_time: state.server_time, stale: state.devices_stale });
}

function connectionInfo(state) { return state.connection || { status: state.configured ? "checking" : "unconfigured" }; }
function connectionLabel(state) {
  const info = connectionInfo(state);
  const labels = { unconfigured: "连接状态：未配置", checking: "连接状态：检测中", offline: "连接状态：服务器离线", unauthorized: "连接状态：设备未授权", ready: "连接状态：正常" };
  return labels[info.status] || "连接状态：异常";
}
function renderAutoStart(state) {
  const checkbox = $("autostart");
  const hint = $("autostart-hint");
  if (!checkbox || !hint) return;
  const supported = state.autostart_supported !== false;
  checkbox.disabled = !supported;
  checkbox.checked = supported && state.autostart === true;
  hint.textContent = supported ? (checkbox.checked ? "已启用：登录 Windows 后自动启动" : "已关闭：需要时可手动启动") : "当前平台不支持开机自动启动";
}

function renderState(state) {
  const connection = connectionInfo(state);
  const connected = connection.status === "ready";
  const indexChanged = state.index_updated_at !== lastIndexUpdatedAt;
  currentRootPath = state.root || currentRootPath;
  lastSyncAt = state.last_sync_at ? formatDate(state.last_sync_at) : "尚未同步";
  lastIndexUpdatedAt = state.index_updated_at || lastIndexUpdatedAt;
  renderAutoStart(state);
  const connectionConfigured = connection.status !== "unconfigured";
  const connectionDetail = connection.error || (connection.status === "checking" ? "正在检测服务器" : connection.status === "unconfigured" ? "打开设置填写服务端连接" : connection.status === "ready" ? "服务器与设备认证正常" : "等待服务器恢复");
  const badge = $("conn-badge");
  if (badge) {
    badge.classList.toggle("bad", !connected && connectionConfigured);
    badge.title = `${connectionLabel(state)}${state.server_url ? ` · ${state.server_url}` : ""} · ${connectionDetail}`;
  }
  setText("conn-badge-text", connected ? "正常" : connection.status === "offline" ? "离线" : connection.status === "unauthorized" ? "未授权" : "未配置");
  monitoring = !!state.monitoring;
  const status = state.status || {};
  const devicesKnown = Array.isArray(state.devices);
  const syncText = !state.configured ? "等待连接" : connected ? (monitoring ? `同步正常 · ${lastSyncAt}` : "同步已暂停") : connection.status === "offline" ? "服务器离线" : "等待连接";
  setText("status-sync", syncText);
  setText("status-pending", `${state.configured ? state.pending ?? 0 : 0} 待同步`);
  $("status-pending")?.classList.toggle("attention", Number(state.pending) > 0);
  setText("status-versions", `${state.configured ? status.Versions ?? 0 : "-"} 版本`);
  setText("status-devices", devicesKnown ? `${state.devices.length} 设备` : "- 设备");
  setText("current-root-display", state.root || "尚未打开本地库");
  const address = document.querySelector(".address-bar");
  if (address) address.title = state.root || "尚未打开本地库";
  if (!state.configured) {
    renderStorage(state);
    closeDetailsDrawer();
    setPageVisibility("details-view", false);
    setPageVisibility("icons-view", false);
    setPageVisibility("empty-state", true);
    renderPageState(state);
    return;
  }
  // The state poll runs while the settings form is open. Do not replace a
  // draft that the user is typing with the last server response.
  if (!libraryDraftDirty) { $("root").value = state.root || ""; $("data-dir").value = state.data_dir || ""; }
  if (!connectionDraftDirty) { $("server-url").value = state.server_url || ""; $("space-id").value = state.space_id || ""; $("device-id").value = state.device_id || ""; }
  renderStorage(state);
  setPageVisibility("details-view", viewMode === "details");
  setPageVisibility("icons-view", viewMode === "icons");
  setPageVisibility("empty-state", false);
  renderPageState(state);
  if (!isInteracting() && currentPage === "overview" && (indexChanged || !browserLoaded)) loadBrowser();
  if (Date.now() - lastConflictAt > 30000) { lastConflictAt = Date.now(); loadConflicts(); }
}
function renderStorage(state) {
  const status = state?.status || {};
  const remoteSynced = state?.quota_synced === true && Number(state?.quota_bytes || 0) > 0;
  const used = Math.max(0, Number(remoteSynced ? state.quota_used_bytes : status.TotalBytes || 0));
  const quota = Math.max(0, Number(remoteSynced ? state.quota_bytes : status.QuotaBytes || 0));
  const usedNode = $("storage-used");
  const bar = $("storage-bar");
  if (usedNode) usedNode.textContent = quota ? `${formatSize(used)} / ${formatSize(quota)}` : formatSize(used);
  if (bar) bar.style.width = quota ? `${Math.min(100, (used / quota) * 100)}%` : "0%";
  const quotaInput = $("quota-gb");
  const quotaHint = $("quota-hint");
  const connectionConfigured = Boolean(state?.server_url && state?.space_id && state?.device_id);
  if (quotaInput && !quotaDraftDirty) quotaInput.value = remoteSynced ? (Number(state.quota_bytes) / 1073741824).toFixed(1) : "";
  if (quotaInput) quotaInput.disabled = !connectionConfigured;
  if (quotaHint) quotaHint.textContent = !connectionConfigured ? "请先保存服务端连接信息" : remoteSynced ? `已用 ${formatSize(used)}，当前容量 ${formatSize(quota)}` : "已保存连接，等待服务端配额同步";
}
async function refresh() {
  if (closing) return;
  if (refreshInFlight) return;
  refreshInFlight = true;
  try { renderState(await request("/api/state")); } catch (error) { message(error.message); } finally { refreshInFlight = false; }
}
async function chooseFolder() { try { const result = await request("/api/select-folder", { method: "POST", body: "{}" }); if (result.cancelled || !result.path) { message("已取消选择同步目录"); return false; } $("root").value = result.path; libraryDraftDirty = true; return true; } catch (error) { message(error.message); return false; } }
async function openLibrary(initialize) { if (!$("root").value.trim() && !(await chooseFolder())) return; const root = $("root").value.trim(), dataDir = $("data-dir").value.trim(); try { await request("/api/library", { method: "POST", body: JSON.stringify({ root, data_dir: dataDir, initialize }) }); libraryDraftDirty = false; message(initialize ? "本地库已初始化并打开" : "本地库已打开", true); browserPath = ""; historyBack = []; historyForward = []; browserLoaded = false; lastIndexUpdatedAt = ""; await refresh(); } catch (error) { message(error.message); } }

function renderBreadcrumbs() {
  const nav = $("breadcrumbs"); nav.innerHTML = ""; const root = document.createElement("button"); root.className = "crumb"; root.textContent = "同步文件夹"; root.addEventListener("click", () => openBrowser("", true)); nav.appendChild(root); let current = "";
  for (const part of (browserPath ? browserPath.split("/") : [])) { current = current ? `${current}/${part}` : part; const button = document.createElement("button"); button.className = "crumb"; button.textContent = part; const target = current; button.addEventListener("click", () => openBrowser(target, true)); nav.appendChild(button); }
}
function renderEntry(entry, mode) {
  const selected = selectedPaths.has(entry.path);
  const status = entry.status || (entry.conflict ? "conflict" : "synced");
  const statusLabel = status === "conflict" ? "冲突副本" : status === "pending" ? "待同步" : "已同步";
  const conflict = entry.conflict ? '<span class="conflict-badge">冲突副本</span>' : "";
  const path = escapeAttribute(entry.path);
  const name = escapeHTML(entry.name);
  const ariaName = escapeAttribute(entry.name);
  if (mode === "icons") return `<button class="icon-card ${selected ? "selected" : ""}" draggable="true" data-path="${path}"><span class="large-file-icon">${iconFor(entry)}</span><span>${name}</span>${conflict}</button>`;
  return `<tr class="file-row ${selected ? "selected" : ""}" draggable="true" data-path="${path}"><td class="check-col"><input class="row-check" type="checkbox" draggable="false" ${selected ? "checked" : ""} aria-label="选择 ${ariaName}"></td><td><span class="file-icon">${iconFor(entry)}</span><span class="file-name">${name}</span>${conflict}</td><td class="status-cell status-${status}">${statusLabel}</td><td>${formatDate(entry.modified)}</td><td>${entry.directory ? "-" : formatSize(entry.size)}</td><td><button class="more-action" draggable="false" title="更多操作" aria-label="更多操作">···</button></td></tr>`;
}
function localFileURL(relativePath) {
  const root = String(currentRootPath || "").trim();
  const relative = String(relativePath || "").replace(/^[/\\]+/, "");
  if (!root || !relative) return "";
  const isUNC = root.startsWith("\\\\");
  if (!isUNC && !/^[A-Za-z]:[\\/]/.test(root)) return "";
  const encodeSegments = (value) => value.split(/[\\/]+/).filter(Boolean).map(encodeURIComponent).join("/");
  const tail = isUNC ? root.replace(/^\\\\/, "") : root.slice(2);
  const path = [isUNC ? "" : root.slice(0, 2), encodeSegments(tail), encodeSegments(relative)].filter(Boolean).join("/");
  return isUNC ? `file://${path}` : `file:///${path}`;
}

function suppressFollowingClick() {
  suppressingClick = true;
  window.clearTimeout(suppressClickTimer);
  suppressClickTimer = window.setTimeout(() => { suppressingClick = false; }, 250);
}
function onGlobalSuppressedClick(event) {
  if (!suppressingClick) return;
  suppressingClick = false;
  window.clearTimeout(suppressClickTimer);
  event.stopPropagation();
  event.preventDefault();
}
function applySelection(nextSelection, options = {}) {
  if (nextSelection) selectedPaths = nextSelection instanceof Set ? nextSelection : new Set(nextSelection);
  document.querySelectorAll("#browser-files [data-path], #icons-view [data-path]").forEach((node) => {
    const isSelected = selectedPaths.has(node.dataset.path);
    node.classList.toggle("selected", isSelected);
    const checkbox = node.querySelector(".row-check");
    if (checkbox) checkbox.checked = isSelected;
  });
  const selectAll = $("select-all");
  if (selectAll) {
    const total = browserEntries.length;
    selectAll.checked = total > 0 && selectedPaths.size >= total;
    selectAll.indeterminate = selectedPaths.size > 0 && selectedPaths.size < total;
  }
  const count = $("manager-count");
  if (count) count.textContent = `已选 ${selectedPaths.size} 项 · 共 ${browserEntries.length} 项`;
  if (!options.skipDrawer) syncDetailsDrawer();
}

function closeDetailsDrawer() {
  detailsDrawerGeneration += 1;
  detailsDrawerPath = "";
  const drawer = $("details-drawer");
  if (!drawer) return;
  drawer.classList.remove("open");
  drawer.setAttribute("aria-hidden", "true");
}
function syncDetailsDrawer() {
  const entries = selectedEntries();
  if (entries.length !== 1 || entries[0].directory) {
    closeDetailsDrawer();
    return;
  }
  if (detailsDrawerPath !== entries[0].path) showProperties(entries[0]);
}

const MARQUEE_THRESHOLD_PX = 4;
const MARQUEE_BLOCKED_SELECTOR = ["[data-path], button, input, select, a", "thead", "th", "textarea", ".row-check", ".more-action", ".address-row", ".command-row", ".status-row", "[contenteditable='true']"].join(",");
function isInteracting() { return activeMarquee !== null || fileDragActive; }
function activeListView() { return viewMode === "icons" ? $("icons-view") : $("details-view"); }
function listViewportRect(view) { const rect = view.getBoundingClientRect(); const left = rect.left + view.clientLeft; const top = rect.top + view.clientTop; return { left, top, right: left + view.clientWidth, bottom: top + view.clientHeight }; }
function isPointOnChrome(view, event) { const outer = view.getBoundingClientRect(); if (event.clientX < outer.left || event.clientX > outer.right || event.clientY < outer.top || event.clientY > outer.bottom) return false; const inner = listViewportRect(view); return event.clientX < inner.left || event.clientX > inner.right || event.clientY < inner.top || event.clientY > inner.bottom; }
function findMarqueeView(event) {
  const target = event.target instanceof Element ? event.target : null;
  if (!target) return null;
  // The legacy contract target.closest("[data-path], button, input, select, a")
  // is represented by the centralized blocked-selector list below.
  const view = target.closest("#details-view, #icons-view");
  if (!view || view !== activeListView() || target.closest(MARQUEE_BLOCKED_SELECTOR) || !view.clientWidth || !view.clientHeight || isPointOnChrome(view, event)) return null;
  const inner = listViewportRect(view);
  if (event.clientX < inner.left || event.clientX > inner.right || event.clientY < inner.top || event.clientY > inner.bottom) return null;
  return view;
}
function clampNumber(value, min, max) { return Math.min(Math.max(value, min), max); }
function marqueeBounds(candidate) { const viewport = listViewportRect(candidate.view); const left = clampNumber(Math.min(candidate.startX, candidate.lastX), viewport.left, viewport.right); const right = clampNumber(Math.max(candidate.startX, candidate.lastX), viewport.left, viewport.right); const top = clampNumber(Math.min(candidate.startY, candidate.lastY), viewport.top, viewport.bottom); const bottom = clampNumber(Math.max(candidate.startY, candidate.lastY), viewport.top, viewport.bottom); return { left, top, right, bottom, width: right - left, height: bottom - top }; }
function marqueeHitPaths(candidate, bounds) { const paths = []; candidate.view.querySelectorAll("[data-path]").forEach((node) => { const rect = node.getBoundingClientRect(); if (rect.right <= bounds.left || rect.left >= bounds.right || rect.bottom <= bounds.top || rect.top >= bounds.bottom) return; paths.push(node.dataset.path); }); return paths; }
function createMarqueeBox(candidate) { document.querySelectorAll(".marquee-selection").forEach((node) => node.remove()); const box = document.createElement("div"); box.className = "marquee-selection"; document.body.appendChild(box); candidate.box = box; }
function paintSelectionClasses(selection) { document.querySelectorAll("#browser-files [data-path], #icons-view [data-path]").forEach((node) => node.classList.toggle("selected", selection.has(node.dataset.path))); }
function updateMarqueePreview(candidate) { const bounds = marqueeBounds(candidate); Object.assign(candidate.box.style, { left: `${bounds.left}px`, top: `${bounds.top}px`, width: `${bounds.width}px`, height: `${bounds.height}px` }); const hit = marqueeHitPaths(candidate, bounds); candidate.preview = candidate.additive ? new Set([...candidate.baseSelection, ...hit]) : new Set(hit); paintSelectionClasses(candidate.preview); }
function beginMarquee(event, view) {
  cancelActiveMarquee();
  const candidate = { view, pointerId: event.pointerId, startX: event.clientX, startY: event.clientY, lastX: event.clientX, lastY: event.clientY, additive: event.ctrlKey || event.metaKey, baseSelection: new Set(selectedPaths), moved: false, box: null, preview: null };
  candidate.onMove = (moveEvent) => onMarqueeMove(candidate, moveEvent); candidate.onUp = (upEvent) => onMarqueeUp(candidate, upEvent); candidate.onCancel = () => cancelActiveMarquee(); activeMarquee = candidate;
  document.body.classList.add("marquee-active");
  window.addEventListener("pointermove", candidate.onMove, true); window.addEventListener("pointerup", candidate.onUp, true); window.addEventListener("pointercancel", candidate.onCancel, true); window.addEventListener("blur", candidate.onCancel); document.addEventListener("visibilitychange", onInteractionVisibilityChange);
  try { view.setPointerCapture(event.pointerId); } catch (_) { /* pointer may already be gone */ }
  if (event.pointerType === "mouse") event.preventDefault();
}
function onInteractionVisibilityChange() { if (document.hidden) cancelActiveMarquee(); }
function onMarqueeMove(candidate, event) { if (activeMarquee !== candidate || event.pointerId !== candidate.pointerId) return; if (event.pointerType === "mouse" && event.buttons === 0) { cancelActiveMarquee(); return; } candidate.lastX = event.clientX; candidate.lastY = event.clientY; if (!candidate.moved) { const movedX = Math.abs(event.clientX - candidate.startX); const movedY = Math.abs(event.clientY - candidate.startY); if (movedX <= MARQUEE_THRESHOLD_PX && movedY <= MARQUEE_THRESHOLD_PX) return; candidate.moved = true; createMarqueeBox(candidate); } updateMarqueePreview(candidate); }
function onMarqueeUp(candidate, event) { if (activeMarquee !== candidate || event.pointerId !== candidate.pointerId) return; finishMarquee(candidate, candidate.moved); }
function finishMarquee(candidate, commit) { if (!candidate || activeMarquee !== candidate) return; activeMarquee = null; window.removeEventListener("pointermove", candidate.onMove, true); window.removeEventListener("pointerup", candidate.onUp, true); window.removeEventListener("pointercancel", candidate.onCancel, true); window.removeEventListener("blur", candidate.onCancel); document.removeEventListener("visibilitychange", onInteractionVisibilityChange); try { candidate.view.releasePointerCapture(candidate.pointerId); } catch (_) { /* already released */ } candidate.box?.remove(); candidate.box = null; document.body.classList.remove("marquee-active"); if (!candidate.moved) return; if (commit) { applySelection(candidate.preview || new Set()); suppressFollowingClick(); } else applySelection(candidate.baseSelection); }
function cancelActiveMarquee() { finishMarquee(activeMarquee, false); }

function installPanelInteraction() {
  if (interactionInstalled) return;
  interactionInstalled = true;
  const panel = $("files");
  if (panel) panel.addEventListener("pointerdown", onPanelPointerDown);
  document.addEventListener("pointerdown", onAnyPointerDownCapture, true); document.addEventListener("click", onGlobalSuppressedClick, true); window.addEventListener("pointerup", clearPointerPress, true); window.addEventListener("pointercancel", clearPointerPress, true); document.addEventListener("dragend", onEntryDragEnd, true); document.addEventListener("drop", onEntryDragEnd, true);
}
function onAnyPointerDownCapture() { if (!suppressingClick) return; suppressingClick = false; window.clearTimeout(suppressClickTimer); }
function clearPointerPress() { pointerPress = null; }
function onPanelPointerDown(event) { if (!event.isPrimary || (event.pointerType === "mouse" && event.button !== 0) || event.pointerType === "touch") return; const target = event.target instanceof Element ? event.target : null; const item = target ? target.closest("[data-path]") : null; const control = target ? target.closest("input, select, textarea, a, .row-check, .more-action") : null; pointerPress = { pointerId: event.pointerId, path: item ? item.dataset.path : null, control: Boolean(control), target }; if (item) { cancelActiveMarquee(); return; } if (control) return; const view = findMarqueeView(event); if (view) beginMarquee(event, view); }
function entryHit(event) { const node = event.target instanceof Element ? event.target.closest("[data-path]") : null; if (!node) return null; const path = node.dataset.path; const entry = browserEntries.find((item) => item.path === path); return entry ? { node, path, entry } : null; }
function installEntryDelegation(container) { if (!container || container.dataset.entriesBound === "true") return; container.dataset.entriesBound = "true"; container.addEventListener("click", onEntryClick); container.addEventListener("dblclick", onEntryDblClick); container.addEventListener("contextmenu", onEntryContextMenu); container.addEventListener("change", onEntryChange); container.addEventListener("dragstart", onEntryDragStart); container.addEventListener("dragend", onEntryDragEnd); }
function onEntryClick(event) { const target = event.target instanceof Element ? event.target : null; const moreAction = target ? target.closest(".more-action") : null; if (moreAction) { event.stopPropagation(); const hit = entryHit(event); if (hit) { applySelection(new Set([hit.path])); renameEntry(hit.entry); } return; } if (target?.closest?.(".row-check")) return; const hit = entryHit(event); if (!hit) return; if (event.ctrlKey || event.metaKey) { if (selectedPaths.has(hit.path)) selectedPaths.delete(hit.path); else selectedPaths.add(hit.path); applySelection(); } else if (event.shiftKey && selectedPaths.size) { selectedPaths.add(hit.path); applySelection(); } else applySelection(new Set([hit.path])); }
function onEntryDblClick(event) { const target = event.target instanceof Element ? event.target : null; if (target?.closest?.(".row-check, .more-action")) return; const hit = entryHit(event); if (!hit) return; if (hit.entry.directory) openBrowser(hit.path, true); else showPreview(hit.entry); }
function onEntryContextMenu(event) { const hit = entryHit(event); if (!hit) return; event.preventDefault(); contextEntry = hit.entry; if (!selectedPaths.has(hit.path)) applySelection(new Set([hit.path])); showContextMenu(event.clientX, event.clientY, true); }
function onEntryChange(event) { const checkbox = event.target; if (!(checkbox instanceof HTMLInputElement) || !checkbox.classList.contains("row-check")) return; const node = checkbox.closest("[data-path]"); if (!node) return; if (checkbox.checked) selectedPaths.add(node.dataset.path); else selectedPaths.delete(node.dataset.path); applySelection(); }
function onEntryDragStart(event) { const hit = entryHit(event); if (!hit || !event.dataTransfer) return; const press = pointerPress; if (!press || press.path !== hit.path || press.control) { event.preventDefault(); return; } const target = event.target instanceof Element ? event.target : null; if (target?.closest?.("input, select, textarea, a, .row-check, .more-action")) { event.preventDefault(); return; } const uri = localFileURL(hit.path); if (!uri) { event.preventDefault(); return; } const urls = [uri]; event.dataTransfer.effectAllowed = "copy"; event.dataTransfer.setData("text/uri-list", urls.join("\r\n")); event.dataTransfer.setData("text/plain", urls.join("\r\n")); if (!hit.entry.directory) event.dataTransfer.setData("DownloadURL", `application/octet-stream:${hit.entry.name}:${urls[0]}`); fileDragActive = true; pointerPress = null; hit.node.classList.add("dragging"); applySelection(new Set([hit.path]), { skipDrawer: true }); event.stopPropagation(); }
function onEntryDragEnd() { document.querySelectorAll(".dragging").forEach((node) => node.classList.remove("dragging")); document.querySelectorAll(".drop-active").forEach((node) => node.classList.remove("drop-active")); fileDragActive = false; pointerPress = null; }
function renderBrowser(entries) {
  cancelActiveMarquee();
  pointerPress = null;
  browserEntries = entries || [];
  browserLoaded = true;
  const details = $("browser-files"), detailsView = $("details-view"), icons = $("icons-view");
  const empty = !browserEntries.length;
  if (viewMode === "details") {
    const detailsHTML = empty ? '<tr><td colspan="6" class="muted">当前文件夹为空</td></tr>' : browserEntries.map((entry) => renderEntry(entry, "details")).join("");
    if (details.innerHTML !== detailsHTML) details.innerHTML = detailsHTML;
    icons.replaceChildren();
  } else {
    details.replaceChildren();
    const iconsHTML = empty ? '<div class="muted">当前文件夹为空</div>' : browserEntries.map((entry) => renderEntry(entry, "icons")).join("");
    if (icons.innerHTML !== iconsHTML) icons.innerHTML = iconsHTML;
  }
  installEntryDelegation(detailsView); installEntryDelegation(icons);
  bindDropTarget(detailsView); bindDropTarget(icons);
  applySelection();
}
async function uploadDroppedFiles(files) {
  if (!files?.length) return;
  const form = new FormData();
  Array.from(files).forEach((file) => form.append("files", file, file.name));
  try {
    const result = await request(`/api/upload?path=${encodeURIComponent(browserPath)}`, { method: "POST", headers: {}, body: form });
    message(`已复制 ${result.uploaded?.length || files.length} 个文件到同步目录`, true);
    await loadBrowser(); await refresh();
  } catch (error) { message(error.message); }
}
function bindDropTarget(container) {
  if (!container || container.dataset.dropBound === "true") return;
  container.dataset.dropBound = "true";
  const carriesFiles = (event) => Array.from(event.dataTransfer?.types || []).includes("Files");
  container.addEventListener("dragenter", (event) => { if (!carriesFiles(event)) return; event.preventDefault(); container.classList.add("drop-active"); });
  container.addEventListener("dragover", (event) => { if (!carriesFiles(event)) return; event.preventDefault(); if (event.dataTransfer) event.dataTransfer.dropEffect = "copy"; container.classList.add("drop-active"); });
  container.addEventListener("dragleave", (event) => { if (container.contains(event.relatedTarget)) return; container.classList.remove("drop-active"); });
  container.addEventListener("drop", (event) => { container.classList.remove("drop-active"); if (!carriesFiles(event)) return; event.preventDefault(); event.stopPropagation(); uploadDroppedFiles(event.dataTransfer?.files); });
}
async function loadBrowser() {
  if (!$("root") || !$("root").value.trim()) return;
  browserLoadController?.abort();
  const controller = new AbortController();
  const generation = ++browserLoadGeneration;
  browserLoadController = controller;
  try {
    const query = encodeURIComponent($("file-search").value.trim());
    const result = await request(`/api/browser?path=${encodeURIComponent(browserPath)}&sort=${browserSort}&order=${browserOrder}&query=${query}`, { signal: controller.signal });
    if (controller.signal.aborted || generation !== browserLoadGeneration || isInteracting()) return;
    browserPath = result.path || "";
    allBrowserEntries = result.entries || [];
    const entries = browserFilter === "all" ? allBrowserEntries : allBrowserEntries.filter((entry) => browserFilter === "folder" ? entry.directory : browserFilter === "file" ? !entry.directory && entry.kind !== "image" && entry.kind !== "text" : !entry.directory && entry.kind === browserFilter);
    renderBreadcrumbs(); renderBrowser(entries);
    $("go-up").disabled = !result.parent;
  } catch (error) {
    if (error.name !== "AbortError") message(error.message);
  } finally {
    if (browserLoadController === controller) browserLoadController = null;
  }
}
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
async function openBrowser(path, recordHistory) { if (recordHistory && path !== browserPath) { historyBack.push(browserPath); historyForward = []; } cancelActiveMarquee(); browserPath = path || ""; applySelection(new Set()); await loadBrowser(); }
function selectedEntries() { return browserEntries.filter((entry) => selectedPaths.has(entry.path)); }
async function fileAction(action, path, name = "", paths = []) { await request("/api/file", { method: "POST", body: JSON.stringify({ action, path, name, paths }) }); await loadBrowser(); await refresh(); }
function clearPreview() {
  previewController?.abort();
  previewController = null;
  const content = $("preview-content");
  content.querySelectorAll("img").forEach((image) => { image.src = ""; image.removeAttribute("src"); });
  content.replaceChildren();
  $("preview-panel").classList.add("hidden");
}
async function showPreview(entry) { clearPreview(); const controller = new AbortController(); previewController = controller; $("preview-title").textContent = entry.name; const content = $("preview-content"); if (entry.kind === "image") { const image = document.createElement("img"); image.className = "preview-image"; image.src = `/api/preview?path=${encodeURIComponent(entry.path)}`; content.appendChild(image); } else if (entry.kind === "text") { const pre = document.createElement("pre"); pre.className = "preview-text"; try { pre.textContent = await fetch(`/api/preview?path=${encodeURIComponent(entry.path)}`, { signal: controller.signal }).then((response) => response.text()); } catch (error) { if (error.name === "AbortError") return; message(error.message); return; } if (controller.signal.aborted || previewController !== controller) return; content.appendChild(pre); } else { content.innerHTML = `<p class="muted">该文件类型不支持内置预览。</p><a class="download-link" href="/api/preview?path=${encodeURIComponent(entry.path)}" target="_blank">打开文件</a>`; } if (!controller.signal.aborted && previewController === controller) $("preview-panel").classList.remove("hidden"); }
async function showVersions(path) { try { const versions = await request(`/api/versions?path=${encodeURIComponent(path)}`); $("selected-path").textContent = path; $("versions-list").innerHTML = versions.length ? versions.map((version) => `<tr><td><code>${escapeHTML(version.ID)}</code></td><td>${escapeHTML(formatDate(version.CreatedAt))}</td><td><code>${escapeHTML(version.Manifest)}</code></td><td><button class="secondary restore-version" data-version="${escapeHTML(version.ID)}">恢复</button></td></tr>`).join("") : '<tr><td colspan="4" class="muted">暂无历史版本</td></tr>'; $("versions-list").querySelectorAll(".restore-version").forEach((button) => button.addEventListener("click", async () => { try { await request("/api/restore", { method: "POST", body: JSON.stringify({ path, version: button.dataset.version }) }); message("已恢复历史版本", true); $("versions-panel").classList.add("hidden"); await refresh(); } catch (error) { message(error.message); } })); $("versions-panel").classList.remove("hidden"); } catch (error) { message(error.message); } }
async function showProperties(entry) {
  if (!entry || entry.directory) return;
  const drawer = $("details-drawer");
  if (!drawer) return;
  const generation = ++detailsDrawerGeneration;
  detailsDrawerPath = entry.path;
  drawer.dataset.path = entry.path;
  drawer.classList.add("open");
  drawer.setAttribute("aria-hidden", "false");
  setText("drawer-title", entry.name);
  setText("drawer-status", "正在读取文件属性和历史版本");
  setText("drawer-version-count", "读取中");
  $("drawer-properties").innerHTML = '<dt>位置</dt><dd>读取中...</dd>';
  $("drawer-version-list").innerHTML = '<p class="page-muted">读取中...</p>';
  try {
    const props = await request("/api/file", { method: "POST", body: JSON.stringify({ action: "properties", path: entry.path }) });
    if (generation !== detailsDrawerGeneration) return;
    $("drawer-properties").innerHTML = `<dt>名称</dt><dd>${escapeHTML(props.name)}</dd><dt>位置</dt><dd>${escapeHTML(props.path || entry.path)}</dd><dt>类型</dt><dd>${props.directory ? "文件夹" : "文件"}</dd><dt>大小</dt><dd>${props.directory ? "-" : formatSize(Number(props.size || 0))}</dd><dt>修改时间</dt><dd>${escapeHTML(formatDate(props.modified))}</dd>`;
    setText("drawer-status", entry.status === "pending" ? "待同步文件" : entry.status === "conflict" ? "冲突副本" : "已同步文件");
    const versions = await request(`/api/versions?path=${encodeURIComponent(entry.path)}`);
    if (generation !== detailsDrawerGeneration) return;
    setText("drawer-version-count", `${versions.length} 个版本`);
    $("drawer-version-list").innerHTML = versions.length ? versions.slice(-4).reverse().map((version) => `<div class="drawer-version-item"><span>${escapeHTML(formatDate(version.CreatedAt))}</span><code>${escapeHTML(String(version.ID).slice(0, 12))}</code></div>`).join("") : '<p class="page-muted">暂无历史版本</p>';
  } catch (error) {
    if (generation === detailsDrawerGeneration) { setText("drawer-status", "无法读取文件详情"); $("drawer-version-list").innerHTML = '<p class="page-muted">历史版本暂不可用</p>'; message(error.message); }
  }
}
function showContextMenu(x, y, hasEntry) { const menu = $("context-menu"); menu.classList.remove("hidden"); const paste = menu.querySelector('[data-context-action="paste"]'); paste.disabled = clipboardPaths.length === 0; menu.querySelectorAll("[data-context-action]").forEach((button) => { const action = button.dataset.contextAction; button.disabled = (action !== "paste" && !hasEntry); }); menu.style.left = `${Math.min(x, window.innerWidth - menu.offsetWidth - 8)}px`; menu.style.top = `${Math.min(y, window.innerHeight - menu.offsetHeight - 8)}px`; }
function hideContextMenu() { $("context-menu").classList.add("hidden"); contextEntry = null; }
async function pasteClipboard(targetPath = browserPath) { if (!clipboardPaths.length) return; try { await request("/api/file", { method: "POST", body: JSON.stringify({ action: clipboardMode === "cut" ? "move" : "copy", paths: clipboardPaths, target: targetPath }) }); if (clipboardMode === "cut") clipboardPaths = []; message("粘贴完成", true); await loadBrowser(); await refresh(); } catch (error) { message(error.message); } }
async function contextAction(action) { const entry = contextEntry; hideContextMenu(); if (action === "paste") { await pasteClipboard(entry?.directory ? entry.path : browserPath); return; } if (!entry) return; const entries = selectedEntries(); if (action === "open") { if (entry.directory) await openBrowser(entry.path, true); else await showPreview(entry); return; } if (action === "copy" || action === "cut") { clipboardPaths = entries.length ? entries.map((item) => item.path) : [entry.path]; clipboardMode = action; message(action === "copy" ? "已复制到剪贴板" : "已剪切到剪贴板", true); return; } if (action === "rename") { await renameEntry(entry); return; } if (action === "delete") { await deleteSelected(); return; } if (action === "properties") await showProperties(entry); }

$("settings").addEventListener("click", () => navigateTo("settings")); $("close-settings").addEventListener("click", () => navigateTo("overview"));
$("conn-badge").addEventListener("click", () => navigateTo("settings"));
$("library-form").addEventListener("input", () => { libraryDraftDirty = true; });
$("library-form").addEventListener("submit", (event) => { event.preventDefault(); openLibrary(false); }); $("init").addEventListener("click", () => openLibrary(true)); $("browse").addEventListener("click", chooseFolder);
$("autostart").addEventListener("change", async (event) => { const checkbox = event.target; checkbox.disabled = true; try { const result = await request("/api/autostart", { method: "POST", body: JSON.stringify({ enabled: checkbox.checked }) }); renderAutoStart({ autostart: result.enabled, autostart_supported: result.supported }); message(result.enabled ? "已启用开机自动启动" : "已关闭开机自动启动", true); } catch (error) { checkbox.checked = !checkbox.checked; message(error.message); } finally { if (checkbox) checkbox.disabled = false; } });
$("empty-select-folder")?.addEventListener("click", () => { navigateTo("settings", { focus: "root" }); chooseFolder(); });
$("scan").addEventListener("click", async () => { try { const result = await request("/api/scan", { method: "POST", body: "{}" }); message(`扫描完成：${result.Files} 个文件，新增 ${result.NewVersions} 个版本`, true); await refresh(); } catch (error) { message(error.message); } });
const monitorButton = $("monitor"); if (monitorButton) monitorButton.addEventListener("click", async () => { try { await request("/api/monitor", { method: "POST", body: JSON.stringify({ action: monitoring ? "stop" : "start" }) }); await refresh(); } catch (error) { message(error.message); } });
$("connection-form").addEventListener("input", () => { connectionDraftDirty = true; });
$("connection-form").addEventListener("submit", async (event) => { event.preventDefault(); try { await request("/api/connection", { method: "POST", body: JSON.stringify({ server_url: $("server-url").value, space_id: $("space-id").value, device_id: $("device-id").value, device_token: $("device-token").value, pairing_token: $("pairing-token").value }) }); connectionDraftDirty = false; quotaDraftDirty = false; message("连接配置已保存", true); navigateTo("overview"); await refresh(); } catch (error) { message(error.message); } });
$("quota-gb").addEventListener("input", () => { quotaDraftDirty = true; });
$("quota-form").addEventListener("submit", async (event) => { event.preventDefault(); const value = Number($("quota-gb").value); if (!Number.isFinite(value) || value <= 0) { message("请输入大于 0 的容量"); return; } try { await request("/api/quota", { method: "POST", body: JSON.stringify({ quota_bytes: Math.round(value * 1073741824) }) }); quotaDraftDirty = false; message("空间容量已保存", true); await refresh(); } catch (error) { message(error.message); } });
$("quota-reset").addEventListener("click", async () => { try { await request("/api/quota", { method: "POST", body: JSON.stringify({ quota_bytes: 0 }) }); quotaDraftDirty = false; message("已恢复服务端默认容量", true); await refresh(); } catch (error) { message(error.message); } });
async function runSync() { try { await request("/api/sync", { method: "POST", body: "{}" }); lastSyncAt = new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }); message("同步完成", true); await refresh(); } catch (error) { message(error.message); } }
$("sync").addEventListener("click", runSync); $("quick-sync").addEventListener("click", runSync); $("refresh-action").addEventListener("click", loadBrowser);
$("sort-select").addEventListener("change", (event) => { browserSort = event.target.value; browserOrder = "desc"; loadBrowser(); });
$("type-filter").addEventListener("change", (event) => { browserFilter = event.target.value; const entries = browserFilter === "all" ? allBrowserEntries : allBrowserEntries.filter((entry) => browserFilter === "folder" ? entry.directory : browserFilter === "file" ? !entry.directory && entry.kind !== "image" && entry.kind !== "text" : !entry.directory && entry.kind === browserFilter); renderBrowser(entries); });
$("select-all").addEventListener("change", (event) => { applySelection(event.target.checked ? new Set(browserEntries.map((entry) => entry.path)) : new Set()); });
document.querySelectorAll("[data-nav-target]").forEach((button) => button.addEventListener("click", () => navigateTo(button.dataset.navTarget)));
$("status-versions").addEventListener("click", () => navigateTo("versions"));
$("status-devices").addEventListener("click", () => navigateTo("devices"));
$("status-pending").addEventListener("click", () => navigateTo("tasks"));
$("drawer-close").addEventListener("click", closeDetailsDrawer);
$("drawer-open-versions").addEventListener("click", () => { const path = $("details-drawer").dataset.path || detailsDrawerPath; if (!path) return; closeDetailsDrawer(); navigateTo("versions"); showVersions(path); });
$("tasks-sync").addEventListener("click", runSync); $("tasks-scan").addEventListener("click", async () => { try { const result = await request("/api/scan", { method: "POST", body: "{}" }); message(`扫描完成：${result.Files} 个文件，新增 ${result.NewVersions} 个版本`, true); await refresh(); } catch (error) { message(error.message); } });
$("tasks-monitor").addEventListener("click", async () => { try { await request("/api/monitor", { method: "POST", body: JSON.stringify({ action: monitoring ? "stop" : "start" }) }); await refresh(); } catch (error) { message(error.message); } });
$("devices-refresh").addEventListener("click", loadDevices); $("devices-open-settings").addEventListener("click", () => navigateTo("settings"));
$("go-back").addEventListener("click", async () => { if (!historyBack.length) return; historyForward.push(browserPath); browserPath = historyBack.pop(); await loadBrowser(); }); $("go-forward").addEventListener("click", async () => { if (!historyForward.length) return; historyBack.push(browserPath); browserPath = historyForward.pop(); await loadBrowser(); }); $("go-up").addEventListener("click", async () => { const parts = browserPath ? browserPath.split("/") : []; parts.pop(); await openBrowser(parts.join("/"), true); });
$("file-search").addEventListener("input", () => loadBrowser());
$("new-folder").addEventListener("click", () => $("new-menu-list").classList.toggle("hidden"));
document.querySelectorAll("[data-create]").forEach((button) => button.addEventListener("click", async () => { $("new-menu-list").classList.add("hidden"); const name = prompt(button.dataset.create === "folder" ? "文件夹名称" : "文件名"); if (!name) return; try { await fileAction(button.dataset.create === "folder" ? "mkdir" : "create", browserPath, name); message(button.dataset.create === "folder" ? "文件夹已创建" : "文件已创建", true); } catch (error) { message(error.message); } }));
$("new-file").addEventListener("click", async () => { const name = prompt("文件名"); if (name) try { await fileAction("create", browserPath, name); message("文件已创建", true); } catch (error) { message(error.message); } });
async function renameEntry(entry) { const name = prompt("新名称", entry?.name || ""); if (name && name !== entry.name) try { await fileAction("rename", entry.path, name); message("已重命名", true); } catch (error) { message(error.message); } }
async function deleteSelected() { const entries = selectedEntries(); if (!entries.length) { message("请先选择文件或文件夹"); return; } if (!confirm(`确定删除选中的 ${entries.length} 项吗？`)) return; try { await fileAction("delete", "", "", entries.map((entry) => entry.path)); selectedPaths.clear(); message("已删除", true); } catch (error) { message(error.message); } }
document.querySelectorAll(".sortable").forEach((header) => header.addEventListener("click", () => { const sort = header.dataset.sort; if (browserSort === sort) browserOrder = browserOrder === "asc" ? "desc" : "asc"; else { browserSort = sort; browserOrder = "asc"; } loadBrowser(); }));
function setViewMode(mode) { if (viewMode === mode) return; cancelActiveMarquee(); viewMode = mode; const details = mode === "details"; $("details-view").classList.toggle("hidden", !details); $("icons-view").classList.toggle("hidden", details); $("view-details").classList.toggle("active", details); $("view-icons").classList.toggle("active", !details); renderBrowser(browserEntries); }
$("view-details").addEventListener("click", () => setViewMode("details")); $("view-icons").addEventListener("click", () => setViewMode("icons"));
$("close-preview").addEventListener("click", clearPreview); $("close-versions").addEventListener("click", () => navigateTo("overview")); $("close-properties").addEventListener("click", () => $("properties-panel").classList.add("hidden"));
document.querySelectorAll("[data-context-action]").forEach((button) => button.addEventListener("click", () => contextAction(button.dataset.contextAction)));
function showBackgroundContextMenu(event) { if (event.target.closest("[data-path]")) return; event.preventDefault(); cancelActiveMarquee(); contextEntry = null; applySelection(new Set()); showContextMenu(event.clientX, event.clientY, false); }
$("details-view").addEventListener("contextmenu", showBackgroundContextMenu); $("icons-view").addEventListener("contextmenu", showBackgroundContextMenu);
document.addEventListener("click", (event) => { if (!event.target.closest("#context-menu")) hideContextMenu(); });
document.addEventListener("keydown", (event) => { if (event.key === "Escape" && activeMarquee) { cancelActiveMarquee(); return; } const editing = event.target.matches("input, textarea, select"); if (event.ctrlKey && event.key.toLowerCase() === "f") { event.preventDefault(); navigateTo("overview", { focus: "file-search" }); $("file-search").select(); return; } if (!editing && event.ctrlKey && event.key.toLowerCase() === "c") { const entries = selectedEntries(); if (entries.length) { clipboardPaths = entries.map((entry) => entry.path); clipboardMode = "copy"; message("已复制到剪贴板", true); } return; } if (!editing && event.ctrlKey && event.key.toLowerCase() === "x") { const entries = selectedEntries(); if (entries.length) { clipboardPaths = entries.map((entry) => entry.path); clipboardMode = "cut"; message("已剪切到剪贴板", true); } return; } if (!editing && event.ctrlKey && event.key.toLowerCase() === "v") { event.preventDefault(); pasteClipboard(); return; } if (!editing && event.key === "F2") { event.preventDefault(); const entry = selectedEntries()[0]; if (entry) renameEntry(entry); } if (!editing && event.key === "Delete") { event.preventDefault(); deleteSelected(); } });
installPanelInteraction();
navigateTo(pageFromLocation(), { replace: true });
refresh();
refreshTimer = window.setInterval(() => { if (!document.hidden && !closing) refresh(); }, 30000);
const browserRefreshTimer = window.setInterval(() => { if (document.hidden || closing || currentPage !== "overview" || isInteracting()) return; loadBrowser(); }, 10000);
document.addEventListener("visibilitychange", () => { if (!document.hidden && !closing && currentPage === "overview") loadBrowser(); });
window.addEventListener("beforeunload", () => { cancelActiveMarquee(); closing = true; if (refreshTimer) window.clearInterval(refreshTimer); });
