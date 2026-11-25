/* servers panel: manages platform servers in browser localStorage.
 * Rendered from the settings view (S9); all queries are scoped to the
 * container (no global ids). */
"use strict";

// 12x12 pixel-style copy icon (two stacked rectangles), same style as the
// PIX_ICON_* icons in blocks-chat.js. The servers block is self-contained;
// it does not reference other block files.
const PIX_COPY =
  '<svg width="12" height="12" viewBox="0 0 12 12" shape-rendering="crispEdges" fill="none" stroke="currentColor" stroke-width="1">' +
  '<rect x="4" y="4" width="6" height="6"/><path d="M2 8V2h6"/></svg>';

function renderServersPanel(container) {
  // The add row is the last row of the same table (tfoot); each of its three
  // inputs occupies one column, pixel-aligned with the header columns.
  container.innerHTML =
    '<table><thead><tr><th>' + WB.t("Name") + '</th><th>' + WB.t("Address") + '</th><th>Token</th><th>' + WB.t("Actions") + '</th></tr></thead>' +
    '<tbody class="srv-rows"></tbody>' +
    '<tfoot><tr class="srv-add-row">' +
    '<td><input class="srv-name" placeholder="' + WB.t("Name (e.g. staging)") + '"></td>' +
    '<td><input class="srv-url" placeholder="https://host:8080"></td>' +
    '<td><input class="srv-token" type="password" placeholder="' + WB.t("token (optional)") + '"></td>' +
    '<td><button class="btn srv-add">' + WB.t("Add") + '</button></td>' +
    "</tr></tfoot></table>";
  renderServerRows(container);
  container.querySelector(".srv-add").onclick = () => addServer(container);
}

function renderServerRows(container) {
  const rows = container.querySelector(".srv-rows");
  rows.innerHTML = WB.state.servers.map(function (s) {
    const isCur = s.id === WB.state.activeServer;
    // "Current" is no longer its own column: it is inlined to the right of the name.
    const name = WB.esc(s.name || s.id) +
      (isCur ? ' <span class="tag online">' + WB.t("Current") + '</span>' : "");
    const sw = isCur ? "" :
      '<button class="btn small secondary srv-switch" data-id="' + WB.esc(s.id) + '">' + WB.t("Switch") + '</button> ';
    // Token column: fixed 8-star mask (leaks neither the real value nor its
    // length); the copy icon renders only when a token is stored.
    const tok = WB.state.tokens[s.id];
    const tokCell = tok
      ? '<span class="srv-token-mask">********</span>' +
        '<button class="srv-copy" data-id="' + WB.esc(s.id) + '" title="' + WB.t("Copy token") + '" aria-label="' + WB.t("Copy token") + '">' + PIX_COPY + "</button>"
      : '<span class="srv-token-none">—</span>';
    return "<tr><td>" + name + "</td><td>" + WB.esc(s.url) + "</td><td>" + tokCell +
      '</td><td>' + sw +
      '<button class="btn small danger srv-del" data-id="' + WB.esc(s.id) + '">' + WB.t("Delete") + '</button></td></tr>';
  }).join("");
  rows.querySelectorAll(".srv-del").forEach(function (b) {
    b.onclick = function () { removeServer(container, b.getAttribute("data-id")); };
  });
  rows.querySelectorAll(".srv-switch").forEach(function (b) {
    b.onclick = function () { switchServer(b.getAttribute("data-id")); };
  });
  rows.querySelectorAll(".srv-copy").forEach(function (b) {
    b.onclick = function () { copyServerToken(b.getAttribute("data-id")); };
  });
}

// Copies the real token to the clipboard (the display layer only ever shows
// stars). Prefers the Clipboard API, falls back to execCommand in restricted
// environments; both success and failure get a toast.
async function copyServerToken(id) {
  const tok = WB.state.tokens[id];
  if (!tok) { WB.toast(WB.t("This server has no stored token, please log in first"), true); return; }
  let ok = false;
  try { await navigator.clipboard.writeText(tok); ok = true; } catch (_) { /* fall back to execCommand */ }
  if (!ok) {
    try {
      const ta = document.createElement("textarea");
      ta.value = tok;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      ok = document.execCommand("copy");
      ta.remove();
    } catch (_) { ok = false; }
  }
  WB.toast(ok ? WB.t("Token copied") : WB.t("Copy failed: clipboard unavailable"), !ok);
}

function addServer(container) {
  const name = container.querySelector(".srv-name").value.trim();
  let url = container.querySelector(".srv-url").value.trim();
  url = url.replace(/\/+$/, "");
  const tok = container.querySelector(".srv-token").value.trim();
  if (!url) { WB.toast(WB.t("Please enter a server URL"), true); return; }
  if (!/^https?:\/\//.test(url)) { WB.toast(WB.t("URL must start with http:// or https://"), true); return; }
  const st = WB.state;
  const id = "s" + Date.now();
  st.servers.push({ id: id, name: name || url, url: url });
  if (tok) { st.tokens[id] = tok; WB.saveTokens(); }
  WB.saveServers();
  container.querySelector(".srv-name").value = "";
  container.querySelector(".srv-url").value = "";
  container.querySelector(".srv-token").value = "";
  renderServerRows(container);
  WB.toast(WB.t("Server added"));
}

function removeServer(container, id) {
  const st = WB.state;
  if (st.servers.length <= 1) { WB.toast(WB.t("Keep at least one server"), true); return; }
  const wasActive = st.activeServer === id;
  st.servers = st.servers.filter(function (s) { return s.id !== id; });
  if (wasActive) {
    st.activeServer = st.servers[0].id;
    WB.saveActive();
  }
  delete st.tokens[id]; // drop this server's local token key, avoiding a localStorage orphan
  WB.saveTokens();
  WB.saveServers();
  renderServerRows(container);
  WB.toast(WB.t("Server deleted"));
  if (wasActive) WB.bootServer(); // the session was attached to the deleted server; reboot onto the newly active one
}

function switchServer(id) {
  WB.state.activeServer = id;
  WB.saveActive();
  WB.bootServer();
}
