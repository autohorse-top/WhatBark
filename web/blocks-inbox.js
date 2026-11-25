/* inbox block: per-employee message inbox with human reply/complete/reject actions. */
"use strict";

WB.register("inbox", {
  render: function (el) {
    const isAdmin = !!(WB.state.me && WB.state.me.admin);
    el.innerHTML =
      '<div class="card"><h3>' + WB.t("Inbox") + '</h3>' +
      (isAdmin
        ? '<div class="form-row"><label>' + WB.t("Recipient") + '</label><select id="inbox-target"></select><button class="btn small" id="inbox-load">' + WB.t("View") + '</button></div>'
        : '<p class="note">' + WB.t("The messages below were sent to me (") + WB.esc(WB.loginLabel()) + ')</p>') +
      '<div id="inbox-msgs"></div></div>';

    if (isAdmin) {
      fillInboxTarget();
      document.getElementById("inbox-load").onclick = async (ev) => {
        // sweep-followup: if the dropdown is empty (load failed / not ready), top it up first
        // so a currentTarget()=0 cannot make loadInbox silently early-return with zero feedback.
        const btn = ev.currentTarget; // capture now: ev.currentTarget is nulled after the await
        const sel = document.getElementById("inbox-target");
        if (!sel) return;
        if (sel.options.length === 0) {
          if (btn.disabled) return; // a top-up fetch is in flight: ignore duplicate clicks (no N parallel chains)
          btn.disabled = true;
          let ok = false;
          try { ok = await fillInboxTarget(); } finally { btn.disabled = false; }
          if (!ok) return;
          // If the admin sub-tab is switched away from and back during the top-up fetch
          // (block re-render, dropdown rebuilt), the result lands on the old node: toast if
          // the live dropdown is still empty, instead of silently early-returning.
          const live = document.getElementById("inbox-target");
          if (live && live.options.length === 0) { WB.toast(WB.t("Recipient list not ready, please retry"), true); return; }
        }
        loadInbox();
      };
    }
    loadInbox();
  }
});

/* sweep-followup: fill the #inbox-target recipient dropdown.
   loadEmployees failure -> retry once -> still failing -> red toast (the old .catch(()=>{})
   silently swallowed the error: clicking "View" after a failed fill early-returned via
   currentTarget()=0 with zero user feedback). Returns true if the dropdown was filled. */
async function fillInboxTarget() {
  const sel = document.getElementById("inbox-target");
  if (!sel) return false;
  const paint = () => {
    sel.innerHTML = Object.values(WB.state.empIndex).map(e =>
      '<option value="' + e.id + '">' + WB.esc(e.name) + "</option>").join("");
    return sel.options.length > 0;
  };
  for (let attempt = 0; attempt < 2; attempt++) {
    try {
      await WB.loadEmployees();
      if (paint()) return true;
      if (attempt === 1) { WB.toast(WB.t("Recipient list is empty"), true); return false; }
    } catch (e) {
      if (attempt === 0) { await new Promise((r) => setTimeout(r, 300)); continue; }
      WB.apiCatch(e, WB.t("Load recipient list"));
      return false;
    }
  }
  return false;
}

function currentTarget() {
  if (WB.state.me && WB.state.me.admin) {
    return Number(document.getElementById("inbox-target").value || 0);
  }
  return WB.state.me.employee_id;
}

async function loadInbox() {
  const target = currentTarget();
  if (!target) return;
  try {
    const d = await WB.api("GET", "/api/messages?to=" + target + "&limit=100");
    renderInbox(d.messages || []);
  } catch (e) { WB.apiCatch(e, WB.t("Load inbox")); }
}

function renderInbox(msgs) {
  const host = document.getElementById("inbox-msgs");
  if (!host) return;
  const me = WB.state.me;
  host.innerHTML = msgs.length === 0
    ? '<p class="muted">' + WB.t("No messages") + '</p>'
    : msgs.slice().reverse().map(m => {
      const from = m.from_id == null ? "boss" : WB.empName(m.from_id);
      const mineTask = me && !me.admin && m.task_id;
      let actions = "";
      if (mineTask) {
        actions = '<div class="actions">';
        // Boss messages have from_id == null; replying to them via API is
        // not possible (S7-Minor3: dead button -> 403), so only offer it
        // for messages from a real employee.
        if (m.from_id != null) {
          actions +=
            '<button class="btn small secondary" data-reply="' + m.id + '" data-from="' + m.from_id + '">' + WB.t("Reply") + '</button>';
        }
        actions +=
          '<button class="btn small" data-finish="' + m.task_id + '">' + WB.t("Complete") + '</button>' +
          '<button class="btn small danger" data-fail="' + m.task_id + '">' + WB.t("Reject") + '</button>' +
          '</div>';
      } else if (me && me.admin && m.task_id) {
        // S27: reassign button sits alongside cancel (the server 409s done tasks, so no client-side state filtering here).
        actions =
          '<div class="actions">' +
          '<button class="btn small danger" data-cancel="' + m.task_id + '">' + WB.t("Cancel task") + '</button>' +
          '<button class="btn small secondary" data-reassign="' + m.task_id + '">' + WB.t("Reassign") + '</button>' +
          '</div>';
      }
      // task-duration-artifacts A: server-annotated task_duration on ?to= rows
      const dur = m.task_duration ? " · " + WB.esc(m.task_duration) : "";
      return '<div class="msg"><div class="from">' + WB.esc(from) + " · " + WB.esc(m.kind) +
        (m.task_id ? " · " + WB.t("Task #") + m.task_id + dur : "") + " · " + WB.esc(m.created_at || "") + "</div>" +
        '<div class="body">' + WB.esc(m.body) + "</div>" + actions + "</div>";
    }).join("");

  host.querySelectorAll("[data-reply]").forEach(b => {
    b.onclick = async () => {
      const text = prompt(WB.t("Reply content:"));
      if (!text) return;
      try {
        await WB.api("POST", "/api/messages", { to: Number(b.getAttribute("data-from")), kind: "answer", body: text });
        loadInbox();
      } catch (e) { WB.apiCatch(e, WB.t("Reply")); }
    };
  });
  host.querySelectorAll("[data-finish]").forEach(b => {
    b.onclick = async () => {
      const out = prompt(WB.t("Complete task — enter output/notes:"));
      if (out === null) return;
      try {
        await WB.api("POST", "/api/tasks/" + b.getAttribute("data-finish") + "/finish", { output: out });
        WB.toast(WB.t("Task completed"));
        loadInbox();
      } catch (e) { WB.apiCatch(e, WB.t("Complete task")); }
    };
  });
  host.querySelectorAll("[data-fail]").forEach(b => {
    b.onclick = async () => {
      const why = prompt(WB.t("Reject task — enter reason:"));
      if (why === null) return;
      try {
        await WB.api("POST", "/api/tasks/" + b.getAttribute("data-fail") + "/fail", { output: why });
        WB.toast(WB.t("Task rejected"));
        loadInbox();
      } catch (e) { WB.apiCatch(e, WB.t("Reject task")); }
    };
  });
  host.querySelectorAll("[data-cancel]").forEach(b => {
    b.onclick = async () => {
      if (!confirm(WB.t("Confirm cancel this task?"))) return;
      try {
        // reason is stored payload data: keep it language-stable English.
        await WB.api("POST", "/api/tasks/" + b.getAttribute("data-cancel") + "/cancel", { reason: "Cancelled by admin" });
        loadInbox();
      } catch (e) { WB.apiCatch(e, WB.t("Cancel task")); }
    };
  });
  // S27: reassign — inline picker replaces prompt (shared implementation: reassignPicker in blocks-chat.js).
  host.querySelectorAll("[data-reassign]").forEach(b => {
    b.onclick = () => {
      const taskId = Number(b.getAttribute("data-reassign"));
      // reason is stored payload data: keep it language-stable English.
      reassignPicker(b, taskId, currentTarget(), "Reassigned from inbox", () => loadInbox());
    };
  });
}

WB.on("message.new", () => {
  if (WB.state.view === "admin" && WB.state.activeTab === "inbox") loadInbox();
});
