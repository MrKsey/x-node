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
  quotas: [],
  settings: null,
  peers: null,
  logs: [],
  samples: [],
  view: "dashboard",
  timer: null,
  logTimer: null,
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
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  let number = Number(value) || 0;
  let index = 0;
  while (number >= 1024 && index < units.length - 1) {
    number /= 1024;
    index += 1;
  }
  const digits = number >= 100 || index === 0 ? 0 : 1;
  return number.toFixed(digits) + " " + units[index];
}

function rate(kbps) {
  const value = Number(kbps) || 0;
  if (value <= 0) return "unlimited";
  if (value >= 1000000) return (value / 1000000).toFixed(1) + " Gbps";
  if (value >= 1000) return (value / 1000).toFixed(1) + " Mbps";
  return value + " Kbps";
}

function duration(seconds) {
  let value = Math.max(0, Math.floor(seconds));
  const days = Math.floor(value / 86400); value -= days * 86400;
  const hours = Math.floor(value / 3600); value -= hours * 3600;
  const minutes = Math.floor(value / 60);
  if (days > 0) return days + "d " + hours + "h";
  if (hours > 0) return hours + "h " + minutes + "m";
  return minutes + "m";
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
  refreshStatus();
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
  quotas: "Quotas",
  settings: "Settings",
  logs: "Logs",
};

function navigate(view) {
  if (!titles[view]) view = "dashboard";
  state.view = view;
  $("#view-title").textContent = titles[view];
  $$("#nav a").forEach((link) => link.classList.toggle("active", link.dataset.view === view));
  $("#app").classList.remove("menu-open");
  render();
}

window.addEventListener("hashchange", () => navigate(location.hash.replace("#", "")));
$("#menu").addEventListener("click", () => $("#app").classList.toggle("menu-open"));
$$("[data-theme-set]").forEach((button) => {
  button.addEventListener("click", () => applyTheme(button.dataset.themeSet));
});

/* ---------- data ---------- */

async function refreshStatus() {
  try {
    const status = await api("GET", "api/status");
    state.status = status;
    const health = $("#health");
    health.textContent = "online · " + duration(status.uptimeSeconds);
    health.className = "pill ok";
    $("#side-name").textContent = status.name || "";
    $("#clock").textContent = new Date().toLocaleTimeString();
    if (state.view === "dashboard") renderDashboard();
    if (state.view === "quotas") renderQuotas();
  } catch (err) {
    const health = $("#health");
    health.textContent = "offline";
    health.className = "pill bad";
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

async function loadQuotas() {
  const data = await api("GET", "api/quotas");
  state.quotas = data.clients || [];
  return state.quotas;
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
      await Promise.all([refreshStatus(), loadPeers()]);
      view.innerHTML = dashboardHtml();
      drawChart();
    } else if (state.view === "clients") {
      await loadClients();
      view.innerHTML = clientsHtml();
      wireClients();
    } else if (state.view === "invites") {
      await loadInvites();
      view.innerHTML = invitesHtml();
      wireInvites();
    } else if (state.view === "quotas") {
      await Promise.all([loadQuotas(), loadClients()]);
      view.innerHTML = quotasHtml();
      wireQuotas();
    } else if (state.view === "settings") {
      await loadSettings();
      view.innerHTML = settingsHtml();
      wireSettings();
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

function dashboardHtml() {
  const status = state.status || {};
  const peers = state.peers || {};
  const chart = '<canvas class="chart" id="chart"></canvas>';
  return `
    <div class="grid kpi">
      <div class="card kpi-card">
        <div class="label">Yggdrasil address</div>
        <div class="value mono" style="font-size:15px;word-break:break-all">${escapeHtml(status.address || "—")}</div>
        <div class="sub">${escapeHtml(status.name || "")}</div>
      </div>
      <div class="card kpi-card">
        <div class="label">Clients</div>
        <div class="value">${status.clients || 0}</div>
        <div class="sub">${status.activeConnections || 0} TCP · ${status.activeUDP || 0} UDP</div>
      </div>
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

    <div class="grid two">
      <div class="card section">
        <header><h3>Traffic</h3><span class="muted small">last 5 minutes</span></header>
        ${chart}
      </div>
      <div class="card section">
        <header><h3>Health</h3></header>
        <table>
          <tr><th>Service</th><td class="right"><span class="pill ok">running</span></td></tr>
          <tr><th>Version</th><td class="right">${escapeHtml(status.version || "")}</td></tr>
          <tr><th>SOCKS5</th><td class="right mono">[${escapeHtml(status.address || "")}]:${status.socksPort || ""}</td></tr>
          <tr><th>UDP relay</th><td class="right">${status.udpEnabled ? "enabled" : "disabled"}</td></tr>
          <tr><th>Open mode</th><td class="right">${status.allowAll ? '<span class="pill warn">allowAll</span>' : "allowlist"}</td></tr>
          <tr><th>IPv6 outbound</th><td class="right">${status.ipv6Available ? '<span class="pill ok">' + escapeHtml(status.ipv6Mode) + "</span>" : '<span class="pill warn">IPv4 only</span>'}</td></tr>
          <tr><th>Public peers</th><td class="right">${peers.up || 0} up · ${peers.managed || 0} managed</td></tr>
        </table>
      </div>
    </div>

    <div class="card section">
      <header><h3>Web interface</h3><span class="muted small">${escapeHtml(status.webPath || "")}</span></header>
      <p class="hint">Open this address from a browser that can reach the server. A certificate warning is expected while the node uses a self-signed certificate.</p>
      <div class="actions">
        ${(status.webURLs || []).map((url) => `<button class="btn small" data-copy="${escapeHtml(url)}">${escapeHtml(url)}</button>`).join("")}
      </div>
      ${status.pendingRestart ? '<p class="error">Some settings need a restart: sudo systemctl restart x-node</p>' : ""}
    </div>
  `;
}

function drawChart() {
  const canvas = $("#chart");
  if (!canvas || !state.status) return;
  const status = state.status;
  state.samples.push({
    at: Date.now(),
    rx: (status.todayRx || 0) + (status.todayTx || 0),
  });
  if (state.samples.length > 120) state.samples.shift();

  const ratio = window.devicePixelRatio || 1;
  const width = canvas.clientWidth || 600;
  const height = canvas.clientHeight || 160;
  canvas.width = width * ratio;
  canvas.height = height * ratio;
  const ctx = canvas.getContext("2d");
  ctx.scale(ratio, ratio);
  ctx.clearRect(0, 0, width, height);

  const style = getComputedStyle(document.documentElement);
  const border = style.getPropertyValue("--border").trim() || "#ddd";
  const accent = style.getPropertyValue("--accent").trim() || "#3d5afe";

  ctx.strokeStyle = border;
  ctx.lineWidth = 1;
  for (let i = 1; i < 4; i += 1) {
    const y = (height / 4) * i;
    ctx.beginPath();
    ctx.moveTo(0, y);
    ctx.lineTo(width, y);
    ctx.stroke();
  }

  const samples = state.samples;
  if (samples.length < 2) {
    ctx.fillStyle = style.getPropertyValue("--muted").trim() || "#888";
    ctx.font = "13px sans-serif";
    ctx.fillText("Collecting samples…", 12, 24);
    return;
  }
  let max = 1;
  for (let i = 1; i < samples.length; i += 1) {
    const delta = samples[i].rx - samples[i - 1].rx;
    if (delta > max) max = delta;
  }
  ctx.beginPath();
  samples.forEach((sample, index) => {
    const previous = index === 0 ? sample.rx : samples[index - 1].rx;
    const delta = Math.max(0, sample.rx - previous);
    const x = (index / (samples.length - 1)) * width;
    const y = height - (delta / max) * (height - 12) - 6;
    if (index === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
  });
  ctx.lineTo(width, height);
  ctx.lineTo(0, height);
  ctx.closePath();
  ctx.fillStyle = accent + "22";
  ctx.fill();
  ctx.strokeStyle = accent;
  ctx.lineWidth = 2;
  ctx.stroke();
}

/* clients */

function clientsHtml() {
  const rows = state.clients.map((client) => {
    const used = (client.quota && client.quota.monthlyBytes > 0)
      ? Math.min(100, (client.quota.monthUsed / client.quota.monthlyBytes) * 100)
      : 0;
    const barClass = used >= 95 ? "bar bad" : used >= 75 ? "bar warn" : "bar";
    return `
      <tr>
        <td>
          <div class="mono">${escapeHtml(client.address)}</div>
          <div class="muted small">${escapeHtml(client.label || "no label")}</div>
        </td>
        <td><span class="pill ${client.state === "connected" ? "ok" : ""}">${escapeHtml(client.state)}</span></td>
        <td class="right">${client.connections || 0}</td>
        <td class="right">${client.udpAssociations || 0}</td>
        <td class="right">${bytes((client.dayUsed || 0))}</td>
        <td class="right">${bytes((client.monthUsed || 0))}</td>
        <td style="min-width:120px">
          <div class="${barClass}"><i style="width:${used}%"></i></div>
          <div class="muted small">${client.quota && client.quota.monthlyBytes > 0 ? bytes(client.quota.monthlyBytes) : "no cap"}</div>
        </td>
        <td class="right hide-sm">${when(client.lastSeenAt)}</td>
        <td class="right nowrap">
          <button class="btn small" data-token="${escapeHtml(client.address)}">Token</button>
          <button class="btn small danger" data-remove="${escapeHtml(client.address)}">Remove</button>
        </td>
      </tr>`;
  }).join("");

  return `
    <div class="card section">
      <header>
        <h3>Registered clients</h3>
        <span class="muted small">${state.clients.length} total</span>
      </header>
      ${state.clients.length ? `<div class="table-wrap"><table>
        <thead><tr>
          <th>Client</th><th>State</th><th class="right">TCP</th><th class="right">UDP</th>
          <th class="right">Today</th><th class="right">Month</th><th>Quota</th>
          <th class="right hide-sm">Last seen</th><th></th>
        </tr></thead>
        <tbody>${rows}</tbody>
      </table></div>` : '<div class="empty">No clients yet. Create an invite or add an address.</div>'}
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
  `;
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
}

/* invites */

function invitesHtml() {
  const rows = state.invites.map((invite) => `
    <tr>
      <td class="mono">${escapeHtml(invite.hash.slice(0, 12))}…</td>
      <td>${escapeHtml(invite.note || "—")}</td>
      <td class="right">${invite.used} / ${invite.uses}</td>
      <td class="right">${new Date(invite.expiresAt).toLocaleString()}</td>
      <td class="right"><button class="btn small danger" data-invite="${escapeHtml(invite.hash)}">Delete</button></td>
    </tr>`).join("");

  return `
    <div class="card section">
      <header><h3>New invite</h3></header>
      <p class="hint">The link is shown once. Open it on the phone with Y-Node, or paste it into Exit node → Add → Import from link.</p>
      <form id="new-invite" class="form-grid">
        <label class="field"><span>Note</span><input id="invite-note" placeholder="family phone"></label>
        <label class="field"><span>Uses</span><input id="invite-uses" type="number" min="1" max="100" value="1"></label>
        <label class="field"><span>Valid for</span>
          <select id="invite-ttl">
            <option value="1h">1 hour</option>
            <option value="24h" selected>24 hours</option>
            <option value="168h">7 days</option>
          </select>
        </label>
        <div class="full"><button class="btn primary" type="submit">Create invite</button></div>
      </form>
      <div id="invite-result"></div>
    </div>

    <div class="card section">
      <header><h3>Active invites</h3><span class="muted small">${state.invites.length}</span></header>
      ${state.invites.length ? `<div class="table-wrap"><table>
        <thead><tr><th>ID</th><th>Note</th><th class="right">Used</th><th class="right">Expires</th><th></th></tr></thead>
        <tbody>${rows}</tbody>
      </table></div>` : '<div class="empty">No active invites.</div>'}
    </div>
  `;
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
      $("#invite-result").innerHTML = `
        <div class="card section" style="margin-top:14px">
          <header><h3>Invite created</h3></header>
          <p class="hint">Expires ${new Date(data.expiresAt).toLocaleString()} · ${data.uses} use(s)</p>
          <div class="mono" style="word-break:break-all">${escapeHtml(data.link)}</div>
          <div class="actions">
            <button class="btn primary small" id="copy-invite">Copy link</button>
          </div>
        </div>`;
      $("#copy-invite").addEventListener("click", () => copy(data.link));
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
}

/* quotas */

function quotasHtml() {
  const rows = state.quotas.map((client) => {
    const monthly = client.monthlyBytes > 0
      ? Math.min(100, (client.monthUsed / client.monthlyBytes) * 100) : 0;
    const barClass = monthly >= 95 ? "bar bad" : monthly >= 75 ? "bar warn" : "bar";
    return `
      <tr>
        <td class="mono">${escapeHtml(client.address)}</td>
        <td class="right">${bytes(client.dayUsed)}</td>
        <td class="right">${bytes(client.monthUsed)}</td>
        <td class="right">${client.dailyBytes > 0 ? bytes(client.dailyBytes) : "—"}</td>
        <td class="right">${client.monthlyBytes > 0 ? bytes(client.monthlyBytes) : "—"}</td>
        <td class="right">${rate(client.downKbps)}</td>
        <td class="right">${rate(client.upKbps)}</td>
        <td style="min-width:110px"><div class="${barClass}"><i style="width:${monthly}%"></i></div></td>
        <td class="right nowrap">
          <button class="btn small" data-quota="${escapeHtml(client.address)}">Edit</button>
          <button class="btn small danger" data-quota-reset="${escapeHtml(client.address)}">Reset</button>
        </td>
      </tr>`;
  }).join("");

  return `
    <div class="card section">
      <header><h3>Per-client quotas</h3><span class="muted small">enforced on new connections</span></header>
      <p class="hint">Zero means no limit. Quotas are checked when a connection or UDP association starts; open connections are not interrupted.</p>
      ${state.quotas.length ? `<div class="table-wrap"><table>
        <thead><tr>
          <th>Client</th><th class="right">Today</th><th class="right">Month</th>
          <th class="right">Daily cap</th><th class="right">Monthly cap</th>
          <th class="right">Down</th><th class="right">Up</th><th></th><th></th>
        </tr></thead><tbody>${rows}</tbody>
      </table></div>` : '<div class="empty">No clients.</div>'}
      <div id="quota-editor"></div>
    </div>

    <div class="card section">
      <header><h3>Default quota</h3></header>
      <p class="hint">Applied to every client without its own entry.</p>
      <form id="quota-defaults" class="form-grid">
        <label class="field"><span>Monthly cap (GiB, 0 = none)</span><input id="def-monthly" type="number" min="0" step="1" value="${(state.settings && state.settings.quotas.monthlyBytes ? state.settings.quotas.monthlyBytes / 1073741824 : 0)}"></label>
        <label class="field"><span>Daily cap (GiB, 0 = none)</span><input id="def-daily" type="number" min="0" step="1" value="${(state.settings && state.settings.quotas.dailyBytes ? state.settings.quotas.dailyBytes / 1073741824 : 0)}"></label>
        <label class="field"><span>Download (Mbps, 0 = none)</span><input id="def-down" type="number" min="0" step="1" value="${(state.settings ? state.settings.quotas.downKbps / 1000 : 0)}"></label>
        <label class="field"><span>Upload (Mbps, 0 = none)</span><input id="def-up" type="number" min="0" step="1" value="${(state.settings ? state.settings.quotas.upKbps / 1000 : 0)}"></label>
        <div class="full"><button class="btn primary" type="submit">Save defaults</button></div>
      </form>
    </div>
  `;
}

function wireQuotas() {
  $$("[data-quota]").forEach((button) => {
    button.addEventListener("click", () => {
      const client = state.quotas.find((item) => item.address === button.dataset.quota);
      if (!client) return;
      $("#quota-editor").innerHTML = `
        <form class="card section" id="quota-form" style="margin-top:14px">
          <header><h3>${escapeHtml(client.address)}</h3></header>
          <div class="form-grid">
            <label class="field"><span>Monthly cap (GiB, 0 = none)</span><input id="q-monthly" type="number" min="0" step="1" value="${client.monthlyBytes / 1073741824}"></label>
            <label class="field"><span>Daily cap (GiB, 0 = none)</span><input id="q-daily" type="number" min="0" step="1" value="${client.dailyBytes / 1073741824}"></label>
            <label class="field"><span>Download (Mbps, 0 = none)</span><input id="q-down" type="number" min="0" step="1" value="${client.downKbps / 1000}"></label>
            <label class="field"><span>Upload (Mbps, 0 = none)</span><input id="q-up" type="number" min="0" step="1" value="${client.upKbps / 1000}"></label>
            <div class="full actions">
              <button class="btn primary" type="submit">Save</button>
              <button class="btn" type="button" id="q-cancel">Cancel</button>
            </div>
          </div>
        </form>`;
      $("#q-cancel").addEventListener("click", () => { $("#quota-editor").innerHTML = ""; });
      $("#quota-form").addEventListener("submit", async (event) => {
        event.preventDefault();
        try {
          await api("PUT", "api/quotas/" + encodeURIComponent(client.address), {
            monthlyBytes: Math.round(Number($("#q-monthly").value) * 1073741824),
            dailyBytes: Math.round(Number($("#q-daily").value) * 1073741824),
            downKbps: Math.round(Number($("#q-down").value) * 1000),
            upKbps: Math.round(Number($("#q-up").value) * 1000),
          });
          toast("Quota saved");
          render();
        } catch (err) { toast(err.message, true); }
      });
    });
  });

  $$("[data-quota-reset]").forEach((button) => {
    button.addEventListener("click", async () => {
      if (!confirm("Reset the quota and the traffic counters of " + button.dataset.quotaReset + "?")) return;
      try {
        await api("DELETE", "api/quotas/" + encodeURIComponent(button.dataset.quotaReset));
        toast("Quota reset");
        render();
      } catch (err) { toast(err.message, true); }
    });
  });

  $("#quota-defaults").addEventListener("submit", async (event) => {
    event.preventDefault();
    try {
      await api("PUT", "api/settings", {
        quotas: {
          monthlyBytes: Math.round(Number($("#def-monthly").value) * 1073741824),
          dailyBytes: Math.round(Number($("#def-daily").value) * 1073741824),
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

/* settings */

function settingsHtml() {
  const settings = state.settings || {};
  const web = settings.web || {};
  return `
    <form id="settings-form" class="grid">
      <div class="card section">
        <header><h3>Access</h3></header>
        <div class="form-grid">
          <label class="switch"><input type="checkbox" id="s-allowall" ${settings.allowAll ? "checked" : ""}><span>Open mode (allowAll)</span></label>
          <label class="switch"><input type="checkbox" id="s-registration" ${settings.registration ? "checked" : ""}><span>Registration API</span></label>
          <label class="field"><span>Max clients</span><input id="s-maxclients" type="number" min="1" value="${settings.maxClients || 64}"></label>
          <label class="field"><span>Invite lifetime</span><input id="s-invitetll" value="${escapeHtml(settings.inviteTTL || "24h")}"></label>
          <label class="field"><span>Invite uses</span><input id="s-inviteuses" type="number" min="1" value="${settings.inviteUses || 1}"></label>
        </div>
      </div>

      <div class="card section">
        <header><h3>Limits</h3></header>
        <div class="form-grid">
          <label class="field"><span>Connections per client</span><input id="s-maxconns" type="number" min="1" value="${settings.maxConnsPerClient || 64}"></label>
          <label class="field"><span>Connections total</span><input id="s-maxtotal" type="number" min="1" value="${settings.maxConnsTotal || 512}"></label>
          <label class="field"><span>Idle timeout</span><input id="s-idle" value="${escapeHtml(settings.idleTimeout || "5m")}"></label>
          <label class="field"><span>Dial timeout</span><input id="s-dial" value="${escapeHtml(settings.dialTimeout || "15s")}"></label>
          <label class="field full"><span>Blocked ports (comma separated)</span><input id="s-blocked" value="${(settings.blockedPorts || []).join(", ")}"></label>
        </div>
      </div>

      <div class="card section">
        <header><h3>UDP and IPv6</h3></header>
        <div class="form-grid">
          <label class="switch"><input type="checkbox" id="s-udp" ${settings.udpEnabled ? "checked" : ""}><span>UDP relay</span></label>
          <label class="switch"><input type="checkbox" id="s-domain" ${settings.domainResolution ? "checked" : ""}><span>Resolve domain names (ATYP=domain)</span></label>
          <label class="field"><span>Max UDP associations</span><input id="s-maxudp" type="number" min="1" value="${settings.maxUDPAssociations || 64}"></label>
          <label class="field"><span>UDP idle timeout</span><input id="s-udpidle" value="${escapeHtml(settings.udpIdleTimeout || "2m")}"></label>
          <label class="field"><span>IPv6 outbound</span>
            <select id="s-ipv6">
              <option value="auto" ${settings.ipv6 === "auto" ? "selected" : ""}>auto — use it when available</option>
              <option value="prefer" ${settings.ipv6 === "prefer" ? "selected" : ""}>prefer — try IPv6 first</option>
              <option value="off" ${settings.ipv6 === "off" ? "selected" : ""}>off — IPv4 only</option>
            </select>
          </label>
        </div>
      </div>

      <div class="card section">
        <header><h3>Peers and logging</h3></header>
        <div class="form-grid">
          <label class="field"><span>Public peers</span><input id="s-peercount" type="number" min="1" max="50" value="${settings.peerCount || 3}"></label>
          <label class="switch"><input type="checkbox" id="s-peerauto" ${settings.peerAutoReplace ? "checked" : ""}><span>Replace dead or slow peers</span></label>
          <label class="field"><span>Log level</span>
            <select id="s-loglevel">
              ${["debug", "info", "warn", "error"].map((level) => `<option value="${level}" ${settings.logLevel === level ? "selected" : ""}>${level}</option>`).join("")}
            </select>
          </label>
          <label class="field"><span>Usage timezone</span><input id="s-timezone" value="${escapeHtml((settings.usage && settings.usage.timezone) || "UTC")}"></label>
        </div>
      </div>

      <div class="card section">
        <header><h3>Web interface</h3></header>
        <p class="hint">Network settings are read at start. Change them in /etc/x-node/config.json and restart the service.</p>
        <div class="form-grid">
          <label class="field"><span>URL path</span><input id="s-webpath" value="${escapeHtml(web.path || "")}"></label>
          <label class="field"><span>Session lifetime</span><input id="s-sessionttl" value="${escapeHtml(web.sessionTTL || "12h")}"></label>
          <label class="field"><span>Failed logins before a block</span><input id="s-maxfail" type="number" min="0" value="${web.maxFailedLogins || 5}"></label>
          <label class="field"><span>Block duration</span><input id="s-lockout" value="${escapeHtml(web.lockout || "15m")}"></label>
          <label class="field"><span>Listen</span><input value="${escapeHtml(web.listen || "all")}" disabled></label>
          <label class="field"><span>Port</span><input value="${web.port || 443}" disabled></label>
        </div>
      </div>

      <div class="actions">
        <button class="btn primary" type="submit">Save settings</button>
        <span class="muted small">Changes apply to new connections immediately. Network listeners need a restart.</span>
      </div>
    </form>
  `;
}

function wireSettings() {
  $("#settings-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const blocked = $("#s-blocked").value.split(",").map((item) => Number(item.trim())).filter((item) => item > 0);
    const payload = {
      allowAll: $("#s-allowall").checked,
      registration: $("#s-registration").checked,
      maxClients: Number($("#s-maxclients").value),
      inviteTTL: $("#s-invitetll").value.trim(),
      inviteUses: Number($("#s-inviteuses").value),
      maxConnsPerClient: Number($("#s-maxconns").value),
      maxConnsTotal: Number($("#s-maxtotal").value),
      idleTimeout: $("#s-idle").value.trim(),
      dialTimeout: $("#s-dial").value.trim(),
      blockedPorts: blocked,
      udpEnabled: $("#s-udp").checked,
      domainResolution: $("#s-domain").checked,
      maxUDPAssociations: Number($("#s-maxudp").value),
      udpIdleTimeout: $("#s-udpidle").value.trim(),
      ipv6: $("#s-ipv6").value,
      peerCount: Number($("#s-peercount").value),
      peerAutoReplace: $("#s-peerauto").checked,
      logLevel: $("#s-loglevel").value,
      usage: { enabled: true, timezone: $("#s-timezone").value.trim() },
      web: {
        path: $("#s-webpath").value.trim(),
        sessionTTL: $("#s-sessionttl").value.trim(),
        maxFailedLogins: Number($("#s-maxfail").value),
        lockout: $("#s-lockout").value.trim(),
      },
    };
    try {
      await api("PUT", "api/settings", payload);
      toast("Settings saved");
      await loadSettings();
      render();
    } catch (err) { toast(err.message, true); }
  });
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
