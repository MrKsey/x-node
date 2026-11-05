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
  traffic: null,
  trafficRange: "1h",
  logs: [],
  view: "dashboard",
  timer: null,
  logTimer: null,
  editing: "",
};

const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));

/* ---------- helpers ---------- */

function api(method, path, body) {
  const options = {
    method,
    credentials: "same-origin",
    headers: { "X-Requested-With": "x-node" },
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

/* ---------- auth ---------- */

function showLogin() {
  clearInterval(state.timer);
  clearInterval(state.logTimer);
  $("#app").classList.add("hidden");
  $("#login").classList.remove("hidden");
  $("#boot").classList.add("hidden");
  setTimeout(() => $("#login-password").focus(), 50);
}

function showApp() {
  $("#login").classList.add("hidden");
  $("#app").classList.remove("hidden");
  $("#boot").classList.add("hidden");
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
  $("#view-title").textContent = titles[view];
  $$("#nav a").forEach((link) => link.classList.toggle("active", link.dataset.view === view));
  $("#app").classList.remove("menu-open");
  render();
}

window.addEventListener("hashchange", () => navigate(location.hash.replace("#", "")));
$("#menu").addEventListener("click", () => $("#app").classList.toggle("menu-open"));
// A tap anywhere outside an open client popover closes it.
document.addEventListener("click", (event) => {
  if (event.target.closest("[data-client-stats]") || event.target.closest(".client-stats")) return;
  $$(".client-stats").forEach((item) => item.classList.add("hidden"));
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

async function refreshStatus() {
  let status;
  try {
    status = await api("GET", "api/status");
  } catch (err) {
    // Only the status endpoint decides whether the node is online. A broken
    // admin socket or a failed peer/traffic request must not paint it red.
    const health = $("#health");
    health.textContent = "offline";
    health.className = "pill bad";
    return;
  }
  state.status = status;
  const health = $("#health");
  health.textContent = "online · " + duration(status.uptimeSeconds);
  health.className = "pill ok";
  $("#side-name").textContent = status.name || "";
  $("#clock").textContent = new Date().toLocaleTimeString();
  if (state.view === "dashboard") {
    await Promise.allSettled([loadPeers(), loadTraffic()]);
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

async function loadLogs() {
  const data = await api("GET", "api/logs?limit=300");
  state.logs = data.entries || [];
  return state.logs;
}

/* ---------- rendering ---------- */

async function render() {
  const view = $("#view");
  try {
    if (state.view === "dashboard") {
      await refreshStatus();
      view.innerHTML = dashboardHtml();
      wireDashboard();
      drawChart();
    } else if (state.view === "clients") {
      await Promise.all([loadClients(), loadSettings()]);
      view.innerHTML = clientsHtml();
      wireClients();
    } else if (state.view === "invites") {
      await Promise.all([loadInvites(), loadSettings()]);
      view.innerHTML = invitesHtml();
      wireInvites();
    } else if (state.view === "settings") {
      await Promise.all([loadSettings(), loadPeers()]);
      view.innerHTML = settingsHtml();
      wireSettings();
    } else if (state.view === "config") {
      view.innerHTML = configHtml();
      wireConfig();
    } else if (state.view === "logs") {
      await loadLogs();
      view.innerHTML = logsHtml();
      wireLogs();
    }
  } catch (err) {
    view.innerHTML = '<div class="card empty">' + escapeHtml(err.message) + "</div>";
  }
}

/* dashboard */

// shortPeerURI keeps the peering link on one line. The key is 64 hex digits,
// so the full link wraps into a second, almost empty line; the copy button and
// the tooltip still carry the whole value.
function shortPeerURI(uri) {
  const match = /^(.*[?&]key=)([0-9a-fA-F]{16,})(.*)$/.exec(uri || "");
  if (!match) return uri || "";
  const key = match[2];
  return match[1] + key.slice(0, 8) + "…" + key.slice(-4) + match[3];
}

function dashboardHtml() {
  const status = state.status || {};
  const peers = state.peers || {};
  const self = peers.self || {};
  const quotas = status.quotas || {};
  const apiShared = status.apiPort && status.apiPort === status.socksPort;
  return `
    <div class="grid kpi">
      <div class="card kpi-card">
        <div class="label">Yggdrasil address</div>
        <div class="value mono small-value">${escapeHtml(status.address || "—")}</div>
        <div class="sub">${escapeHtml(status.name || "")}</div>
      </div>
      <a class="card kpi-card link-card" href="#clients" title="Open the Clients tab">
        <div class="label">Clients</div>
        <div class="value">${status.clients || 0}</div>
        <div class="sub">${status.activeConnections || 0} TCP · ${status.activeUdp || 0} UDP</div>
      </a>
      <div class="card kpi-card">
        <div class="label">Today</div>
        <div class="value">${bytes((status.todayRx || 0) + (status.todayTx || 0))}</div>
        <div class="sub">↑ ${bytes(status.todayRx)} · ↓ ${bytes(status.todayTx)}</div>
      </div>
      <div class="card kpi-card">
        <div class="label">This month</div>
        <div class="value">${bytes((status.monthRx || 0) + (status.monthTx || 0))}</div>
        <div class="sub">↑ ${bytes(status.monthRx)} · ↓ ${bytes(status.monthTx)}</div>
      </div>
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
          <tr><th>SOCKS5</th><td class="right mono">[${escapeHtml(status.address || "")}]:${status.socksPort || ""}</td></tr>
          <tr><th>API</th><td class="right mono">${apiShared ? "shared with SOCKS5" : "port " + (status.apiPort || "")} · auth ${escapeHtml(status.apiAuth || "")}</td></tr>
          <tr><th>Registration</th><td class="right">${status.registration ? "enabled" : "disabled"}</td></tr>
          <tr><th>Open mode</th><td class="right">${status.allowAll ? '<span class="pill warn">allowAll</span>' : "allowlist"}</td></tr>
          <tr><th>Client slots</th><td class="right">${status.maxClients || 0} allowed</td></tr>
          <tr><th>Connections</th><td class="right">${status.maxConnsPerClient || 0} per client · ${status.maxConnsTotal || 0} in total</td></tr>
          <tr><th>UDP relay</th><td class="right">${status.udpEnabled ? "enabled · " + (status.maxUdpAssociations || 0) + " association(s)" : "disabled"}</td></tr>
          <tr><th>IPv6 outbound</th><td class="right">${status.ipv6Available ? '<span class="pill ok">' + escapeHtml(status.ipv6Mode) + "</span>" : '<span class="pill warn">IPv4 only</span>'}</td></tr>
          ${status.ipv6Available ? `<tr><th>IPv6 source</th><td class="right mono break">${escapeHtml(status.ipv6Detail || "—")}</td></tr>` : ""}
          <tr><th>Domain names</th><td class="right">${status.domainResolution ? "resolved on the exit" : "client resolves"}</td></tr>
          <tr><th>Log level</th><td class="right">${escapeHtml(status.logLevel || "")}</td></tr>
          <tr><th>Config</th><td class="right mono ellipsis" title="${escapeHtml(status.configPath || "")}">${escapeHtml(status.configPath || "—")}</td></tr>
          <tr><th>State</th><td class="right mono ellipsis" title="${escapeHtml(status.stateDir || "")}">${escapeHtml(status.stateDir || "—")}</td></tr>
        </table>
        <p class="hint">IPv6 source is the address the exit uses for outbound IPv6 connections. It is detected automatically and does not need any setting.</p>
      </div>

      <div class="card section">
        <header><h3>Yggdrasil</h3><span class="muted small">${peers.adminReachable ? "admin socket connected" : "admin socket unavailable"}</span></header>
        <table class="facts">
          <tr><th>Version</th><td class="right">${escapeHtml(self.build_version || "—")}</td></tr>
          <tr><th>Address</th><td class="right mono">${escapeHtml(self.address || status.address || "—")}</td></tr>
          <tr><th>Public key</th><td class="right mono ellipsis" title="${escapeHtml(self.key || "")}">${escapeHtml(self.key || "—")}</td></tr>
          <tr><th>Routing entries</th><td class="right">${self.routing_entries == null ? "—" : self.routing_entries}</td></tr>
          <tr><th>Subnet</th><td class="right mono">${escapeHtml(self.subnet || "—")}</td></tr>
          <tr><th>Peers</th><td class="right">${peers.up || 0} up · ${peers.managed || 0} automatic · ${peers.manualPeers ? peers.manualPeers.length : 0} manual · ${peers.inbound || 0} inbound</td></tr>
          <tr><th>Auto replace</th><td class="right">${peers.autoReplace ? "on" : "off"} · ${peers.peerCount || 0} wanted</td></tr>
        </table>
        <div class="peer-box">
          <div class="label">Peer link</div>
          ${status.peerURI
            ? `<div class="peer-link mono" title="${escapeHtml(status.peerURI)}">${escapeHtml(shortPeerURI(status.peerURI))}</div>
               <div class="actions">
                 <button class="btn small" data-copy="${escapeHtml(status.peerURI)}">Copy peer link</button>
               </div>
               <p class="hint">Add it in Y-Node → Exit node → Add → Peers, or append it to the <span class="mono">Peers</span> list in yggdrasil.conf.</p>`
            : '<p class="hint">Incoming peering is disabled. Start the installer with a public host and a peering port to get a wss:// or tls:// link.</p>'}
        </div>
      </div>
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

function peerTableHtml(peers) {
  if (!peers.length) {
    return '<div class="empty">No Yggdrasil links. Check the admin socket and the peer settings.</div>';
  }
  const rows = peers.map((peer) => {
    const statePill = peer.up ? '<span class="pill ok">up</span>' : '<span class="pill bad">down</span>';
    const direction = peer.inbound ? '<span class="pill warn">in</span>' : '<span class="pill">out</span>';
    const managed = peer.managed ? '<span class="pill ok">auto</span>' : "";
    const manual = peer.manual ? '<span class="pill">manual</span>' : "";
    return `
      <tr>
        <td data-label="Link"><div class="mono break">${escapeHtml(peer.uri)}</div></td>
        <td data-label="Direction">${direction}</td>
        <td data-label="State">${statePill}</td>
        <td data-label="Address"><div class="mono break">${escapeHtml(peer.address || "—")}</div></td>
        <td class="right" data-label="RTT">${peer.latencyMs > 0 ? peer.latencyMs.toFixed(1) + " ms" : "—"}</td>
        <td class="right hide-sm" data-label="Uptime">${peer.uptimeSeconds > 0 ? duration(peer.uptimeSeconds) : "—"}</td>
        <td class="right hide-sm" data-label="Traffic">↑ ${bytes(peer.bytesRecv)} · ↓ ${bytes(peer.bytesSent)}</td>
        <td class="right" data-label="Managed">${managed}${manual}</td>
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
}

function renderDashboard() {
  if (state.view !== "dashboard") return;
  const view = $("#view");
  if (!view || state.editing) return;
  view.innerHTML = dashboardHtml();
  wireDashboard();
  drawChart();
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

window.addEventListener("resize", () => {
  if (state.view === "dashboard") drawChart();
});

/* clients */

function clientsHtml() {
  const rows = state.clients.map((client) => clientRowHtml(client)).join("");
  const defaults = (state.settings && state.settings.quotas) || {};
  return `
    <div class="card section">
      <header>
        <h3>Registered clients</h3>
        <span class="muted small">${state.clients.length} total</span>
      </header>
      ${state.clients.length ? `<div class="table-wrap"><table class="cards">
        <thead><tr>
          <th>Client</th><th>State</th><th class="right">TCP</th><th class="right">UDP</th>
          <th class="right">Today</th><th class="right">Month</th><th>Quota</th>
          <th class="right hide-sm">Last seen</th><th></th>
        </tr></thead>
        <tbody>${rows}</tbody>
      </table></div>` : '<div class="empty">No clients yet. Create an invite or add an address.</div>'}
      <div id="client-editor"></div>
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
  const stats = [
    client.source ? "added via " + client.source : "",
    (client.udpDatagramsIn || client.udpDatagramsOut)
      ? "UDP " + (client.udpDatagramsIn || 0) + " in / " + (client.udpDatagramsOut || 0) + " out · "
        + bytes((client.udpBytesIn || 0) + (client.udpBytesOut || 0))
      : "",
    (client.destinations || []).length ? "destinations: " + client.destinations.join(", ") : "",
    client.refused ? client.refused + " refused (" + (client.quotaRefused || 0) + " by quota)" : "",
  ].filter(Boolean).join(" · ");
  const quotaPill = quota.exhausted ? '<span class="pill bad">quota exhausted</span>' : "";
  return `
    <tr>
      <td data-label="Client">
        <div class="client-line">
          <span class="mono break">${escapeHtml(client.address)}</span>
          ${client.label ? `<span class="pill">${escapeHtml(client.label)}</span>` : ""}
          <button class="btn small icon-only stats-btn" data-client-stats title="Traffic details" aria-label="Traffic details">ⓘ</button>
        </div>
        ${meta ? `<div class="muted small">${escapeHtml(meta)}</div>` : ""}
        <div class="client-stats hidden">${stats ? escapeHtml(stats) : "No traffic recorded yet"}</div>
      </td>
      <td data-label="State"><span class="pill ${client.state === "connected" ? "ok" : ""}">${escapeHtml(client.state)}</span></td>
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
      <td class="right nowrap" data-label="Actions">
        <button class="btn small" data-edit="${escapeHtml(client.address)}">Edit</button>
        <button class="btn small" data-token="${escapeHtml(client.address)}">Token</button>
        <button class="btn small danger" data-remove="${escapeHtml(client.address)}">Remove</button>
      </td>
    </tr>`;
}

function wireClients() {
  $$("[data-client-stats]").forEach((button) => {
    button.addEventListener("click", (event) => {
      event.stopPropagation();
      const cell = button.closest("td");
      const popover = cell && cell.querySelector(".client-stats");
      if (!popover) return;
      const open = !popover.classList.contains("hidden");
      // Only one popover at a time: two open balloons on a phone cover the
      // whole table.
      $$(".client-stats").forEach((item) => item.classList.add("hidden"));
      popover.classList.toggle("hidden", open);
    });
  });

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

  $$("[data-edit]").forEach((button) => {
    button.addEventListener("click", () => openClientEditor(button.dataset.edit));
  });

  $$("[data-remove]").forEach((button) => {
    button.addEventListener("click", async () => {
      if (!confirm("Remove " + button.dataset.remove + " from the allowlist?")) return;
      try {
        await api("DELETE", "api/clients/" + encodeURIComponent(button.dataset.remove));
        toast("Client removed");
        render();
      } catch (err) { toast(err.message, true); }
    });
  });

  $$("[data-token]").forEach((button) => {
    button.addEventListener("click", async () => {
      if (!confirm("Issue a new API token for " + button.dataset.token + "? The previous token stops working.")) return;
      try {
        const data = await api("POST", "api/clients/" + encodeURIComponent(button.dataset.token) + "/token");
        prompt("New API token (shown once):", data.token);
      } catch (err) { toast(err.message, true); }
    });
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
}

function openClientEditor(address) {
  const client = state.clients.find((item) => item.address === address);
  if (!client) return;
  state.editing = address;
  const quota = client.quota || {};
  const editor = $("#client-editor");
  editor.innerHTML = `
    <form class="card section editor" id="client-form">
      <header><h3>${escapeHtml(client.address)}</h3></header>
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

/* invites */

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
  const rows = state.invites.map((invite) => `
    <tr>
      <td class="mono" data-label="ID">${escapeHtml(invite.hash.slice(0, 12))}…</td>
      <td data-label="Note">${escapeHtml(invite.note || "—")}</td>
      <td class="right" data-label="Devices">${invite.used} / ${invite.uses}</td>
      <td class="right" data-label="Expires">${new Date(invite.expiresAt).toLocaleString()}</td>
      <td class="right nowrap" data-label="">
        ${invite.link ? `<button class="btn small" data-qr="${escapeHtml(invite.hash)}">QR</button>
        <button class="btn small" data-invite-copy="${escapeHtml(invite.link)}">Copy link</button>` : ""}
        <button class="btn small danger" data-invite="${escapeHtml(invite.hash)}">Delete</button>
      </td>
    </tr>`).join("");

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
      <header><h3>Active invites</h3><span class="muted small">${state.invites.length}</span></header>
      ${state.invites.length ? `<div class="table-wrap"><table class="cards">
        <thead><tr><th>ID</th><th>Note</th><th class="right">Devices</th><th class="right">Expires</th><th></th></tr></thead>
        <tbody>${rows}</tbody>
      </table></div>` : '<div class="empty">No active invites.</div>'}
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
    } catch (err) { toast(err.message, true); }
  });
  $$("[data-invite]").forEach((button) => {
    button.addEventListener("click", async () => {
      if (!confirm("Delete this invite?")) return;
      try {
        await api("DELETE", "api/invites/" + encodeURIComponent(button.dataset.invite));
        toast("Invite deleted");
        render();
      } catch (err) { toast(err.message, true); }
    });
  });
  $$("[data-qr]").forEach((button) => {
    button.addEventListener("click", () => {
      const invite = state.invites.find((item) => item.hash === button.dataset.qr);
      const view = $("#invite-view");
      if (!invite || !view) return;
      view.innerHTML = invitePanel(invite);
      wireInviteActions(view);
      view.scrollIntoView({ behavior: "smooth", block: "nearest" });
    });
  });
  wireInviteActions(document);
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

function settingsHtml() {
  const settings = state.settings || {};
  const web = settings.web || {};
  const usage = settings.usage || {};
  const manualPeers = (state.peers && state.peers.manualPeers) || [];
  const candidates = (state.peers && state.peers.candidates) || [];
  const probing = Boolean(state.peers && state.peers.probing);
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
        <label class="switch"><input type="checkbox" id="s-usage" ${usage.enabled ? "checked" : ""}><span>Traffic accounting (needed for quotas)</span></label>
        <label class="field"><span>Usage timezone</span>
          <select id="s-timezone">
            ${zones.map((zone) => `<option value="${escapeHtml(zone)}" ${zone === timezone ? "selected" : ""}>${escapeHtml(zone)}</option>`).join("")}
          </select>
        </label>
      </div>
      <p class="hint">The name is shown in the app, in the invite QR code and in the dashboard. The timezone decides when the daily and monthly quota counters roll over.</p>
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
        <label class="field full"><span>Blocked ports (comma separated)</span><input id="s-blocked" value="${(settings.blockedPorts || []).join(", ")}"></label>
      </div>
      <p class="hint">"Clients allowed" is how many Yggdrasil addresses may be in the allowlist. The connection limits are counted per client and for the whole exit.</p>
    </form>

    <form class="card section settings-card" data-settings="udp">
      <header>
        <h3>UDP relay</h3>
        <button class="btn primary small save-btn hidden" type="submit">Save</button>
      </header>
      <div class="form-grid">
        <label class="switch"><input type="checkbox" id="s-udp" ${settings.udpEnabled ? "checked" : ""}><span>Relay UDP through the exit</span></label>
        <label class="switch"><input type="checkbox" id="s-domain" ${settings.domainResolution ? "checked" : ""}><span>Resolve domain names (ATYP=domain)</span></label>
        <label class="field"><span>Max UDP associations</span><input id="s-maxudp" type="number" min="1" value="${settings.maxUDPAssociations || 64}"></label>
        <label class="field"><span>UDP idle timeout</span><input id="s-udpidle" value="${escapeHtml(settings.udpIdleTimeout || "2m")}"></label>
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
      </div>
      <p class="hint">This only controls how the exit dials destinations. The Yggdrasil mesh itself is always IPv6.</p>
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
        <button class="btn small" type="button" id="peers-refresh" ${probing ? "disabled" : ""}>${probing ? "Searching…" : "Find peers"}</button>
      </header>
      <p class="hint">A pinned peer is kept connected on top of the automatic count. It is never replaced or trimmed, so it is the right place for a private peer.</p>
      <div id="manual-peers">${manualRows}</div>
      <form id="manual-peer-pick" class="row">
        <select class="grow" id="manual-candidate" ${candidateOptions ? "" : "disabled"}>
          ${candidateOptions || '<option value="">No probed peers yet — press "Find peers"</option>'}
        </select>
        <button class="btn primary" type="submit" ${candidateOptions ? "" : "disabled"}>Add selected</button>
      </form>
      <form id="manual-peer-add" class="row">
        <input class="grow" id="manual-uri" placeholder="tls://peer.example:13338?key=…" required>
        <button class="btn" type="submit">Add by address</button>
      </form>
    </div>

    <form class="card section settings-card" data-settings="web">
      <header>
        <h3>Web interface</h3>
        <button class="btn primary small save-btn hidden" type="submit">Save</button>
      </header>
      <div class="form-grid">
        <label class="field full"><span>Public URL (shown on the dashboard and by "x-node web url")</span><input id="s-publicurl" value="${escapeHtml(web.publicURL || "")}" placeholder="https://example.com/abc123/web/"></label>
        <label class="field"><span>URL path</span><input id="s-webpath" value="${escapeHtml(web.path || "")}"></label>
        <label class="field"><span>Session lifetime</span><input id="s-sessionttl" value="${escapeHtml(web.sessionTTL || "12h")}"></label>
        <label class="field"><span>Failed logins before a block</span><input id="s-maxfail" type="number" min="0" value="${web.maxFailedLogins || 5}"></label>
        <label class="field"><span>Block duration</span><input id="s-lockout" value="${escapeHtml(web.lockout || "15m")}"></label>
        <label class="field"><span>Listen</span><input value="${escapeHtml(web.listen || "all")}" disabled></label>
        <label class="field"><span>Port</span><input value="${web.port || 443}" disabled></label>
      </div>
      <p class="hint">Listen address, port and TLS are read at start: change them in the Configuration tab and restart x-node.</p>
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
    case "web":
      return {
        web: {
          path: $("#s-webpath").value.trim(),
          publicURL: $("#s-publicurl").value.trim(),
          sessionTTL: $("#s-sessionttl").value.trim(),
          maxFailedLogins: Number($("#s-maxfail").value),
          lockout: $("#s-lockout").value.trim(),
        },
      };
    default:
      throw new Error("unknown form");
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
        const data = await api("PUT", "api/settings", settingsPayload(kind));
        toast(data.restartRequired ? "Saved · restart x-node to apply the listener change" : "Saved");
        await loadSettings();
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
      refresh.disabled = true;
      refresh.textContent = "Searching…";
      try {
        await api("POST", "api/peers", { refresh: true });
        toast("Peer search started");
        // The probe runs in the daemon; poll until it reports the result.
        for (let attempt = 0; attempt < 20; attempt += 1) {
          await new Promise((resolve) => setTimeout(resolve, 3000));
          await loadPeers();
          if (!state.peers || !state.peers.probing) break;
        }
        render();
      } catch (err) {
        toast(err.message, true);
        refresh.disabled = false;
        refresh.textContent = "Find peers";
      }
    });
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
  return `
    <div class="card section">
      <header><h3>Configuration files</h3><span class="muted small" id="config-status"></span></header>
      <p class="hint">The file is validated before it replaces the current one; a backup is written to /var/backups. Applying the main configuration removes the dashboard overlay, so the file becomes the only source of truth.</p>
      <div class="row">
        <select id="config-file" style="max-width:280px">
          <option value="main">/etc/x-node/config.json</option>
          <option value="yggdrasil">yggdrasil.conf</option>
        </select>
        <button class="btn small" id="config-load" type="button">Load</button>
        <button class="btn primary small" id="config-save" type="button">Validate and save</button>
        <button class="btn small" data-restart="x-node" type="button">Restart x-node</button>
        <button class="btn small" data-restart="yggdrasil" type="button">Restart yggdrasil</button>
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
  $("#config-load").addEventListener("click", loadConfig);
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
  loadConfig();
}

/* logs */

function logsHtml() {
  return `
    <div class="card section">
      <header>
        <h3>Recent log lines</h3>
        <label class="switch"><input type="checkbox" id="log-auto" checked><span>Auto refresh</span></label>
        <button class="btn small" id="log-refresh">Refresh</button>
      </header>
      <pre class="logs" id="log-box">${state.logs.map((entry) =>
        `<span class="${entry.level.toLowerCase()}">${escapeHtml(entry.time)} ${escapeHtml(entry.level)} ${escapeHtml(entry.message)}</span>`
      ).join("\n")}</pre>
      <p class="hint">journalctl -u x-node -f shows the full log.</p>
    </div>`;
}

function wireLogs() {
  const box = $("#log-box");
  box.scrollTop = box.scrollHeight;
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
