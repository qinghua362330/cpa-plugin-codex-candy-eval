"use strict";
const BASE = "/v0/management/plugins/cpa-codex-candy-eval";
const KEY_STORE = "cpa-codex-candy-eval.key";
const VIEW_STORE = "cpa-codex-candy-eval.view.v1.";
const DEFAULT_MODEL = "gpt-6.1-sol";
const DEFAULT_EFFORT = "low";
const DEFAULT_EFFORTS = ["none", "low", "medium", "high", "xhigh", "max"];
const HIDDEN_MODEL = (id) => id.toLowerCase().includes("image");
// Each test has a tab, a panel and controls whose IDs start with its name.
const TESTS = ["candy", "fingerprint", "pelican"];
const TEST_LABELS = { candy: "糖果测试", fingerprint: "指纹检测", pelican: "鹈鹕测试" };

let key = "";
let credentials = [];
let pollTimer = 0;
let loadError = "";
let loadController = null;
// Whether a refresh started with the button is running; polling never shows progress.
let refreshing = false;
let pending = false;
let storageError = "";
let clearPath = "";
let automationStatus = null;
let automationDirty = false;
let automationError = "";
let automationScopeIDs = [];
const AUTOMATION_MANAGEMENT_DEFAULT = "http://127.0.0.1:8317";
let automationManagement = null;
let automationManagementDirty = false;
let automationManagementError = "";
let degradationWebhook = null;
let degradationWebhookDirty = false;
let degradationWebhookError = "";
let degradationEvents = [];

const automationManagementReady = () => !!automationManagement?.ready && !automationManagementError;
const automationModelSupported = (model) => !/[()]/.test(model) &&
  (FINGERPRINT_CONFIG.models || []).includes(fingerprintModelID(model));

function automationManagementOrigin(value) {
  const text = value.trim();
  const match = /^https?:\/\/(127\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})|\[::1\])(?::(\d+))?\/?$/i.exec(text);
  if (!match || match.slice(2, 5).some((part) => part !== undefined && (Number(part) > 255 || String(Number(part)) !== part)) ||
    (match[5] && (Number(match[5]) < 1 || Number(match[5]) > 65535))) throw new Error("请输入数字回环地址的 HTTP(S) origin，不含路径、用户名或参数");
  return new URL(text).origin;
}

function acceptAutomationManagement(data) {
  if (typeof data?.base_url !== "string" || typeof data.ready !== "boolean") throw new Error("管理连接状态格式无效");
  const base_url = data.base_url || AUTOMATION_MANAGEMENT_DEFAULT;
  automationManagementOrigin(base_url);
  // Only retain the public status contract, even if a server accidentally includes extra fields.
  automationManagement = { base_url, ready: data.ready };
  automationManagementError = "";
  if (!automationManagementDirty) $("automation-management-url").value = base_url;
}

function renderAutomationManagement() {
  $("automation-management-fields").disabled = pending;
  $("automation-management-save").disabled = pending || !automationManagementDirty;
  $("automation-management-dirty").hidden = !automationManagementDirty;
  $("automation-management-status").textContent = automationManagementReady() ? "已配置" : "未配置";
  $("automation-management-error").textContent = automationManagementError;
  $("automation-management-error").hidden = !automationManagementError;
}

function saveAutomationManagement() {
  if (pending) return;
  return update("管理连接保存失败", async () => {
    let base_url = automationManagementOrigin($("automation-management-url").value);
    const api_key = $("automation-management-key").value;
    if (!api_key && (!automationManagementReady() || base_url !== automationManagementOrigin(automationManagement.base_url))) {
      throw new Error("新地址须填写管理密钥；仅同一地址可留空保留已存密钥");
    }
    if (!api_key) base_url = automationManagement.base_url;
    const requestKey = key;
    try {
      let data;
      try {
        data = await api(BASE + "/automation/management", { method: "PUT", body: { base_url, api_key } });
      } catch (err) {
        if (err instanceof AuthError) throw err;
        // Do not display server response bodies from a request carrying a secret.
        throw new Error("请检查回环地址、管理密钥及服务端状态");
      }
      if (key !== requestKey) return;
      acceptAutomationManagement(data);
      automationManagementDirty = false;
      $("automation-management-url").value = automationManagement.base_url;
      setNotice("flash", "管理连接已保存", "info");
    } finally {
      $("automation-management-key").value = "";
    }
  });
}

function fillAutomationModels() {
  const selected = $("automation-model").value;
  const ids = [...new Set(catalogCache.models?.ids || [])].filter(automationModelSupported).sort(compareModels);
  // Retain an unavailable saved/draft model without silently choosing a replacement.
  const options = [["", "请选择模型"], ...ids.map((id) => [id, id])];
  if (selected && !ids.includes(selected)) options.push([selected, selected + "（当前不可用）"]);
  fillSelect("automation-model", options, selected, "");
}

function acceptAutomation(data) {
  const c = data?.config;
  if (!c || typeof c.enabled !== "boolean" || typeof c.auto_disable !== "boolean" || typeof c.model !== "string" ||
    !Number.isInteger(c.interval_minutes) || c.interval_minutes < 1 || c.interval_minutes > 10080 ||
    !Array.isArray(c.credential_ids) || c.credential_ids.some((id) => typeof id !== "string" || !id) ||
    typeof data.running !== "boolean" || !Array.isArray(data.events)) throw new Error("巡检状态格式无效");
  automationStatus = data;
  automationError = "";
  if (!automationDirty) {
    automationScopeIDs = [...c.credential_ids];
    $("automation-enabled").checked = c.enabled;
    $("automation-auto-disable").checked = c.auto_disable;
    $("automation-interval").value = String(c.interval_minutes);
    $("automation-scope").value = c.credential_ids.length ? "picked" : "all";
    // Add the saved value before assigning it to a real select element.
    fillSelect("automation-model", [["", "请选择模型"], ...(c.model ? [[c.model, c.model]] : [])], c.model, "");
    fillAutomationModels();
  }
}

function pickAutomationCredentials() {
  automationScopeIDs = credentials.filter((a) => a.source === "auth_files" && a.provider === "codex" &&
    fingerprintSelected.has(a.id) && usableCredential(a)).map((a) => a.id);
  $("automation-scope").value = "picked";
  automationDirty = true;
  renderAutomation();
}

function automationConfig() {
  const config = {
    enabled: $("automation-enabled").checked,
    model: $("automation-model").value,
    interval_minutes: Number($("automation-interval").value),
    auto_disable: $("automation-auto-disable").checked,
    credential_ids: $("automation-scope").value === "all" ? [] : [...automationScopeIDs],
  };
  if (!Number.isInteger(config.interval_minutes) || config.interval_minutes < 1 || config.interval_minutes > 10080) throw new Error("巡检间隔须为 1 到 10080 的整数");
  if ((config.enabled || config.auto_disable) && !config.model) throw new Error("请选择巡检模型");
  if (config.model && !automationModelSupported(config.model)) throw new Error("请选择指纹参考库支持的模型");
  if (config.auto_disable && !automationManagementReady()) throw new Error("请先配置本地 CPA 管理连接");
  if (config.model && !catalogCache.models?.ids.includes(config.model) && config.model !== automationStatus?.config.model) throw new Error("所选模型已不在模型列表中，请重新选择");
  if ($("automation-scope").value === "picked" && !config.credential_ids.length) throw new Error("请勾选 Codex 认证文件并使用当前勾选");
  return config;
}

function renderAutomation() {
  const state = automationStatus;
  renderAutomationManagement();
  $("automation-auto-disable").disabled = pending || !automationManagementReady();
  $("automation-fields").disabled = pending || !state;
  $("automation-save").disabled = pending || !state || !automationDirty;
  $("automation-run").disabled = pending || !state || !!automationError || state.running || automationDirty || !state.config.model ||
    !automationModelSupported(state.config.model) || (state.config.auto_disable && !automationManagementReady());
  $("automation-run").dataset.tip = automationDirty ? "请先保存配置" : !state?.config.model ? "请先选择并保存巡检模型" : "";
  $("automation-cancel").disabled = pending || !state?.running;
  $("automation-dirty").hidden = !automationDirty;
  $("automation-count").textContent = $("automation-scope").value === "picked" ? `指定 ${automationScopeIDs.length} 个` : "全部已启用 Codex 认证文件";
  const date = (value) => value ? fmtTime(value) : "—";
  const fields = [["状态", !state ? "尚未读取" : state.running ? "巡检中" : state.config.enabled ? "等待定时巡检" : "定时巡检已关闭"],
    ["下次巡检", date(state?.next_run)], ["上次开始", date(state?.last_started)], ["上次结束", date(state?.last_finished)]];
  $("automation-status").innerHTML = fields.map(([label, value]) => `<div><dt>${label}</dt><dd>${esc(value)}</dd></div>`).join("");
  const error = automationError || state?.last_error || "";
  $("automation-error").textContent = error;
  $("automation-error").hidden = !error;
  const labels = { match: ["ok", "匹配"], mismatch: ["err", "不匹配"], inconclusive: ["skip", "无法判定"], disabled: ["err", "已停用"], disable_failed: ["bad", "停用失败"], skipped: ["skip", "已跳过"] };
  $("automation-results").innerHTML = (state?.events || []).slice(-50).reverse().map((event) => {
    if (!event || typeof event !== "object") return "";
    const [tone, label] = Object.hasOwn(labels, event.status) ? labels[event.status] : ["skip", "未知状态"];
    return `<article class="automation-event"><div>${statusPill(tone, label)}<div class="meta">${esc(date(event.time))}</div></div><div class="name mono">${esc(event.credential_id)}</div><div><span class="mono">${esc(event.model)} → ${esc(event.prediction || "—")}</span> · ${fmtPercent(event.probability)}</div>${event.message ? `<p>${esc(event.message)}</p>` : ""}</article>`;
  }).join("") || '<p class="meta">暂无巡检结果</p>';
}

function changeAutomation(action) {
  if (!automationStatus || pending) return;
  if (action === "run" && (automationDirty || automationStatus.running || !automationStatus.config.model ||
    !automationModelSupported(automationStatus.config.model) || (automationStatus.config.auto_disable && !automationManagementReady()))) return;
  if (action === "cancel" && !automationStatus.running) return;
  return update("巡检操作失败", async () => {
    const body = action === "save" ? automationConfig() : undefined;
    const data = await api(BASE + "/automation" + (action === "save" ? "" : "/" + action), { method: action === "save" ? "PUT" : "POST", body });
    // Validate before clearing the draft; failed saves must remain editable.
    acceptAutomation(data);
    if (action === "save") { automationDirty = false; acceptAutomation(data); }
    setNotice("flash", action === "save" ? "巡检配置已保存" : action === "run" ? "已开始本次巡检" : "已请求停止本次巡检", "info");
  });
}

function initializeAutomation() {
  $("automation-management-form").addEventListener("input", () => { automationManagementDirty = true; renderAutomationManagement(); });
  $("automation-management-form").addEventListener("submit", (e) => { e.preventDefault(); saveAutomationManagement(); });
  $("automation-form").addEventListener("input", () => { automationDirty = true; renderAutomation(); });
  $("automation-scope").addEventListener("change", () => {
    automationDirty = true;
    if ($("automation-scope").value === "picked") pickAutomationCredentials();
    else renderAutomation();
  });
  $("automation-pick").addEventListener("click", pickAutomationCredentials);
  $("automation-form").addEventListener("submit", (e) => { e.preventDefault(); changeAutomation("save"); });
  $("automation-run").addEventListener("click", () => changeAutomation("run"));
  $("automation-cancel").addEventListener("click", () => changeAutomation("cancel"));
  $("degradation-webhook-form").addEventListener("input", () => { degradationWebhookDirty = true; renderDegradationWebhook(); });
  $("degradation-webhook-form").addEventListener("submit", (e) => { e.preventDefault(); saveDegradationWebhook(); });
  $("degradation-events").addEventListener("click", (e) => {
    const eventID = e.target.closest("[data-degradation-retry]")?.dataset.degradationRetry;
    if (eventID) retryDegradationEvent(eventID);
  });
}

function acceptDegradationWebhook(data) {
  if (!data || typeof data.enabled !== "boolean" || typeof data.endpoint !== "string" ||
    !Number.isInteger(data.timeout_seconds) || typeof data.ready !== "boolean") throw new Error("事件连接状态格式无效");
  degradationWebhook = { enabled: data.enabled, endpoint: data.endpoint, timeout_seconds: data.timeout_seconds, ready: data.ready };
  degradationWebhookError = "";
  if (!degradationWebhookDirty) {
    $("degradation-webhook-enabled").checked = data.enabled;
    $("degradation-webhook-endpoint").value = data.endpoint;
    $("degradation-webhook-timeout").value = String(data.timeout_seconds || 10);
  }
}

function renderDegradationWebhook() {
  const state = degradationWebhook;
  $("degradation-webhook-fields").disabled = pending;
  $("degradation-webhook-save").disabled = pending || !degradationWebhookDirty;
  $("degradation-webhook-dirty").hidden = !degradationWebhookDirty;
  $("degradation-webhook-status").textContent = state?.ready ? "已启用" : state?.endpoint ? "已保存但未启用" : "未配置";
  $("degradation-webhook-error").textContent = degradationWebhookError;
  $("degradation-webhook-error").hidden = !degradationWebhookError;
  const labels = { pending: ["skip", "待投递"], delivered: ["ok", "已送达"] };
  $("degradation-events").innerHTML = degradationEvents.slice().reverse().map((event) => {
    const [tone, label] = labels[event.state] || ["bad", "未知"];
    const payload = event.payload || {};
    const retry = event.state === "delivered" ? "" : `<button class="btn ghost" type="button" data-degradation-retry="${esc(payload.event_id || "")}">重试</button>`;
    return `<article class="automation-event"><div>${statusPill(tone, label)}<div class="meta">尝试 ${Number(event.attempts || 0)} 次</div></div><div class="name mono">${esc(payload.credential_id || "—")}</div><div><span class="mono">${esc(payload.expected_model || "—")} → ${esc(payload.detected_model || "—")}</span>${event.last_error ? `<p>${esc(event.last_error)}</p>` : ""}${retry}</div></article>`;
  }).join("") || '<p class="meta">暂无控制面事件</p>';
}

function saveDegradationWebhook() {
  if (pending) return;
  return update("事件连接保存失败", async () => {
    const body = {
      enabled: $("degradation-webhook-enabled").checked,
      endpoint: $("degradation-webhook-endpoint").value.trim(),
      secret: $("degradation-webhook-secret").value,
      timeout_seconds: Number($("degradation-webhook-timeout").value),
    };
    try {
      acceptDegradationWebhook(await api(BASE + "/degradation-webhook", { method: "PUT", body }));
      degradationWebhookDirty = false;
      setNotice("flash", "Team 控制面事件连接已保存", "info");
    } finally {
      $("degradation-webhook-secret").value = "";
    }
  });
}

function retryDegradationEvent(eventID) {
  return update("事件重试失败", async () => {
    await api(BASE + "/degradation-events/retry", { method: "POST", body: { event_id: eventID } });
    setNotice("flash", "已安排重新投递", "info");
  });
}
// Enable changes in flight, by credential ID; reloads keep these requested states.
const switching = new Map();
const credentialFilter = {
  enabledOnly: preference("credential-filter.enabledOnly", false),
  type: preference("credential-filter.type", "auth_files:codex"),
  plan: preference("credential-filter.plan", "all"),
};

// The CPA management panel keeps its state in same-origin localStorage, optionally obfuscated.
function panelValue(name) {
  try {
    let raw = localStorage.getItem(name);
    if (!raw) return null;
    // CPAMC writes enc::v1:: (keyed by host and user agent), CPAMP enc::v2:: (keyed by host).
    const prefix = /^enc::v([12])::/.exec(raw);
    if (prefix) {
      const scope = prefix[1] === "2" ? "v2|" + location.host : location.host + "|" + navigator.userAgent;
      const secret = new TextEncoder().encode("cli-proxy-api-webui::secure-storage|" + scope);
      const bin = atob(raw.slice(prefix[0].length));
      const bytes = Uint8Array.from(bin, (c, i) => c.charCodeAt(0) ^ secret[i % secret.length]);
      raw = new TextDecoder().decode(bytes);
    }
    return JSON.parse(raw);
  } catch (_) { return null; }
}

// Panels resolve their "auto" theme to white or dark; CPAMC also has a light (paper) theme.
function applyTheme() {
  const theme = panelValue("cli-proxy-theme")?.state?.theme;
  document.documentElement.dataset.theme = ["light", "white", "dark"].includes(theme) ? theme
    : matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "white";
}
function panelKey() {
  const k = panelValue("cli-proxy-auth")?.state?.managementKey;
  return typeof k === "string" ? k.trim() : "";
}

class AuthError extends Error {}

async function api(path, { method = "GET", body, apiKey, signal } = {}) {
  const timeout = AbortSignal.timeout(30000);
  const requestKey = apiKey ?? key;
  const init = { method, cache: "no-store", signal: signal ? AbortSignal.any([signal, timeout]) : timeout, headers: { Authorization: "Bearer " + requestKey } };
  if (body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  let resp, text;
  try {
    resp = await fetch(path, init);
    text = await resp.text();
  } catch (err) {
    if (signal?.aborted) throw err;
    throw new Error(err.name === "TimeoutError" ? "连接超时，请稍后重试。" : "无法连接 CPA，请检查网络后重试。");
  }
  let data = null;
  try { data = JSON.parse(text); } catch (_) {}
  // A 401 for a key replaced by a newer login must not sign that login out.
  if (resp.status === 401 && apiKey === undefined && requestKey === key) throw new AuthError("管理密钥无效，请重新输入。");
  if (!resp.ok) {
    const err = data?.error;
    throw Object.assign(new Error(String((typeof err === "string" ? err : err?.message) || text || "HTTP " + resp.status).slice(0, 500)), { status: resp.status });
  }
  if (!data || typeof data !== "object" || Array.isArray(data)) throw new Error("服务器返回了无效数据，请稍后重试。");
  return data;
}

function stored(name) {
  try { return JSON.parse(localStorage.getItem(name)); } catch (_) { return null; }
}
function store(name, value) {
  try { localStorage.setItem(name, JSON.stringify(value)); } catch (_) {}
}
// Page choices are kept across visits; a saved value replaces its default only when the types match.
function preference(name, fallback) {
  const value = stored(VIEW_STORE + name);
  return value !== null && typeof value === typeof fallback ? value : fallback;
}
function savePreference(name, value) {
  store(VIEW_STORE + name, value);
}
function fillModels() {
  fillAutomationModels();
  const ids = [...new Set(catalogCache.models?.ids || [])].sort(compareModels);
  const unsuffixed = ids.filter((id) => !/[()]/.test(id));
  for (const [id, models] of [["candy-model", ids], ["fingerprint-model", unsuffixed], ["pelican-model", ids]]) {
    fillSelect(id, models, $(id).value || preference(id, DEFAULT_MODEL), DEFAULT_MODEL);
  }
}
// Rows are keyed by credential, and an update patches only the markup that changed, so the rest keeps its
// state, such as focus or a loaded preview.
const renderedLists = new Map();
function renderList(test, renderRow) {
  const list = $(test + "-rows"), previous = renderedLists.get(list) || { rows: new Map(), empty: "" };
  const visible = visibleCredentials();
  if (!visible.length) {
    const message = loadController && !credentials.length ? "正在读取凭证和测试记录…" : "没有符合筛选条件的凭证，请调整筛选或在 CPA 中添加凭证。";
    const empty = `<div class="empty">${esc(loadError || message)}</div>`;
    if (previous.empty !== empty) list.innerHTML = empty;
    renderedLists.set(list, { rows: new Map(), empty });
    return;
  }
  const rows = new Map(visible.map((a) => {
    const html = renderRow(a), row = previous.rows.get(a.id);
    if (row?.html === html) return [a.id, row];
    return [a.id, { html, node: row ? patch(row.node, rowNode(html)) : rowNode(html) }];
  }));
  renderedLists.set(list, { rows, empty: "" });
  const kept = new Set([...rows.values()].map((row) => row.node));
  let cursor = list.firstChild;
  const dropStale = () => {
    while (cursor && !kept.has(cursor)) {
      const next = cursor.nextSibling;
      cursor.remove();
      cursor = next;
    }
  };
  for (const { node } of rows.values()) {
    dropStale();
    if (node === cursor) cursor = cursor.nextSibling;
    else list.insertBefore(node, cursor);
  }
  dropStale();
  refreshTip();
}
// patch updates node to match next and returns the node now in place.
function patch(node, next) {
  if (node.isEqualNode(next)) return node;
  if (node.nodeName !== next.nodeName) {
    node.replaceWith(next);
    return next;
  }
  if (node.nodeType !== Node.ELEMENT_NODE) {
    node.nodeValue = next.nodeValue;
    return node;
  }
  for (const { name } of [...node.attributes]) if (!next.hasAttribute(name)) node.removeAttribute(name);
  for (const { name, value } of next.attributes) if (node.getAttribute(name) !== value) node.setAttribute(name, value);
  // Once clicked, a checkbox no longer follows its checked attribute.
  if (node instanceof HTMLInputElement) node.checked = next.checked;
  const children = [...node.childNodes], updates = [...next.childNodes];
  updates.forEach((update, i) => children[i] ? patch(children[i], update) : node.append(update));
  for (const child of children.slice(updates.length)) child.remove();
  return node;
}
function rowNode(html) {
  const template = document.createElement("template");
  template.innerHTML = html;
  return template.content.firstElementChild;
}

function renderPageActions() {
  $("clear-all").disabled = pending || credentials.every((a) => !a.candies.length && !a.fingerprints.length && !a.pelicans.length);
  $("refresh").disabled = pending || refreshing;
  $("refresh").setAttribute("aria-busy", refreshing);
}

function render() {
  renderPageActions();
  setNotice("storage-error", storageError);
  renderCredentialFilters();
  renderCandy();
  renderFingerprint();
  renderAutomation();
  renderDegradationWebhook();
  renderPelican();
}

function stopPolling() {
  clearTimeout(pollTimer);
  loadController?.abort();
  loadController = null;
}

function stateCredentials(data) {
  const validRecord = (r) => r !== null && typeof r === "object" && !Array.isArray(r);
  if (!Array.isArray(data.credentials) || data.credentials.some((a) => !a || typeof a.id !== "string" || !a.id.trim() ||
    ["candies", "fingerprints", "pelicans"].some((field) => !Array.isArray(a[field]) || !a[field].every(validRecord)))) {
    throw new Error("服务器返回的测试记录格式无效。");
  }
  return data.credentials;
}

async function load({ refresh = false } = {}) {
  stopPolling();
  const controller = new AbortController();
  loadController = controller;
  render();
  try {
    try {
      await refreshCatalog({ signal: controller.signal, force: refresh });
      if (controller.signal.aborted) return;
      fillModels();
      setNotice("catalog-error", "");
    } catch (err) {
      if (controller.signal.aborted) return;
      if (err instanceof AuthError) return showLogin(err.message);
      setNotice("catalog-error", "读取凭证和模型列表失败：" + err.message);
    }
    const data = await api(BASE + "/state", { signal: controller.signal });
    if (controller.signal.aborted) return;
    credentials = stateCredentials(data);
    for (const a of credentials) if (switching.has(a.id)) a.disabled = switching.get(a.id);
    pruneCredentialCatalog();
    storageError = data.storage_error || "";
    loadError = "";
    setNotice("load-error", "");
    const ids = new Set(credentials.map((a) => a.id));
    const usable = new Set(credentials.filter(usableCredential).map((a) => a.id));
    const answers = new Set(credentials.flatMap((a) => a.candies.map((r) => answerKey(a.id, r))));
    const pages = new Set(credentials.flatMap((a) => a.pelicans.map((r) => pelicanKey(a.id, r.id))));
    for (const expanded of [candyExpanded, fingerprintExpanded, pelicanExpanded]) {
      for (const id of expanded) if (!ids.has(id)) expanded.delete(id);
    }
    for (const selection of [candySelected, fingerprintSelected, pelicanSelected]) {
      for (const id of selection) if (!usable.has(id)) selection.delete(id);
    }
    for (const id of candyAnswersExpanded) if (!answers.has(id)) candyAnswersExpanded.delete(id);
    // Failed page loads retry with the next state.
    for (const [key, page] of pelicanPages) if (!pages.has(key) || page instanceof Error) pelicanPages.delete(key);
  } catch (err) {
    if (controller.signal.aborted) return;
    if (err instanceof AuthError) return showLogin(err.message);
    loadError = "读取测试记录失败：" + err.message;
    setNotice("load-error", loadError);
  } finally {
    if (loadController === controller && !controller.signal.aborted) {
      try {
        const status = await api(BASE + "/automation/management", { signal: controller.signal });
        if (!controller.signal.aborted) acceptAutomationManagement(status);
      } catch (err) {
        if (!controller.signal.aborted) {
          if (err instanceof AuthError) showLogin(err.message);
          else automationManagementError = "无法读取管理连接状态，请刷新重试";
        }
      }
    }
    if (loadController === controller && !controller.signal.aborted) {
      try {
        const [webhook, events] = await Promise.all([
          api(BASE + "/degradation-webhook", { signal: controller.signal }),
          api(BASE + "/degradation-events", { signal: controller.signal }),
        ]);
        if (!controller.signal.aborted) {
          acceptDegradationWebhook(webhook);
          if (!Array.isArray(events.events)) throw new Error("事件历史格式无效");
          degradationEvents = events.events;
          degradationWebhookError = events.storage_error || "";
        }
      } catch (err) {
        if (!controller.signal.aborted) degradationWebhookError = "读取 Team 控制面事件失败：" + err.message;
      }
    }
    if (loadController === controller && !controller.signal.aborted) {
      try {
        const status = await api(BASE + "/automation", { signal: controller.signal });
        if (!controller.signal.aborted) acceptAutomation(status);
      } catch (err) {
        if (!controller.signal.aborted) {
          if (err instanceof AuthError) showLogin(err.message);
          else automationError = "读取巡检状态失败：" + err.message;
        }
      }
    }
    if (loadController === controller) {
      loadController = null;
      render();
      pollTimer = setTimeout(load, automationStatus?.running || credentials.some(credentialBusy) ? 2500 : 20000);
    }
  }
}

// update makes one change at a time, reports its failure, and reloads the state afterwards.
async function update(errorPrefix, change) {
  if (pending) return;
  pending = true;
  stopPolling();
  setNotice("flash", "");
  render();
  try {
    await change();
  } catch (err) {
    if (err instanceof AuthError) return showLogin(err.message);
    setNotice("flash", errorPrefix + "：" + err.message);
  } finally {
    pending = false;
    if (!$("app").hidden) await load();
  }
}
// startTest starts test with the credentials' model lists, so the plugin skips credentials without the model.
function startTest(test, body) {
  return update("开始测试失败", async () => {
    await refreshCatalog();
    fillModels();
    setNotice("catalog-error", "");
    if (!catalogCache.models?.ids.includes(body.model)) throw new Error("所选模型已不在模型列表中，请重新选择。");
    const response = await api(`${BASE}/${test}/run`, { method: "POST", body: { ...body, model_catalog: await modelCatalog(body.credential_ids) } });
    const messages = [`已为 ${response.started || 0} 个凭证开始测试`];
    if (response.skipped) messages.push(`${response.skipped} 个凭证不支持所选模型，已跳过`);
    if (response.unchecked) messages.push(`${response.unchecked} 个凭证无法读取模型列表，仍会尝试测试`);
    if (response.busy) messages.push(`${response.busy} 个凭证正在测试，已跳过`);
    setNotice("flash", messages.join("；"), response.unchecked ? "warning" : "info");
  });
}

function showLogin(message) {
  stopPolling();
  key = "";
  try { sessionStorage.removeItem(KEY_STORE); } catch (_) {}
  resetCatalogCache();
  credentials = [];
  automationStatus = null;
  automationDirty = false;
  automationError = "";
  automationScopeIDs = [];
  automationManagement = null;
  automationManagementDirty = false;
  automationManagementError = "";
  degradationWebhook = null;
  degradationWebhookDirty = false;
  degradationWebhookError = "";
  degradationEvents = [];
  $("degradation-webhook-enabled").checked = false;
  $("degradation-webhook-endpoint").value = "";
  $("degradation-webhook-secret").value = "";
  $("degradation-webhook-timeout").value = "10";
  $("automation-management-url").value = AUTOMATION_MANAGEMENT_DEFAULT;
  $("automation-management-key").value = "";
  $("automation-model").value = "";
  $("automation-interval").value = "30";
  $("automation-enabled").checked = $("automation-auto-disable").checked = false;
  $("automation-scope").value = "all";
  loadError = storageError = "";
  for (const collection of [candySelected, fingerprintSelected, pelicanSelected, candyExpanded, fingerprintExpanded, pelicanExpanded, candyAnswersExpanded, pelicanPages]) collection.clear();
  for (const id of notices.keys()) setNotice(id, "");
  hideTip();
  document.querySelectorAll("dialog[open]").forEach((dialog) => dialog.close());
  render();
  $("app").hidden = true;
  $("login").hidden = false;
  $("login-error").hidden = !message;
  $("login-error").textContent = message || "";
  $("login-key").focus();
}

async function start() {
  setNotice("flash", "");
  $("login").hidden = true;
  $("app").hidden = false;
  const managementKey = key;
  await openCatalogCache(managementKey);
  if (managementKey !== key) return;
  fillModels();
  await load({ refresh: true });
}

function setMasked(masked) {
  document.body.classList.toggle("masked", masked);
  for (const test of TESTS) {
    const button = $(test + "-mask");
    button.setAttribute("aria-pressed", masked);
    button.innerHTML = icon(masked ? "eye" : "eye-off");
    setLabel(button, masked ? "取消脱敏" : "脱敏");
  }
}
function switchTab(name) {
  for (const test of TESTS) {
    const active = name === test;
    $("tab-" + test).setAttribute("aria-selected", active);
    $("tab-" + test).tabIndex = active ? 0 : -1;
    $(test + "-panel").hidden = !active;
  }
  hideTip();
  savePreference("tab", name);
}
const busyTest = (a) => TEST_LABELS[a.running?.test] || "";
const credentialBusy = (a) => !!a.running;
const usableCredential = (a) => !a.disabled && !a.unavailable;
const availableCredential = (a) => usableCredential(a) && !credentialBusy(a);
// Why a test cannot start, shown on its disabled button; empty when it can.
const pageBlocker = (test) => !$(test + "-model").value ? "模型列表为空，请刷新后重试" : pending ? "正在处理上一个操作，请稍候" : "";
function runBlocker(test, a) {
  if (a.disabled) return "凭证已停用，启用后才能测试";
  if (a.unavailable) return "凭证暂不可用：" + unavailableReason(a);
  if (credentialBusy(a)) return `${busyTest(a)}进行中，结束后才能开始新测试`;
  return pageBlocker(test);
}
const runnable = (test, a) => !runBlocker(test, a);
const selectedCredentials = (selection) => visibleCredentials().filter((a) => selection.has(a.id));
const batchCredentials = (selection) => {
  const selected = selectedCredentials(selection);
  return (selected.length ? selected : visibleCredentials()).filter(availableCredential);
};
async function setCredentialEnabled(id, enabled) {
  const a = credentials.find((entry) => entry.id === id);
  if (!a || switching.has(id)) return;
  switching.set(id, !enabled);
  a.disabled = !enabled;
  render();
  try {
    await api("/v0/management/auth-files/status", { method: "PATCH", body: { name: id, disabled: !enabled } });
  } catch (err) {
    if (err instanceof AuthError) return showLogin(err.message);
    setNotice("flash", (enabled ? "启用凭证失败：" : "停用凭证失败：") + err.message);
    const current = credentials.find((entry) => entry.id === id);
    if (current) current.disabled = enabled;
  } finally {
    switching.delete(id);
  }
  render();
  // Config API keys live in CPA's config, which the page syncs to the plugin.
  if (!$("app").hidden) await load({ refresh: a.source !== "auth_files" });
}
function renderSelection(test, selection, action) {
  const available = visibleCredentials().filter(availableCredential);
  const selected = selectedCredentials(selection);
  const targets = batchCredentials(selection);
  const button = $(test + "-run-batch");
  button.innerHTML = `${icon("play")}${action}${selected.length ? "所选" : "全部"} (${targets.length})`;
  const blocker = pageBlocker(test) || (targets.length ? "" : selected.length ? "所选凭证均已停用、不可用或正在测试" : "当前列表没有可测试的凭证");
  button.disabled = !!blocker;
  button.dataset.tip = blocker;
  const checkbox = $(test + "-select-all");
  checkbox.checked = available.length > 0 && available.every((a) => selection.has(a.id));
  checkbox.indeterminate = selected.length > 0 && !checkbox.checked;
  checkbox.disabled = pending || (!available.length && !selected.length);
}

function bindCollectionActions({ test, expanded, renderRows, run, showDetail }) {
  const rows = $(test + "-rows"), dialog = $(test + "-detail");
  $(test + "-detail-close").addEventListener("click", () => dialog.close());
  dialog.addEventListener("close", () => {
    rows.querySelector(`[data-${test}-detail="${CSS.escape(dialog.dataset.record)}"][data-${test}-credential="${CSS.escape(dialog.dataset.credential)}"]`)?.focus();
  });
  rows.addEventListener("click", (e) => {
    if (e.target.closest("label")) return;
    const action = (name) => e.target.closest(`[data-${test}-${name}]`)?.getAttribute(`data-${test}-${name}`);
    const runID = action("run");
    if (runID) return run({ credential_ids: [runID] });
    const cancelID = action("cancel");
    if (cancelID) return update("停止测试失败", () => api(`${BASE}/cancel`, { method: "POST", body: { credential_ids: [cancelID] } }));
    const recordID = action("detail"), credentialID = action("credential");
    if (recordID && credentialID) return openResultDetail(test, credentialID, recordID, showDetail);
    const toggleID = e.target.closest("[data-row]")?.dataset.row;
    if (toggleID) {
      expanded.has(toggleID) ? expanded.delete(toggleID) : expanded.add(toggleID);
      renderRows();
    }
  });
}

// CPA names each OpenAI-compatible provider after its config entry; they share one type.
const providerType = (a) => a.provider === "openai-compatibility" || a.provider.startsWith("openai-compatible-") ? "openai-compatibility" : a.provider;
const credentialType = (a) => a.source + ":" + providerType(a);
function credentialTypeLabel(type) {
  const [source, provider] = type.split(/:(.*)/);
  return `${source === "ai_providers" ? "AI 提供商" : "认证文件"} · ${provider}`;
}
const ofType = (a, type) => type === "all" || credentialType(a) === type;
const PRO_ALL = "Pro ALL";
// Plans of one credential type, best first, grouping Pro plans when there are several.
function planChoices(type) {
  const plans = [...new Map(credentials.filter((a) => ofType(a, type)).map(credentialPlan).filter(Boolean).map((plan) => [plan.label, plan])).values()]
    .sort((a, b) => a.rank - b.rank || a.label.localeCompare(b.label));
  return [...(plans.filter((plan) => plan.pro).length > 1 ? [PRO_ALL] : []), ...plans.map((plan) => plan.label)];
}
function visibleCredentials() {
  const { enabledOnly, type, plan } = credentialFilter;
  const matches = (p) => plan === "all" || (plan === PRO_ALL ? p?.pro : p?.label === plan);
  return credentials.filter((a) => ofType(a, type) && (!enabledOnly || !a.disabled) && matches(credentialPlan(a))).sort((a, b) => planRank(a) - planRank(b));
}
// Every card shows the filter; chosen values stay listed while no credential matches them.
function renderCredentialFilters() {
  const { enabledOnly, type, plan } = credentialFilter;
  const types = [...new Set([type, ...credentials.map(credentialType)])].filter((t) => t !== "all").sort((a, b) => a.localeCompare(b));
  const plans = [...new Set([...planChoices(type), plan])].filter((p) => p !== "all");
  for (const test of TESTS) {
    $(test + "-enabled-only").checked = enabledOnly;
    fillSelect(test + "-credential-type", [["all", "全部凭证"], ...types.map((t) => [t, credentialTypeLabel(t)])], type);
    fillSelect(test + "-credential-plan", [["all", "所有订阅类型"], ...plans], plan);
    $(test + "-plan-filter").hidden = !plans.length;
  }
}
function setCredentialFilter(change) {
  for (const [name, value] of Object.entries(change)) {
    credentialFilter[name] = value;
    savePreference("credential-filter." + name, value);
  }
  for (const selection of [candySelected, fingerprintSelected, pelicanSelected]) selection.clear();
  render();
}

function initializeLayout() {
  $("candy-credentials").innerHTML = credentialCard("candy", ["凭证", "最近一次", "正确率", "最近 20 次"]);
  $("fingerprint-credentials").innerHTML = credentialCard("fingerprint", ["凭证", "归因结果", "测试模型", "测试时间"]);
  $("pelican-credentials").innerHTML = credentialCard("pelican", ["凭证", "最近一次", "画面"]);
  $("notifications").innerHTML = ["flash", "load-error", "catalog-error", "storage-error"].map((id) => `<div id="${id}" class="global-flash" role="alert" hidden><span class="notice-symbol" aria-hidden="true"></span><span class="notice-message"></span><button class="notice-close" type="button" aria-label="隐藏提示" data-tip>${icon("x")}</button></div>`).join("");
}

// Events and initialization.
initializeLayout();
initializeTips();
initializeCopyButtons();
initializeCandy();
initializeFingerprint();
initializeAutomation();
initializePelican();
document.querySelectorAll("[data-icon]").forEach((el) => { el.outerHTML = icon(el.dataset.icon, el.dataset.class); });

for (const id of ["flash", "load-error", "catalog-error", "storage-error"]) {
  $(id).querySelector(".notice-close").addEventListener("click", () => hideNotice(id));
}

$("refresh").addEventListener("click", async () => {
  if (pending) return;
  resetCatalogCache();
  refreshing = true;
  try {
    await load({ refresh: true });
  } finally {
    refreshing = false;
    renderPageActions();
  }
});

applyTheme();
addEventListener("storage", applyTheme);
matchMedia("(prefers-color-scheme: dark)").addEventListener("change", applyTheme);

// Masking and credential filters apply to every tab, whichever card changes them.
setMasked(preference("masked", false));
renderCredentialFilters();
for (const test of TESTS) {
  $(test + "-mask").addEventListener("click", () => {
    const masked = !document.body.classList.contains("masked");
    setMasked(masked);
    savePreference("masked", masked);
  });
  $(test + "-enabled-only").addEventListener("change", (e) => setCredentialFilter({ enabledOnly: e.target.checked }));
  $(test + "-credential-type").addEventListener("change", (e) => {
    const type = e.target.value;
    setCredentialFilter({ type, plan: planChoices(type).includes(credentialFilter.plan) ? credentialFilter.plan : "all" });
  });
  $(test + "-credential-plan").addEventListener("change", (e) => setCredentialFilter({ plan: e.target.value }));
}

for (const [id, path] of [["candy-clear", "/candy/records"], ["fingerprint-clear", "/fingerprint/records"], ["pelican-clear", "/pelican/records"], ["clear-all", "/records"]]) {
  $(id).addEventListener("click", () => {
    clearPath = path;
    $("clear-title").textContent = `${$(id).getAttribute("aria-label")}？`;
    $("confirm-clear").returnValue = ""; // Esc keeps the previous returnValue.
    $("confirm-clear").showModal();
  });
}
$("confirm-clear").addEventListener("close", () => {
  if ($("confirm-clear").returnValue !== "confirm") return;
  update("清空测试记录失败", () => api(BASE + clearPath, { method: "DELETE" }));
});

$("login-form").addEventListener("submit", (e) => {
  e.preventDefault();
  key = $("login-key").value.trim();
  try { sessionStorage.setItem(KEY_STORE, key); } catch (_) {}
  start();
});

for (const name of TESTS) {
  $("tab-" + name).addEventListener("click", () => switchTab(name));
  $("tab-" + name).addEventListener("keydown", (e) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(e.key)) return;
    e.preventDefault();
    const next = e.key === "Home" ? TESTS[0] : e.key === "End" ? TESTS.at(-1) : TESTS[(TESTS.indexOf(name) + (e.key === "ArrowRight" ? 1 : -1) + TESTS.length) % TESTS.length];
    switchTab(next); $("tab-" + next).focus();
  });
}
const savedTab = preference("tab", "candy");
switchTab(TESTS.includes(savedTab) ? savedTab : "candy");

for (const [test, selection, submit] of [["candy", candySelected, runCandy], ["fingerprint", fingerprintSelected, runFingerprint], ["pelican", pelicanSelected, runPelican]]) {
  // Each test keeps its own settings, saved by control after the control normalizes its value.
  $(test + "-toolbar").addEventListener("change", (e) => savePreference(e.target.id, e.target.type === "number" ? e.target.valueAsNumber : e.target.value));
  $(test + "-toolbar").addEventListener("submit", (e) => {
    e.preventDefault();
    const ids = batchCredentials(selection).map((a) => a.id);
    if (ids.length) submit({ credential_ids: ids });
  });
  $(test + "-select-all").addEventListener("change", (e) => {
    for (const a of visibleCredentials()) {
      if (!e.target.checked) selection.delete(a.id);
      else if (availableCredential(a)) selection.add(a.id);
    }
    render();
  });
  $(test + "-rows").addEventListener("change", (e) => {
    if (e.target.dataset.enable) return setCredentialEnabled(e.target.dataset.enable, e.target.checked);
    const id = e.target.getAttribute(`data-${test}-select`);
    if (!id) return;
    e.target.checked ? selection.add(id) : selection.delete(id);
    render();
  });
}
key = panelKey();
if (!key) { try { key = sessionStorage.getItem(KEY_STORE) || ""; } catch (_) {} }
key ? start() : showLogin("");
