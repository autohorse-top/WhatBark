/* wflow block: workflow list + SVG drag canvas (nodes/edges). */
"use strict";
const WFLOW = {
  wfId: 0,
  nodes: [],
  edges: [],
  selected: 0,
  linkFrom: 0,
  drag: null,
  canvasW: 960,
  canvasH: 420,
};

WB.register("wflow", {
  render: function (el) {
    el.innerHTML =
      '<div class="card"><h3>' + WB.t("Workflows") + '</h3>' +
      '<table><thead><tr><th>ID</th><th>' + WB.t("Name") + '</th><th>' + WB.t("Actions") + '</th></tr></thead><tbody id="wf-rows"></tbody></table>' +
      '<div class="form-row admin-only">' +
      '<input id="wf-name" placeholder="' + WB.t("New workflow name") + '">' +
      '<button class="btn" id="wf-add">' + WB.t("Create") + '</button>' +
      '</div></div>' +
      '<div class="card"><h3 id="wf-canvas-title">' + WB.t("Canvas (select a workflow)") + '</h3>' +
      '<div class="form-row admin-only" id="wf-tools">' +
      '<select id="wf-node-emp"></select>' +
      '<button class="btn" id="wf-node-add">' + WB.t("Add node") + '</button>' +
      '<button class="btn secondary" id="wf-link-toggle">' + WB.t("Link mode") + '</button>' +
      '<button class="btn danger" id="wf-node-del">' + WB.t("Delete selected node") + '</button>' +
      '</div>' +
      '<p class="note">' + WB.t("Drag nodes to reposition (auto-saved). In link mode, click two nodes in sequence to create a directed edge; click an edge to delete it.") + '</p>' +
      '<div id="canvas-wrap"><svg id="wf-svg" class="wflow" width="960" height="420" viewBox="0 0 960 420">' +
      '<defs><marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">' +
      '<path d="M 0 0 L 10 5 L 0 10 z" fill="#6b7280"/></marker></defs>' +
       '<g id="wf-edges"></g><g id="wf-nodes"></g></svg></div>' +
       '</div>';
    // Lazy render: the button only exists after innerHTML injection, so it must be
    // bound here (at DOMContentLoaded bind time #block-wflow is empty, so the handler could never attach — same existing bug class as the org block).
    const add = document.getElementById("wf-add");
    if (add) add.onclick = async () => {
      const name = document.getElementById("wf-name").value.trim();
      if (!name) { WB.toast(WB.t("Please enter the name"), true); return; }
      try {
        const d = await WB.api("POST", "/api/workflows", { name: name });
        WFLOW.wfId = d.id;
        WB.toast(WB.t("Workflow created"));
        WB.blocks.wflow.refresh();
      } catch (e) { WB.apiCatch(e, WB.t("Create workflow")); }
    };
    const nadd = document.getElementById("wf-node-add");
    if (nadd) nadd.onclick = async () => {
      const empId = Number(document.getElementById("wf-node-emp").value);
      if (!empId) { WB.toast(WB.t("Create an employee first"), true); return; }
      if (!WFLOW.wfId) { WB.toast(WB.t("Create or select a workflow first"), true); return; }
      const x = 40 + (WFLOW.nodes.length % 4) * 160;
      const y = 40 + Math.floor(WFLOW.nodes.length / 4) * 80;
      try {
        await WB.api("POST", "/api/workflows/" + WFLOW.wfId + "/nodes", { employee_id: empId, x: x, y: y });
        await loadCanvas();
      } catch (e) { WB.apiCatch(e, WB.t("Add node")); }
    };
    const link = document.getElementById("wf-link-toggle");
    if (link) link.onclick = () => {
      WFLOW.linkFrom = WFLOW.linkFrom ? 0 : (WFLOW.selected || 0);
      link.textContent = WFLOW.linkFrom ? WB.t("Linking (click target node)") : WB.t("Link mode");
      if (WFLOW.linkFrom) WB.toast(WB.t("Link mode: start selected, click target node"));
    };
    const ndel = document.getElementById("wf-node-del");
    if (ndel) ndel.onclick = async () => {
      if (!WFLOW.selected) { WB.toast(WB.t("Click a node to select it first"), true); return; }
      if (!confirm(WB.t("Delete this node and its edges?"))) return;
      try {
        await WB.api("DELETE", "/api/workflows/" + WFLOW.wfId + "/nodes/" + WFLOW.selected);
        WFLOW.selected = 0;
        WB.toast(WB.t("Node deleted"));
        await loadCanvas();
      } catch (e) { WB.apiCatch(e, WB.t("Delete node")); }
    };
    this.refresh();
  },
  refresh: async function () {
    try {
      if (WB.canManage()) await WB.loadEmployees();
      const d = await WB.api("GET", "/api/workflows");
      renderWfRows(d.workflows || []);
      if (!WFLOW.wfId || !(d.workflows || []).some(w => w.id === WFLOW.wfId)) {
        WFLOW.wfId = d.workflows.length ? d.workflows[0].id : 0;
      }
      await loadCanvas();
    } catch (e) { WB.apiCatch(e, WB.t("Load workflows")); }
    const canManage = WB.canManage();
    document.querySelectorAll(".admin-only").forEach(n => { n.style.display = canManage ? "flex" : "none"; });
  }
});

function renderWfRows(wfs) {
  const tbody = document.getElementById("wf-rows");
  if (!tbody) return;
  tbody.innerHTML = wfs.map(w => {
    const sel = w.id === WFLOW.wfId ? ' class="tag online"' : "";
    const del = WB.state.me && WB.state.me.admin
      ? ' <button class="btn small danger" data-del-wf="' + w.id + '">' + WB.t("Delete") + '</button>' : "";
    return '<tr><td><button class="btn small secondary" data-pick-wf="' + w.id + '"> #' + w.id + "</button></td><td" +
      sel + ">" + WB.esc(w.name) + "</td><td>" + del + "</td></tr>";
  }).join("");
  tbody.querySelectorAll("[data-pick-wf]").forEach(b => {
    b.onclick = async () => { WFLOW.wfId = Number(b.getAttribute("data-pick-wf")); WFLOW.selected = 0; WFLOW.linkFrom = 0; await loadCanvas(); };
  });
  tbody.querySelectorAll("[data-del-wf]").forEach(b => {
    b.onclick = async () => {
      if (!confirm(WB.t("Confirm delete this workflow?"))) return;
      try {
        await WB.api("DELETE", "/api/workflows/" + b.getAttribute("data-del-wf"));
        WFLOW.wfId = 0;
        WB.toast(WB.t("Workflow deleted"));
        WB.blocks.wflow.refresh();
      } catch (e) { WB.apiCatch(e, WB.t("Delete workflow")); }
    };
  });
}

async function loadCanvas() {
  const title = document.getElementById("wf-canvas-title");
  if (!WFLOW.wfId) {
    WFLOW.nodes = []; WFLOW.edges = [];
    drawCanvas();
    if (title) title.textContent = WB.t("Canvas (select a workflow)");
    return;
  }
  try {
    const d = await WB.api("GET", "/api/workflows/" + WFLOW.wfId);
    WFLOW.nodes = d.nodes || [];
    WFLOW.edges = d.edges || [];
    if (title) title.textContent = WB.t("Canvas: ") + WB.esc(d.workflow.name);
    fillWfEmpSelect();
    drawCanvas();
  } catch (e) { WB.apiCatch(e, WB.t("Load canvas")); }
}

function fillWfEmpSelect() {
  const sel = document.getElementById("wf-node-emp");
  if (!sel) return;
  const all = Object.values(WB.state.empIndex);
  // Route B: non-admins only see their subordinates
  const subs = (WB.state.me && !WB.state.me.admin)
    ? all.filter(e => WB.state.subordinates.indexOf(e.id) !== -1)
    : all;
  sel.innerHTML = subs.length
    ? subs.map(e => '<option value="' + e.id + '">' + WB.esc(e.name) + "</option>").join("")
    : '<option value="">' + WB.t("No subordinates") + '</option>';
}

function nodeCenter(n) { return { x: n.x + 60, y: n.y + 20 }; }

function drawCanvas() {
  const eg = document.getElementById("wf-edges");
  const ng = document.getElementById("wf-nodes");
  if (!eg || !ng) return;
  const byId = {};
  WFLOW.nodes.forEach(n => { byId[n.id] = n; });
  eg.innerHTML = WFLOW.edges.map(e => {
    const a = byId[e.from_node_id], b = byId[e.to_node_id];
    if (!a || !b) return "";
    const p1 = nodeCenter(a), p2 = nodeCenter(b);
    return '<line class="wf-edge" data-edge="' + e.id + '" x1="' + p1.x + '" y1="' + p1.y +
      '" x2="' + (p2.x - 12) + '" y2="' + p2.y + '" marker-end="url(#arrow)"/>';
  }).join("");
  ng.innerHTML = WFLOW.nodes.map(n => {
    const name = WB.empName(n.employee_id);
    const sel = n.id === WFLOW.selected ? ' stroke="#dc2626" stroke-width="2.5"' : "";
    return '<g class="wf-node" data-node="' + n.id + '" transform="translate(' + n.x + ',' + n.y + ')">' +
      '<rect width="120" height="40" rx="8"' + sel + "></rect>" +
      '<text x="10" y="17">' + WB.esc(name) + '</text><text x="10" y="32" fill="#6b7280">#' + n.employee_id + "</text></g>";
  }).join("");
  bindCanvasEvents();
}

function bindCanvasEvents() {
  const svg = document.getElementById("wf-svg");
  if (!svg) return;
  svg.querySelectorAll(".wf-edge").forEach(line => {
      line.onclick = async () => {
        if (!WB.canManage()) return;
      if (!confirm(WB.t("Delete this edge?"))) return;
      try {
        await WB.api("DELETE", "/api/workflows/" + WFLOW.wfId + "/edges/" + line.getAttribute("data-edge"));
        WB.toast(WB.t("Edge deleted"));
        await loadCanvas();
      } catch (e) { WB.apiCatch(e, WB.t("Delete edge")); }
    };
  });
  svg.querySelectorAll(".wf-node").forEach(g => {
    g.onpointerdown = (ev) => {
      const id = Number(g.getAttribute("data-node"));
      if (!WB.canManage()) { WFLOW.selected = id; drawCanvas(); return; }
      const n = WFLOW.nodes.find(x => x.id === id);
      if (!n) return;
      const pt = svgPoint(svg, ev);
      WFLOW.drag = { id: id, dx: pt.x - n.x, dy: pt.y - n.y };
      g.setPointerCapture(ev.pointerId);
      g.onpointermove = (ev2) => {
        if (!WFLOW.drag) return;
        const p = svgPoint(svg, ev2);
        const nx = Math.max(0, Math.min(WFLOW.canvasW - 120, p.x - WFLOW.drag.dx));
        const ny = Math.max(0, Math.min(WFLOW.canvasH - 40, p.y - WFLOW.drag.dy));
        n.x = Math.round(nx); n.y = Math.round(ny);
        g.setAttribute("transform", "translate(" + n.x + "," + n.y + ")");
        redrawEdges();
      };
      g.onpointerup = async () => {
        if (!WFLOW.drag) return;
        WFLOW.drag = null;
        g.onpointermove = null;
        const n2 = WFLOW.nodes.find(x => x.id === id);
        try {
          await WB.api("PUT", "/api/workflows/" + WFLOW.wfId + "/nodes/" + id, { x: n2.x, y: n2.y });
        } catch (e) { WB.apiCatch(e, WB.t("Save position")); }
      };
      g.onclick = () => handleNodeClick(id);
    };
  });
}

function svgPoint(svg, ev) {
  const r = svg.getBoundingClientRect();
  return { x: (ev.clientX - r.left) * (WFLOW.canvasW / r.width), y: (ev.clientY - r.top) * (WFLOW.canvasH / r.height) };
}

function redrawEdges() {
  const eg = document.getElementById("wf-edges");
  if (!eg) return;
  const byId = {};
  WFLOW.nodes.forEach(n => { byId[n.id] = n; });
  eg.innerHTML = WFLOW.edges.map(e => {
    const a = byId[e.from_node_id], b = byId[e.to_node_id];
    if (!a || !b) return "";
    const p1 = nodeCenter(a), p2 = nodeCenter(b);
    return '<line class="wf-edge" data-edge="' + e.id + '" x1="' + p1.x + '" y1="' + p1.y +
      '" x2="' + (p2.x - 12) + '" y2="' + p2.y + '" marker-end="url(#arrow)"/>';
  }).join("");
}

function handleNodeClick(id) {
  if (!WFLOW.linkFrom) {
    WFLOW.selected = id;
    drawCanvas();
    return;
  }
  if (WFLOW.linkFrom === id) { WFLOW.linkFrom = 0; return; }
  const doLink = async () => {
    try {
      await WB.api("POST", "/api/workflows/" + WFLOW.wfId + "/edges",
        { from_node_id: WFLOW.linkFrom, to_node_id: id });
      WB.toast(WB.t("Edge created"));
      WFLOW.linkFrom = 0;
      await loadCanvas();
    } catch (e) { WB.apiCatch(e, WB.t("Create edge")); }
  };
  doLink();
}
