/* status block: live employee status, running tasks and agent output feed. */
"use strict";

const STATUS_TITLES = {
  queued: WB.t("Queued"), running: WB.t("Running"), done: WB.t("Done"),
  failed: WB.t("Failed"), canceled: WB.t("Canceled")
};

WB.register("status", {
  render: function (el) {
    el.innerHTML =
      '<div class="card"><h3>' + WB.t("Employee status") + ' <span id="ws-status" class="tag"></span></h3>' +
      '<table><thead><tr><th>' + WB.t("Employee") + '</th><th>' + WB.t("Role") + '</th><th>' + WB.t("Status") + '</th><th>' + WB.t("Last heartbeat") + '</th></tr></thead><tbody id="status-rows"></tbody></table></div>' +
      '<div class="card"><h3>' + WB.t("Task activity") + '</h3><table><thead><tr><th>' + WB.t("Task") + '</th><th>' + WB.t("Employee") + '</th><th>' + WB.t("Status") + '</th><th>' + WB.t("Time") + '</th><th>' + WB.t("Duration") + '</th></tr></thead><tbody id="task-rows"></tbody></table></div>' +
      '<div class="card"><h3>' + WB.t("Live output") + '</h3><pre id="live-log" style="max-height:240px;overflow:auto;background:#f9fafb;padding:10px;border-radius:6px;font-size:12px;"></pre></div>';
    refreshStatus();
    updateWsStatus();
  }
});

// Listeners are registered once at module level (after core.js loaded) so
// re-rendering the tab does not stack duplicate handlers (S7-Minor1).
WB.on("agent.online", updateWsStatus);
WB.on("agent.offline", updateWsStatus);
WB.on("task.state", refreshStatus);
WB.on("agent.output", onAgentOutput);

function updateWsStatus() {
  const el = document.getElementById("ws-status");
  if (el) el.textContent = WB.wsStatusText();
}

function onAgentOutput(payload) {
  const box = document.getElementById("live-log");
  if (!box) return;
  // Server broadcasts employee_id (S7-M2); escape before innerHTML.
  const who = payload.employee_id ? WB.empName(payload.employee_id) : "agent";
  box.innerHTML += "[" + WB.esc(who) + "] " + WB.esc(payload.line || "") + "\n";
  box.scrollTop = box.scrollHeight;
}

function statusTag(status) {
  return '<span class="tag ' + (status || "") + '">' + (STATUS_TITLES[status] || status || "-") + "</span>";
}

async function refreshStatus() {
  try {
    const d = await WB.api("GET", "/api/employees");
    renderStatusRows(d.employees || []);
  } catch (e) { WB.apiCatch(e, WB.t("Load employee status")); }
  try {
    const d = await WB.api("GET", "/api/tasks?status=running");
    renderTaskRows(d.tasks || []);
  } catch (e) { WB.apiCatch(e, WB.t("Load tasks")); }
}

function renderStatusRows(emps) {
  const tbody = document.getElementById("status-rows");
  if (!tbody) return;
  tbody.innerHTML = emps.map(e =>
    "<tr><td>" + WB.esc(e.name) + "</td><td>" + WB.esc(e.role) +
    '</td><td><span class="tag ' + (e.status === "online" ? "online" : e.status === "offline" ? "offline" : "") + '">' +
    (e.status === "online" ? WB.t("Online") : e.status === "offline" ? WB.t("Offline") : WB.t("Manual")) + "</span></td><td>" +
    WB.esc(e.last_seen_at || "-") + "</td></tr>"
  ).join("");
}

function renderTaskRows(tasks) {
  const tbody = document.getElementById("task-rows");
  if (!tbody) return;
  tbody.innerHTML = tasks.length === 0
    ? '<tr><td colspan="5" class="muted">' + WB.t("No tasks in progress") + '</td></tr>'
    : tasks.map(t =>
      "<tr><td>#" + t.id + "</td><td>" + WB.esc(t.employee_name || WB.empName(t.employee_id)) +
      "</td><td>" + statusTag(t.status) + "</td><td>" + WB.esc(t.updated_at || t.created_at || "") +
      '</td><td>' + WB.esc(t.duration || "—") + "</td></tr>"
    ).join("");
}
