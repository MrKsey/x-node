/* X-Node web interface.
 *
 * Plain JavaScript, no build step and no external requests: the same file is
 * embedded into the Go binary. The API lives under the random path, so every
 * request is relative to the page URL (which always ends with a slash).
 */
"use strict";

const state = {
  user: "",
  status: null,
  clients: [],
  invites: [],
  settings: null,
  peers: null,
  update: null,
  updatePolling: false,
  traffic: null,
  trafficRange: "1h",
  system: null,
  logs: [],
  logWrap: true,
  view: "dashboard",
  timer: null,
  logTimer: null,
  clientTimer: null,
  clientRefreshing: false,
  peerPolling: false,
  editing: "",
  popover: null,
};

const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));

/* ---------- helpers ---------- */

// A request that never answers freezes the view: the browser can reuse a
// keep-alive connection that a proxy or a NAT has already dropped, and the
// first request after a long idle then waits for the TCP retransmission
// timeout. Every call gets a deadline, and a GET is retried once on a fresh
// connection, which is exactly what a page reload used to do by hand.
//
// The first attempt is the short one: a dead pooled connection is the common
// case, and the retry opens a new socket, so waiting long there only delays
// the page. The retry gets the full budget for a slow but alive server.
const apiAttemptTimeouts = [10000, 20000];

function apiFetch(method, path, body, signal) {
  const options = {
    method,
    credentials: "same-origin",
    headers: { "X-Requested-With": "x-node" },
    signal,
  };
  if (body !== undefined) {
    options.headers["Content-Type"] = "application/json";
    options.body = JSON.stringify(body);
  }
  return fetch(path, options).then(async (response) => {
    if (response.status === 401) {
      showLogin();
      throw new Error("Not signed in");
    }
    const text = await response.text();
    let data = {};
    if (text) {
      try { data = JSON.parse(text); } catch (_) { data = { error: text }; }
    }
    if (!response.ok) {
      throw new Error(data.message || data.error || ("HTTP " + response.status));
    }
    return data;
  });
}

function api(method, path, body) {
  const attempt = (index) => {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), apiAttemptTimeouts[index]);
    return apiFetch(method, path, body, controller.signal)
      .catch((err) => {
        const timedOut = err.name === "AbortError";
        // Only a transport failure is repeated, and only for a request that is
        // safe to repeat: a POST may already have been applied on the server.
        if (index + 1 < apiAttemptTimeouts.length && method === "GET" &&
            (timedOut || err instanceof TypeError)) {
          return attempt(index + 1);
        }
        if (timedOut) throw new Error("The node did not answer in time");
        throw err;
      })
      .finally(() => clearTimeout(timer));
  };
  return attempt(0);
}

function bytes(value) {
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  let number = Number(value) || 0;
  let index = 0;
  while (number >= 1000 && index < units.length - 1) {
    number /= 1000;
    index += 1;
  }
  const digits = index === 0 || number >= 100 ? 0 : 1;
  return number.toFixed(digits) + " " + units[index];
}

// bitrate renders bits per second. The dashboard shows throughput in bit/s,
// because that is the unit of a provider tariff and of the quota form.
function bitrate(bitsPerSecond) {
  const units = ["bps", "Kbps", "Mbps", "Gbps", "Tbps"];
  let number = Number(bitsPerSecond) || 0;
  let index = 0;
  while (number >= 1000 && index < units.length - 1) {
    number /= 1000;
    index += 1;
  }
  const digits = index === 0 || number >= 100 ? 0 : 1;
  return number.toFixed(digits) + " " + units[index];
}

function rate(kbps) {
  const value = Number(kbps) || 0;
  if (value <= 0) return "unlimited";
  if (value >= 1000000) return trimNumber(value / 1000000) + " Gbps";
  if (value >= 1000) return trimNumber(value / 1000) + " Mbps";
  return trimNumber(value) + " Kbps";
}

function trimNumber(value) {
  const text = Number(value).toFixed(2);
  return text.replace(/\.?0+$/, "");
}

function duration(seconds) {
  let value = Math.max(0, Math.floor(seconds));
  const days = Math.floor(value / 86400); value -= days * 86400;
  const hours = Math.floor(value / 3600); value -= hours * 3600;
  const minutes = Math.floor(value / 60);
  if (days > 0) return days + "d " + hours + "h";
  if (hours > 0) return hours + "h " + minutes + "m";
  if (minutes > 0) return minutes + "m";
  return value + "s";
}

function when(value) {
  if (!value || value.startsWith("0001-")) return "never";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "never";
  const diff = (Date.now() - date.getTime()) / 1000;
  if (diff < 60) return "just now";
  if (diff < 3600) return Math.floor(diff / 60) + " min ago";
  if (diff < 86400) return Math.floor(diff / 3600) + " h ago";
  return date.toLocaleDateString() + " " + date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

function clockTime(value) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

function escapeHtml(value) {
  return String(value == null ? "" : value)
    .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
}

// bracketIPv6 wraps a bare IPv6 address in square brackets, the form used in
// URLs and host:port pairs. Host names, IPv4 addresses and values that already
// carry brackets are returned unchanged.
function bracketIPv6(value) {
  const text = String(value == null ? "" : value).trim();
  if (!text || text.startsWith("[")) return text;
  return text.includes(":") ? "[" + text + "]" : text;
}

// bracketHostPort does the same for a host:port pair: [address]:port.
function bracketHostPort(value) {
  const text = String(value == null ? "" : value).trim();
  if (!text) return text;
  const match = text.match(/^([^\[\]]+):(\d+)$/);
  if (!match) return bracketIPv6(text);
  const host = match[1];
  return host.includes(":") ? "[" + host + "]:" + match[2] : text;
}

function toast(message, bad) {
  const node = $("#toast");
  node.textContent = message;
  node.className = "toast" + (bad ? " bad" : "");
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => node.classList.add("hidden"), 4000);
}

async function copy(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast("Copied");
  } catch (_) {
    const area = document.createElement("textarea");
    area.value = text;
    document.body.appendChild(area);
    area.select();
    document.execCommand("copy");
    area.remove();
    toast("Copied");
  }
}

function mb(value) {
  return (Number(value) || 0) / 1000000;
}

/* ---------- theme ---------- */

function applyTheme(name) {
  const theme = ["auto", "light", "dark"].includes(name) ? name : "auto";
  document.documentElement.dataset.theme = theme;
  localStorage.setItem("x-node-theme", theme);
  $$("[data-theme-set]").forEach((button) => {
    button.classList.toggle("active", button.dataset.themeSet === theme);
  });
}

// Compact keeps the desktop layout 25% narrower. It is a server-side setting,
// because every browser that opens the interface should see the same page.
function applyCompact(enabled) {
  const app = $("#app");
  if (app) app.classList.toggle("compact", Boolean(enabled));
}

function currentCompact() {
  return Boolean(state.settings && state.settings.web && state.settings.web.compact);
}

/* ---------- auth ---------- */

function showLogin() {
  clearInterval(state.timer);
  clearInterval(state.logTimer);
  clearInterval(state.clientTimer);
  hideClientPopover();
  hideUsagePopover();
  $("#app").classList.add("hidden");
  $("#login").classList.remove("hidden");
  $("#boot").classList.add("hidden");
  setTimeout(() => $("#login-password").focus(), 50);
}

function showApp() {
  $("#login").classList.add("hidden");
  $("#app").classList.remove("hidden");
  $("#boot").classList.add("hidden");
  // The compact layout is stored on the server. Reading it here keeps the
  // first paint on every browser identical to the saved setting.
  loadSettings().then(() => applyCompact(currentCompact())).catch(() => {});
  navigate(location.hash.replace("#", "") || "dashboard");
  clearInterval(state.timer);
  state.timer = setInterval(() => {
    if (!document.hidden) refreshStatus();
  }, 5000);
}

async function checkSession() {
  try {
    const data = await api("GET", "api/session");
    if (data.authenticated) {
      state.user = data.user;
      showApp();
      return;
    }
  } catch (_) { /* not signed in */ }
  showLogin();
}

$("#login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = $("#login-submit");
  const error = $("#login-error");
  error.classList.add("hidden");
  button.disabled = true;
  try {
    const data = await api("POST", "api/login", {
      username: $("#login-user").value,
      password: $("#login-password").value,
    });
    state.user = data.user;
    $("#login-password").value = "";
    showApp();
  } catch (err) {
    error.textContent = err.message;
    error.classList.remove("hidden");
  } finally {
    button.disabled = false;
  }
});

$("#logout").addEventListener("click", async () => {
  try { await api("POST", "api/logout"); } catch (_) { /* ignore */ }
  showLogin();
});

/* ---------- navigation ---------- */

const titles = {
  dashboard: "Dashboard",
  clients: "Clients",
  invites: "Invites",
  settings: "Settings",
  config: "Configuration",
  logs: "Logs",
};

function navigate(view) {
  if (!titles[view]) view = "dashboard";
  state.view = view;
  state.editing = "";
  clearInterval(state.clientTimer);
  state.clientTimer = null;
  hideClientPopover();
  hideUsagePopover();
  $("#view-title").textContent = titles[view];
  $$("#nav a").forEach((link) => link.classList.toggle("active", link.dataset.view === view));
  $("#app").classList.remove("menu-open");
  render();
}

window.addEventListener("hashchange", () => navigate(location.hash.replace("#", "")));
$("#menu").addEventListener("click", () => $("#app").classList.toggle("menu-open"));
// A tap anywhere outside a floating card closes it; a tap on another icon
// opens that client instead, because its own handler runs first.
document.addEventListener("click", (event) => {
  const copyField = event.target.closest("[data-copy-value]");
  if (copyField) {
    copy(copyField.dataset.copyValue || "");
    return;
  }
  const popover = $("#client-popover");
  if (popover && !popover.classList.contains("hidden") &&
      !event.target.closest("#client-popover") &&
      !event.target.closest("[data-client-stats]")) {
    hideClientPopover();
  }
  const usage = $("#usage-popover");
  if (usage && !usage.classList.contains("hidden") &&
      !event.target.closest("#usage-popover") &&
      !event.target.closest("[data-usage]")) {
    hideUsagePopover();
  }
});
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape") {
    hideClientPopover();
    hideUsagePopover();
  }
});
window.addEventListener("resize", () => {
  hideClientPopover();
  hideUsagePopover();
});
$("#app").addEventListener("click", (event) => {
  // A tap on the dimmed area closes the mobile menu.
  if (!$("#app").classList.contains("menu-open")) return;
  if (event.target.closest(".sidebar") || event.target.closest("#menu")) return;
  $("#app").classList.remove("menu-open");
});
$$("[data-theme-set]").forEach((button) => {
  button.addEventListener("click", () => applyTheme(button.dataset.themeSet));
});

/* ---------- data ---------- */

// Several renders may ask for the status at once (the five-second timer runs
// while a navigation starts its own request). Only the newest answer is
// allowed to paint the health pill, so a stale timeout cannot mark a healthy
// node offline.
let statusSeq = 0;

// A request already in flight is reused instead of stacking a second one: a
// slow node would otherwise collect a new status call every five seconds.
// A call older than this is considered stuck and replaced, so a navigation
// never waits behind a request that started before the user clicked.
const statusReuseMs = 3000;
let statusPending = null;
let statusPendingAt = 0;

function refreshStatus() {
  const now = Date.now();
  if (statusPending && now - statusPendingAt < statusReuseMs) return statusPending;
  statusPendingAt = now;
  statusPending = fetchStatus().finally(() => {
    if (statusPendingAt === now) statusPending = null;
  });
  return statusPending;
}

async function fetchStatus() {
  const seq = ++statusSeq;
  let status;
  try {
    status = await api("GET", "api/status");
  } catch (err) {
    if (seq !== statusSeq) return;
    // Only the status endpoint decides whether the node is online. A broken
    // admin socket or a failed peer/traffic request must not paint it red.
    const health = $("#health");
    health.textContent = "offline";
    health.className = "pill bad";
    return;
  }
  state.status = status;
  if (seq !== statusSeq) return;
  const health = $("#health");
  health.textContent = "online · " + duration(status.uptimeSeconds);
  health.className = "pill ok";
  $("#side-name").textContent = status.name || "";
  $("#clock").textContent = new Date().toLocaleTimeString();
  if (state.view === "dashboard") {
    await Promise.allSettled([loadPeers(), loadTraffic(), loadSystem()]);
    // During the first render the dashboard is not in the DOM yet: render()
    // builds it right after this call and draws the chart itself.
    if ($("#chart")) renderDashboard();
  }
}

async function loadClients() {
  const data = await api("GET", "api/clients");
  state.clients = data.clients || [];
  return state.clients;
}

async function loadInvites() {
  const data = await api("GET", "api/invites");
  state.invites = data.invites || [];
  return state.invites;
}

async function loadSettings() {
  const data = await api("GET", "api/settings");
  state.settings = data;
  return data;
}

async function loadPeers() {
  const data = await api("GET", "api/peers");
  state.peers = data;
  return data;
}

async function loadTraffic() {
  const data = await api("GET", "api/traffic?range=" + encodeURIComponent(state.trafficRange));
  state.traffic = data;
  return data;
}

// loadSystem fetches the CPU and memory history the daemon samples from /proc.
// It is separate from the traffic history: the card shows the last 15 minutes.
async function loadSystem() {
  const data = await api("GET", "api/system");
  state.system = data;
  return data;
}

async function loadLogs() {
  const data = await api("GET", "api/logs?limit=300");
  state.logs = data.entries || [];
  return state.logs;
}

async function loadUpdate(refresh) {
  const data = await api("GET", "api/update" + (refresh ? "?refresh=1" : ""));
  state.update = data;
  return data;
}

/* ---------- rendering ---------- */

// Every render takes a number. A newer render or a navigation bumps it, so a
// slow response that arrives after the user has moved on cannot paint its
// view over the one that is on screen: that is what made the menu look stuck.
let renderSeq = 0;

function superseded(seq) {
  return seq !== renderSeq;
}

async function render() {
  const view = $("#view");
  const seq = ++renderSeq;
  // A click on the menu must never look ignored. Fast answers paint straight
  // away; a slow one first replaces the old view with a short placeholder.
  const placeholder = setTimeout(() => {
    if (!superseded(seq)) view.innerHTML = '<div class="card empty">Loading…</div>';
  }, 150);
  const paint = (html) => {
    clearTimeout(placeholder);
    if (superseded(seq)) return false;
    view.innerHTML = html;
    return true;
  };
  try {
    if (state.view === "dashboard") {
      await refreshStatus();
      if (!paint(dashboardHtml())) return;
      wireDashboard();
      drawChart();
      drawSystemCharts();
      if (state.update && state.update.running) pollUpdateUpgrade();
      // The update check talks to GitHub and can take its own time. It must
      // not hold the first paint: the card shows the last known answer and
      // the dashboard is repainted once a fresh one arrives.
      loadUpdate(false)
        .catch((err) => {
          // A failed check must not replace the whole dashboard with an error.
          state.update = { current: (state.status || {}).version || "", error: err.message };
        })
        .then(() => {
          if (superseded(seq)) return;
          renderDashboard();
          // An upgrade started from another tab keeps running: follow it.
          if (state.update && state.update.running) pollUpdateUpgrade();
        });
    } else if (state.view === "clients") {
      await Promise.all([loadClients(), loadSettings()]);
      if (!paint(clientsHtml())) return;
      wireClients();
      startClientAutoRefresh();
    } else if (state.view === "invites") {
      await Promise.all([loadInvites(), loadSettings()]);
      if (!paint(invitesHtml())) return;
      wireInvites();
    } else if (state.view === "settings") {
      // The IPv6 card shows the source address and interface from the status,
      // so a direct visit to Settings must fetch it as well.
      await Promise.all([loadSettings(), loadPeers(), refreshStatus()]);
      if (!paint(settingsHtml())) return;
      wireSettings();
    } else if (state.view === "config") {
      // The file list carries the real paths, so a direct visit to the tab
      // needs the status as well.
      await refreshStatus();
      if (!paint(configHtml())) return;
      wireConfig();
    } else if (state.view === "logs") {
      await loadLogs();
      if (!paint(logsHtml())) return;
      wireLogs();
    }
  } catch (err) {
    if (!paint('<div class="card empty">' + escapeHtml(err.message) + "</div>")) return;
  } finally {
    clearTimeout(placeholder);
  }
}

/* dashboard */

function dashboardHtml() {
  const status = state.status || {};
  const peers = state.peers || {};
  const self = peers.self || {};
  const quotas = status.quotas || {};
  const apiShared = status.apiPort && status.apiPort === status.socksPort;
  return `
    <div class="grid kpi">
      <a class="card kpi-card link-card" href="#settings" title="Open the Settings tab">
        <div class="label">Yggdrasil address</div>
        <div class="value mono small-value address-value" title="${escapeHtml(bracketIPv6(status.address || ""))}">${escapeHtml(bracketIPv6(status.address || "—"))}</div>
        <div class="sub">${escapeHtml(status.name || "")}</div>
      </a>
      <a class="card kpi-card link-card" href="#clients" title="Open the Clients tab">
        <div class="label">Clients</div>
        <div class="value">${status.clients || 0}</div>
        <div class="sub">${status.activeConnections || 0} TCP · ${status.activeUdp || 0} UDP</div>
      </a>
      <button class="card kpi-card link-card" type="button" data-usage="today" title="Traffic by client today">
        <div class="label">Today</div>
        <div class="value">${bytes((status.todayRx || 0) + (status.todayTx || 0))}</div>
        <div class="sub">↑ ${bytes(status.todayRx)} · ↓ ${bytes(status.todayTx)}</div>
      </button>
      <button class="card kpi-card link-card" type="button" data-usage="month" title="Traffic by client this month">
        <div class="label">This month</div>
        <div class="value">${bytes((status.monthRx || 0) + (status.monthTx || 0))}</div>
        <div class="sub">↑ ${bytes(status.monthRx)} · ↓ ${bytes(status.monthTx)}</div>
      </button>
    </div>

    <div class="card section">
      <header>
        <h3>Traffic</h3>
        <div class="range-switch" role="group" aria-label="Chart range">
          ${["15m", "1h", "6h", "24h"].map((value) =>
            `<button data-range="${value}" class="${state.trafficRange === value ? "active" : ""}">${value}</button>`
          ).join("")}
        </div>
        <span class="chart-legend" id="chart-legend"></span>
      </header>
      <div class="chart-wrap">
        <canvas class="chart" id="chart"></canvas>
        <div class="chart-tip hidden" id="chart-tip"></div>
      </div>
      <p class="hint" id="chart-hint">Download and upload through the exit node, in bit/s. Updated every 5 seconds.</p>
    </div>

    <div class="grid two">
      <div class="card section">
        <header><h3>X-Node</h3><span class="muted small">${escapeHtml(status.version || "")}</span></header>
        <table class="facts">
          <tr><th>Service</th><td class="right"><span class="pill ok">running</span> · ${duration(status.uptimeSeconds || 0)}</td></tr>
          <tr><th>SOCKS5</th><td class="right">
            <input class="scroll-field mono right" readonly value="${escapeHtml(bracketHostPort((status.address || "") + ":" + (status.socksPort || "")))}" title="${escapeHtml(bracketHostPort((status.address || "") + ":" + (status.socksPort || "")))}" data-copy-value="${escapeHtml((status.address || "") + ":" + (status.socksPort || ""))}">
          </td></tr>
          <tr><th>API</th><td class="right mono">${apiShared ? "shared with SOCKS5" : "port " + (status.apiPort || "")} · auth ${escapeHtml(status.apiAuth || "")}</td></tr>
          <tr><th>Registration</th><td class="right">${status.registration ? "enabled" : "disabled"}</td></tr>
          <tr><th>Open mode</th><td class="right">${status.allowAll ? '<span class="pill warn">allowAll</span>' : "allowlist"}</td></tr>
          <tr><th>Client slots</th><td class="right">${status.maxClients || 0} allowed</td></tr>
          <tr><th>Connections</th><td class="right">${status.maxConnsPerClient || 0} per client · ${status.maxConnsTotal || 0} in total</td></tr>
          <tr><th>UDP relay</th><td class="right">${status.udpEnabled ? "enabled · " + (status.maxUdpAssociations || 0) + " association(s)" : "disabled"}</td></tr>
          <tr><th>IPv6 outbound</th><td class="right">${ipv6StatePill(status)}</td></tr>
          ${status.ipv6Source ? `<tr><th>IPv6 source</th><td class="right">
            <input class="scroll-field mono right" readonly value="${escapeHtml(bracketIPv6(status.ipv6Source))}" title="${escapeHtml(bracketIPv6(status.ipv6Source))}" data-copy-value="${escapeHtml(status.ipv6Source)}">
          </td></tr>` : ""}
          <tr><th>Domain names</th><td class="right">${status.domainResolution ? "resolved on the exit" : "client resolves"}</td></tr>
          <tr><th>Log level</th><td class="right">${escapeHtml(status.logLevel || "")}</td></tr>
          <tr><th>Config</th><td class="right mono ellipsis" title="${escapeHtml(status.configPath || "")}">${escapeHtml(status.configPath || "—")}</td></tr>
          <tr><th>State</th><td class="right mono ellipsis" title="${escapeHtml(status.stateDir || "")}">${escapeHtml(status.stateDir || "—")}</td></tr>
        </table>
        <p class="hint">${ipv6StatusHint(status)}</p>
      </div>

      <div class="stack">
        <div class="card section">
          <header><h3>Yggdrasil</h3><span class="muted small">${peers.adminReachable ? "admin socket connected" : "admin socket unavailable"}</span></header>
          <table class="facts">
            <tr><th>Version</th><td class="right">${escapeHtml(self.build_version || "—")}</td></tr>
            <tr><th>Project</th><td class="right"><a class="link-wrap" href="https://github.com/yggdrasil-network/yggdrasil-go" title="yggdrasil-network/yggdrasil-go" target="_blank" rel="noopener noreferrer">yggdrasil-go</a></td></tr>
            <tr><th>Address</th><td class="right">
              <input class="scroll-field mono right" readonly value="${escapeHtml(bracketIPv6(self.address || status.address || "—"))}" title="${escapeHtml(bracketIPv6(self.address || status.address || ""))}" data-copy-value="${escapeHtml(self.address || status.address || "")}">
            </td></tr>
            <tr><th>Subnet</th><td class="right">
              <input class="scroll-field mono right" readonly value="${escapeHtml(bracketIPv6(self.subnet || "—"))}" title="${escapeHtml(bracketIPv6(self.subnet || ""))}" data-copy-value="${escapeHtml(self.subnet || "")}">
            </td></tr>
            <tr><th>Peers</th><td class="right">${peers.up || 0} up · ${peers.managed || 0} automatic · ${peers.manualPeers ? peers.manualPeers.length : 0} manual · ${peers.inbound || 0} inbound</td></tr>
            <tr><th>Auto replace</th><td class="right">${peers.autoReplace ? "on" : "off"} · ${peers.peerCount || 0} wanted</td></tr>
            <tr><th>Peer link</th><td class="right">
              <div class="copy-line">
                <input class="scroll-field mono" readonly value="${escapeHtml(status.peerURI || "")}" placeholder="not advertised" title="${escapeHtml(status.peerURI || "")}">
                ${status.peerURI ? `<button class="btn small" data-copy="${escapeHtml(status.peerURI)}">Copy</button>` : ""}
              </div>
            </td></tr>
          </table>
          <p class="hint">Add the peer link in Y-Node → Exit node → Add → Peers, or append it to the <span class="mono">Peers</span> list in yggdrasil.conf.</p>
        </div>

        ${dnsCardHtml(status.dns || {})}
      </div>
    </div>

    <div class="grid two">
      ${systemCardHtml()}
      ${updateCardHtml(status)}
    </div>

    <div class="grid two">
      <div class="card section">
        <header>
          <h3>Quotas</h3>
          <span class="muted small">${status.usageEnabled ? "accounting on · " + escapeHtml(status.usageTimezone || "") : "accounting off"}</span>
        </header>
        <table class="facts">
          <tr><th>Default monthly cap</th><td class="right">${quotas.monthlyBytes > 0 ? bytes(quotas.monthlyBytes) : "unlimited"}</td></tr>
          <tr><th>Default daily cap</th><td class="right">${quotas.dailyBytes > 0 ? bytes(quotas.dailyBytes) : "unlimited"}</td></tr>
          <tr><th>Default download</th><td class="right">${rate(quotas.downKbps)}</td></tr>
          <tr><th>Default upload</th><td class="right">${rate(quotas.upKbps)}</td></tr>
        </table>
        <p class="hint">These defaults apply to a client without its own limits. Per-client limits are on the Clients page. Traffic is measured in decimal MB, speed in Mbit/s.</p>
      </div>

      <div class="card section">
        <header><h3>Web interface</h3><span class="muted small">${escapeHtml(status.webPath || "")} · login ${escapeHtml(status.webLogin || "")}</span></header>
        <div class="actions">
          ${(status.webURLs || []).map((url) => `<button class="btn small" data-copy="${escapeHtml(url)}">${escapeHtml(url)}</button>`).join("")}
        </div>
        ${status.pendingRestart ? '<p class="error">The configuration file changed and the running service has not read it yet.</p>' : ""}
        ${status.restartSupported
          ? `<div class="actions">
               <button class="btn small" data-restart="x-node">Restart x-node</button>
               <button class="btn small" data-restart="yggdrasil">Restart yggdrasil</button>
             </div>
             <p class="hint">Restarting x-node drops the open tunnels for a moment. Restarting yggdrasil rebuilds every peering link.</p>`
          : '<p class="hint">Restarts are disabled: the root helper is not installed. Run install.sh again to enable them.</p>'}
      </div>
    </div>

    <div class="card section">
      <header>
        <h3>Yggdrasil peers</h3>
        <span class="muted small">${(peers.peers || []).length} link(s)</span>
      </header>
      ${peerTableHtml(peers.peers || [])}
    </div>
  `;
}

// systemCardHtml shows the host load next to the update card. System Info sits
// on the left and Update on the right; the grid stacks them on a phone.
function systemCardHtml() {
  const data = state.system;
  const available = !data || data.available !== false;
  return `<div class="card section">
    <header>
      <h3>System Info</h3>
      <span class="muted small">last 15 minutes</span>
    </header>
    ${available ? `
      <div class="system-chart">
        <div class="chart-label"><span>CPU</span><span id="system-cpu">—</span></div>
        <canvas class="chart mini" id="system-cpu-chart"></canvas>
      </div>
      <div class="system-chart">
        <div class="chart-label"><span>Memory</span><span id="system-mem">—</span></div>
        <canvas class="chart mini" id="system-mem-chart"></canvas>
      </div>
      <p class="hint" id="system-hint">Sampled every 5 seconds. The history starts empty after a restart.</p>
    ` : '<p class="hint">CPU and memory history is read from /proc and is available on Linux only.</p>'}
  </div>`;
}

// dnsCardHtml shows the Alfis DNS server that x-node runs for its clients. It
// sits next to the X-Node and Yggdrasil cards, so the three tables share one
// row on a desktop.
function dnsCardHtml(dns) {
  const enabled = Boolean(dns.enabled);
  const running = Boolean(dns.running);
  const syncing = Boolean(dns.syncing);
  let statePill;
  if (!enabled) {
    statePill = '<span class="pill">off</span>';
  } else if (dns.error) {
    statePill = '<span class="pill warn">unknown</span>';
  } else if (syncing) {
    const progress = dns.syncHeight > 0
      ? " " + (dns.syncPercent || 0) + "%"
      : "";
    statePill = '<span class="pill warn">syncing' + progress + "</span>";
  } else if (running) {
    statePill = '<span class="pill ok">running</span>';
  } else {
    statePill = '<span class="pill bad">stopped</span>';
  }
  let hint = "Alfis answers DNS queries for Yggdrasil clients and runs its own web UI.";
  if (!enabled) {
    hint = "The DNS server is off. Turn it on in Settings → DNS server.";
  } else if (!dns.installed) {
    hint = "Alfis is not installed on this host. Run install.sh again to add it.";
  } else if (dns.error) {
    hint = "The root helper could not read the DNS state: " + dns.error;
  } else if (dns.webUI === false) {
    hint = "This Alfis build has no web interface. Reinstall X-Node to get the bundled build with the web UI.";
  } else if (syncing) {
    hint = "Alfis is downloading the blockchain on its first start. This can take a while and keeps a CPU core busy; the resolver starts answering as soon as the sync finishes.";
  }
  // The panel is proxied through the dashboard address, so it opens on the
  // same TLS endpoint as the rest of the web interface. The button waits for
  // the first successful answer from Alfis: right after a restart the process
  // still loads the chain and refuses connections for a few minutes.
  const panel = enabled && dns.webUI !== false ? (dns.proxyPath || dns.webURL || "") : "";
  const panelAction = panel
    ? (dns.webReady
      ? `<div class="actions"><a class="btn small" href="${escapeHtml(panel)}" target="_blank" rel="noopener noreferrer">Open Alfis web UI</a></div>`
      : `<div class="actions"><button class="btn small" type="button" disabled title="The Alfis process is still starting"><span class="spinner"></span>Waiting for the Alfis web UI…</button></div>`)
    : "";
  const listen = dns.listen || "";
  return `<div class="card section">
    <header><h3>DNS server</h3><span class="muted small">Alfis</span></header>
    <table class="facts">
      <tr><th>Service</th><td class="right">${statePill}</td></tr>
      ${syncing && dns.syncHeight > 0 ? `<tr><th>Sync</th><td class="right mono">${dns.syncHave || 0} / ${dns.syncHeight} blocks</td></tr>` : ""}
      <tr><th>Listen</th><td class="right">
        <input class="scroll-field mono right" readonly value="${escapeHtml(bracketHostPort(listen || "—"))}" title="Click to copy" data-copy-value="${escapeHtml(listen)}">
      </td></tr>
      ${enabled && dns.password ? `<tr><th>Web password</th><td class="right">
        <input class="scroll-field mono right" readonly value="${escapeHtml(dns.password)}" title="Click to copy" data-copy-value="${escapeHtml(dns.password)}">
      </td></tr>` : ""}
    </table>
    ${panelAction}
    <p class="hint">${escapeHtml(hint)}</p>
  </div>`;
}

// updateCardHtml shows the release check that the daemon keeps for the
// dashboard. The upgrade itself runs through the root helper, so the button is
// offered only when the helper is installed and the release has a matching
// archive with checksums.
function updateCardHtml(status) {
  const update = state.update;
  if (!update) {
    return `<div class="card section">
      <header><h3>Update</h3></header>
      <p class="hint">The release check has not finished yet.</p>
    </div>`;
  }
  const running = Boolean(update.running);
  const checkFailed = Boolean(update.error && !update.latest);
  const latest = update.latest || "—";
  let statePill;
  if (running) {
    statePill = '<span class="spinner"></span>upgrading…';
  } else if (update.available) {
    statePill = '<span class="pill warn">update available</span>';
  } else if (checkFailed) {
    statePill = '<span class="pill bad">check failed</span>';
  } else {
    statePill = '<span class="pill ok">up to date</span>';
  }
  const canUpgrade = update.available && update.canUpgrade && update.helper && !running;
  let hint = "The updater downloads the release archive from GitHub, verifies its SHA-256 checksum and replaces the binaries. The service restarts automatically.";
  if (checkFailed) {
    hint = "The release check could not reach GitHub. Press \"Check again\" to retry.";
  } else if (update.available && !update.canUpgrade) {
    hint = "The latest release has no archive for this platform. Update from the release page on GitHub.";
  } else if (!update.helper) {
    hint = "The root helper is not installed, so the dashboard cannot install the release. Run \"sudo x-node update upgrade\" on the server.";
  }
  return `<div class="card section">
    <header>
      <h3>Update</h3>
      <span class="muted small">${update.helper ? "root helper connected" : "root helper not installed"}</span>
    </header>
    <table class="facts">
      <tr><th>Installed</th><td class="right mono">${escapeHtml(update.current || status.version || "—")}</td></tr>
      <tr><th>Latest release</th><td class="right mono">${escapeHtml(latest)}</td></tr>
      <tr><th>Status</th><td class="right">${statePill}</td></tr>
    </table>
    <div class="actions">
      <button class="btn small" data-update-check ${running ? "disabled" : ""}>Check again</button>
      ${canUpgrade ? `<button class="btn primary small" data-update-upgrade>Upgrade to ${escapeHtml(latest)}</button>` : ""}
      ${update.url ? `<a class="btn small" href="${escapeHtml(update.url)}" target="_blank" rel="noopener noreferrer">Release notes</a>` : ""}
    </div>
    <p class="hint">${escapeHtml(hint)}</p>
    ${update.error ? `<p class="error">${escapeHtml(update.error)}</p>` : ""}
  </div>`;
}

function peerTableHtml(peers) {
  if (!peers.length) {
    return '<div class="empty">No Yggdrasil links. Check the admin socket and the peer settings.</div>';
  }
  const rows = peers.map((peer) => {
    const statePill = peer.up ? '<span class="pill ok">up</span>' : '<span class="pill bad">down</span>';
    const direction = peer.inbound ? '<span class="pill warn">in</span>' : '<span class="pill">out</span>';
    // A manual peer is also kept in the managed list, so the automatic flag
    // alone would put both badges on it. The operator's peer is manual first.
    const managed = peer.managed && !peer.manual ? '<span class="pill ok">auto</span>' : "";
    const manual = peer.manual ? '<span class="pill">manual</span>' : "";
    // An inbound link is a client of the exit, not a peer the supervisor
    // manages, so it gets its own badge instead of an empty Managed cell.
    const client = peer.inbound ? '<span class="pill client">client</span>' : "";
    const label = peer.label ? ` <span class="peer-label">${escapeHtml(peer.label)}</span>` : "";
    return `
      <tr>
        <td data-label="Link"><div class="mono break">${escapeHtml(peer.uri)}</div></td>
        <td data-label="Direction">${direction}</td>
        <td data-label="State">${statePill}</td>
        <td data-label="Address"><span class="mono break">${escapeHtml(bracketIPv6(peer.address || "—"))}</span>${label}</td>
        <td class="right" data-label="RTT">${peer.latencyMs > 0 ? peer.latencyMs.toFixed(1) + " ms" : "—"}</td>
        <td class="right hide-sm" data-label="Uptime">${peer.uptimeSeconds > 0 ? duration(peer.uptimeSeconds) : "—"}</td>
        <td class="right hide-sm" data-label="Traffic">↑ ${bytes(peer.bytesRecv)} · ↓ ${bytes(peer.bytesSent)}</td>
        <td class="right" data-label="Managed">${managed}${manual}${client}</td>
      </tr>`;
  }).join("");
  return `<div class="table-wrap"><table class="cards">
    <thead><tr><th>Link</th><th>Direction</th><th>State</th><th>Address</th>
      <th class="right">RTT</th><th class="right hide-sm">Uptime</th>
      <th class="right hide-sm">Traffic</th><th class="right">Managed</th></tr></thead>
    <tbody>${rows}</tbody>
  </table></div>`;
}

function wireDashboard() {
  $$("[data-copy]").forEach((button) => {
    button.addEventListener("click", () => copy(button.dataset.copy));
  });
  $$("[data-range]").forEach((button) => {
    button.addEventListener("click", async () => {
      state.trafficRange = button.dataset.range;
      $$("[data-range]").forEach((item) => item.classList.toggle("active", item === button));
      try {
        await loadTraffic();
        drawChart();
      } catch (err) { toast(err.message, true); }
    });
  });
  $$("[data-usage]").forEach((button) => {
    button.addEventListener("click", async () => {
      try {
        await loadClients();
        showUsagePopover(button, button.dataset.usage);
      } catch (err) { toast(err.message, true); }
    });
  });
  $$("[data-restart]").forEach((button) => {
    button.addEventListener("click", async () => {
      const service = button.dataset.restart;
      if (!confirm("Restart " + service + "? Open tunnels will reconnect.")) return;
      button.disabled = true;
      try {
        await api("POST", "api/restart", { service });
        toast("Restart requested: " + service);
      } catch (err) {
        toast(err.message, true);
        button.disabled = false;
      }
    });
  });
  const updateCheck = $("[data-update-check]");
  if (updateCheck) {
    updateCheck.addEventListener("click", async () => {
      updateCheck.disabled = true;
      updateCheck.innerHTML = '<span class="spinner"></span>Checking…';
      try {
        await loadUpdate(true);
        renderDashboard();
        toast(state.update && state.update.available
          ? "Update available: " + state.update.latest
          : "x-node is up to date");
      } catch (err) {
        toast(err.message, true);
        renderDashboard();
      }
    });
  }
  const updateUpgrade = $("[data-update-upgrade]");
  if (updateUpgrade) {
    updateUpgrade.addEventListener("click", async () => {
      const latest = (state.update && state.update.latest) || "the latest release";
      if (!confirm("Upgrade x-node to " + latest + "? The service restarts and open tunnels reconnect.")) return;
      updateUpgrade.disabled = true;
      updateUpgrade.innerHTML = '<span class="spinner"></span>Starting…';
      try {
        await api("POST", "api/update");
        state.update.running = true;
        renderDashboard();
        toast("Upgrade started");
        pollUpdateUpgrade();
      } catch (err) {
        toast(err.message, true);
        renderDashboard();
      }
    });
  }
}

// pollUpdateUpgrade waits for the helper to replace the binaries and the
// service to come back. Requests fail while the daemon restarts, so a failed
// poll is expected and simply retried.
async function pollUpdateUpgrade() {
  if (state.updatePolling) return;
  state.updatePolling = true;
  try {
    for (let attempt = 0; attempt < 120; attempt += 1) {
      await new Promise((resolve) => setTimeout(resolve, 3000));
      try {
        await loadUpdate(false);
      } catch (_) {
        continue;
      }
      if (!state.update || !state.update.running) break;
    }
    if (state.view !== "dashboard") return;
    renderDashboard();
    if (state.update && state.update.error) {
      toast("Upgrade failed: " + state.update.error, true);
    } else if (state.update && !state.update.available) {
      toast("x-node updated to " + state.update.current);
    }
  } finally {
    state.updatePolling = false;
  }
}

function renderDashboard() {
  if (state.view !== "dashboard") return;
  const view = $("#view");
  if (!view || state.editing) return;
  view.innerHTML = dashboardHtml();
  wireDashboard();
  drawChart();
  drawSystemCharts();
}

let chartGeometry = null;

function drawChart() {
  const canvas = $("#chart");
  const data = state.traffic;
  if (!canvas || !data) return;
  const points = data.points || [];
  const interval = Math.max(1, parseInt(String(data.interval || "5s").replace("s", ""), 10));
  const legend = $("#chart-legend");
  const hint = $("#chart-hint");
  const last = points.length ? points[points.length - 1] : null;
  if (legend) {
    const down = last ? bitrate((last.tx / interval) * 8) : "—";
    const up = last ? bitrate((last.rx / interval) * 8) : "—";
    legend.innerHTML =
      '<span class="legend-item"><i class="legend-swatch in"></i>In (download) ' + down + "</span>" +
      '<span class="legend-item"><i class="legend-swatch out"></i>Out (upload) ' + up + "</span>";
  }
  if (hint) {
    let freshness = "";
    const generated = data.generatedAt ? new Date(data.generatedAt) : null;
    if (generated && !Number.isNaN(generated.getTime())) {
      const age = Math.max(0, Math.round((Date.now() - generated.getTime()) / 1000));
      freshness = " · data " + (age < 2 ? "just now" : age + " s old");
    }
    hint.textContent = (points.length < 3
      ? "Collecting samples: the chart fills up within a few minutes."
      : "Download and upload through the exit node, in bit/s.") + freshness;
  }

  const ratio = window.devicePixelRatio || 1;
  const width = canvas.clientWidth || 600;
  const height = canvas.clientHeight || 200;
  canvas.width = width * ratio;
  canvas.height = height * ratio;
  const ctx = canvas.getContext("2d");
  ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
  ctx.clearRect(0, 0, width, height);

  const style = getComputedStyle(document.documentElement);
  const border = style.getPropertyValue("--border").trim() || "#ddd";
  const accent = style.getPropertyValue("--accent").trim() || "#3d5afe";
  const ok = style.getPropertyValue("--ok").trim() || "#12805c";
  const muted = style.getPropertyValue("--muted").trim() || "#888";

  const padLeft = 58;
  const padBottom = 20;
  const padTop = 8;
  const plotW = width - padLeft;
  const plotH = height - padBottom - padTop;

  ctx.strokeStyle = border;
  ctx.lineWidth = 1;
  for (let i = 0; i <= 3; i += 1) {
    const y = padTop + (plotH / 3) * i;
    ctx.beginPath();
    ctx.moveTo(padLeft, y);
    ctx.lineTo(width, y);
    ctx.stroke();
  }

  if (points.length < 2) {
    chartGeometry = null;
    ctx.fillStyle = muted;
    ctx.font = "13px sans-serif";
    ctx.fillText("Collecting samples…", padLeft + 8, padTop + 24);
    return;
  }

  // Everything on the chart is in bits per second: a point stores bytes in its
  // bucket, so dividing by the bucket length and multiplying by 8 converts it.
  let max = 1;
  points.forEach((point) => {
    max = Math.max(max, (point.rx / interval) * 8, (point.tx / interval) * 8);
  });
  max *= 1.15;

  const xAt = (index) => padLeft + (index / (points.length - 1)) * plotW;
  const yAt = (value) => padTop + plotH - (value / max) * plotH;

  const series = [
    { key: "tx", color: accent, fill: true },
    { key: "rx", color: ok, fill: false },
  ];
  series.forEach((line) => {
    ctx.beginPath();
    points.forEach((point, index) => {
      const x = xAt(index);
      const y = yAt(((point[line.key] || 0) / interval) * 8);
      if (index === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
    });
    if (line.fill) {
      ctx.lineTo(xAt(points.length - 1), padTop + plotH);
      ctx.lineTo(xAt(0), padTop + plotH);
      ctx.closePath();
      ctx.fillStyle = line.color + "22";
      ctx.fill();
    }
    ctx.strokeStyle = line.color;
    ctx.lineWidth = 2;
    ctx.stroke();
  });

  ctx.fillStyle = muted;
  ctx.font = "11px sans-serif";
  ctx.fillText(bitrate(max), 4, padTop + 10);
  ctx.fillText("0", 4, padTop + plotH);

  const labels = 5;
  for (let i = 0; i < labels; i += 1) {
    const index = Math.round((i / (labels - 1)) * (points.length - 1));
    const x = xAt(index);
    const text = clockTime(points[index].at);
    const textWidth = ctx.measureText(text).width;
    const left = i === 0 ? x : i === labels - 1 ? x - textWidth : x - textWidth / 2;
    ctx.fillText(text, left, height - 4);
  }

  chartGeometry = { points, interval, xAt, yAt, padLeft, padTop, plotW, plotH, width, height };
  wireChartTooltip(canvas);
}

// wireChartTooltip shows the exact time and both rates under the pointer. The
// listener is assigned, not added, so a redraw does not stack handlers.
function wireChartTooltip(canvas) {
  const tip = $("#chart-tip");
  if (!tip) return;
  const show = (clientX, clientY) => {
    const geometry = chartGeometry;
    if (!geometry) return;
    const rect = canvas.getBoundingClientRect();
    const x = clientX - rect.left;
    if (x < geometry.padLeft || x > geometry.width) {
      tip.classList.add("hidden");
      return;
    }
    const relative = (x - geometry.padLeft) / geometry.plotW;
    const index = Math.max(0, Math.min(geometry.points.length - 1,
      Math.round(relative * (geometry.points.length - 1))));
    const point = geometry.points[index];
    const down = ((point.tx || 0) / geometry.interval) * 8;
    const up = ((point.rx || 0) / geometry.interval) * 8;
    tip.innerHTML = `<b>${clockTime(point.at)}</b><br>↓ ${bitrate(down)}<br>↑ ${bitrate(up)}`;
    const wrapRect = tip.parentElement.getBoundingClientRect();
    tip.style.left = Math.min(Math.max(8, clientX - wrapRect.left + 12), wrapRect.width - 150) + "px";
    tip.style.top = Math.max(8, clientY - wrapRect.top - 54) + "px";
    tip.classList.remove("hidden");
  };
  canvas.onmousemove = (event) => show(event.clientX, event.clientY);
  canvas.onmouseleave = () => tip.classList.add("hidden");
  canvas.ontouchstart = (event) => {
    if (event.touches.length) show(event.touches[0].clientX, event.touches[0].clientY);
  };
  canvas.ontouchmove = (event) => {
    if (event.touches.length) {
      event.preventDefault();
      show(event.touches[0].clientX, event.touches[0].clientY);
    }
  };
  canvas.ontouchend = () => tip.classList.add("hidden");
}

// drawSystemCharts draws the CPU and memory history of the System Info card.
// Both charts share the 0–100% scale, so the two lines can be compared at a
// glance; the value next to each title is the current reading.
function drawSystemCharts() {
  const data = state.system;
  if (!data || data.available === false) return;
  const points = data.points || [];
  const last = points.length ? points[points.length - 1] : null;
  const cpu = Number(data.cpu) || (last ? Number(last.cpu) || 0 : 0);
  const mem = Number(data.mem) || (last ? Number(last.mem) || 0 : 0);
  const cpuNode = $("#system-cpu");
  if (cpuNode) cpuNode.textContent = percent(cpu);
  const memNode = $("#system-mem");
  if (memNode) {
    const detail = data.memTotal ? " · " + bytes(data.memUsed) + " of " + bytes(data.memTotal) : "";
    memNode.textContent = percent(mem) + detail;
  }
  const hint = $("#system-hint");
  if (hint) {
    const generated = last ? new Date(last.at) : null;
    const age = generated && !Number.isNaN(generated.getTime())
      ? Math.max(0, Math.round((Date.now() - generated.getTime()) / 1000))
      : null;
    hint.textContent = points.length < 3
      ? "Collecting samples: the chart fills up within a few minutes."
      : "Sampled every 5 seconds." + (age == null ? "" : " Last sample " + (age < 2 ? "just now" : age + " s ago") + ".");
  }
  const style = getComputedStyle(document.documentElement);
  const accent = style.getPropertyValue("--accent").trim() || "#3d5afe";
  const ok = style.getPropertyValue("--ok").trim() || "#12805c";
  drawPercentChart($("#system-cpu-chart"), points, "cpu", accent);
  drawPercentChart($("#system-mem-chart"), points, "mem", ok);
}

function percent(value) {
  return (Math.max(0, Math.min(100, Number(value) || 0))).toFixed(0) + " %";
}

function drawPercentChart(canvas, points, key, color) {
  if (!canvas) return;
  const ratio = window.devicePixelRatio || 1;
  const width = canvas.clientWidth || 320;
  const height = canvas.clientHeight || 110;
  canvas.width = width * ratio;
  canvas.height = height * ratio;
  const ctx = canvas.getContext("2d");
  ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
  ctx.clearRect(0, 0, width, height);

  const style = getComputedStyle(document.documentElement);
  const border = style.getPropertyValue("--border").trim() || "#ddd";
  const muted = style.getPropertyValue("--muted").trim() || "#888";
  const padLeft = 34;
  const padBottom = 18;
  const padTop = 6;
  const plotW = width - padLeft;
  const plotH = height - padBottom - padTop;

  ctx.strokeStyle = border;
  ctx.lineWidth = 1;
  for (let i = 0; i <= 2; i += 1) {
    const y = padTop + (plotH / 2) * i;
    ctx.beginPath();
    ctx.moveTo(padLeft, y);
    ctx.lineTo(width, y);
    ctx.stroke();
  }
  ctx.fillStyle = muted;
  ctx.font = "10px sans-serif";
  ctx.fillText("100", 2, padTop + 4);
  ctx.fillText("50", 8, padTop + plotH / 2 + 4);
  ctx.fillText("0", 20, padTop + plotH + 4);

  if (points.length < 2) {
    ctx.fillStyle = muted;
    ctx.font = "12px sans-serif";
    ctx.fillText("Collecting samples…", padLeft + 8, padTop + 20);
    return;
  }

  const xAt = (index) => padLeft + (index / (points.length - 1)) * plotW;
  const yAt = (value) => padTop + plotH - (Math.max(0, Math.min(100, value)) / 100) * plotH;

  ctx.beginPath();
  points.forEach((point, index) => {
    const x = xAt(index);
    const y = yAt(Number(point[key]) || 0);
    if (index === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
  });
  ctx.lineTo(xAt(points.length - 1), padTop + plotH);
  ctx.lineTo(xAt(0), padTop + plotH);
  ctx.closePath();
  ctx.fillStyle = color + "22";
  ctx.fill();

  ctx.beginPath();
  points.forEach((point, index) => {
    const x = xAt(index);
    const y = yAt(Number(point[key]) || 0);
    if (index === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
  });
  ctx.strokeStyle = color;
  ctx.lineWidth = 1.5;
  ctx.stroke();

  ctx.fillStyle = muted;
  ctx.font = "10px sans-serif";
  ctx.fillText(clockTime(points[0].at), padLeft, height - 3);
  const end = clockTime(points[points.length - 1].at);
  ctx.fillText(end, width - ctx.measureText(end).width, height - 3);
}

window.addEventListener("resize", () => {
  if (state.view === "dashboard") {
    drawChart();
    drawSystemCharts();
  }
});

/* clients */

function clientsHtml() {
  const rows = state.clients.map((client) => clientRowHtml(client)).join("");
  const defaults = (state.settings && state.settings.quotas) || {};
  return `
    <div class="card section">
      <header>
        <h3>Registered clients</h3>
        <span class="muted small" id="clients-count">${state.clients.length} total</span>
      </header>
      ${state.clients.length ? `<div class="table-wrap"><table class="cards">
        <colgroup>
          <col class="col-client"><col class="col-state">
          <col class="col-num"><col class="col-num">
          <col class="col-traffic"><col class="col-traffic">
          <col class="col-quota"><col class="col-seen"><col class="col-actions">
        </colgroup>
        <thead><tr>
          <th>Client</th><th>State</th><th class="right">TCP</th><th class="right">UDP</th>
          <th class="right">Today</th><th class="right">Month</th><th>Quota</th>
          <th class="right hide-sm">Last seen</th><th></th>
        </tr></thead>
        <tbody id="clients-body">${rows}</tbody>
      </table></div>` : '<div class="empty" id="clients-empty">No clients yet. Create an invite or add an address.</div>'}
      <div id="client-editor"></div>
      <div id="client-link"></div>
    </div>

    <div class="card section">
      <header><h3>Add a client by address</h3></header>
      <p class="hint">The address is the Yggdrasil node address of the device. Prefer an invite: it also issues an API token.</p>
      <form id="add-client" class="row">
        <input class="grow" id="add-address" placeholder="201:1ba8:15c:b1be:7555:b3d7:7651:8e2e" required>
        <input id="add-label" placeholder="label (optional)">
        <button class="btn primary" type="submit">Add</button>
      </form>
    </div>

    <div class="card section">
      <header><h3>Default quota</h3></header>
      <p class="hint">Used for every client without its own limits. Zero means no limit.</p>
      <form id="quota-defaults" class="form-grid">
        <label class="field"><span>Monthly cap (MB)</span><input id="def-monthly" type="number" min="0" step="1" value="${Math.round(mb(defaults.monthlyBytes))}"></label>
        <label class="field"><span>Daily cap (MB)</span><input id="def-daily" type="number" min="0" step="1" value="${Math.round(mb(defaults.dailyBytes))}"></label>
        <label class="field"><span>Download (Mbps)</span><input id="def-down" type="number" min="0" step="1" value="${(defaults.downKbps || 0) / 1000}"></label>
        <label class="field"><span>Upload (Mbps)</span><input id="def-up" type="number" min="0" step="1" value="${(defaults.upKbps || 0) / 1000}"></label>
        <div class="full"><button class="btn primary" type="submit">Save defaults</button></div>
      </form>
    </div>
  `;
}

// An info icon drawn as SVG: the text glyph ⓘ has no hinting in most fonts and
// looks blurry next to the monospace address.
const INFO_ICON =
  '<svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true" focusable="false">' +
  '<circle cx="8" cy="8" r="6.6" fill="none" stroke="currentColor" stroke-width="1.4"/>' +
  '<circle cx="8" cy="4.9" r="1" fill="currentColor"/>' +
  '<path d="M8 7.3v4.1" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/>' +
  "</svg>";

function clientRowHtml(client) {
  const quota = client.quota || {};
  const used = quota.monthlyBytes > 0 ? Math.min(100, (quota.monthUsed / quota.monthlyBytes) * 100) : 0;
  const barClass = used >= 95 ? "bar bad" : used >= 75 ? "bar warn" : "bar";
  // The row itself keeps only what identifies the client. Everything that
  // changes all the time (datagrams, destinations, refusals) goes into the
  // popover behind the icon, so the table stays readable.
  const meta = [
    client.deviceName ? "device " + client.deviceName : "",
    client.appVersion ? "app " + client.appVersion : "",
    client.registeredAt ? "registered " + when(client.registeredAt) : "",
  ].filter(Boolean).join(" · ");
  const quotaPill = quota.exhausted ? '<span class="pill bad">quota exhausted</span>' : "";
  const suspendedPill = client.suspended ? '<span class="pill warn">suspended</span>' : "";
  const stateClass = client.suspended ? "warn" : client.state === "connected" ? "ok" : "";
  // The label and the state pill sit on the second line together with the
  // device details: the address and the info icon stay alone on the first
  // line, so a long label never pushes the address out of its column.
  const tags = [
    client.label ? `<span class="pill">${escapeHtml(client.label)}</span>` : "",
    suspendedPill,
  ].filter(Boolean).join("");
  return `
    <tr>
      <td data-label="Client">
        <div class="client-line">
          <span class="mono break">${escapeHtml(bracketIPv6(client.address))}</span>
          <button class="icon-btn stats-btn" type="button" data-client-stats="${escapeHtml(client.address)}" title="Traffic details" aria-label="Traffic details">${INFO_ICON}</button>
        </div>
        ${tags || meta ? `<div class="client-meta">${tags}${meta ? `<span class="muted small">${escapeHtml(meta)}</span>` : ""}</div>` : ""}
      </td>
      <td data-label="State"><span class="pill ${stateClass}">${escapeHtml(client.state)}</span></td>
      <td class="right" data-label="TCP">${client.connections || 0}</td>
      <td class="right" data-label="UDP">${client.udpAssociations || 0}</td>
      <td class="right" data-label="Today">${bytes(client.dayUsed || 0)}</td>
      <td class="right" data-label="Month">${bytes(client.monthUsed || 0)}</td>
      <td data-label="Quota">
        <div class="${barClass}"><i style="width:${used}%"></i></div>
        <div class="muted small">${quota.monthlyBytes > 0 ? bytes(quota.monthUsed) + " of " + bytes(quota.monthlyBytes) : "no cap"}</div>
        <div class="muted small">${quota.dailyBytes > 0 ? "today " + bytes(quota.dayUsed) + " of " + bytes(quota.dailyBytes) : ""}</div>
        <div class="muted small">${rate(quota.downKbps)} down · ${rate(quota.upKbps)} up</div>
        ${quotaPill}
        ${quota.reason ? `<div class="muted small">${escapeHtml(quota.reason)}</div>` : ""}
      </td>
      <td class="right hide-sm" data-label="Last seen">${when(client.lastSeenAt)}</td>
      <td class="right nowrap row-actions" data-label="">
        <div class="action-grid">
          <button class="btn small" data-edit="${escapeHtml(client.address)}">Edit</button>
          <button class="btn small" data-link="${escapeHtml(client.address)}" title="Show the QR code and the link that add this exit to the client">QR / link</button>
          <button class="btn small" data-token="${escapeHtml(client.address)}">New token</button>
          <button class="btn small" data-disconnect="${escapeHtml(client.address)}" title="Close all TCP and UDP connections of this client">Disconnect all</button>
          <button class="btn small ${client.suspended ? "primary" : ""}" data-suspend="${escapeHtml(client.address)}" data-suspended="${client.suspended ? "1" : "0"}">${client.suspended ? "Resume" : "Suspend"}</button>
          <button class="btn small danger" data-remove="${escapeHtml(client.address)}">Remove</button>
        </div>
      </td>
    </tr>`;
}

// clientDetailsHtml is the content of the floating client card: the values that
// change all the time and would make the table unreadable.
function clientDetailsHtml(client) {
  const rows = [
    ["Device", client.deviceName || "—"],
    ["App version", client.appVersion || "—"],
    ["Registered", client.registeredAt ? when(client.registeredAt) : "—"],
    ["Last seen", client.lastSeenAt ? when(client.lastSeenAt) : "—"],
    ["Added via", client.source || "—"],
    ["TCP now", String(client.connections || 0)],
    ["UDP now", String(client.udpAssociations || 0)],
    [
      "UDP total",
      (client.udpDatagramsIn || client.udpDatagramsOut)
        ? (client.udpDatagramsIn || 0) + " in / " + (client.udpDatagramsOut || 0) + " out · "
          + bytes((client.udpBytesIn || 0) + (client.udpBytesOut || 0))
        : "—",
    ],
    ["Refusals", client.refused
      ? client.refused + " (" + (client.quotaRefused || 0) + " by quota)"
      : "none"],
    ["Suspended", client.suspended ? "yes — the operator blocked this client" : "no"],
  ];
  const destinations = (client.destinations || []).length
    ? `<div class="popover-dest"><span class="muted">Destinations</span><div class="mono small">${client.destinations.map(escapeHtml).join("<br>")}</div></div>`
    : "";
  return `
    <header>
      <div>
        <h3>${escapeHtml(client.label || client.deviceName || "Client")}</h3>
        <div class="muted small mono break">${escapeHtml(bracketIPv6(client.address))}</div>
      </div>
      <button class="icon-btn" type="button" data-popover-close aria-label="Close">×</button>
    </header>
    <table class="facts popover-facts">
      ${rows.map(([name, value]) => `<tr><th>${escapeHtml(name)}</th><td class="right">${escapeHtml(value)}</td></tr>`).join("")}
    </table>
    ${destinations}`;
}

// positionPopover puts a floating card next to the control that opened it and
// keeps it inside the window. The card is measured while invisible, so the
// first paint never jumps.
function positionPopover(popover, button) {
  popover.style.visibility = "hidden";
  const rect = button.getBoundingClientRect();
  const width = popover.offsetWidth;
  const height = popover.offsetHeight;
  const margin = 8;
  let left = rect.right - width;
  left = Math.min(Math.max(margin, left), window.innerWidth - width - margin);
  let top = rect.bottom + margin;
  if (top + height > window.innerHeight - margin) {
    top = Math.max(margin, rect.top - height - margin);
  }
  popover.style.left = Math.round(left) + "px";
  popover.style.top = Math.round(top) + "px";
  popover.style.visibility = "";
}

// showClientPopover opens one floating card next to the icon. It is a separate
// element instead of a row that grows, so opening it does not move the table.
function showClientPopover(button, client) {
  const popover = $("#client-popover");
  if (!popover) return;
  hideUsagePopover();
  popover.innerHTML = clientDetailsHtml(client);
  popover.classList.remove("hidden");
  positionPopover(popover, button);
  popover.querySelector("[data-popover-close]").addEventListener("click", hideClientPopover);
  popover.querySelector("[data-popover-close]").focus({ preventScroll: true });
}

function hideClientPopover() {
  const popover = $("#client-popover");
  if (popover) popover.classList.add("hidden");
}

// usageHtml is the content of the Today / This month card: how the traffic of
// the whole exit is split between the clients. The figures are the same ones
// the Clients table shows, so the two views never disagree.
function usageHtml(period) {
  const monthly = period === "month";
  const key = monthly ? "monthUsed" : "dayUsed";
  const title = monthly ? "This month by client" : "Today by client";
  const clients = state.clients
    .slice()
    .sort((a, b) => (b[key] || 0) - (a[key] || 0));
  const total = clients.reduce((sum, client) => sum + (client[key] || 0), 0);
  const rows = clients.map((client) => {
    const used = client[key] || 0;
    const share = total > 0 ? (used / total) * 100 : 0;
    const name = client.label || client.deviceName || client.address;
    return `
      <div class="usage-row">
        <div class="usage-line">
          <span class="usage-name">${escapeHtml(name)}</span>
          <span class="muted small nowrap">${bytes(used)} · ${share.toFixed(share >= 10 ? 0 : 1)}%</span>
        </div>
        <div class="usage-address mono small">${escapeHtml(bracketIPv6(client.address))}</div>
        <div class="bar"><i style="width:${share}%"></i></div>
      </div>`;
  }).join("");
  return `
    <header>
      <div>
        <h3>${title}</h3>
        <div class="muted small">${bytes(total)} in total · ${clients.length} client(s)</div>
      </div>
      <button class="icon-btn" type="button" data-usage-close aria-label="Close">×</button>
    </header>
    ${clients.length ? `<div class="usage-list">${rows}</div>` : '<p class="hint">No clients yet.</p>'}`;
}

function showUsagePopover(button, period) {
  const popover = $("#usage-popover");
  if (!popover) return;
  hideClientPopover();
  popover.innerHTML = usageHtml(period);
  popover.classList.remove("hidden");
  positionPopover(popover, button);
  popover.querySelector("[data-usage-close]").addEventListener("click", hideUsagePopover);
  popover.querySelector("[data-usage-close]").focus({ preventScroll: true });
}

function hideUsagePopover() {
  const popover = $("#usage-popover");
  if (popover) popover.classList.add("hidden");
}

// wireClientRows attaches the actions of the table rows. It is called again
// after the automatic refresh replaces the rows, so the handlers use onclick
// instead of addEventListener and never stack.
function wireClientRows() {
  $$("[data-client-stats]").forEach((button) => {
    button.onclick = (event) => {
      event.stopPropagation();
      const client = state.clients.find((item) => item.address === button.dataset.clientStats);
      if (client) showClientPopover(button, client);
    };
  });

  $$("[data-edit]").forEach((button) => {
    button.onclick = () => openClientEditor(button.dataset.edit);
  });

  $$("[data-remove]").forEach((button) => {
    button.onclick = async () => {
      if (!confirm("Remove " + button.dataset.remove + " from the allowlist?")) return;
      try {
        await api("DELETE", "api/clients/" + encodeURIComponent(button.dataset.remove));
        toast("Client removed");
        render();
      } catch (err) { toast(err.message, true); }
    };
  });

  $$("[data-link]").forEach((button) => {
    button.onclick = async () => {
      const address = button.dataset.link;
      try {
        const data = await api("GET", "api/clients/" + encodeURIComponent(address) + "/link");
        const view = $("#client-link");
        if (!view) return;
        view.innerHTML = clientLinkPanel(data);
        wireClientLinkActions(view);
        view.scrollIntoView({ behavior: "smooth", block: "nearest" });
      } catch (err) { toast(err.message, true); }
    };
  });

  $$("[data-token]").forEach((button) => {
    button.onclick = async () => {
      if (!confirm("Issue a new API token for " + button.dataset.token + "? The previous token stops working.")) return;
      try {
        const data = await api("POST", "api/clients/" + encodeURIComponent(button.dataset.token) + "/token");
        prompt("New API token (shown once):", data.token);
      } catch (err) { toast(err.message, true); }
    };
  });

  $$("[data-disconnect]").forEach((button) => {
    button.onclick = async () => {
      const address = button.dataset.disconnect;
      button.disabled = true;
      try {
        const data = await api("POST", "api/clients/" + encodeURIComponent(address) + "/disconnect");
        toast(data.closed
          ? "Closed " + data.closed + " connection(s)"
          : "This client has no open connections");
        await refreshClientRows();
      } catch (err) {
        toast(err.message, true);
        button.disabled = false;
      }
    };
  });

  $$("[data-suspend]").forEach((button) => {
    button.onclick = async () => {
      const address = button.dataset.suspend;
      const suspend = button.dataset.suspended !== "1";
      if (suspend && !confirm("Suspend " + address +
          "?\n\nOpen connections are closed and the client cannot reconnect until you resume it. " +
          "Its quota, counters and token are kept.")) {
        return;
      }
      button.disabled = true;
      try {
        const data = await api("POST", "api/clients/" + encodeURIComponent(address) + "/suspend",
          { suspended: suspend });
        toast(suspend
          ? "Client suspended" + (data.closed ? " · " + data.closed + " connection(s) closed" : "")
          : "Client resumed");
        await refreshClientRows();
      } catch (err) {
        toast(err.message, true);
        button.disabled = false;
      }
    };
  });
}

// startClientAutoRefresh keeps the Clients tab live. The table is rebuilt from
// a fresh API answer, while the editor and any floating card stay untouched.
function startClientAutoRefresh() {
  clearInterval(state.clientTimer);
  state.clientTimer = setInterval(() => {
    if (!document.hidden) refreshClientRows();
  }, 5000);
}

async function refreshClientRows() {
  if (state.view !== "clients" || state.clientRefreshing) return;
  state.clientRefreshing = true;
  try {
    await loadClients();
    const body = $("#clients-body");
    if (!body || !state.clients.length) {
      // The empty state and the table are different elements: switch between
      // them with a full render instead of leaving a stale message behind.
      if (!state.editing) render();
      return;
    }
    body.innerHTML = state.clients.map((client) => clientRowHtml(client)).join("");
    const count = $("#clients-count");
    if (count) count.textContent = state.clients.length + " total";
    wireClientRows();
  } catch (_) {
    // A transient API error is not worth a toast every five seconds.
  } finally {
    state.clientRefreshing = false;
  }
}

function wireClients() {
  $("#add-client").addEventListener("submit", async (event) => {
    event.preventDefault();
    try {
      await api("POST", "api/clients", {
        address: $("#add-address").value.trim(),
        label: $("#add-label").value.trim(),
      });
      toast("Client added");
      render();
    } catch (err) { toast(err.message, true); }
  });

  $("#quota-defaults").addEventListener("submit", async (event) => {
    event.preventDefault();
    try {
      await api("PUT", "api/settings", {
        quotas: {
          monthlyBytes: Math.round(Number($("#def-monthly").value) * 1000000),
          dailyBytes: Math.round(Number($("#def-daily").value) * 1000000),
          downKbps: Math.round(Number($("#def-down").value) * 1000),
          upKbps: Math.round(Number($("#def-up").value) * 1000),
        },
      });
      toast("Defaults saved");
      await loadSettings();
      render();
    } catch (err) { toast(err.message, true); }
  });

  wireClientRows();
}

function openClientEditor(address) {
  const client = state.clients.find((item) => item.address === address);
  if (!client) return;
  state.editing = address;
  const quota = client.quota || {};
  const editor = $("#client-editor");
  editor.innerHTML = `
    <form class="card section editor" id="client-form">
      <header><h3>${escapeHtml(bracketIPv6(client.address))}</h3></header>
      <div class="form-grid">
        <label class="field"><span>Label</span><input id="e-label" value="${escapeHtml(client.label || "")}" placeholder="family phone"></label>
        <label class="field"><span>Monthly cap (MB, 0 = none)</span><input id="e-monthly" type="number" min="0" step="1" value="${Math.round(mb(quota.monthlyBytes))}"></label>
        <label class="field"><span>Daily cap (MB, 0 = none)</span><input id="e-daily" type="number" min="0" step="1" value="${Math.round(mb(quota.dailyBytes))}"></label>
        <label class="field"><span>Download (Mbps, 0 = none)</span><input id="e-down" type="number" min="0" step="1" value="${(quota.downKbps || 0) / 1000}"></label>
        <label class="field"><span>Upload (Mbps, 0 = none)</span><input id="e-up" type="number" min="0" step="1" value="${(quota.upKbps || 0) / 1000}"></label>
        <div class="field"><span>Traffic</span><div class="muted">Today ${bytes(client.dayUsed || 0)} · month ${bytes(client.monthUsed || 0)}</div></div>
      </div>
      <div class="actions">
        <button class="btn primary" type="submit">Save</button>
        <button class="btn" type="button" id="e-reset">Reset traffic</button>
        <button class="btn" type="button" id="e-cancel">Cancel</button>
      </div>
    </form>`;
  editor.scrollIntoView({ behavior: "smooth", block: "nearest" });
  $("#e-cancel").addEventListener("click", () => {
    state.editing = "";
    editor.innerHTML = "";
  });
  $("#e-reset").addEventListener("click", async () => {
    if (!confirm("Reset the quota and the traffic counters of " + client.address + "?")) return;
    try {
      await api("DELETE", "api/quotas/" + encodeURIComponent(client.address));
      toast("Traffic reset");
      state.editing = "";
      render();
    } catch (err) { toast(err.message, true); }
  });
  $("#client-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    try {
      await api("PUT", "api/clients/" + encodeURIComponent(client.address), {
        label: $("#e-label").value.trim(),
      });
      await api("PUT", "api/quotas/" + encodeURIComponent(client.address), {
        monthlyBytes: Math.round(Number($("#e-monthly").value) * 1000000),
        dailyBytes: Math.round(Number($("#e-daily").value) * 1000000),
        downKbps: Math.round(Number($("#e-down").value) * 1000),
        upKbps: Math.round(Number($("#e-up").value) * 1000),
      });
      toast("Client saved");
      state.editing = "";
      render();
    } catch (err) { toast(err.message, true); }
  });
}

// clientLinkPanel is the QR code and the link for a client that is already in
// the allowlist. It looks like the invite panel, but the link has no invite:
// the exit recognises the device by its Yggdrasil address.
function clientLinkPanel(data) {
  return `
    <div class="card section editor">
      <header>
        <h3>${escapeHtml(data.label || "Client")}</h3>
        <span class="muted small mono break">${escapeHtml(bracketIPv6(data.address || ""))}</span>
        <button class="btn small" type="button" data-link-close>Close</button>
      </header>
      <div class="invite-flex">
        ${data.qr ? `<div class="qr"><img src="${escapeHtml(data.qr)}" alt="Exit node QR code"></div>` : ""}
        <div class="invite-text">
          <div class="mono break">${escapeHtml(data.link || "")}</div>
          <div class="actions">
            <button class="btn primary small" data-link-copy="${escapeHtml(data.link || "")}">Copy link</button>
          </div>
          <p class="hint">Scan the QR in Y-Node → Exit node → Add, or open the link on the phone. The address is already allowed, so no invite is used: the app fetches an API token from the exit on its own.</p>
        </div>
      </div>
    </div>`;
}

function wireClientLinkActions(root) {
  $$("[data-link-copy]", root).forEach((button) => {
    button.addEventListener("click", () => copy(button.dataset.linkCopy));
  });
  $$("[data-link-close]", root).forEach((button) => {
    button.addEventListener("click", () => {
      const panel = button.closest(".editor");
      if (panel) panel.remove();
    });
  });
}

/* invites */

// invitesTableHtml renders the Active invites table. It is a separate function
// because creating an invite reloads the list in place: the page used to show
// the old table until a full refresh.
function invitesTableHtml() {
  if (!state.invites.length) return '<div class="empty">No active invites.</div>';
  const rows = state.invites.map((invite) => `
    <tr>
      <td class="mono" data-label="ID">${escapeHtml(invite.hash.slice(0, 12))}…</td>
      <td data-label="Note">${escapeHtml(invite.note || "—")}</td>
      <td class="right" data-label="Devices">${invite.used} / ${invite.uses}</td>
      <td class="right" data-label="Expires">${new Date(invite.expiresAt).toLocaleString()}</td>
      <td class="right nowrap row-actions" data-label="">
        ${invite.link ? `<button class="btn small" data-qr="${escapeHtml(invite.hash)}">QR</button>
        <button class="btn small" data-invite-copy="${escapeHtml(invite.link)}">Copy link</button>` : ""}
        <button class="btn small danger" data-invite="${escapeHtml(invite.hash)}">Delete</button>
      </td>
    </tr>`).join("");
  return `<div class="table-wrap"><table class="cards">
    <thead><tr><th>ID</th><th>Note</th><th class="right">Devices</th><th class="right">Expires</th><th></th></tr></thead>
    <tbody>${rows}</tbody>
  </table></div>`;
}

function invitesHtml() {
  const settings = state.settings || {};
  const defaultUses = settings.inviteUses || 1;
  const defaultTTL = settings.inviteTTL || "24h";
  const ttlOptions = [
    ["1h", "1 hour"],
    ["24h", "24 hours"],
    ["168h", "7 days"],
    ["720h", "30 days"],
  ];

  return `
    <div class="card section">
      <header><h3>New invite</h3></header>
      <p class="hint">The invite is stored with its token, so the QR code and the link can be shown again at any time. Open the link on the phone with Y-Node, or paste it into Exit node → Add → Import from link.</p>
      <form id="new-invite" class="form-grid">
        <label class="field"><span>Note</span><input id="invite-note" placeholder="family phone"></label>
        <label class="field">
          <span>Devices allowed to register</span>
          <input id="invite-uses" type="number" min="1" max="100" value="${defaultUses}">
        </label>
        <label class="field"><span>Valid for</span>
          <select id="invite-ttl">
            ${ttlOptions.map(([value, label]) =>
              `<option value="${value}" ${defaultTTL === value ? "selected" : ""}>${label}</option>`
            ).join("")}
          </select>
        </label>
        <p class="hint full">"Devices allowed to register" is how many phones may redeem this invite before it disappears. 1 means a single device: after it registers, the invite is deleted.</p>
        <div class="full"><button class="btn primary" type="submit">Create invite</button></div>
      </form>
      <div id="invite-result"></div>
    </div>

    <div class="card section">
      <header><h3>Active invites</h3><span class="muted small" id="invites-count">${state.invites.length}</span></header>
      <div id="invites-table">${invitesTableHtml()}</div>
      <div id="invite-view"></div>
    </div>
  `;
}

function invitePanel(invite) {
  return `
    <div class="card section editor">
      <header>
        <h3>${escapeHtml(invite.note || "Invite")}</h3>
        <span class="muted small">expires ${new Date(invite.expiresAt).toLocaleString()} · ${invite.used || 0} of ${invite.uses || 1} device(s) registered</span>
        <button class="btn small" type="button" data-invite-close>Close</button>
      </header>
      <div class="invite-flex">
        ${invite.qr ? `<div class="qr"><img src="${escapeHtml(invite.qr)}" alt="Invite QR code"></div>` : ""}
        <div class="invite-text">
          <div class="mono break">${escapeHtml(invite.link || invite.token || "")}</div>
          <div class="actions">
            <button class="btn primary small" data-invite-copy="${escapeHtml(invite.link || "")}">Copy link</button>
            ${invite.token ? `<button class="btn small" data-invite-token="${escapeHtml(invite.token)}">Copy token</button>` : ""}
          </div>
          <p class="hint">Scan the QR in Y-Node → Exit node → Add, or open the link on the phone. Registration goes over Yggdrasil, so the tunnel must be up first.</p>
        </div>
      </div>
    </div>`;
}

function wireInvites() {
  $("#new-invite").addEventListener("submit", async (event) => {
    event.preventDefault();
    try {
      const data = await api("POST", "api/invites", {
        note: $("#invite-note").value.trim(),
        uses: Number($("#invite-uses").value) || 1,
        ttl: $("#invite-ttl").value,
      });
      $("#invite-result").innerHTML = invitePanel({
        note: data.note,
        expiresAt: data.expiresAt,
        uses: data.uses,
        used: 0,
        link: data.link,
        token: data.invite,
        qr: data.qr,
      });
      wireInviteActions($("#invite-result"));
      await loadInvites();
      // The new invite belongs in Active invites at once, not only after a
      // page reload: the panel above is closed by its own button.
      const table = $("#invites-table");
      if (table) {
        table.innerHTML = invitesTableHtml();
        wireInviteRows(table);
      }
      const count = $("#invites-count");
      if (count) count.textContent = String(state.invites.length);
    } catch (err) { toast(err.message, true); }
  });
  wireInviteRows(document);
  wireInviteActions(document);
}

// wireInviteRows binds the buttons of the Active invites table. It runs again
// after the table is re-rendered in place.
function wireInviteRows(root) {
  $$("[data-invite]", root).forEach((button) => {
    button.addEventListener("click", async () => {
      if (!confirm("Delete this invite?")) return;
      try {
        await api("DELETE", "api/invites/" + encodeURIComponent(button.dataset.invite));
        toast("Invite deleted");
        render();
      } catch (err) { toast(err.message, true); }
    });
  });
  $$("[data-qr]", root).forEach((button) => {
    button.addEventListener("click", () => {
      const invite = state.invites.find((item) => item.hash === button.dataset.qr);
      const view = $("#invite-view");
      if (!invite || !view) return;
      view.innerHTML = invitePanel(invite);
      wireInviteActions(view);
      view.scrollIntoView({ behavior: "smooth", block: "nearest" });
    });
  });
}

function wireInviteActions(root) {
  $$("[data-invite-copy]", root).forEach((button) => {
    button.addEventListener("click", () => copy(button.dataset.inviteCopy));
  });
  $$("[data-invite-token]", root).forEach((button) => {
    button.addEventListener("click", () => copy(button.dataset.inviteToken));
  });
  $$("[data-invite-close]", root).forEach((button) => {
    button.addEventListener("click", () => {
      const panel = button.closest(".editor");
      if (panel) panel.remove();
    });
  });
}

/* settings */

// ipv6StateOf normalizes the IPv6 state the daemon reports. An answer from
// before 1.6.21 carries only ipv6Available, so a missing state falls back to it.
function ipv6StateOf(status) {
  if (!status) return "checking";
  if (status.ipv6State) return status.ipv6State;
  if (status.ipv6Available) return "ok";
  return "none";
}

// ipv6StatePill is the badge next to "IPv6 outbound". "broken" is the case
// that used to look healthy: the interface is up and has an address, but the
// provider drops every IPv6 packet, so the exit must not advertise IPv6.
function ipv6StatePill(status) {
  const state = ipv6StateOf(status);
  const mode = (status && status.ipv6Mode) || "auto";
  if (mode === "off") {
    return '<span class="pill warn">off · IPv4 only</span>';
  }
  switch (state) {
    case "ok":
      return '<span class="pill ok">' + escapeHtml(mode) + "</span>";
    case "broken":
      return '<span class="pill bad">address, no route</span>';
    case "none":
      return '<span class="pill warn">no address · IPv4 only</span>';
    default:
      return '<span class="pill">checking…</span>';
  }
}

// ipv6StatusHint explains the state under the dashboard table.
function ipv6StatusHint(status) {
  const state = ipv6StateOf(status);
  const detail = status && status.ipv6Detail ? " (" + escapeHtml(status.ipv6Detail) + ")" : "";
  const source = status && status.ipv6Source ? status.ipv6Source : "";
  if ((status && status.ipv6Mode) === "off") {
    return "The mode is off, so outbound connections use IPv4 even though the host may have IPv6.";
  }
  switch (state) {
    case "broken":
      return "The host has " + escapeHtml(bracketIPv6(source || "a global IPv6 address")) +
        ", but no IPv6 destination answered" + detail +
        ". The provider does not route IPv6, so outbound connections use IPv4 and clients are told not to send IPv6 traffic.";
    case "none":
      return "No global IPv6 address is detected on this host" + detail + ", so outbound connections use IPv4.";
    case "checking":
      return "The IPv6 route is being checked. Until a destination answers, outbound connections use IPv4.";
    default:
      return "IPv6 source is the address the exit uses for outbound IPv6 connections. It is detected automatically and does not need any setting.";
  }
}

// ipv6SourceField is the read-only "Source address" input next to the mode.
// It shows the address the kernel picks for a global IPv6 destination, which is
// what outbound connections really use. A host without IPv6 leaves it empty:
// the placeholder and the hint below say why.
function ipv6SourceField(status) {
  const value = (status && status.ipv6Source) || "";
  const placeholder = ipv6StateOf(status) === "broken" ? "assigned, but the provider does not route it" : "no global IPv6 address";
  return `<label class="field"><span>Source address</span><input class="mono" value="${escapeHtml(bracketIPv6(value))}" placeholder="${placeholder}" readonly></label>`;
}

// ipv6SourceHint explains the source address and the selected mode in one line.
function ipv6SourceHint(status, mode) {
  if (!status || (status.ipv6State === undefined && status.ipv6Available === undefined)) {
    return "IPv6 detection has not finished yet.";
  }
  const state = ipv6StateOf(status);
  const detail = status.ipv6Detail ? " (" + escapeHtml(status.ipv6Detail) + ")" : "";
  if (mode === "off") {
    if (state === "ok") {
      return "The mode is off, so outbound connections use IPv4 even though this host has a global IPv6 address.";
    }
    if (state === "broken") {
      return "The mode is off. Note that the host has " + escapeHtml(bracketIPv6(status.ipv6Source || "a global IPv6 address")) +
        " but the provider does not route IPv6" + detail + ".";
    }
    return "The mode is off, so outbound connections use IPv4 only.";
  }
  if (state === "broken") {
    return "The host has " + escapeHtml(bracketIPv6(status.ipv6Source || "a global IPv6 address")) +
      ", but no IPv6 destination answered" + detail +
      ". The provider does not route IPv6: connections fall back to IPv4 and clients are told not to send IPv6 traffic.";
  }
  if (state === "none") {
    return "No global IPv6 address is detected on this host" + detail + ", so outbound connections use IPv4 only.";
  }
  if (state === "checking") {
    return "The IPv6 route is being checked. Until a destination answers, outbound connections use IPv4.";
  }
  return "This address is used for outbound IPv6 connections. It is detected automatically and needs no setting.";
}

function settingsHtml() {
  const settings = state.settings || {};
  const web = settings.web || {};
  const usage = settings.usage || {};
  const dns = settings.dns || {};
  const dnsStatus = (state.status && state.status.dns) || {};
  const manualPeers = (state.peers && state.peers.manualPeers) || [];
  const candidates = (state.peers && state.peers.candidates) || [];
  const probing = Boolean(state.peers && state.peers.probing);
  const probe = (state.peers && state.peers.probe) || {};
  const manualRows = manualPeers.length
    ? manualPeers.map((uri) => `
        <div class="manual-peer">
          <span class="mono break">${escapeHtml(uri)}</span>
          <div class="actions">
            <button class="btn small" data-manual-replace="${escapeHtml(uri)}">Replace</button>
            <button class="btn small danger" data-manual-remove="${escapeHtml(uri)}">Remove</button>
          </div>
        </div>`).join("")
    : '<div class="empty">No manual peers. Automatic peers are managed by the supervisor.</div>';
  const candidateOptions = candidates.map((item) => {
    const place = [item.country, item.city].filter(Boolean).join(", ");
    const label = (place ? place + " · " : "") + item.uri
      + (item.latencyMs > 0 ? " · " + item.latencyMs.toFixed(0) + " ms" : "");
    return `<option value="${escapeHtml(item.uri)}">${escapeHtml(label)}</option>`;
  }).join("");
  const zones = [
    "UTC", "Europe/Helsinki", "Europe/Moscow", "Europe/Kyiv", "Europe/Berlin",
    "Europe/London", "Europe/Paris", "Europe/Warsaw", "Europe/Belgrade",
    "America/New_York", "America/Chicago", "America/Los_Angeles",
    "Asia/Dubai", "Asia/Tokyo", "Asia/Shanghai", "Asia/Singapore",
    "Australia/Sydney",
  ];
  const timezone = usage.timezone || "UTC";
  if (!zones.includes(timezone)) zones.unshift(timezone);
  return `
    <div class="settings-grid">
    <form class="card section settings-card" data-settings="node">
      <header>
        <h3>Node</h3>
        <button class="btn primary small save-btn hidden" type="submit">Save</button>
      </header>
      <div class="form-grid">
        <label class="field"><span>Node name</span><input id="s-name" value="${escapeHtml(settings.name || "")}" required></label>
        <label class="field"><span>Log level</span>
          <select id="s-loglevel">
            ${["debug", "info", "warn", "error"].map((level) => `<option value="${level}" ${settings.logLevel === level ? "selected" : ""}>${level}</option>`).join("")}
          </select>
        </label>
        <label class="field"><span>Usage timezone</span>
          <select id="s-timezone">
            ${zones.map((zone) => `<option value="${escapeHtml(zone)}" ${zone === timezone ? "selected" : ""}>${escapeHtml(zone)}</option>`).join("")}
          </select>
        </label>
      </div>
      <label class="switch"><input type="checkbox" id="s-usage" ${usage.enabled ? "checked" : ""}><span>Traffic accounting (needed for quotas)</span></label>
      <p class="hint">The name is shown in the app, in the invite QR code and in the dashboard. The timezone decides when the daily and monthly quota counters roll over.</p>
    </form>

    <form class="card section settings-card" data-settings="dns">
      <header>
        <h3>DNS server</h3>
        <button class="btn primary small save-btn hidden" type="submit">Save</button>
      </header>
      <div class="form-grid">
        <label class="switch full"><input type="checkbox" id="s-dns" ${dns.enabled ? "checked" : ""}><span>Run the Alfis DNS server for Yggdrasil clients</span></label>
        <label class="field"><span>Listen</span>
          <input class="scroll-field mono right" value="${escapeHtml(bracketHostPort(dns.listen || ""))}" readonly title="Click to copy" data-copy-value="${escapeHtml(dns.listen || "")}">
        </label>
        <label class="field"><span>Alfis web password</span>
          <div class="copy-line">
            <input class="scroll-field mono right" id="s-dnspass" value="${escapeHtml(dns.webPassword || "")}" readonly title="Click to copy" data-copy-value="${escapeHtml(dns.webPassword || "")}">
            <button class="btn small" type="button" id="s-dnspass-new" title="Generate a new password and save it with this card">New</button>
          </div>
        </label>
      </div>
      <div class="actions">
        ${dnsStatus.enabled && (dnsStatus.proxyPath || dnsStatus.webURL)
          ? (dnsStatus.webReady
            ? `<a class="btn small" href="${escapeHtml(dnsStatus.proxyPath || dnsStatus.webURL)}" target="_blank" rel="noopener noreferrer">Open Alfis web UI</a>`
            : `<button class="btn small" type="button" disabled title="The Alfis process is still starting"><span class="spinner"></span>Waiting for the Alfis web UI…</button>`)
          : ""}
      </div>
      <p class="hint">Alfis answers <span class="mono">.ygg</span> names and forwards ordinary names to the upstream resolvers. The client finds this server on its own: Y-Node lists it under Available servers as "DNS from exit node". The link opens the Alfis admin page in a new tab on the same address as this dashboard.</p>
      ${dnsStatus.syncing ? '<p class="hint">Alfis is downloading the blockchain on its first start. This can take a while and keeps a CPU core busy; the resolver starts answering as soon as the sync finishes.</p>' : ""}
    </form>

    <form class="card section settings-card" data-settings="access">
      <header>
        <h3>Access</h3>
        <button class="btn primary small save-btn hidden" type="submit">Save</button>
      </header>
      <div class="form-grid">
        <label class="switch"><input type="checkbox" id="s-allowall" ${settings.allowAll ? "checked" : ""}><span>Open mode (allowAll)</span></label>
        <label class="switch"><input type="checkbox" id="s-registration" ${settings.registration ? "checked" : ""}><span>Registration API</span></label>
      </div>
      <p class="hint">Open mode lets any Yggdrasil node use this exit without an invite or a token. It disables the allowlist and the per-client quotas, and it makes the exit usable by strangers. Leave it off on a public server.</p>
    </form>

    <form class="card section settings-card" data-settings="limits">
      <header>
        <h3>Limits</h3>
        <button class="btn primary small save-btn hidden" type="submit">Save</button>
      </header>
      <div class="form-grid">
        <label class="field"><span>Clients allowed</span><input id="s-maxclients" type="number" min="1" value="${settings.maxClients || 64}"></label>
        <label class="field"><span>Connections per client</span><input id="s-maxconns" type="number" min="1" value="${settings.maxConnsPerClient || 64}"></label>
        <label class="field"><span>Connections total</span><input id="s-maxtotal" type="number" min="1" value="${settings.maxConnsTotal || 512}"></label>
        <label class="field"><span>Idle timeout</span><input id="s-idle" value="${escapeHtml(settings.idleTimeout || "5m")}"></label>
        <label class="field"><span>Dial timeout</span><input id="s-dial" value="${escapeHtml(settings.dialTimeout || "15s")}"></label>
        <label class="field"><span>Blocked ports (comma separated)</span><input id="s-blocked" value="${(settings.blockedPorts || []).join(", ")}"></label>
      </div>
      <p class="hint">"Clients allowed" is how many Yggdrasil addresses may be in the allowlist. The connection limits are counted per client and for the whole exit.</p>
      <p class="hint">Dial timeout is how long the exit waits for a destination to accept a TCP connection before it reports an error to the client. Idle timeout closes a connection that carries no traffic for that long.</p>
    </form>

    <form class="card section settings-card" data-settings="udp">
      <header>
        <h3>UDP relay</h3>
        <button class="btn primary small save-btn hidden" type="submit">Save</button>
      </header>
      <div class="form-grid">
        <div class="field-pair full">
          <label class="switch"><input type="checkbox" id="s-udp" ${settings.udpEnabled ? "checked" : ""}><span>Relay UDP through the exit</span></label>
          <label class="switch"><input type="checkbox" id="s-domain" ${settings.domainResolution ? "checked" : ""}><span>Resolve domain names (ATYP=domain)</span></label>
        </div>
        <div class="field-pair full">
          <label class="field"><span>Max UDP associations</span><input id="s-maxudp" type="number" min="1" value="${settings.maxUDPAssociations || 64}"></label>
          <label class="field"><span>UDP idle timeout</span><input id="s-udpidle" value="${escapeHtml(settings.udpIdleTimeout || "2m")}"></label>
        </div>
      </div>
      <p class="hint">UDP is what DNS and QUIC need. Without the relay, apps fall back to TCP or lose those lookups.</p>
    </form>

    <form class="card section settings-card" data-settings="ipv6">
      <header>
        <h3>IPv6 outbound</h3>
        <button class="btn primary small save-btn hidden" type="submit">Save</button>
      </header>
      <div class="form-grid">
        <label class="field"><span>Mode</span>
          <select id="s-ipv6">
            <option value="auto" ${settings.ipv6 === "auto" ? "selected" : ""}>auto — use it when available</option>
            <option value="prefer" ${settings.ipv6 === "prefer" ? "selected" : ""}>prefer — try IPv6 first</option>
            <option value="off" ${settings.ipv6 === "off" ? "selected" : ""}>off — IPv4 only</option>
          </select>
        </label>
        ${ipv6SourceField(state.status)}
      </div>
      <p class="hint">This only controls how the exit dials destinations. The Yggdrasil mesh itself is always IPv6.</p>
      <p class="hint">${ipv6SourceHint(state.status, settings.ipv6)}</p>
    </form>

    <form class="card section settings-card" data-settings="peers">
      <header>
        <h3>Public peers</h3>
        <button class="btn primary small save-btn hidden" type="submit">Save</button>
      </header>
      <div class="form-grid">
        <label class="field"><span>Automatic peers to keep</span><input id="s-peercount" type="number" min="1" max="50" value="${settings.peerCount || 3}"></label>
        <label class="switch"><input type="checkbox" id="s-peerauto" ${settings.peerAutoReplace ? "checked" : ""}><span>Replace dead or slow peers</span></label>
      </div>
      <p class="hint">The supervisor keeps this many public peers connected and swaps a dead or slow one for a better candidate.</p>
    </form>

    <div class="card section">
      <header>
        <h3>Manual Yggdrasil peers</h3>
        <span class="muted small">${manualPeers.length} pinned</span>
        <button class="btn small" type="button" id="peers-refresh" ${probing ? "disabled" : ""}>${probing ? '<span class="spinner"></span>Searching…' : "Find peers"}</button>
      </header>
      <p class="hint" id="peer-progress">${escapeHtml(peerProgressText(probe, probing))}</p>
      <p class="hint">A pinned peer is kept connected on top of the automatic count. It is never replaced or trimmed, so it is the right place for a private peer.</p>
      <div id="manual-peers">${manualRows}</div>
      <form id="manual-peer-pick" class="manual-peer-form">
        <select id="manual-candidate" ${candidateOptions ? "" : "disabled"}>
          ${candidateOptions || '<option value="">No probed peers yet — press "Find peers"</option>'}
        </select>
        <button class="btn primary" type="submit" ${candidateOptions ? "" : "disabled"}>Add selected</button>
      </form>
      <form id="manual-peer-add" class="manual-peer-form">
        <input id="manual-uri" placeholder="tls://peer.example:13338?key=…" required>
        <button class="btn" type="submit">Add by address</button>
      </form>
    </div>

    <form class="card section settings-card" data-settings="web">
      <header>
        <h3>Web interface</h3>
        <button class="btn primary small save-btn hidden" type="submit">Save</button>
      </header>
      <div class="form-grid">
        <label class="field full"><span>Public URL (shown on the dashboard and by "x-node web url")</span><input id="s-publicurl" value="${escapeHtml(web.publicURL || "")}" placeholder="https://example.com/abc123/web/" readonly></label>
        <label class="field"><span>URL path</span><input id="s-webpath" value="${escapeHtml(web.path || "")}"></label>
        <label class="field"><span>Session lifetime</span><input id="s-sessionttl" value="${escapeHtml(web.sessionTTL || "12h")}"></label>
        <label class="field"><span>Failed logins before a block</span><input id="s-maxfail" type="number" min="0" value="${web.maxFailedLogins || 5}"></label>
        <label class="field"><span>Block duration</span><input id="s-lockout" value="${escapeHtml(web.lockout || "15m")}"></label>
        <label class="field"><span>Listen</span><input value="${escapeHtml(web.listen || "all")}" disabled></label>
        <label class="field"><span>Port</span><input value="${web.port || 443}" disabled></label>
        <label class="switch full"><input type="checkbox" id="s-compact" ${web.compact ? "checked" : ""}><span>Compact layout — keep the interface 25% narrower on a desktop screen</span></label>
      </div>
      <p class="hint">Session lifetime takes a duration such as <span class="mono">12h</span>; <span class="mono">0</span> or <span class="mono">unlimited</span> means the session never expires and the password is not asked again.</p>
      <p class="hint">The public URL is filled by the installer and is read-only here: change <span class="mono">web.publicURL</span> in the Configuration tab if it is wrong. Listen address, port and TLS are read at start: change them there too and restart x-node.</p>
    </form>

    <div class="card section">
      <header><h3>Admin password</h3><span class="muted small">login ${escapeHtml((state.status && state.status.webLogin) || "")}</span></header>
      <form id="password-form" class="form-grid">
        <label class="field"><span>Current password</span><input id="p-current" type="password" autocomplete="current-password" required></label>
        <label class="field"><span>New password (12+ characters)</span><input id="p-new" type="password" autocomplete="new-password" minlength="12" required></label>
        <label class="field"><span>Repeat the new password</span><input id="p-repeat" type="password" autocomplete="new-password" minlength="12" required></label>
        <div class="full"><button class="btn primary" type="submit">Change password</button></div>
      </form>
      <p class="hint">Other signed-in browsers stay signed in until their session expires.</p>
    </div>
    </div>
  `;
}

// settingsPayload builds the partial update of one form. The server accepts a
// partial patch, so a form never overwrites a field it does not show.
function settingsPayload(kind) {
  switch (kind) {
    case "node":
      return {
        name: $("#s-name").value.trim(),
        logLevel: $("#s-loglevel").value,
        usage: { enabled: $("#s-usage").checked, timezone: $("#s-timezone").value },
      };
    case "access":
      return { allowAll: $("#s-allowall").checked, registration: $("#s-registration").checked };
    case "limits": {
      const blocked = $("#s-blocked").value.split(",").map((item) => Number(item.trim())).filter((item) => item > 0);
      return {
        maxClients: Number($("#s-maxclients").value),
        maxConnsPerClient: Number($("#s-maxconns").value),
        maxConnsTotal: Number($("#s-maxtotal").value),
        idleTimeout: $("#s-idle").value.trim(),
        dialTimeout: $("#s-dial").value.trim(),
        blockedPorts: blocked,
      };
    }
    case "udp":
      return {
        udpEnabled: $("#s-udp").checked,
        domainResolution: $("#s-domain").checked,
        maxUDPAssociations: Number($("#s-maxudp").value),
        udpIdleTimeout: $("#s-udpidle").value.trim(),
      };
    case "ipv6":
      return { ipv6: $("#s-ipv6").value };
    case "peers":
      return { peerCount: Number($("#s-peercount").value), peerAutoReplace: $("#s-peerauto").checked };
    case "dns":
      return {
        dns: {
          enabled: $("#s-dns").checked,
          webPassword: $("#s-dnspass").value.trim(),
        },
      };
    case "web":
      return {
        web: {
          path: $("#s-webpath").value.trim(),
          sessionTTL: $("#s-sessionttl").value.trim(),
          maxFailedLogins: Number($("#s-maxfail").value),
          lockout: $("#s-lockout").value.trim(),
          compact: $("#s-compact").checked,
        },
      };
    default:
      throw new Error("unknown form");
  }
}

// setPeerSearchButton keeps the label and the spinner in one place, so the
// button looks the same whether the probe was started here or was already
// running when the page opened.
function setPeerSearchButton(button, probing) {
  if (!button) return;
  button.disabled = probing;
  button.innerHTML = probing ? '<span class="spinner"></span>Searching…' : "Find peers";
}

// peerProgressText explains what the catalogue search is doing. "Total" is the
// public catalogue, "checked" is how many peers got a protocol probe, and
// "alive" is how many completed a handshake.
function peerProgressText(probe, probing) {
  const total = Number(probe && probe.total) || 0;
  const checked = Number(probe && probe.checked) || 0;
  const alive = Number(probe && probe.alive) || 0;
  if (!total && !checked) {
    if (probing) return "Downloading the public peer catalogue…";
    if (probe && probe.stage === "failed") return "The last search failed. Check the log for the reason.";
    return "";
  }
  const parts = [
    total + " in the catalogue",
    checked + " checked",
    alive + " alive",
  ];
  return (probing ? "Searching: " : "Last search: ") + parts.join(" · ");
}

// updatePeerProgress refreshes only the progress line while the poll loop runs;
// a full render would rebuild the form and lose the current selection.
function updatePeerProgress() {
  const node = $("#peer-progress");
  if (!node) return;
  const probe = (state.peers && state.peers.probe) || {};
  node.textContent = peerProgressText(probe, Boolean(state.peers && state.peers.probing));
}

// pollPeerProbe watches the daemon until the catalogue probe finishes. The
// probe walks every public peer, so it can take minutes; a fixed short loop
// used to give up and leave the button on "Searching…" until a reload.
async function pollPeerProbe() {
  if (state.peerPolling) return;
  state.peerPolling = true;
  try {
    let started = false;
    for (let attempt = 0; attempt < 300; attempt += 1) {
      // Before the supervisor picks up the refresh request the flag is still
      // false; poll quickly at first so the button does not stop the spinner
      // before the probe has even started.
      await new Promise((resolve) => setTimeout(resolve, started ? 2000 : 500));
      try {
        await loadPeers();
      } catch (_) {
        continue;
      }
      updatePeerProgress();
      if (state.peers && state.peers.probing) {
        started = true;
        continue;
      }
      if (started) break;
      // Ten seconds without a running probe means the request never reached
      // the supervisor; fall back to the normal label instead of spinning.
      if (attempt >= 19) break;
    }
    if (state.view !== "settings") return;
    const found = (state.peers && state.peers.candidates)
      ? state.peers.candidates.length
      : 0;
    render();
    toast(found
      ? "Peer search finished: " + found + " candidate(s) found"
      : "Peer search finished: no new candidates");
  } finally {
    state.peerPolling = false;
  }
}

function wireSettings() {
  // Each card saves only its own fields, and its button stays hidden until
  // something on that card actually changes.
  $$("[data-settings]").forEach((form) => {
    const button = form.querySelector(".save-btn");
    const mark = () => { if (button) button.classList.remove("hidden"); };
    form.addEventListener("input", mark);
    form.addEventListener("change", mark);
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      const kind = form.dataset.settings;
      if (kind === "access" && $("#s-allowall").checked) {
        const ok = confirm(
          "Enable open mode?\n\n" +
          "Any Yggdrasil node will be able to use this exit without an invite. " +
          "The allowlist and the per-client quotas stop being enforced, so strangers " +
          "can send their traffic through this server. It can be abused, and the " +
          "provider may block the host for the traffic.\n\nEnable open mode anyway?"
        );
        if (!ok) {
          $("#s-allowall").checked = false;
          return;
        }
      }
      if (button) button.disabled = true;
      try {
        const wasDns = Boolean(state.settings && state.settings.dns && state.settings.dns.enabled);
        const data = await api("PUT", "api/settings", settingsPayload(kind));
        await loadSettings();
        if (kind === "dns" && !wasDns && state.settings.dns && state.settings.dns.enabled) {
          toast("DNS server is on · Alfis is synchronizing the blockchain on its first start, this can take a few minutes");
        } else {
          toast(data.restartRequired ? "Saved · restart x-node to apply the listener change" : "Saved");
        }
        await refreshStatus();
        render();
      } catch (err) {
        toast(err.message, true);
        if (button) button.disabled = false;
      }
    });
  });

  // Turning open mode on is confirmed at once, not only when the form is
  // submitted: the warning must be seen before the switch looks enabled.
  const allowAll = $("#s-allowall");
  if (allowAll) {
    allowAll.addEventListener("change", () => {
      if (!allowAll.checked) return;
      const ok = confirm(
        "Open mode lets every Yggdrasil node use this exit without an invite. " +
        "The allowlist and per-client quotas are no longer enforced, so anyone " +
        "can relay traffic through this server.\n\nContinue?"
      );
      if (!ok) allowAll.checked = false;
    });
  }

  // The compact layout is a display preference, so it applies immediately
  // instead of waiting for the Save button of the Web interface card.
  const compact = $("#s-compact");
  if (compact) {
    // The switch fires "input" before "change". Stopping both keeps the card's
    // Save button hidden while the switch saves itself.
    compact.addEventListener("input", (event) => event.stopPropagation());
    compact.addEventListener("change", async (event) => {
      // The card's own Save button must not appear: this switch saves itself.
      event.stopPropagation();
      const enabled = compact.checked;
      applyCompact(enabled);
      try {
        await api("PUT", "api/settings", { web: { compact: enabled } });
        if (state.settings && state.settings.web) state.settings.web.compact = enabled;
        const form = compact.closest("[data-settings]");
        const save = form && form.querySelector(".save-btn");
        if (save) save.classList.add("hidden");
        toast(enabled ? "Compact layout on" : "Compact layout off");
      } catch (err) {
        compact.checked = !enabled;
        applyCompact(!enabled);
        toast(err.message, true);
      }
    });
  }

  // The Alfis password is shown read-only so a click copies it. A new one is
  // generated here and saved with the card, like the installer does on the
  // first start.
  const newDnsPassword = $("#s-dnspass-new");
  if (newDnsPassword) {
    newDnsPassword.addEventListener("click", () => {
      const field = $("#s-dnspass");
      if (!field) return;
      const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789";
      const bytes = new Uint8Array(24);
      crypto.getRandomValues(bytes);
      let value = "";
      for (const byte of bytes) value += alphabet[byte % alphabet.length];
      field.value = value;
      field.dataset.copyValue = value;
      const form = field.closest("[data-settings]");
      const save = form && form.querySelector(".save-btn");
      if (save) save.classList.remove("hidden");
    });
  }

  $("#password-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    if ($("#p-new").value !== $("#p-repeat").value) {
      toast("The new passwords do not match", true);
      return;
    }
    try {
      await api("POST", "api/password", {
        currentPassword: $("#p-current").value,
        newPassword: $("#p-new").value,
      });
      $("#password-form").reset();
      toast("Password changed");
    } catch (err) { toast(err.message, true); }
  });

  const addManualPeer = async (uri) => {
    try {
      await api("POST", "api/peers/manual", { action: "add", uri });
      toast("Manual peer added");
      await loadPeers();
      render();
    } catch (err) { toast(err.message, true); }
  };

  $("#manual-peer-pick").addEventListener("submit", async (event) => {
    event.preventDefault();
    const uri = $("#manual-candidate").value;
    if (!uri) return;
    await addManualPeer(uri);
  });

  $("#manual-peer-add").addEventListener("submit", async (event) => {
    event.preventDefault();
    await addManualPeer($("#manual-uri").value.trim());
  });

  const refresh = $("#peers-refresh");
  if (refresh) {
    refresh.addEventListener("click", async () => {
      setPeerSearchButton(refresh, true);
      try {
        await api("POST", "api/peers", { refresh: true });
        toast("Peer search started");
        pollPeerProbe();
      } catch (err) {
        toast(err.message, true);
        setPeerSearchButton(refresh, false);
      }
    });
    // A page that was opened while a probe was already running must keep the
    // button live as well, otherwise it stays on "Searching…" until a reload.
    if (state.peers && state.peers.probing) pollPeerProbe();
  }

  $$("[data-manual-remove]").forEach((button) => {
    button.addEventListener("click", async () => {
      if (!confirm("Remove this manual peer?")) return;
      try {
        await api("POST", "api/peers/manual", { action: "remove", uri: button.dataset.manualRemove });
        toast("Manual peer removed");
        await loadPeers();
        render();
      } catch (err) { toast(err.message, true); }
    });
  });
  $$("[data-manual-replace]").forEach((button) => {
    button.addEventListener("click", async () => {
      const next = prompt("New peer URI:", button.dataset.manualReplace);
      if (!next || next === button.dataset.manualReplace) return;
      try {
        await api("POST", "api/peers/manual", {
          action: "replace", from: button.dataset.manualReplace, to: next.trim(),
        });
        toast("Manual peer replaced");
        await loadPeers();
        render();
      } catch (err) { toast(err.message, true); }
    });
  });
}

/* configuration files */

function configHtml() {
  // The daemon reports the real paths: yggdrasil.conf can live outside /etc
  // when the installer was pointed at another directory.
  const files = (state.status && state.status.configFiles) || [
    { id: "main", path: "/etc/x-node/config.json" },
    { id: "yggdrasil", path: "/etc/yggdrasil/yggdrasil.conf" },
    { id: "alfis", path: "/etc/x-node/alfis.conf" },
  ];
  return `
    <div class="card section config-card">
      <header><h3>Configuration files</h3><span class="muted small" id="config-status"></span></header>
      <p class="hint">The file is validated before it replaces the current one; a backup is written to /var/backups. Applying the main configuration removes the dashboard overlay, so the file becomes the only source of truth. A restart applies a saved file to the running service.</p>
      <div class="config-toolbar">
        <select id="config-file">
          ${files.map((file) => `<option value="${escapeHtml(file.id)}">${escapeHtml(file.path)}</option>`).join("")}
        </select>
        <div class="actions">
          <button class="btn primary small" id="config-save" type="button">Validate and save</button>
          <button class="btn small" data-restart="x-node" type="button">Restart x-node</button>
          <button class="btn small" data-restart="yggdrasil" type="button">Restart yggdrasil</button>
          <button class="btn small" data-restart="x-node-dns" type="button">Restart DNS (Alfis)</button>
        </div>
      </div>
      <div class="muted small mono" id="config-path"></div>
      <textarea id="config-content" class="config-editor" spellcheck="false" placeholder="Load a file to edit it"></textarea>
    </div>
  `;
}

function wireConfig() {
  const loadConfig = async () => {
    const file = $("#config-file").value;
    const status = $("#config-status");
    status.textContent = "loading…";
    try {
      const data = await api("GET", "api/config?file=" + encodeURIComponent(file));
      $("#config-content").value = data.content || "";
      $("#config-path").textContent = data.path + " · " + bytes(data.size) + " · modified " + when(data.modified);
      status.textContent = data.helper ? "" : "root helper missing";
    } catch (err) {
      status.textContent = err.message;
      toast(err.message, true);
    }
  };
  $("#config-file").addEventListener("change", loadConfig);
  $("#config-save").addEventListener("click", async () => {
    const file = $("#config-file").value;
    if (!confirm("Validate and save this file? A backup is kept in /var/backups.")) return;
    try {
      const data = await api("PUT", "api/config", { file, content: $("#config-content").value });
      toast("Saved " + data.path);
      $("#config-status").textContent = "saved";
      await refreshStatus();
    } catch (err) { toast(err.message, true); }
  });
  $$("[data-restart]").forEach((button) => {
    button.addEventListener("click", async () => {
      const service = button.dataset.restart;
      const questions = {
        "x-node": "Restart x-node? Open tunnels will reconnect.",
        "yggdrasil": "Restart yggdrasil? Every peering link is rebuilt.",
        "x-node-dns": "Restart x-node-dns? The resolver is unavailable for a moment.",
      };
      if (!confirm(questions[service] || "Restart " + service + "?")) return;
      button.disabled = true;
      try {
        await api("POST", "api/restart", { service });
        toast("Restart requested: " + service);
      } catch (err) {
        toast(err.message, true);
        button.disabled = false;
      }
    });
  });
  loadConfig();
}

/* logs */

function logsHtml() {
  const wrap = state.logWrap !== false;
  return `
    <div class="card section logs-card">
      <header>
        <h3>Recent log lines</h3>
        <label class="switch"><input type="checkbox" id="log-wrap" ${wrap ? "checked" : ""}><span>Wrap lines</span></label>
        <label class="switch"><input type="checkbox" id="log-auto" checked><span>Auto refresh</span></label>
        <button class="btn small" id="log-refresh">Refresh</button>
      </header>
      <pre class="logs${wrap ? " wrap" : ""}" id="log-box">${state.logs.map((entry) =>
        `<span class="${entry.level.toLowerCase()}">${escapeHtml(entry.time)} ${escapeHtml(entry.level)} ${escapeHtml(entry.message)}</span>`
      ).join("\n")}</pre>
      <div class="actions">
        <span class="hint">journalctl -u x-node -f shows the full log. The guide is in the sidebar under Logs.</span>
      </div>
    </div>`;
}

function wireLogs() {
  const box = $("#log-box");
  box.scrollTop = box.scrollHeight;
  const wrap = $("#log-wrap");
  if (wrap) {
    wrap.addEventListener("change", () => {
      state.logWrap = wrap.checked;
      box.classList.toggle("wrap", wrap.checked);
    });
  }
  $("#log-refresh").addEventListener("click", async () => {
    await loadLogs();
    render();
  });
  clearInterval(state.logTimer);
  state.logTimer = setInterval(async () => {
    if (document.hidden || !$("#log-auto") || !$("#log-auto").checked) return;
    try {
      await loadLogs();
      box.innerHTML = state.logs.map((entry) =>
        `<span class="${entry.level.toLowerCase()}">${escapeHtml(entry.time)} ${escapeHtml(entry.level)} ${escapeHtml(entry.message)}</span>`
      ).join("\n");
      box.scrollTop = box.scrollHeight;
    } catch (_) { /* ignore */ }
  }, 3000);
}

/* boot */

applyTheme(localStorage.getItem("x-node-theme") || "auto");
checkSession();
setInterval(() => {
  if (state.status) $("#clock").textContent = new Date().toLocaleTimeString();
}, 1000);
