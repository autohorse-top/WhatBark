/* settings block (S9): account, server management and connection status.
 * Server management is shared with the admin "servers" block via the
 * renderServersPanel helper (blocks-servers.js). */
"use strict";
let serversExpanded = false;

WB.register("settings", {
  render: function (el) {
    const isAdmin = !!(WB.state.me && WB.state.me.admin);
    let html =
      '<div class="card"><h3>' + WB.t("Account") + '</h3><div id="settings-account"></div></div>';

    // Progressive disclosure: with a single server and no manual expansion, show only a one-line summary
    const collapsed = WB.state.servers.length <= 1 && !serversExpanded;
    if (collapsed) {
      const srv = WB.state.servers[0];
      html +=
        '<div class="card"><h3>' + WB.t("Servers") + '</h3>' +
        '<p>' + WB.t("Current: ") + WB.esc(srv ? srv.name || srv.id : "") + ' (' + WB.esc(srv ? srv.url : "") + ')</p>' +
        '<button class="btn small secondary" id="srv-expand">' + WB.t("Manage servers") + '</button>' +
        '</div>';
    } else {
      html +=
        '<div class="card"><h3>' + WB.t("Servers") + '</h3>' +
        '<p class="note">' + WB.t("Server list and tokens are stored in browser localStorage. The token entered when adding a server is used to auto-login when switching to that server.") + '</p>' +
        '<div class="servers-panel"></div></div>';
    }

    if (isAdmin) {
      html +=
        '<div class="card"><h3>' + WB.t("Audit Log") + '</h3>' +
        '<p class="note">' + WB.t("Most recent 100 admin operations and auth failures (read-only).") + '</p>' +
        '<div id="settings-audit"></div></div>' +
        '<div class="card"><h3>' + WB.t("Storage") + '</h3>' +
        '<div id="settings-storage"></div></div>';
    }
    html +=
      '<div class="card"><h3>' + WB.t("Connection") + '</h3>' +
      '<p id="settings-ws"></p>' +
      '<button class="btn small secondary" id="settings-ws-reconnect">' + WB.t("Reconnect") + '</button>' +
      '</div>';
    // S39: Language card — one button per language available from the server
    // (/api/i18n/languages, populated into WB.state.langs on boot).
    html +=
      '<div class="card" id="settings-lang"><h3>' + WB.t("Language") + '</h3>' +
      '<p class="note">' + WB.t("Interface language. Only languages with an installed language file are listed.") + '</p>' +
      '<div class="form-row">' +
      WB.state.langs.map(function (code) {
        const active = WB.state.lang === code ? " active" : "";
        return '<button class="btn small' + active + '" data-lang="' + code + '">' +
          (WB.LANG_NAMES[code] || code) + '</button>';
      }).join("") +
      '</div></div>';
    el.innerHTML = html;
    renderSettingsAccount(el);

    if (collapsed) {
      el.querySelector("#srv-expand").onclick = () => {
        serversExpanded = true;
        WB.blocks.settings.render(el);
      };
    } else {
      renderServersPanel(el.querySelector(".servers-panel"));
    }

    if (isAdmin) {
      renderSettingsAudit(el);
      renderSettingsStorage(el);
    }
    el.querySelector("#settings-ws-reconnect").onclick = () => {
      WB.connectWS();
      WB.toast(WB.t("Reconnect triggered"));
    };
    el.querySelectorAll("#settings-lang [data-lang]").forEach(function (btn) {
      btn.onclick = () => WB.setLang(btn.getAttribute("data-lang"));
    });
    updateSettingsWs();
  },
});

// H-7: admin-only audit log (newest 100, read-only).
async function renderSettingsAudit(el) {
  const host = el.querySelector("#settings-audit");
  if (!host) return;
  host.innerHTML = '<p class="muted">' + WB.t("Loading…") + '</p>';
  try {
    const d = await WB.api("GET", "/api/audit?limit=100");
    const items = (d && d.items) || [];
    if (!items.length) {
      host.innerHTML = '<p class="muted">' + WB.t("No audit records yet") + '</p>';
      return;
    }
    const actor = (it) =>
      it.actor_kind === "admin" ? WB.t("boss (admin)")
        : it.actor_kind === "employee" ? "Employee #" + it.actor_id
        : WB.t("Anonymous");
    host.innerHTML =
      '<table><thead><tr><th>' + WB.t("Time") + '</th><th>' + WB.t("Actor") + '</th><th>' + WB.t("Action") + '</th><th>' + WB.t("Target") + '</th><th>' + WB.t("Details") + '</th></tr></thead><tbody>' +
      items.map((it) =>
        "<tr><td>" + WB.esc(it.ts) + "</td>" +
        "<td>" + WB.esc(actor(it)) + "</td>" +
        "<td>" + WB.esc(it.action) + "</td>" +
        "<td>" + WB.esc(it.target) + "</td>" +
        "<td>" + WB.esc(it.detail || "") + "</td></tr>"
      ).join("") + "</tbody></table>";
  } catch (e) {
    WB.apiCatch(e, WB.t("Load audit log"));
    host.innerHTML = '<p class="muted">' + WB.t("Failed to load audit log") + '</p>';
  }
}

// Disk management: admin-only storage status (GET /api/admin/storage) plus
// the policy editor (POST /api/settings). The path field only applies on the
// next restart; numeric fields apply immediately.
function humanBytesJS(n) {
  if (n == null || n < 0) return "—";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let x = Number(n);
  let i = 0;
  while (x >= 1024 && i < u.length - 1) { x /= 1024; i++; }
  return (i === 0 ? x : Math.round(x * 10) / 10) + " " + u[i];
}

async function renderSettingsStorage(el) {
  const host = el.querySelector("#settings-storage");
  if (!host) return;
  host.innerHTML = '<p class="muted">' + WB.t("Loading…") + '</p>';
  let info;
  try {
    info = await WB.api("GET", "/api/admin/storage");
  } catch (e) {
    WB.apiCatch(e, WB.t("Load storage status"));
    host.innerHTML = '<p class="muted">' + WB.t("Failed to load storage status") + '</p>';
    return;
  }
  let raw = {};
  try {
    raw = await WB.api("GET", "/api/settings");
  } catch (e) { /* non-fatal: path prefill stays empty */ }
  const MIB = 1048576;
  host.innerHTML =
    "<p>" + WB.t("Path: ") + WB.esc(info.path) + "</p>" +
    "<p>" + WB.t("Used: ") + humanBytesJS(info.used_bytes) + " / " +
      (info.quota_bytes > 0 ? humanBytesJS(info.quota_bytes) : WB.t("unlimited")) + "</p>" +
    "<p>" + WB.t("Free disk: ") + humanBytesJS(info.disk_free_bytes) + "</p>" +
    "<p>" + WB.t("Artifacts: ") + info.artifacts.count + " (" + humanBytesJS(info.artifacts.bytes) +
      "); " + WB.t("Files: ") + info.files.count + " (" + humanBytesJS(info.files.bytes) + ")</p>" +
    "<p>" + WB.t("Retention: ") + (info.retention_days > 0 ? info.retention_days + " " + WB.t("days") : WB.t("off")) + "</p>" +
    '<div class="form-row">' +
    '<label>' + WB.t("Max file size (MiB): ") +
    '<input id="storage-max-file" type="number" min="1" max="1024" step="1" value="' + Math.round(info.max_file_bytes / MIB) + '"></label>' +
    "</div>" +
    '<div class="form-row">' +
    '<label>' + WB.t("Total quota (MiB, 0 = unlimited): ") +
    '<input id="storage-quota" type="number" min="0" max="1048576" step="1" value="' + Math.round(info.quota_bytes / MIB) + '"></label>' +
    "</div>" +
    '<div class="form-row">' +
    '<label>' + WB.t("Retention (days, 0 = off): ") +
    '<input id="storage-retention" type="number" min="0" max="3650" step="1" value="' + info.retention_days + '"></label>' +
    "</div>" +
    '<div class="form-row">' +
    '<label>' + WB.t("Storage path: ") +
    '<input id="storage-path" type="text" maxlength="512" value="' + WB.esc(raw["storage_path"] || "") + '" placeholder="' + WB.esc(info.path) + '"></label>' +
    "</div>" +
    '<p class="note">' + WB.t("Storage path applies on next restart; existing files are not moved.") + '</p>' +
    '<div class="form-row"><button class="btn small" id="storage-save">' + WB.t("Save") + '</button></div>';
  host.querySelector("#storage-save").onclick = async function () {
    const btn = this;
    const payload = {
      storage_max_file_bytes: String(Math.round(Number(host.querySelector("#storage-max-file").value)) * MIB),
      storage_max_bytes: String(Math.round(Number(host.querySelector("#storage-quota").value)) * MIB),
      storage_retention_days: String(Math.round(Number(host.querySelector("#storage-retention").value))),
      storage_path: host.querySelector("#storage-path").value.trim()
    };
    btn.disabled = true;
    try {
      await WB.api("POST", "/api/settings", payload);
      WB.toast(WB.t("Storage settings saved"));
      renderSettingsStorage(el);
    } catch (e) {
      WB.apiCatch(e, WB.t("Save storage settings"));
    } finally {
      btn.disabled = false;
    }
  };
}

function renderSettingsAccount(el) {
  const host = el.querySelector("#settings-account");
  const me = WB.state.me;
  if (!me) {
    host.innerHTML = '<p class="muted">' + WB.t("Not signed in") + '</p>';
    return;
  }
  const name = me.admin ? WB.t("boss (admin)")
    : (me.employee ? me.employee.name : "Employee #" + me.employee_id);
  // S15: avatar row (enhanced on all clients; after the mobile rail is hidden this is the avatar entry point, same capability as S12).
  const avaName = me.admin ? "boss" : (me.employee ? me.employee.name : "Employee");
  const avaID = me.admin ? 0 : me.employee_id;
  host.innerHTML =
    '<div class="form-row">' +
    WB.avatarSpan(avaName, "set-ava", avaID) +
    '<button class="btn small" id="settings-change-ava">' + WB.t("Change avatar") + '</button>' +
    "</div>" +
    "<p>" + WB.t("Current identity: ") + WB.esc(name) + "</p>" +
    '<div class="form-row">' +
    '<button class="btn small danger" id="settings-logout">' + WB.t("Sign out") + '</button>' +
    "</div>";
  el.querySelector("#settings-change-ava").onclick = () => WB.changeAvatar();
  el.querySelector("#settings-logout").onclick = () => WB.logout();
}

function updateSettingsWs() {
  const el = document.getElementById("settings-ws");
  if (el) el.textContent = WB.wsStatusText();
}
WB.on("ws.state", updateSettingsWs);
