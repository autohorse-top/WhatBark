/* org block: department tree + employee management (admin edits, others read-only). */
"use strict";
WB.register("org", {
  render: function (el) {
    const isAdmin = !!(WB.state.me && WB.state.me.admin);
    const chkTh = isAdmin ? '<th class="emp-chk-col"></th>' : '';
    const reissueBtn = isAdmin ? '<button class="btn" id="emp-reissue-btn">' + WB.t("Reissue tokens") + '</button> ' : '';
    el.innerHTML =
      '<div class="card"><h3>' + WB.t("Organization chart") + '</h3><div id="org-depts"></div>' +
      '<div class="form-row admin-only">' +
      '<input id="dept-name" placeholder="' + WB.t("Department name") + '">' +
      '<select id="dept-parent"><option value="">' + WB.t("Top-level department") + '</option></select>' +
      '<button class="btn" id="dept-add">' + WB.t("Add department") + '</button>' +
      '</div></div>' +
      '<div class="card"><h3>' + WB.t("Employees") + '</h3>' +
      '<table><thead><tr>' + chkTh + '<th>ID</th><th>' + WB.t("Full name") + '</th><th>' + WB.t("Role") + '</th><th>' + WB.t("Type") + '</th><th>' + WB.t("Tool") + '</th><th>' + WB.t("Status") + '</th><th>' + WB.t("Actions") + '</th></tr></thead><tbody id="emp-rows"></tbody></table>' +
      '<div class="form-row admin-only">' +
      reissueBtn +
      '<input id="emp-name" placeholder="' + WB.t("Full name") + '">' +
      '<input id="emp-role" placeholder="' + WB.t("Role (e.g. backend)") + '">' +
      '<select id="emp-kind"><option value="ai">AI</option><option value="human">' + WB.t("Human") + '</option></select>' +
      '<select id="emp-dept"><option value="">' + WB.t("No department") + '</option></select>' +
      '<button class="btn" id="emp-add">' + WB.t("Add employee") + '</button>' +
      '</div><div id="emp-token-box"></div></div>';
    // Lazy render: the button only exists after innerHTML injection, so it must be
    // bound here (at DOMContentLoaded bind time #block-org is empty, so the handler could never attach — existing bug surfaced by S31 acceptance).
    const addDept = document.getElementById("dept-add");
    if (addDept) addDept.onclick = async () => {
      const name = document.getElementById("dept-name").value.trim();
      const p = document.getElementById("dept-parent").value;
      if (!name) { WB.toast(WB.t("Please enter a department name"), true); return; }
      try {
        await WB.api("POST", "/api/departments", { name: name, parent_id: p ? Number(p) : null });
        WB.toast(WB.t("Department added"));
        WB.blocks.org.refresh();
      } catch (e) { WB.apiCatch(e, WB.t("Add department")); }
    };
    const addEmp = document.getElementById("emp-add");
    if (addEmp) addEmp.onclick = async () => {
      const body = {
        name: document.getElementById("emp-name").value.trim(),
        role: document.getElementById("emp-role").value.trim(),
        kind: document.getElementById("emp-kind").value,
        department_id: Number(document.getElementById("emp-dept").value || 0),
      };
      if (!body.name) { WB.toast(WB.t("Please enter a name"), true); return; }
      try {
        const d = await WB.api("POST", "/api/employees", body);
        WB.toast(WB.t("Employee created"));
        const box = document.getElementById("emp-token-box");
        if (box) box.innerHTML = '<div class="token-box">token: ' + WB.esc(d.token) + ' ' + WB.t("(shown only once, save it)") + '</div>';
        WB.blocks.org.refresh();
      } catch (e) { WB.apiCatch(e, WB.t("Add employee")); }
    };
    const reissueBtnEl = document.getElementById("emp-reissue-btn");
    if (reissueBtnEl) reissueBtnEl.onclick = async () => {
      const ids = [...document.querySelectorAll("#emp-rows input[type=checkbox]:checked")]
        .map(c => Number(c.dataset.empId));
      if (!ids.length) { WB.toast(WB.t("Select employees first"), true); return; }
      if (!confirm(WB.t("Reissue tokens for the selected employees? Old tokens will be invalidated immediately."))) return;
      try {
        const d = await WB.api("POST", "/api/employees/reissue-tokens", { ids: ids });
        const blob = new Blob([JSON.stringify(d.tokens, null, 2)], { type: "application/json" });
        const a = document.createElement("a");
        a.href = URL.createObjectURL(blob);
        a.download = "whatbark-tokens-" + new Date().toISOString().slice(0, 10) + ".json";
        document.body.appendChild(a);
        a.click();
        a.remove();
        setTimeout(() => URL.revokeObjectURL(a.href), 1000);
        WB.toast(WB.t("Tokens reissued and downloaded"));
      } catch (e) { WB.apiCatch(e, WB.t("Reissue tokens")); }
    };
    this.refresh();
  },
  refresh: async function () {
    const me = WB.state.me;
    const isAdmin = !!(me && me.admin);
    document.querySelectorAll(".admin-only").forEach(n => { n.style.display = isAdmin ? "flex" : "none"; });
    try {
      const d = await WB.api("GET", "/api/departments");
      renderDeptTree(d.departments || []);
      fillDeptSelects(d.departments || []);
    } catch (e) { WB.apiCatch(e, WB.t("Load departments")); }
    try {
      const d = await WB.api("GET", "/api/employees");
      renderEmpTable(d.employees || []);
    } catch (e) { WB.apiCatch(e, WB.t("Load employees")); }
  }
});

function renderDeptTree(depts) {
  const host = document.getElementById("org-depts");
  if (!host) return;
  const byParent = {};
  depts.forEach(d => { (byParent[d.parent_id || 0] = byParent[d.parent_id || 0] || []).push(d); });
  function build(pid, depth) {
    let html = "";
    (byParent[pid] || []).forEach(d => {
      const leader = d.leader_name
        ? '<select class="dept-leader-sel" data-dept="' + d.id + '"></select>'
        : "";
      const del = WB.state.me && WB.state.me.admin
        ? ' <button class="btn small danger" data-del-dept="' + d.id + '">' + WB.t("Delete") + '</button>' : "";
      // S31: departments own a channel (group_id) -> show a channel tag.
      const tag = d.group_id != null ? ' <span class="tag">' + WB.t("Channel") + '</span>' : "";
      const ren = WB.state.me && WB.state.me.admin
        ? ' <button class="btn small" data-rename-dept="' + d.id + '" data-name="' + WB.esc(d.name) + '">' + WB.t("Rename") + '</button>' : "";
      html += '<div class="msg" style="margin-left:' + depth * 16 + 'px">' +
        "<span>" + WB.esc(d.name) + "</span>" + tag + " " + leader + ren + del + "</div>";
      html += build(d.id, depth + 1);
    });
    return html;
  }
  host.innerHTML = build(0, 0);
  host.querySelectorAll(".dept-leader-sel").forEach(sel => {
    WB.api("GET", "/api/employees").then(d => {
      sel.innerHTML = '<option value="0">' + WB.t("— Remove leader —") + '</option>' +
        (d.employees || []).map(e => '<option value="' + e.id + '">' + WB.esc(e.name) + "</option>").join("");
    }).catch(() => {});
    sel.onchange = async () => {
      try {
        await WB.api("PUT", "/api/departments/" + sel.getAttribute("data-dept") + "/leader", { employee_id: Number(sel.value) });
        WB.toast(WB.t("Leader updated"));
        WB.blocks.org.refresh();
      } catch (e) { WB.apiCatch(e, WB.t("Set leader")); }
    };
  });
  host.querySelectorAll("[data-del-dept]").forEach(btn => {
    btn.onclick = async () => {
      if (!confirm(WB.t("Confirm delete this department?"))) return;
      try {
        await WB.api("DELETE", "/api/departments/" + btn.getAttribute("data-del-dept"));
        WB.toast(WB.t("Department deleted"));
        WB.blocks.org.refresh();
      } catch (e) { WB.apiCatch(e, WB.t("Delete department")); }
    };
  });
  // S31: admin rename via native prompt (same class as S26's confirm()).
  // Empty or unchanged input sends no request.
  host.querySelectorAll("[data-rename-dept]").forEach(btn => {
    btn.onclick = async () => {
      const cur = btn.getAttribute("data-name") || "";
      const name = prompt(WB.t("Rename department"), cur);
      if (name == null) return;
      const trimmed = name.trim();
      if (!trimmed || trimmed === cur) return;
      try {
        await WB.api("PUT", "/api/departments/" + btn.getAttribute("data-rename-dept"), { name: trimmed });
        WB.toast(WB.t("Department renamed"));
        WB.blocks.org.refresh();
      } catch (e) { WB.apiCatch(e, WB.t("Rename department")); }
    };
  });
}

function fillDeptSelects(depts) {
  ["dept-parent", "emp-dept"].forEach(id => {
    const sel = document.getElementById(id);
    if (!sel) return;
    const first = sel.options[0].outerHTML;
    sel.innerHTML = first + depts.map(d => '<option value="' + d.id + '">' + WB.esc(d.name) + "</option>").join("");
  });
}

const STATUS_LABELS = { online: WB.t("Online"), offline: WB.t("Offline") };

function renderEmpTable(emps) {
  const tbody = document.getElementById("emp-rows");
  if (!tbody) return;
  const isAdmin = !!(WB.state.me && WB.state.me.admin);
  tbody.innerHTML = emps.map(e => {
    const cls = e.status === "online" ? "online" : e.status === "offline" ? "offline" : "";
    const statusLabel = STATUS_LABELS[e.status] || e.status || "";
    const del = isAdmin
      ? '<button class="btn small danger" data-del-emp="' + e.id + '">' + WB.t("Delete") + '</button>' : "";
    const chk = isAdmin
      ? '<td><input type="checkbox" data-emp-id="' + e.id + '"></td>' : "";
    return "<tr>" + chk + "<td>" + e.id + "</td><td>" + WB.esc(e.name) + "</td><td>" + WB.esc(e.role) +
      "</td><td>" + (e.kind === "human" ? WB.t("Human") : "AI") + "</td><td>" + WB.esc(e.tool || "mock") +
      '</td><td><span class="tag ' + cls + '">' + statusLabel + "</span></td><td>" + del + "</td></tr>";
  }).join("");
  tbody.querySelectorAll("[data-del-emp]").forEach(btn => {
    btn.onclick = async () => {
      if (!confirm(WB.t("Confirm delete this employee?"))) return;
      try {
        await WB.api("DELETE", "/api/employees/" + btn.getAttribute("data-del-emp"));
        WB.toast(WB.t("Employee deleted"));
        WB.blocks.org.refresh();
      } catch (e) { WB.apiCatch(e, WB.t("Delete employee")); }
    };
  });
}
