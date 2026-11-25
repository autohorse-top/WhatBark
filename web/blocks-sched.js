/* sched block: scheduled prompts CRUD. */
"use strict";
WB.register("sched", {
  render: function (el) {
    el.innerHTML =
      '<div class="card"><h3>' + WB.t("Scheduled tasks") + '</h3>' +
      '<table><thead><tr><th>ID</th><th>' + WB.t("Name") + '</th><th>' + WB.t("Employee") + '</th><th>' + WB.t("Interval (s)") + '</th><th>Prompt</th><th>' + WB.t("Enabled") + '</th><th>' + WB.t("Last fired") + '</th><th>' + WB.t("Actions") + '</th></tr></thead><tbody id="sched-rows"></tbody></table>' +
      '<div class="form-row admin-only">' +
      '<input id="sched-name" placeholder="' + WB.t("Name") + '">' +
      '<select id="sched-emp"></select>' +
      '<input id="sched-every" type="number" placeholder="' + WB.t("Interval seconds") + '" value="3600">' +
      '<input id="sched-prompt" placeholder="' + WB.t("Scheduled prompt") + '">' +
       '<button class="btn" id="sched-add">' + WB.t("Create") + '</button>' +
       '</div></div>';
    // Lazy render: the button only exists after innerHTML injection, so it must be
    // bound here (at DOMContentLoaded bind time #block-sched is empty, so the handler could never attach — same existing bug class as the org block).
    const add = document.getElementById("sched-add");
    if (add) add.onclick = async () => {
      const body = {
        name: document.getElementById("sched-name").value.trim(),
        employee_id: Number(document.getElementById("sched-emp").value),
        every_seconds: Number(document.getElementById("sched-every").value),
        prompt: document.getElementById("sched-prompt").value.trim(),
      };
      if (!body.name || !body.prompt || !body.every_seconds) { WB.toast(WB.t("Please fill in name, employee, interval and prompt"), true); return; }
      try {
        await WB.api("POST", "/api/schedules", body);
        WB.toast(WB.t("Scheduled task created"));
        WB.blocks.sched.refresh();
      } catch (e) { WB.apiCatch(e, WB.t("Create scheduled task")); }
    };
    this.refresh();
  },
  refresh: async function () {
    const canManage = WB.canManage();
    document.querySelectorAll(".admin-only").forEach(n => { n.style.display = canManage ? "flex" : "none"; });
    if (canManage) {
      try {
        await WB.loadEmployees();
        const sel = document.getElementById("sched-emp");
        if (sel) {
          // Route B: non-admins only see their subordinates in the dropdown.
          const all = Object.values(WB.state.empIndex);
          const subs = WB.state.me && !WB.state.me.admin
            ? all.filter(e => WB.state.subordinates.indexOf(e.id) !== -1)
            : all;
          sel.innerHTML = subs.length
            ? subs.map(e => '<option value="' + e.id + '">' + WB.esc(e.name) + "</option>").join("")
            : '<option value="">' + WB.t("No subordinates") + '</option>';
        }
      } catch (e) { /* ignore */ }
    }
    try {
      const d = await WB.api("GET", "/api/schedules");
      renderSchedRows(d.schedules || []);
    } catch (e) { WB.apiCatch(e, WB.t("Load scheduled tasks")); }
  }
});

function renderSchedRows(scs) {
  const tbody = document.getElementById("sched-rows");
  if (!tbody) return;
  const canManage = WB.canManage();
  tbody.innerHTML = scs.length === 0
    ? '<tr><td colspan="8" class="muted">' + WB.t("No scheduled tasks") + '</td></tr>'
    : scs.map(s =>
      "<tr><td>" + s.id + "</td><td>" + WB.esc(s.name) + "</td><td>" + WB.esc(s.employee_name || WB.empName(s.employee_id)) +
      "</td><td>" + s.every_seconds + "</td><td>" + WB.esc(s.prompt) + "</td><td>" +
      (s.enabled ? '<span class="tag online">' + WB.t("Enabled") + '</span>' : '<span class="tag canceled">' + WB.t("Disabled") + '</span>') +
      "</td><td>" + WB.esc(s.last_fired_at || "-") + "</td><td>" +
      (canManage
        ? '<button class="btn small secondary" data-toggle="' + s.id + '">' + (s.enabled ? WB.t("Disable") : WB.t("Enable")) + '</button> ' +
          '<button class="btn small danger" data-del-sched="' + s.id + '">' + WB.t("Delete") + '</button>'
        : "") + "</td></tr>"
    ).join("");
  tbody.querySelectorAll("[data-toggle]").forEach(b => {
    b.onclick = async () => {
      const id = Number(b.getAttribute("data-toggle"));
      try {
        const cur = await WB.api("GET", "/api/schedules");
        const sc = (cur.schedules || []).find(x => x.id === id);
        if (!sc) return;
        await WB.api("PATCH", "/api/schedules/" + id, { enabled: !sc.enabled });
        WB.blocks.sched.refresh();
      } catch (e) { WB.apiCatch(e, WB.t("Toggle enabled state")); }
    };
  });
  tbody.querySelectorAll("[data-del-sched]").forEach(b => {
    b.onclick = async () => {
      if (!confirm(WB.t("Confirm delete this scheduled task?"))) return;
      try {
        await WB.api("DELETE", "/api/schedules/" + b.getAttribute("data-del-sched"));
        WB.toast(WB.t("Scheduled task deleted"));
        WB.blocks.sched.refresh();
      } catch (e) { WB.apiCatch(e, WB.t("Delete scheduled task")); }
    };
  });
}
