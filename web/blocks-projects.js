/* projects block: project list, create/delete, per-project task view. */
"use strict";
WB.register("projects", {
  render: function (el) {
    el.innerHTML =
      '<div class="card"><h3>' + WB.t("Projects") + '</h3>' +
      '<table><thead><tr><th>ID</th><th>' + WB.t("Name") + '</th><th>' + WB.t("Brief") + '</th><th>' + WB.t("Entry employee") + '</th><th>' + WB.t("Workflow") + '</th><th>' + WB.t("Actions") + '</th></tr></thead><tbody id="proj-rows"></tbody></table>' +
      '<div class="form-row admin-only">' +
      '<input id="proj-name" placeholder="' + WB.t("Project name") + '">' +
      '<input id="proj-brief" placeholder="' + WB.t("Project brief") + '">' +
      '<select id="proj-entry"><option value="">' + WB.t("No entry") + '</option></select>' +
      '<select id="proj-wf"><option value="">' + WB.t("No workflow") + '</option></select>' +
       '<button class="btn" id="proj-add">' + WB.t("Create project") + '</button>' +
       '</div></div>' +
       '<div id="proj-detail"></div>';
    // Lazy render: the button only exists after innerHTML injection, so it must be
    // bound here (at DOMContentLoaded bind time #block-projects is empty, so the handler could never attach — same existing bug class as the org block).
    const add = document.getElementById("proj-add");
    if (add) add.onclick = async () => {
      const body = {
        name: document.getElementById("proj-name").value.trim(),
        brief: document.getElementById("proj-brief").value.trim(),
      };
      const entry = document.getElementById("proj-entry").value;
      const wf = document.getElementById("proj-wf").value;
      if (entry) body.entry_employee_id = Number(entry);
      if (wf) body.workflow_id = Number(wf);
      if (!body.name) { WB.toast(WB.t("Please enter a project name"), true); return; }
      try {
        const d = await WB.api("POST", "/api/projects", body);
        WB.toast(WB.t("Project created #") + d.id);
        WB.blocks.projects.refresh();
      } catch (e) { WB.apiCatch(e, WB.t("Create project")); }
    };
    const entrySel = document.getElementById("proj-entry");
    if (entrySel) {
      WB.loadEmployees().then(() => {
        entrySel.innerHTML = '<option value="">' + WB.t("No entry") + '</option>' +
          Object.values(WB.state.empIndex).map(e =>
            '<option value="' + e.id + '">' + WB.esc(e.name) + "</option>").join("");
      }).catch(() => {});
    }
    this.refresh();
  },
  refresh: async function () {
    try {
      const d = await WB.api("GET", "/api/projects");
      renderProjRows(d.projects || []);
    } catch (e) { WB.apiCatch(e, WB.t("Load projects")); }
    try {
      const d = await WB.api("GET", "/api/workflows");
      const sel = document.getElementById("proj-wf");
      if (sel) sel.innerHTML = '<option value="">' + WB.t("No workflow") + '</option>' +
        (d.workflows || []).map(w => '<option value="' + w.id + '">' + WB.esc(w.name) + "</option>").join("");
    } catch (e) { /* dropdown is optional */ }
    if (!(WB.state.me && WB.state.me.admin)) {
      document.querySelectorAll(".admin-only").forEach(n => { n.style.display = "none"; });
    }
  }
});

function renderProjRows(projs) {
  const tbody = document.getElementById("proj-rows");
  if (!tbody) return;
  tbody.innerHTML = projs.map(p => {
    const entry = p.entry_employee_name ? WB.esc(p.entry_employee_name) : "-";
    const wf = p.workflow_id ? "#" + p.workflow_id : "-";
    const detail = '<button class="btn small secondary" data-pick-proj="' + p.id + '">' + WB.t("Tasks") + '</button> ';
    const del = WB.state.me && WB.state.me.admin
      ? '<button class="btn small danger" data-del-proj="' + p.id + '">' + WB.t("Delete") + '</button>' : "";
    return '<tr><td>' + p.id + "</td><td>" + WB.esc(p.name) + "</td><td>" + WB.esc(p.brief || "") +
      "</td><td>" + entry + "</td><td>" + wf + "</td><td>" + detail + del + "</td></tr>";
  }).join("");
  tbody.querySelectorAll("[data-pick-proj]").forEach(b => {
    b.onclick = () => showProjectTasks(Number(b.getAttribute("data-pick-proj")));
  });
  tbody.querySelectorAll("[data-del-proj]").forEach(b => {
    b.onclick = async () => {
      if (!confirm(WB.t("Confirm delete this project?"))) return;
      try {
        await WB.api("DELETE", "/api/projects/" + b.getAttribute("data-del-proj"));
        WB.toast(WB.t("Project deleted"));
        document.getElementById("proj-detail").innerHTML = "";
        WB.blocks.projects.refresh();
      } catch (e) { WB.apiCatch(e, WB.t("Delete project")); }
    };
  });
}

async function showProjectTasks(id) {
  const host = document.getElementById("proj-detail");
  if (!host) return;
  try {
    const d = await WB.api("GET", "/api/projects/" + id);
    host.innerHTML =
      '<div class="card"><h3>' + WB.t("Tasks of project #") + id + ": " + WB.esc(d.project.name) + '</h3>' +
      '<table><thead><tr><th>' + WB.t("Task ID") + '</th><th>' + WB.t("Employee") + '</th><th>' + WB.t("Type") + '</th><th>' + WB.t("Status") + '</th><th>' + WB.t("Input") + '</th><th>' + WB.t("Output") + '</th><th>' + WB.t("Created") + '</th><th>' + WB.t("Duration") + '</th></tr></thead><tbody>' +
      (d.tasks || []).map(t => {
        const cls = t.status || "";
        return '<tr><td>' + t.id + "</td><td>" + WB.esc(t.employee_name || t.employee_id) + "</td><td>" +
          WB.esc(t.kind) + '</td><td><span class="tag ' + cls + '">' + WB.esc(t.status) + "</span></td><td>" +
          WB.esc(t.input || "") + "</td><td>" + WB.esc(t.output || "") + "</td><td>" + WB.esc(t.created_at || "") +
          '</td><td>' + (t.duration ? WB.esc(t.duration) : "—") + "</td></tr>";
      }).join("") +
      "</tbody></table></div>";
  } catch (e) { WB.apiCatch(e, WB.t("Load project tasks")); }
}
