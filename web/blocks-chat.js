/* chat block (S9): two-pane chat-first view.
 * Left pane: conversation list (GET /api/conversations) with search +
 * all/unread filters, unread badges and open-task chips.
 * Right pane: bidirectional thread (GET /api/messages?with=N) with a
 * composer whose kind options are pre-filtered by the route rules
 * (GET /api/routes; the server remains the final authority).
 * Realtime: WS message.new / task.state / agent.* -> refresh list + thread. */
"use strict";

const KIND_LABELS = {
  system: "System",
  chat: "Chat",
  dispatch: "Dispatch",
  ask: "Ask",
  answer: "Answer",
  report: "Report",
  summary: "Summary",
  scheduled: "Scheduled",
  selfreview: "Self-review",
  review: "Review",
};

const TASK_LABELS = {
  queued: "Queued", running: "Running", done: "Done", failed: "Failed", canceled: "Canceled",
};

// S12: pixel icons for the composer attachment buttons (12x12, crispEdges,
// square 2px-hard-edge styling lives in style.css .pix-btn).
// 2026-11-05 user directive: the screenshot button was removed (its only behavior was reading a clipboard image; Ctrl+V paste already covers that scenario).
const PIX_ICON_IMG =
  '<svg width="12" height="12" viewBox="0 0 12 12" shape-rendering="crispEdges" fill="none" stroke="currentColor" stroke-width="1">' +
  '<rect x="1" y="1.5" width="10" height="9"/><circle cx="4" cy="4.5" r="1"/>' +
  '<path d="M2 9.5l3-3 2 2 1.5-2 2.5 3"/></svg>';
const PIX_ICON_FILE =
  '<svg width="12" height="12" viewBox="0 0 12 12" shape-rendering="crispEdges" fill="none" stroke="currentColor" stroke-width="1">' +
  '<path d="M3 1h5l3 3v7H3z"/><path d="M8 1v3h3"/><path d="M5 7h4"/></svg>';

const chatState = {
  search: "",
  filter: "all",
  selectedPeer: null,
  conversations: [],
  routes: null, // {reporting:[], dispatch:[], chat:[]} for the caller
  tasks: {},    // task_id -> task object (status cache for chips/actions)
  attachments: [], // S12: pending uploads for the open thread
  messages: [],    // S13: last rendered thread (for right-click menu lookups)
  replyTo: null,   // S16.1: full message object being quoted (set by Reply; its .id
                   // is sent as reply_to, .body drives the composer quote preview
                   // and the "> " body prefix)
  searchResults: null, // S19-C: last /api/search response (null = not searching)
  virtualPeer: null,   // S19-C: {id,name,role} for a search-hit contact with no
                        // existing conversation; openThread falls back to it for
                        // the head until a real conversation appears
  groupMembers: {},    // S22/S23: gid -> [{employee_id,name}] (in-group dispatch
                       // target list for boss and leaders; invalidated on group.members)
  projects: null,      // S29: project list cache (null = not loaded); loadProjects lazy-loads once
};

WB.register("chat", {
  render: function (el) {
      el.innerHTML =
        '<div class="chat-pane">' +
        '  <div class="conv-search-row">' +
        '    <input id="conv-search" placeholder="' + WB.t("Search contacts, groups and messages") + '">' +
        '    <button id="conv-new-group" class="pix-btn" type="button" title="' + WB.t("New group chat") + '">+</button>' +
        '  </div>' +
        '  <div class="conv-tabs">' +
      '    <button data-filter="all" class="active">' + WB.t("All") + '</button>' +
      '    <button data-filter="unread">' + WB.t("Unread") + '</button>' +
      '  </div>' +
      '  <div id="conv-list"></div>' +
      '</div>' +
      '<div class="thread-pane">' +
      '  <div id="thread-empty" class="thread-empty">' + WB.t("Select a conversation on the left to start chatting") + '</div>' +
      '  <div id="thread" class="thread">' +
      '    <div class="thread-head" id="thread-head"></div>' +
      '    <div class="thread-msgs" id="thread-msgs"></div>' +
      '    <div class="composer" id="composer"></div>' +
      '  </div>' +
      '</div>';
    el.querySelector("#conv-search").oninput = (ev) => {
      chatState.search = ev.target.value;
      // S19-C: fire the unified search after a 300ms debounce (with fewer
      // than 2 characters runUnifiedSearch falls back to the plain conversation
      // list on its own, so no branch is needed here).
      if (searchDebounce) clearTimeout(searchDebounce);
      searchDebounce = setTimeout(runUnifiedSearch, 300);
    };
    // S14: entry point for creating a new group
    el.querySelector("#conv-new-group").onclick = () => openNewGroupModal();
    el.querySelectorAll(".conv-tabs button").forEach((b) => {
      b.onclick = () => {
        chatState.filter = b.getAttribute("data-filter");
        el.querySelectorAll(".conv-tabs button").forEach((x) => {
          x.classList.toggle("active", x === b);
        });
        renderConvList();
      };
    });
    loadConversations();
    if (chatState.selectedPeer != null) openThread(chatState.selectedPeer);
  },
  closeThread: closeThread,
});

// ---------- conversation list ----------

async function loadConversations() {
  try {
    const d = await WB.api("GET", "/api/conversations");
    chatState.conversations = d.conversations || [];
    renderConvList();
    updateTitleBadge(); // S35: tab title mirrors total unread mentions
    // S35: prime the sound baseline once (first post-login list load, so the
    // FIRST new message arriving while the app is open still beeps — stale
    // pre-session mentions only show in the title) and re-sync when returning
    // to the chat view after a view-gated message.new (mentionBaselineStale):
    // a stale baseline would make the next diff fire for a mention the user
    // already sees in the title (review F1).
    if (lastMentionTotal < 0 || mentionBaselineStale) {
      lastMentionTotal = mentionTotal();
      mentionBaselineStale = false;
    }
  } catch (e) {
    WB.apiCatch(e, WB.t("Load conversations"));
  }
  loadRoutes();
  // await: task chip labels read chatState.tasks, so callers that chain a
  // thread re-render on this promise see a fresh task cache (S18 chip race).
  await loadTasks();
}

async function loadRoutes() {
  const me = WB.state.me;
  if (!me) return;
  try {
    chatState.routes = await WB.api(
      "GET",
      "/api/routes?employee_id=" + me.employee_id
    );
  } catch (e) {
    /* non-fatal: composer falls back to an empty kind list */
  }
}

async function loadTasks() {
  const me = WB.state.me;
  if (!me) return;
  try {
    const url = me.admin
      ? "/api/tasks"
      : "/api/tasks?employee=" + me.employee_id;
    const d = await WB.api("GET", url);
    const map = {};
    (d.tasks || []).forEach((t) => { map[t.id] = t; });
    chatState.tasks = map;
  } catch (e) {
    /* non-fatal */
  }
}

// S33: pin/unpin a conversation (per-viewer). POST success -> reload so the
// server-side partition (pinned rows first) re-renders; failure -> toast and
// re-render the unchanged state. Pinned rows keep their unread/mention badges
// (pinning is display-only; it never touches read cursors).
async function togglePin(peer) {
  const c = chatState.conversations.find((x) => x.peer_id === peer);
  if (!c) return;
  try {
    await WB.api("POST", "/api/conversations/" + peer + "/pin", { pinned: !c.pinned });
    await loadConversations();
  } catch (e) {
    WB.apiCatch(e, WB.t("Pin operation failed"));
    renderConvList();
  }
}

function renderConvList() {
  const host = document.getElementById("conv-list");
  if (!host) return;
  const q = chatState.search.trim().toLowerCase();
  // S19-C: unified search results replace the conversation list; hide the
  // All/Unread tabs while searching.
  // searchResults is non-null only after a valid response with >=2 characters
  // (oninput already counts by code point), so no q check is needed here. The
  // empty state echoes the user's original input (lowercased q is only used for
  // L173's local filter).
  const searching = chatState.searchResults != null;
  const pane = host.closest(".chat-pane");
  if (pane) pane.classList.toggle("searching", searching);
  if (searching) {
    host.innerHTML = renderSearchResults(chatState.searchResults, chatState.search.trim());
    host.querySelectorAll("[data-sr-contact]").forEach((n) => {
      n.onclick = () => openVirtualThread(Number(n.getAttribute("data-sr-contact")));
    });
    host.querySelectorAll("[data-sr-msg]").forEach((n) => {
      n.onclick = () => openRecordThread(Number(n.getAttribute("data-sr-msg")));
    });
    // S32: group/channel result row -> open the group thread
    host.querySelectorAll("[data-sr-group]").forEach((n) => {
      n.onclick = () => openSearchGroup(Number(n.getAttribute("data-sr-group")));
    });
    // S36: mention result row -> open the owning thread and jump to the original message (same mechanism as message rows)
    host.querySelectorAll("[data-sr-mention]").forEach((n) => {
      n.onclick = () => openRecordThread(Number(n.getAttribute("data-sr-mention")));
    });
    return;
  }
  // S17: group entries are split by ignored into the main list and an
  // "Ignored" section at the bottom; in the Unread tab ignored groups are
  // hidden, muted groups stay but the badge is replaced with a "Mute" tag.
  const mainList = [];
  const ignoredList = [];
  for (const c of chatState.conversations) {
    if (q && c.name.toLowerCase().indexOf(q) === -1) continue;
    // S11e: the Unread tab = strictly unread messages. Open-task conversations
    // (no unread badge) only appear under All — clicking a contact should
    // remove it from Unread, otherwise it looks like mark-read failed.
    if (chatState.filter === "unread" && c.unread <= 0) continue;
    if (chatState.filter === "unread" && c.is_group && c.ignored) continue; // S17
    if (c.is_group && c.ignored) {
      ignoredList.push(c);
    } else {
      mainList.push(c);
    }
  }
  let html = "";
  if (mainList.length > 0) {
    html += mainList.map((c) => {
      const statusTag = contactStatusTag(c.status);
      // S14: groups always use the group avatar + "Group" tag; S21: project channels use a "Channel" tag; 1:1 conversation logic is unchanged
      const kindTag = c.kind === "system" ? '<span class="tag">' + WB.t("System") + '</span>'
        : c.is_group ? (c.kind === "channel"
            ? '<span class="tag channel-tag">' + WB.t("Channel") + '</span>'
            : '<span class="tag">' + WB.t("Group") + '</span>') : "";
      // S17: muted groups show no unread badge; a "Mute" tag is shown instead
      let unread;
      if (c.is_group && c.muted) {
        unread = '<span class="tag muted-tag">M</span>';
      } else {
        unread = c.unread > 0
          ? '<span class="badge">' + (c.unread > 99 ? "99+" : c.unread) + "</span>"
          : "";
      }
      // S20: mention-unread badge — messages past the read cursor that @ the
      // current user (shown even in muted groups; mentions take precedence over mute).
      const mentioned = c.unread_mentioned > 0
        ? '<span class="tag mention-tag">' + WB.t("Mentioned") + '</span>'
        : "";
      // S33: pinned rows show a Pin tag; every main-list row gets a small
      // pin button (ignored section rows get the tag but no button).
      const pinTag = c.pinned ? '<span class="tag pin-tag">' + WB.t("Pinned") + '</span>' : "";
      const pinBtn = '<button class="conv-pin pix-btn" data-pin="' + c.peer_id +
        '" title="' + (c.pinned ? WB.t("Unpin") : WB.t("Pin")) + '">📌</button>';
      // S39: system messages carry EN contract strings -> WB.tDyn (zh renders
      // via WB_I18N_SYS); user content is never translated.
      const preview = c.last_msg_body
        ? (c.last_msg_kind === "system" ? WB.tDyn(c.last_msg_body) : c.last_msg_body)
        : c.last_msg_kind
          ? "[" + WB.t(KIND_LABELS[c.last_msg_kind] || c.last_msg_kind) + "]"
          : WB.t("No messages");
      const tasks = c.open_tasks > 0
        ? '<span class="task-chip">' + c.open_tasks + " " + WB.t("tasks") + "</span>"
        : "";
      return (
        '<div class="conv' + (chatState.selectedPeer === c.peer_id ? " active" : "") +
        '" data-peer="' + c.peer_id + '">' +
        '<div class="conv-avatar">' +
        (c.is_group ? WB.avatarSpan("group", "conv-ava", null) : WB.avatarSpan(c.name, "conv-ava", c.peer_id)) +
        "</div>" +
        '<div class="conv-main">' +
        '<div class="conv-top"><span class="conv-name">' + WB.esc(c.name) + "</span>" +
        pinTag + statusTag + kindTag + mentioned + unread + "</div>" +
        '<div class="conv-preview">' + WB.esc(truncate(preview, 42)) + tasks + "</div>" +
        '<div class="conv-time">' + WB.esc(fmtTime(c.last_msg_at)) + "</div>" +
        "</div>" + pinBtn + "</div>"
      );
    }).join("");
  }
  // S17: ignored groups get a separate section, greyed out but still clickable.
  if (ignoredList.length > 0) {
    html += '<div class="gs-ignored-header">' + WB.t("Ignored") + '</div>';
    html += ignoredList.map((c) => {
      // S39: system messages carry EN contract strings -> WB.tDyn; user content raw.
      const preview = c.last_msg_body
        ? (c.last_msg_kind === "system" ? WB.tDyn(c.last_msg_body) : c.last_msg_body)
        : c.last_msg_kind
          ? "[" + WB.t(KIND_LABELS[c.last_msg_kind] || c.last_msg_kind) + "]"
          : WB.t("No messages");
      return (
        '<div class="conv ignored' + (chatState.selectedPeer === c.peer_id ? " active" : "") +
        '" data-peer="' + c.peer_id + '">' +
        '<div class="conv-avatar">' +
        WB.avatarSpan("group", "conv-ava", null) +
        "</div>" +
        '<div class="conv-main">' +
         '<div class="conv-top"><span class="conv-name">' + WB.esc(c.name) + '</span>' +
          (c.pinned ? '<span class="tag pin-tag">' + WB.t("Pinned") + '</span>' : "") +
          (c.kind === "channel" ? '<span class="tag channel-tag">' + WB.t("Channel") + '</span>' : '<span class="tag">' + WB.t("Group") + '</span>') +
         '</div>' +
        '<div class="conv-preview">' + WB.esc(truncate(preview, 42)) + "</div>" +
        '<div class="conv-time">' + WB.esc(fmtTime(c.last_msg_at)) + "</div>" +
        "</div></div>"
      );
    }).join("");
  }
  if (!html) html = '<p class="muted" style="padding:12px">' + WB.t("No conversations") + '</p>';
  host.innerHTML = html;
  host.querySelectorAll(".conv").forEach((n) => {
    n.onclick = () => openThread(Number(n.getAttribute("data-peer")));
  });
  // S33: pin buttons — stopPropagation so the row click (openThread) doesn't fire.
  host.querySelectorAll(".conv-pin").forEach((n) => {
    n.onclick = (e) => {
      e.stopPropagation();
      togglePin(Number(n.getAttribute("data-pin")));
    };
  });
}

// ---------- S19-C: unified search (contacts + messages) ----------
// 300ms input debounce; <2 characters falls back to the plain conversation
// list. The results panel has two sections (contacts / messages):
// clicking a contact opens a virtual 1:1 thread (when the peer has no
// conversation yet, openThread renders the header from virtualPeer);
// clicking a message opens its thread and jumps to the original message
// (reusing S16's scroll+flash; toast when outside the window).

let searchDebounce = null;

// Count by code point (matches the server's utf8.RuneCountInString >=2
// threshold; a surrogate pair counts as one, not two).
function runeCount(s) {
  return s.length ? Array.from(s).length : 0;
}

async function runUnifiedSearch() {
  const q = chatState.search.trim();
  if (runeCount(q) < 2) {
    chatState.searchResults = null;
    renderConvList();
    return;
  }
  try {
    const d = await WB.api(
      "GET",
      "/api/search?q=" + encodeURIComponent(q) + "&limit=20"
    );
    if (chatState.search.trim() !== q) return; // stale response: the user changed the query
    chatState.searchResults = d;
    renderConvList();
  } catch (e) {
    if (chatState.search.trim() === q) {
      chatState.searchResults = null;
      renderConvList();
      WB.apiCatch(e, WB.t("Search"));
    }
  }
}

function clearSearch() {
  if (searchDebounce) clearTimeout(searchDebounce);
  chatState.search = "";
  chatState.searchResults = null;
  const input = document.getElementById("conv-search");
  if (input) input.value = "";
}

// srThreadName labels a message result row with which conversation it is in:
// group = the group name (falls back to "Group" when unknown),
// 1:1 = "sender ↔ receiver".
function srThreadName(m) {
  if (m.group_id != null) {
    const c = chatState.conversations.find(
      (x) => x.is_group && x.group_id === m.group_id
    );
    return c ? c.name : WB.t("Group chat");
  }
  const from = m.from_id == null ? "boss" : WB.empName(m.from_id) || "?";
  const to = m.to_id == null ? "boss" : WB.empName(m.to_id) || "?";
  return from + " ↔ " + to;
}

function renderSearchResults(d, q) {
  let html = "";
  const contacts = d.contacts || [];
  if (contacts.length > 0) {
    html += '<div class="sr-section">' + WB.t("Contacts") + '</div>' + contacts.map((c) => (
      '<div class="conv sr-contact" data-sr-contact="' + c.employee_id + '">' +
      '<div class="conv-avatar">' +
      WB.avatarSpan(c.name, "conv-ava", c.employee_id) +
      "</div>" +
      '<div class="conv-main">' +
      '<div class="conv-top"><span class="conv-name">' + WB.esc(c.name) + "</span>" +
      (c.role ? '<span class="tag">' + WB.esc(c.role) + "</span>" : "") +
      contactStatusTag(c.status) +
      "</div>" +
      '<div class="conv-preview">' + WB.t("Click to start a conversation") + '</div>' +
      "</div></div>"
    )).join("");
  }
  // S32: group/channel name search (only groups the viewer belongs to, so every hit is openable).
  const groups = d.groups || [];
  if (groups.length > 0) {
    html += '<div class="sr-section">' + WB.t("Group chats") + '</div>' + groups.map((g) => (
      '<div class="conv sr-group" data-sr-group="' + g.group_id + '">' +
      '<div class="conv-avatar">' +
      WB.avatarSpan("group", "conv-ava", null) +
      "</div>" +
      '<div class="conv-main">' +
       '<div class="conv-top"><span class="conv-name">' + WB.esc(g.name) + "</span>" +
        (g.kind === "channel" ? '<span class="tag channel-tag">' + WB.t("Channel") + '</span>' : '<span class="tag">' + WB.t("Group chat") + '</span>') +
       "</div>" +
      '<div class="conv-preview">' + g.member_count + " " + WB.t("members") + " · " + WB.t("Click to open") + "</div>" +
      "</div></div>"
    )).join("");
  }
  // S36: mention section — messages that @ the current user and match the
  // query, ordered between groups and messages. A message matching both an @
  // and the body appears once here and once in the message section (row
  // attribute data-sr-mention; clicks reuse openRecordThread).
  const mentions = d.mentions || [];
  if (mentions.length > 0) {
    html += '<div class="sr-section">' + WB.t("Mentions") + '</div>' + mentions.map((m) => {
      const fromName = m.from_id == null ? "boss" : WB.empName(m.from_id) || "boss";
      // S39: translate system-message bodies; user content raw.
      const preview = (m.kind === "system" ? WB.tDyn(m.body || "") : (m.body || "")).replace(/\s+/g, " ").trim();
      return (
        '<div class="conv sr-mention" data-sr-mention="' + m.id + '">' +
        '<div class="conv-avatar">' +
        (m.group_id != null
          ? WB.avatarSpan("group", "conv-ava", null)
          : WB.avatarSpan(fromName, "conv-ava", m.from_id == null ? 0 : m.from_id)) +
        "</div>" +
        '<div class="conv-main">' +
        '<div class="conv-top"><span class="conv-name">' + WB.esc(srThreadName(m)) + "</span>" +
        '<span class="tag mention-tag">' + WB.t("Mentions you") + '</span>' +
        '<span class="tag">' + WB.t(KIND_LABELS[m.kind] || m.kind) + "</span></div>" +
        '<div class="conv-preview">' + WB.esc(truncate(preview || WB.t("(attachment)"), 42)) + "</div>" +
        '<div class="conv-time">' + WB.esc(fmtTime(m.created_at)) + "</div>" +
        "</div></div>"
      );
    }).join("");
  }
  const msgs = d.messages || [];
  if (msgs.length > 0) {
    html += '<div class="sr-section">' + WB.t("Messages") + '</div>' + msgs.map((m) => {
      const fromName = m.from_id == null ? "boss" : WB.empName(m.from_id) || "boss";
      // S39: translate system-message bodies; user content raw.
      const preview = (m.kind === "system" ? WB.tDyn(m.body || "") : (m.body || "")).replace(/\s+/g, " ").trim();
      return (
        '<div class="conv sr-msg" data-sr-msg="' + m.id + '">' +
        '<div class="conv-avatar">' +
        (m.group_id != null
          ? WB.avatarSpan("group", "conv-ava", null)
          : WB.avatarSpan(fromName, "conv-ava", m.from_id == null ? 0 : m.from_id)) +
        "</div>" +
        '<div class="conv-main">' +
        '<div class="conv-top"><span class="conv-name">' + WB.esc(srThreadName(m)) + "</span>" +
        '<span class="tag">' + WB.t(KIND_LABELS[m.kind] || m.kind) + "</span></div>" +
        '<div class="conv-preview">' + WB.esc(truncate(preview || WB.t("(attachment)"), 42)) + "</div>" +
        '<div class="conv-time">' + WB.esc(fmtTime(m.created_at)) + "</div>" +
        "</div></div>"
      );
    }).join("");
  }
  if (!html) html = '<p class="muted" style="padding:12px">' + WB.t("No results for") + " \"" + WB.esc(q) + "\"</p>";
  return html;
}

// Clicking a searched contact: if the peer has no conversation yet this is a
// "virtual thread" — the header shows the search result's name/role, and after
// the first message is sent, loadConversations brings back the real
// conversation and the header normalizes.
function openVirtualThread(contactID) {
  const list = (chatState.searchResults && chatState.searchResults.contacts) || [];
  const c = list.find((x) => x.employee_id === contactID);
  if (!c) return;
  chatState.virtualPeer = { id: c.employee_id, name: c.name, role: c.role };
  clearSearch();
  openThread(c.employee_id);
}

// S32: clicking a group/channel in the search results -> open its group thread (same negative peer-id convention as 1:1).
function openSearchGroup(groupID) {
  const list = (chatState.searchResults && chatState.searchResults.groups) || [];
  if (!list.find((x) => x.group_id === groupID)) return;
  chatState.virtualPeer = null;
  clearSearch();
  openThread(-(groupID + 1));
}

// Clicking a searched message: open its thread (group = negative peer id;
// 1:1 = the peer), then jump to the original message after render.
// Peer = the other party, with boss normalized to 0 (boss from_id/to_id are
// stored NULL): I sent it -> peer = receiver; otherwise -> peer = sender.
// When the boss views someone else's 1:1, the boss↔other thread is opened
// (the original message may be outside the window -> toast).
async function openRecordThread(messageID) {
  // S36: mention rows and message rows share the same open logic — look the id up in both facets.
  const sr = chatState.searchResults || {};
  const list = (sr.messages || []).concat(sr.mentions || []);
  const m = list.find((x) => x.id === messageID);
  if (!m) return;
  clearSearch();
  const me = WB.state.me;
  const meID = me && !me.admin ? me.employee_id : 0;
  const fromID = m.from_id == null ? 0 : m.from_id;
  const toID = m.to_id == null ? 0 : m.to_id;
  let peer;
  if (m.group_id != null) peer = -(m.group_id + 1); // S14 group conversation convention
  else if (fromID === meID) peer = toID;            // I sent it: peer = receiver
  else peer = fromID;                                // they sent it: peer = sender
  chatState.virtualPeer = null;
  await openThread(peer);
  jumpToMessage(m.id);
}

// Jump to the original message inside the loaded window: scroll to center +
// flash the bubble for 1.5s (same mechanism as S16 quote jump); if the
// original message is outside the window (beyond limit=100 or cross-conversation)
// -> toast, never silent.
function jumpToMessage(mid) {
  const host = document.getElementById("thread-msgs");
  if (!host) return;
  const row = host.querySelector('.bubble-row[data-mid="' + mid + '"]');
  if (!row) {
    WB.toast(WB.t("Original message not in current window"));
    return;
  }
  row.scrollIntoView({ behavior: "smooth", block: "center" });
  const bub = row.querySelector(".bubble");
  if (!bub) return;
  bub.classList.remove("flash");
  void bub.offsetWidth; // restart the animation so it can trigger again
  bub.classList.add("flash");
  setTimeout(() => bub.classList.remove("flash"), 1500);
}

// Online-status label shared by the conversation list and search results.
function contactStatusTag(status) {
  if (status === "online") return '<span class="tag online">' + WB.t("Online") + '</span>';
  if (status === "offline") return '<span class="tag offline">' + WB.t("Offline") + '</span>';
  if (status === "manual") return '<span class="tag">' + WB.t("Manual") + '</span>';
  return "";
}

// ---------- thread ----------

// S19-C: thread header render. A normal conversation renders from its
// conversation row; if the peer has no conversation yet and came from search
// results (chatState.virtualPeer), render a "virtual conversation" header from
// the result's name/role — it self-heals once the first message lands.
function renderThreadHead(peer) {
  const c = chatState.conversations.find((x) => x.peer_id === peer);
  if (c) chatState.virtualPeer = null; // real conversation exists; drop the virtual header
  const vp = !c && chatState.virtualPeer && chatState.virtualPeer.id === peer
    ? chatState.virtualPeer : null;
  const head = document.getElementById("thread-head");
  if (!head || (!c && !vp)) return;
  // S15: back button pinned on top (.pix-btn pixel style); on desktop #thread-back is display:none and takes no layout space.
  const backBtn =
    '<button id="thread-back" class="pix-btn" type="button" title="' + WB.t("Back to conversations") + '">←</button>';
  if (c && c.is_group) {
    // S21: groups and project channels share one mechanism; only the header tag and settings title differ
    const kindLabel = c.kind === "channel" ? WB.t("Project channel") : WB.t("Group chat");
    head.innerHTML =
      backBtn +
      '<span class="thread-name">' + WB.esc(c.name) + '</span>' +
      '<span class="muted">' + kindLabel + ' · ' + (c.member_count || 0) + ' ' + WB.t("members") + '</span>' +
      '<span class="thread-actions">' +
      '<button id="thread-tasks" class="pix-btn" type="button" title="' + WB.t("Task board") + '">' + WB.t("Tasks") + '</button>' +
      '<button id="thread-more" class="pix-btn" type="button" title="' + WB.t("Group settings") + '">…</button>' +
      '</span>';
    head.querySelector("#thread-tasks").onclick = () => openGroupTasksModal(c.group_id);
    head.querySelector("#thread-more").onclick = () => openGroupSettingsModal(c.group_id);
  } else {
    const name = c ? c.name : vp.name + " (" + WB.t("virtual session") + ")";
    const role = c ? c.role : (vp ? vp.role : null);
    head.innerHTML =
      backBtn +
      '<span class="thread-name">' + WB.esc(name) + "</span>" +
      (role ? '<span class="muted">' + WB.esc(role) + "</span>" : "");
  }
  head.querySelector("#thread-back").onclick = () => closeThread();
}

async function openThread(peer) {
  closeGroupTasksModal(); // S24: collapse the task board when switching threads
  if (chatState.selectedPeer !== peer) chatState.attachments = [];
  chatState.replyTo = null; // S16: clear any pending quote when opening a conversation
  chatState.selectedPeer = peer;
  renderConvList();
  const empty = document.getElementById("thread-empty");
  const thread = document.getElementById("thread");
  if (!empty || !thread) return;
  empty.classList.add("gone");
  thread.classList.add("open");
  // S15: on mobile the thread becomes a full-screen overlay (covering the bottom nav; #thread-back goes back).
  // The class is not added on desktop; even if a stale class remained, #block-chat.m-thread has no CSS effect on desktop.
  if (WB.isMobile()) {
    const block = document.getElementById("block-chat");
    if (block) block.classList.add("m-thread");
  }
  renderThreadHead(peer);
  // S22/S23: in a group thread, the boss or a leader with a dispatch route must
  // have the target-member list ready before the composer renders.
  // S23: the non-admin "Dispatch" option depends on /api/routes, and loadRoutes
  // is not awaited at startup; a leader opening a group thread before the routes
  // response would miss the dispatch option and member fetch, so wait once here.
  const me = WB.state.me;
  if (peer < 0 && me && !me.admin && !chatState.routes) {
    await loadRoutes();
  }
  const dispatchTargets = (chatState.routes || {}).dispatch || [];
  if (peer < 0 && me) {
    // S34: @ autocomplete needs the full member list (incl. the boss row) in
    // EVERY group thread, so load it unconditionally for group peers — the old
    // S22 gate (admin || has-dispatch-route) left leaf workers with an empty
    // cache and a @ menu that never appeared. Cached + silent-fail.
    await loadGroupMembers(-peer - 1);
    if (me.admin || dispatchTargets.length > 0) {
      await loadProjects(); // S29: project dropdown options (only needed by dispatchers)
    }
  }
  renderComposer(peer);
  await reloadThread(peer);
  markRead(peer);
}

// S22: fetch a group's member list (GET /api/groups/{gid}) into chatState.
// Non-fatal: on failure the target dropdown shows the placeholder and the
// server remains the final authority on membership.
async function loadGroupMembers(gid) {
  if (!gid || gid <= 0) return;
  if (chatState.groupMembers[gid]) return; // cached
  try {
    const d = await WB.api("GET", "/api/groups/" + gid);
    // S34: keep the FULL member list (including the boss row employee_id 0)
    // so the @ autocomplete can offer "boss"; the dispatch-target list filters
    // boss out explicitly (dispatchTargetOptions), behavior unchanged.
    chatState.groupMembers[gid] = d.members || [];
  } catch (e) {
    /* silent: dispatch target list falls back to the placeholder option */
  }
}

// S22/S23: options for the in-group dispatch target dropdown.
// boss sees every employee member; leaders only see members that /api/routes
// says they may dispatch to (the server still remains the final authority).
function dispatchTargetOptions(gid) {
  const me = WB.state.me;
  // S34: groupMembers now includes the boss row (employee_id 0) for @ autocomplete;
  // the dispatch target list must stay employee-only.
  let members = (chatState.groupMembers[gid] || []).filter((m) => m.employee_id > 0);
  if (me && !me.admin) {
    const disp = (chatState.routes || {}).dispatch || [];
    members = members.filter((m) => disp.indexOf(m.employee_id) !== -1);
  }
  if (members.length === 0) {
    return '<option value="0">' + WB.t("(no dispatchable members)") + '</option>';
  }
  return '<option value="0">' + WB.t("(select dispatch target)") + '</option>' +
    members.map((m) => '<option value="' + m.employee_id + '">' + WB.esc(m.name) + "</option>").join("");
}

// S22: rebuild the dispatch-target dropdown in place on group.members.
// Does NOT re-render the whole composer (that would wipe the draft body,
// attachments and quote preview); the current selection is preserved when
// the target is still a member.
function refreshDispatchTargets() {
  if (chatState.selectedPeer == null || chatState.selectedPeer >= 0) return;
  const toSel = document.getElementById("composer-dispatch-to");
  if (!toSel) return;
  const gid = -chatState.selectedPeer - 1;
  const cur = toSel.value;
  toSel.innerHTML = dispatchTargetOptions(gid);
  if (cur && toSel.querySelector('option[value="' + cur + '"]')) toSel.value = cur;
}

// S29: lazy-load the project list once (GET /api/projects). On failure do not
// cache (keep null so the next call retries) and stay silent — the project
// dropdown falls back to the "(no project)" placeholder.
async function loadProjects() {
  if (chatState.projects) return;
  try {
    const d = await WB.api("GET", "/api/projects");
    chatState.projects = d.projects || [];
  } catch (e) {
    /* silent: keep null so the next call retries */
  }
}

// S29: project dropdown options for group dispatch (a "(no project)" entry + all projects, sourced from chatState.projects).
function dispatchProjectOptions() {
  return '<option value="0">' + WB.t("(no project)") + '</option>' +
    (chatState.projects || []).map(
      (p) => '<option value="' + p.id + '">' + WB.esc(p.name) + "</option>"
    ).join("");
}

// S15: back to the conversation list. Used on mobile by #thread-back and the bottom-nav "Chat";
// desktop behavior unchanged (core.showView only calls it when isMobile()).
function closeThread() {
  closeGroupTasksModal(); // S24: also collapse the task board when leaving the thread
  chatState.selectedPeer = null;
  chatState.virtualPeer = null; // S19-C: drop the virtual conversation header when leaving the thread
  const block = document.getElementById("block-chat");
  if (block) block.classList.remove("m-thread");
  renderConvList();
}

// S41: tracks which peer's messages #thread-msgs currently shows, so
// echoSentMessage never appends onto a stale list left over from another
// thread whose load failed.
let lastRenderedPeer = null;

async function reloadThread(peer) {
  try {
    // S14: group conversations use peer<0; fetch messages by group id (peer_id = -(gid+1))
    const url = peer < 0
      ? "/api/messages?group=" + (-peer - 1) + "&limit=100"
      : "/api/messages?with=" + peer + "&limit=100";
    const d = await WB.api("GET", url);
    // S41: stale-response guard — if the user switched threads while this
    // fetch was in flight, rendering would clobber the new thread with the
    // old thread's messages.
    if (chatState.selectedPeer !== peer) return false;
    lastRenderedPeer = peer;
    renderThread(d.messages || []);
    return true;
  } catch (e) {
    // S41: only surface the failure while the user is still on this thread;
    // the new thread's own load owns its error path.
    if (chatState.selectedPeer === peer) WB.apiCatch(e, WB.t("Load conversation"));
    return false;
  }
}

// S41: optimistic echo of a just-sent message. Appended to the open thread
// right after a successful POST /api/messages, so the sent bubble is visible
// even if the post-send refresh (WS message.new + reloadThread) fails — e.g.
// the SSH-tunnel network blip that left users with a "Sent" toast but no
// visible bubble. The background reloadThread replaces the echo with the
// server row (real created_at/files/task chip); if the reload keeps failing
// the echo stays, which is still truth (the server accepted the message).
// No-op if the user already switched threads (the conversation list carries
// the last message and will show it on the next open).
function echoSentMessage(peer, payload, d) {
  if (chatState.selectedPeer !== peer) return;
  const me = WB.state.me;
  const now = new Date();
  const p = (n) => String(n).padStart(2, "0");
  // Naive UTC string matching the server's datetime('now') format so
  // fmtTime renders it with the same UTC semantics as real rows (S40).
  const createdAt =
    now.getUTCFullYear() + "-" + p(now.getUTCMonth() + 1) + "-" + p(now.getUTCDate()) +
    " " + p(now.getUTCHours()) + ":" + p(now.getUTCMinutes()) + ":" + p(now.getUTCSeconds());
  const row = {
    id: (d && d.message_id) || 0,
    from_id: me && me.admin ? null : (me ? me.employee_id : null),
    to_id: peer > 0 ? peer : null,
    kind: payload.kind,
    body: payload.body || "",
    group_id: peer < 0 ? -peer - 1 : null,
    task_id: d && d.task_id ? d.task_id : null,
    reply_to: payload.reply_to != null ? payload.reply_to : null,
    files: null,
    mentions: [],
    read: null,
    created_at: createdAt,
  };
  // Only append to a message list that actually belongs to this thread;
  // after a failed initial load chatState.messages may still hold another
  // thread's rows (S41 stale-content guard).
  const base = lastRenderedPeer === peer ? (chatState.messages || []) : [];
  chatState.messages = base.concat([row]);
  renderThread(chatState.messages);
}

// S35: now async so refreshAfterLoad can await the post-API zero-out before
// diffing the mention total (a mention in the active thread must not beep).
async function markRead(peer) {
  // S14: group conversations use the group selector (mutually exclusive with "with"; enforced server-side)
  const payload = peer < 0 ? { group: -peer - 1 } : { with: peer };
  try {
    await WB.api("POST", "/api/messages/read", payload);
    const c = chatState.conversations.find((x) => x.peer_id === peer);
    // S20: cursor advanced to the latest -> clear both the unread and "mentioned" badges
    if (c) { c.unread = 0; c.unread_mentioned = 0; }
    renderConvList();
    updateTitleBadge(); // S35: opening/read clears the mention badge
  } catch (e) { /* silent: unread badge refreshes on next list load */ }
}

// ---------- S35: @-mention title badge + notification sound ----------
// The WS message.new payload only carries message_id (flow.go), so "was I
// mentioned" is derived by diffing the conversations' unread_mentioned total
// before/after a WS-driven refresh. A mention in the *active* thread nets to
// zero because markRead() zeroes it before noteMentionChange() runs.
const WB_TITLE_BASE = document.title; // index.html sets "whatbark"
// -1 = uninitialized. Primed on first post-login list load (loadConversations)
// and re-synced when returning to the chat view after message.new events were
// view-gated (mentionBaselineStale) — a stale baseline would make the next
// diff fire a sound for a mention already visible in the title (review F1).
let lastMentionTotal = -1;
let mentionBaselineStale = false; // set when a message.new is view-gated or trailing-skipped
let mentionAudioCtx = null;

function mentionTotal() {
  let n = 0;
  (chatState.conversations || []).forEach((c) => { n += c.unread_mentioned || 0; });
  return n;
}

function updateTitleBadge() {
  const n = mentionTotal();
  document.title = n > 0 ? "(@" + n + ") " + WB_TITLE_BASE : WB_TITLE_BASE;
}

// Two short square-wave beeps (880→1320Hz, pixel style). No audio asset in
// the repo — synthesized. Best-effort: browsers block audio before the first
// user gesture (autoplay policy), so a pre-gesture mention may be silent — a
// platform boundary, not a bug.
function playMentionSound() {
  try {
    if (!mentionAudioCtx) mentionAudioCtx = new AudioContext();
    if (mentionAudioCtx.state === "suspended") mentionAudioCtx.resume();
    const t0 = mentionAudioCtx.currentTime;
    [880, 1320].forEach((freq, i) => {
      const osc = mentionAudioCtx.createOscillator();
      const gain = mentionAudioCtx.createGain();
      osc.type = "square";
      osc.frequency.value = freq;
      gain.gain.value = 0.04;
      osc.connect(gain);
      gain.connect(mentionAudioCtx.destination);
      const at = t0 + i * 0.09;
      osc.start(at);
      osc.stop(at + 0.07);
    });
  } catch (e) { /* audio is nice-to-have; never break the UI */ }
}

// Called at the tail of refreshAfterLoad("message.new"): a rising
// unread_mentioned total means a new mention landed in a thread I am not
// actively reading.
function noteMentionChange() {
  const now = mentionTotal();
  if (lastMentionTotal >= 0 && now > lastMentionTotal) playMentionSound();
  lastMentionTotal = now;
}

// S19-B: read receipt indicator on messages I sent (1:1 = peer read; group = any member read)
function readIndicator(m, mine, isGroup) {
  if (!mine || m.read == null) return "";
  const title = isGroup
    ? (m.read ? WB.t("Read by someone") : WB.t("Not read yet"))
    : (m.read ? WB.t("Read by them") : WB.t("Unread"));
  return '<span class="msg-read' + (m.read ? " is-read" : "") + '" title="' + title + '">' +
    (m.read ? "✓✓" : "✓") + "</span>";
}

function renderThread(msgs) {
  const host = document.getElementById("thread-msgs");
  if (!host) return;
  chatState.messages = msgs; // S13: for right-click menu lookups
  const me = WB.state.me;
  const myID = me ? me.employee_id : -1;
  // S19-B: group threads use the negative-peer convention in chatState.
  const isGroup = chatState.selectedPeer != null && chatState.selectedPeer < 0;
  host.innerHTML = msgs.length === 0
    ? '<p class="muted">' + WB.t("No messages yet") + '</p>'
    : msgs.map((m) => {
      const mine = me && me.admin ? m.from_id == null : m.from_id === myID;
      const fromName = m.from_id == null ? "boss" : WB.empName(m.from_id);
      const myName = me && me.admin ? "boss" : (me && me.employee ? me.employee.name : null);
      const avaName = mine ? myName : fromName;
      // S12: resolve the avatar owner (0 = boss) so custom avatars render.
      const avaID = mine ? (me && me.admin ? 0 : myID) : (m.from_id == null ? 0 : m.from_id);
      const label = WB.t(KIND_LABELS[m.kind] || m.kind);
      // S20: mentions you — m.mentions contains the current viewer (boss=0 / employee=employee_id).
      const mentionedMe = Array.isArray(m.mentions) && m.mentions.includes(myID);
      const mentionFlag = mentionedMe
        ? '<span class="tag mention-flag">' + WB.t("Mentions you") + '</span>'
        : "";
      // S16: leading consecutive "> " lines render as a quote box (clickable,
      // jumps to the original message); the quote text stays in the body and
      // reply_to only drives the jump. (leading consecutive
      // "> " lines render as a quote box; the text itself stays in the body.)
      // S39: system bodies are EN contract strings -> WB.tDyn; user content raw.
      const rawBody = String(m.body || "");
      const lines = (m.kind === "system" ? WB.tDyn(rawBody) : rawBody).split("\n");
      let qEnd = 0;
      while (qEnd < lines.length && /^> ?/.test(lines[qEnd])) qEnd++;
      const quoteLines = lines.slice(0, qEnd).map((l) => l.replace(/^> ?/, ""));
      const restText = lines.slice(qEnd).join("\n");
      const orig = m.reply_to != null ? msgs.find((x) => x.id === m.reply_to) : null;
      // S16.1: whenever reply_to has a value, always attach data-jump (if the
      // original message is outside the window on click -> toast, no longer
      // silent); only when it cannot be found in the window add a title hint.
      const quote = quoteLines.length > 0
        ? '<div class="msg-quote"' +
          (m.reply_to != null ? ' data-jump="' + m.reply_to + '"' : "") +
          (orig ? "" : ' title="' + WB.t("Original message not in current window") + '"') +
          ">" + quoteLines.map((l) => WB.esc(l)).join("<br>") +
          '<span class="msg-quote-sender"> · ' + WB.t("Reply to ") +
          WB.esc(orig ? (orig.from_id == null ? "boss" : WB.empName(orig.from_id)) : WB.t("Original message")) +
          "</span></div>"
        : "";
      return (
        '<div class="bubble-row ' + (mine ? "mine" : "theirs") + '" data-mid="' + m.id + '">' +
        '<div class="bubble-meta">' + WB.avatarSpan(avaName, "meta-ava", avaID) + " " +
         WB.esc(mine ? WB.t("Me") : fromName) + " · " + label + mentionFlag +
         " · " + WB.esc(fmtTime(m.created_at)) + readIndicator(m, mine, isGroup) + "</div>" +
        '<div class="bubble ' + (mine ? "bubble-mine" : "bubble-theirs") + '">' +
        quote +
        (restText ? highlightMention(WB.esc(restText)) : "") +
        filesHTML(m.files) +
        (m.task_id ? taskChip(m) : "") +
        taskActions(m) +
        "</div></div>"
      );
    }).join("");
  host.querySelectorAll("[data-finish]").forEach(b => {
    b.onclick = async () => {
      const out = prompt(WB.t("Complete task — enter output/notes:"));
      if (out === null) return;
      try {
        await WB.api("POST", "/api/tasks/" + b.getAttribute("data-finish") + "/finish", { output: out });
        WB.toast(WB.t("Task completed"));
        refreshChat();
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
        refreshChat();
      } catch (e) { WB.apiCatch(e, WB.t("Reject task")); }
    };
  });
  host.querySelectorAll("[data-cancel]").forEach(b => {
    b.onclick = async () => {
      if (!confirm(WB.t("Confirm cancel this task?"))) return;
      try {
        await WB.api("POST", "/api/tasks/" + b.getAttribute("data-cancel") + "/cancel", { reason: "Cancelled by admin" });
        WB.toast(WB.t("Task canceled"));
        refreshChat();
      } catch (e) { WB.apiCatch(e, WB.t("Cancel task")); }
    };
  });
  host.querySelectorAll("[data-reassign]").forEach(b => {
    b.onclick = () => {
      const taskId = Number(b.getAttribute("data-reassign"));
      const t = chatState.tasks[taskId];
      if (!t) return;
      reassignPicker(b, taskId, t.employee_id, "Reassigned from chat", () => refreshChat());
    };
  });
  // task-duration-artifacts A: clicking a task chip opens the task detail (with duration and artifacts).
  host.querySelectorAll("[data-task-detail]").forEach((n) => {
    n.onclick = () => openTaskDetail(Number(n.getAttribute("data-task-detail")));
  });
  // S16: clicking the quote box jumps to the original message (within the currently loaded window); the target bubble flashes.
  host.querySelectorAll(".msg-quote[data-jump]").forEach((q) => {
    q.onclick = () => {
      const row = host.querySelector('.bubble-row[data-mid="' + q.getAttribute("data-jump") + '"]');
      if (!row) { WB.toast(WB.t("Original message not in current window")); return; }
      row.scrollIntoView({ behavior: "smooth", block: "center" });
      const bub = row.querySelector(".bubble");
      if (!bub) return;
      bub.classList.remove("flash");
      void bub.offsetWidth; // restart the flash on re-click
      bub.classList.add("flash");
      setTimeout(() => bub.classList.remove("flash"), 1500);
    };
  });
  host.scrollTop = host.scrollHeight;
}

// refreshChat reloads the conversation list, task cache and the open thread.
// The thread re-render is chained after loadConversations so the task cache
// (fetched inside it) is fresh before chips re-render.
function refreshChat() {
  loadConversations().then(() => {
    if (chatState.selectedPeer != null) reloadThread(chatState.selectedPeer);
  });
}

// S28: takes the whole message row. Group threads carry a server-annotated
// task_status (live status) on every row with a task_id, so the chip flips
// for non-owners too; 1:1 rows have no task_status and fall back to the local
// task cache as before.
function taskChip(m) {
  const taskID = m.task_id;
  const cached = chatState.tasks[taskID];
  const st = m.task_status != null ? m.task_status : (cached ? cached.status : "…");
  // task-duration-artifacts A: duration is server-computed on the cached task
  // row (language-neutral "52s"/"4m 51s"); absent for queued tasks.
  const dur = cached && cached.duration ? " · " + WB.esc(cached.duration) : "";
  return (
    '<div class="msg-actions"><span class="tag ' + (st || "") + '" data-task-detail="' + taskID + '" title="' + WB.t("Task detail") + '" style="cursor:pointer">' +
    WB.t("Task #") + taskID + " · " + WB.t(TASK_LABELS[st] || st) + dur + "</span></div>"
  );
}

function taskActions(m) {
  if (!m.task_id) return "";
  const me = WB.state.me;
  if (!me) return "";
  const t = chatState.tasks[m.task_id];
  if (me.admin) {
    if (!t || t.status === "done") return "";
    let html = '<div class="msg-actions">';
    if (t.status === "queued" || t.status === "running") {
      html += '<button class="btn small danger" data-cancel="' + m.task_id + '">' + WB.t("Cancel task") + '</button>';
    }
    // S27: reassign button — shown for queued/running/canceled/failed (S27 review N3: secondary style)
    html += '<button class="btn small secondary" data-reassign="' + m.task_id + '">' + WB.t("Reassign") + '</button>';
    html += '</div>';
    return html;
  }
  if (!t || t.employee_id !== me.employee_id) return "";
  if (t.status !== "queued" && t.status !== "running") return "";
  return (
    '<div class="msg-actions">' +
    '<button class="btn small" data-finish="' + m.task_id + '">' + WB.t("Complete") + '</button>' +
    '<button class="btn small danger" data-fail="' + m.task_id + '">' + WB.t("Reject") + '</button></div>'
  );
}

// task-duration-artifacts A: task detail modal — status / assignee / time / duration + artifact list.
// GET /api/tasks/{id} already returns artifacts inline (with signed url), no second request needed.
// Reused by the 1:1 task chip and the group board Detail button.
let tdEl = null; // detail overlay (null = closed)
let tdOpening = false; // opening (guard the await window against double-tap opening two overlays)

function closeTaskDetailModal() {
  if (!tdEl) return;
  tdEl.remove();
  tdEl = null;
}

function tdBytes(n) {
  if (n == null) return "";
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KiB";
  return (n / (1024 * 1024)).toFixed(1) + " MiB";
}

async function openTaskDetail(taskID) {
  if (tdEl || tdOpening) return; // already open or opening
  tdOpening = true; // guard the await window against double-tap opening two overlays (an orphan overlay cannot be closed)
  let t;
  try {
    t = await WB.api("GET", "/api/tasks/" + taskID);
  } catch (e) {
    WB.apiCatch(e, WB.t("Load task detail"));
    tdOpening = false;
    return;
  }
  const overlay = document.createElement("div");
  overlay.className = "td-overlay";
  const card = document.createElement("div");
  card.className = "td-card";
  overlay.appendChild(card);
  document.body.appendChild(overlay);
  tdEl = overlay;
  tdOpening = false; // overlay mounted; tdEl guards from here on
  overlay.addEventListener("mousedown", (ev) => {
    if (ev.target === overlay) closeTaskDetailModal();
  });

  const arts = t.artifacts || [];
  const artRows = arts.length
    ? arts.map((a) =>
        '<li class="td-art">' +
        '<a class="td-art-link" href="' + WB.esc(a.url) + '" download>' + WB.esc(a.name) + "</a>" +
        '<span class="td-art-meta">' + WB.esc(a.mime || "") + " · " + tdBytes(a.size) +
        (a.created_at ? " · " + WB.esc(a.created_at) : "") + "</span></li>"
      ).join("")
    : '<li class="muted">' + WB.t("No artifacts") + "</li>";

  card.innerHTML =
    '<div class="gk-title-row">' +
    '<span class="gk-title">' + WB.t("Task #") + t.id + " · " +
    '<span class="tag ' + (t.status || "") + '">' + WB.esc(WB.t(TASK_LABELS[t.status] || t.status)) + "</span></span>" +
    '<button class="gk-close" type="button" title="' + WB.t("Close") + '">×</button>' +
    "</div>" +
    '<div class="td-grid">' +
    '<div><span class="muted">' + WB.t("Employee") + "</span><br>" +
    WB.esc(t.employee_name || "Employee #" + t.employee_id) + "</div>" +
    (t.duration
      ? '<div><span class="muted">' + WB.t("Duration") + "</span><br>" + WB.esc(t.duration) + "</div>"
      : "") +
    '<div><span class="muted">' + WB.t("Created") + "</span><br>" + WB.esc(t.created_at || "") + "</div>" +
    (t.finished_at
      ? '<div><span class="muted">' + WB.t("Finished") + "</span><br>" + WB.esc(t.finished_at) + "</div>"
      : "") +
    "</div>" +
    '<div class="td-section"><span class="muted">' + WB.t("Input") + "</span>" +
    '<pre class="td-pre">' + WB.esc(t.input || "") + "</pre></div>" +
    (t.output
      ? '<div class="td-section"><span class="muted">' + WB.t("Output") + "</span>" +
        '<pre class="td-pre">' + WB.esc(t.output) + "</pre></div>"
      : "") +
    '<div class="td-section"><span class="muted">' + WB.t("Artifacts") + "</span>" +
    '<ul class="td-arts">' + artRows + "</ul></div>";

  card.querySelector(".gk-close").onclick = closeTaskDetailModal;
}

// S27: inline reassign picker. Replaces the btn with <select>+OK+×, reused in three places.
function reassignPicker(btn, taskId, currentOwnerId, reason, onRedraw) {
  const emps = Object.values(WB.state.empIndex || {}).filter(e => e.id !== currentOwnerId);
  const opts = emps.map(e =>
    '<option value="' + e.id + '">' + WB.esc(e.name) + "</option>"
  ).join("");
  const span = document.createElement("span");
  span.className = "gk-reassign-inline";
  span.innerHTML =
    '<select class="gk-reassign-select">' + opts + "</select>" +
    '<button class="btn small" type="button" data-ra-confirm>' + WB.t("OK") + '</button>' +
    '<button class="btn small" type="button" data-ra-cancel>×</button>';
  // Stop event bubbling to the outer elements (row onclick / message row, etc.)
  span.onclick = function(e) { e.stopPropagation(); };
  btn.replaceWith(span);

  // S27 review O1: cancel/failure never refetches data (C8 negative case: no
  // refetch); the original button is restored in place; only success triggers
  // the onRedraw refresh.
  const restore = () => { if (span.isConnected) span.replaceWith(btn); };
  span.querySelector("[data-ra-cancel]").onclick = function() {
    restore();
  };
  span.querySelector("[data-ra-confirm]").onclick = async function() {
    const sel = span.querySelector("select");
    const to = Number(sel.value);
    if (!to) { WB.toast(WB.t("Please select an employee"), true); return; }
    try {
      await WB.api("POST", "/api/tasks/" + taskId + "/reassign", { to: to, reason: reason });
      WB.toast(WB.t("Task reassigned"));
      onRedraw();
    } catch (e) {
      WB.apiCatch(e, WB.t("Reassign task"));
      restore();
    }
  };
  span.querySelector("select").focus();
}

function renderComposer(peer) {
  hideAtMenu(); // S34: composer rebuild (thread switch / refresh) — drop any open menu
  const host = document.getElementById("composer");
  if (!host) return;
  const kinds = allowedKinds(peer);
  if (kinds.length === 0) {
    host.innerHTML =
      '<p class="muted">' + WB.t("No message channel available for this recipient (route-restricted); only task actions allowed.") + '</p>';
    return;
  }
  // S22/S23: when "Dispatch" appears in a group chat, a target member must also
  // be specified (the select stays hidden until kind=dispatch is chosen)
  // S29: the project dropdown shows/hides together with the target dropdown
  const groupTarget =
    peer < 0 && kinds.indexOf("dispatch") !== -1
      ? '<select id="composer-dispatch-to" hidden>' + dispatchTargetOptions(-peer - 1) + "</select>" +
        '<select id="composer-dispatch-project" hidden>' + dispatchProjectOptions() + "</select>"
      : "";
  host.innerHTML =
    '<div class="form-row">' +
    '<select id="composer-kind">' +
    kinds.map((k) => '<option value="' + k + '">' + WB.t(KIND_LABELS[k] || k) + "</option>").join("") +
    "</select>" +
    groupTarget +
    '<div class="pix-btns">' +
    '<button type="button" class="pix-btn" id="composer-img" title="' + WB.t("Image") + '">' + PIX_ICON_IMG + "</button>" +
    '<button type="button" class="pix-btn" id="composer-file" title="' + WB.t("File") + '">' + PIX_ICON_FILE + "</button>" +
    "</div>" +
    '<button class="btn" id="composer-send">' + WB.t("Send") + '</button>' +
    "</div>" +
    '<div class="attach-row" id="attach-row"></div>' +
    '<div class="composer-quote" id="composer-quote" hidden></div>' +
    '<div class="composer-ta-wrap">' +
    '<textarea id="composer-body" rows="3" placeholder="' +
    (peer < 0
      ? WB.t("Type a message… (@ to mention, Enter to send, Shift+Enter for newline, Ctrl+V to paste image)")
      : WB.t("Type a message… (Enter to send, Shift+Enter for newline, Ctrl+V to paste image)")) +
    '"></textarea>' +
    "</div>";
  // S22: show the dispatch-target dropdown only while kind=dispatch is chosen.
  // S29: the project dropdown shares the same show/hide cycle.
  const kindSel = host.querySelector("#composer-kind");
  const toSel = host.querySelector("#composer-dispatch-to");
  const projSel = host.querySelector("#composer-dispatch-project");
  if (toSel) {
    const sync = () => {
      const show = kindSel.value === "dispatch";
      toSel.hidden = !show;
      if (projSel) projSel.hidden = !show;
    };
    kindSel.onchange = sync;
    sync();
  }
  host.querySelector("#composer-send").onclick = () => sendMsg(peer);
  host.querySelector("#composer-img").onclick = () => pickFile("image/*", addPendingFile);
  host.querySelector("#composer-file").onclick = () => pickFile("*/*", addPendingFile);
  const ta = host.querySelector("#composer-body");
  ta.onkeydown = (ev) => {
    // S34: when @-mention menu is open, arrow/Enter/Tab/Esc are intercepted
    // before the Enter-to-send logic below.
    if (atMenuOpen() && handleAtKeydown(ev)) return;
    // F1: Enter during IME composition confirms pinyin, it must not send.
    if (ev.key === "Enter" && !ev.shiftKey && !ev.isComposing && ev.keyCode !== 229) {
      ev.preventDefault();
      sendMsg(peer);
    }
  };
  // S34: skip menu updates mid-IME-composition; compositionend re-triggers.
  ta.oninput = (ev) => {
    if (ev && ev.isComposing) return;
    updateAtMenu(ta, peer);
  };
  ta.addEventListener("compositionend", () => updateAtMenu(ta, peer));
  // S12: paste an image straight into the attachment queue.
  ta.onpaste = (ev) => {
    const items = ev.clipboardData ? ev.clipboardData.items : null;
    if (!items) return;
    for (let i = 0; i < items.length; i++) {
      const t = items[i].type || "";
      if (t.indexOf("image") === 0) {
        const f = items[i].getAsFile();
        if (f) { ev.preventDefault(); addPendingFile(f); }
      }
    }
  };
  renderPending();
  renderComposerQuote(); // S16.1: re-sync the quote preview box after rebuilding the composer
}

// ---------- S34: composer @ autocomplete ----------

let atMenuEl = null; // open menu element (null = closed)
let atCands = [];    // visible candidates [{id, name}]
let atActive = 0;    // highlighted index

// Mention candidates for the open composer thread: group members (boss row
// employee_id 0 included, self excluded) for peer<0. 1:1 threads (peer>=0)
// offer no candidates — user directive 2026-11-05: 1:1 chats do not need @
// (the other party is unique, so @ is redundant); group chats keep it.
// Names come from existing caches; the server stays the
// authority on what a mention actually resolves to (S20).
function atCandidates(peer) {
  const me = WB.state.me;
  if (!me || peer == null) return [];
  if (peer >= 0) return []; // no @ menu in 1:1 (typing "@" only inserts a plain character)
  const myID = me.admin ? 0 : me.employee_id;
  // S34: only offer names the mention engine can actually resolve —
  // store.mentionRe is @[A-Za-z0-9_-]{1,64}, so a non-ASCII name would
  // insert a broken mention (partial false highlight on the receiver).
  const mentionable = (n) => typeof n === "string" && /^[A-Za-z0-9_-]{1,64}$/.test(n);
  return (chatState.groupMembers[-peer - 1] || [])
    .filter((m) => m.employee_id !== myID && mentionable(m.name))
    .map((m) => ({ id: m.employee_id, name: m.name }));
}

// The @ token immediately before the caret, or null. The '@' must start a
// token (text start or preceded by whitespace) and the query up to the caret
// must be [A-Za-z0-9_-]* (mirrors store mentionRe; whitespace ends the token,
// so a finished mention never re-triggers the menu).
function atTokenAt(ta) {
  const pos = ta.selectionStart;
  if (pos == null) return null;
  const before = ta.value.slice(0, pos);
  const i = before.lastIndexOf("@");
  if (i < 0) return null;
  if (i > 0 && !/\s/.test(before[i - 1])) return null;
  const query = before.slice(i + 1);
  if (!/^[A-Za-z0-9_-]*$/.test(query)) return null;
  return { start: i, query };
}

function atMenuOpen() {
  return !!(atMenuEl && atMenuEl.isConnected);
}

function updateAtMenu(ta, peer) {
  const token = atTokenAt(ta);
  if (!token) return hideAtMenu();
  const cands = atCandidates(peer).filter((c) =>
    c.name.toLowerCase().indexOf(token.query.toLowerCase()) !== -1);
  if (cands.length === 0) return hideAtMenu();
  if (!atMenuEl) {
    atMenuEl = document.createElement("div");
    atMenuEl.className = "at-menu";
    ta.parentNode.appendChild(atMenuEl);
  }
  atCands = cands;
  atActive = 0;
  atMenuEl.innerHTML = cands.map((c, i) =>
    '<div class="at-item' + (i === 0 ? " active" : "") + '" data-i="' + i + '">' +
    WB.esc(c.name) + "</div>").join("");
  atMenuEl.querySelectorAll(".at-item").forEach((el) => {
    el.onmousedown = (ev) => { // preventDefault keeps the textarea focused
      ev.preventDefault();
      atSelect(Number(el.getAttribute("data-i")));
    };
    el.onmouseenter = () => atHighlight(Number(el.getAttribute("data-i")));
  });
}

function atHighlight(i) {
  if (!atMenuEl || i < 0 || i >= atCands.length) return;
  atActive = i;
  const items = atMenuEl.querySelectorAll(".at-item");
  items.forEach((el, j) => el.classList.toggle("active", j === i));
  if (items[i]) items[i].scrollIntoView({ block: "nearest" });
}

function atMove(d) {
  if (!atCands.length) return;
  atHighlight((atActive + d + atCands.length) % atCands.length);
}

function atSelect(i) {
  const ta = document.getElementById("composer-body");
  if (!ta || !atCands[i]) return;
  const token = atTokenAt(ta);
  if (!token) return hideAtMenu();
  const name = atCands[i].name;
  const pos = ta.selectionStart;
  ta.value = ta.value.slice(0, token.start) + "@" + name + " " + ta.value.slice(pos);
  const caret = token.start + name.length + 2; // "@name "
  ta.setSelectionRange(caret, caret);
  ta.focus();
  hideAtMenu();
}

function handleAtKeydown(ev) {
  // S34 [M]: IME composition in progress — let the input method keep the
  // keys (Enter commits pinyin; arrows page the IME candidate list).
  if (ev.isComposing || ev.keyCode === 229) return false;
  if (ev.key === "ArrowDown") { ev.preventDefault(); atMove(1); return true; }
  if (ev.key === "ArrowUp") { ev.preventDefault(); atMove(-1); return true; }
  if (ev.key === "Enter" || ev.key === "Tab") {
    ev.preventDefault(); atSelect(atActive); return true;
  }
  if (ev.key === "Escape") { ev.preventDefault(); hideAtMenu(); return true; }
  return false;
}

function hideAtMenu() {
  if (atMenuEl) {
    atMenuEl.remove();
    atMenuEl = null;
  }
  atCands = [];
  atActive = 0;
}

// Close on any mousedown outside a candidate item (a press inside the
// textarea also closes: the caret may have left the @ token; pressing the
// menu's own border/scrollbar closes too — only .at-item presses are handled
// by the item's own mousedown handler and skip this listener).
document.addEventListener("mousedown", (ev) => {
  if (!atMenuOpen()) return;
  if (ev.target && ev.target.closest && ev.target.closest(".at-item")) return;
  hideAtMenu();
}, true);

// allowedKinds pre-filters composer options per the route rules; the server
// re-checks and returns 403/400 for anything not allowed.
function allowedKinds(peer) {
  const me = WB.state.me;
  if (!me) return [];
  // S14: group conversations default to the chat channel only (the server remains the final arbiter); S22: the boss can additionally choose
  // "Dispatch" in a group (the target member is picked separately in the composer); S23: for leaders, "Dispatch" only appears when the group has dispatchable members.
  if (peer < 0) {
    if (me.admin) return ["chat", "dispatch"];
    const members = chatState.groupMembers[-peer - 1] || [];
    const disp = (chatState.routes || {}).dispatch || [];
    const canDispatch = members.some((m) => disp.indexOf(m.employee_id) !== -1);
    return canDispatch ? ["chat", "dispatch"] : ["chat"];
  }
  if (me.admin) return ["chat", "dispatch", "ask", "scheduled"];
  const r = chatState.routes || {};
  const kinds = [];
  if ((r.chat || []).indexOf(peer) !== -1) kinds.push("chat");
  if ((r.reporting || []).indexOf(peer) !== -1) kinds.push("report", "answer", "summary");
  if ((r.dispatch || []).indexOf(peer) !== -1) kinds.push("dispatch", "ask");
  return kinds;
}

function sendMsg(peer) {
  hideAtMenu(); // S34: sending (or a blocked send) drops any open @ menu
  const ta = document.getElementById("composer-body");
  const sel = document.getElementById("composer-kind");
  if (!ta || !sel) return;
  const body = ta.value.trim();
  // S12: attachment rules — block while uploads are in flight; an empty body
  // is allowed when attachments are attached (attachments-only message).
  if (chatState.attachments.some((a) => a.status === "uploading")) {
    WB.toast(WB.t("Attachments uploading, please wait"), true);
    return;
  }
  const ready = chatState.attachments.filter((a) => a.status === "ready");
  if (!body && ready.length === 0 && !chatState.replyTo) { WB.toast(WB.t("Please enter a message or add an attachment"), true); return; }
  const kind = sel.value;
  // S14: group conversations carry the group id, not "to"; 1:1 conversations are unchanged
  const payload = peer < 0
    ? { group: -peer - 1, kind: kind, body: body }
    : { to: peer, kind: kind, body: body };
  // S22: dispatching in a group requires a target member
  if (peer < 0 && kind === "dispatch") {
    const toSel = document.getElementById("composer-dispatch-to");
    const to = toSel ? Number(toSel.value) : 0;
    if (!to) { WB.toast(WB.t("Please select a dispatch target"), true); return; }
    payload.to = to;
    // S29: include project_id when a project is selected (>0); 0 = no project (server default)
    const projSel = document.getElementById("composer-dispatch-project");
    const proj = projSel ? Number(projSel.value) : 0;
    if (proj > 0) payload.project_id = proj;
  }
  if (ready.length > 0) payload.file_ids = ready.map((a) => a.fileID);
  if (chatState.replyTo) { // S16.1: consume the quote preview — body still carries the "> " lines (agent-readable), reply_to drives the jump
    const raw = String(chatState.replyTo.body || "").trim() || WB.t("(attachment)");
    const qLines = raw.split("\n").map((l) => "> " + l).join("\n");
    payload.body = qLines + (body ? "\n" + body : "");
    payload.reply_to = chatState.replyTo.id;
  }
  WB.api("POST", "/api/messages", payload)
    .then((d) => {
      ta.value = "";
      chatState.attachments = [];
      chatState.replyTo = null; // S16.1: quote consumed by this send
      renderComposerQuote(); // S16.1: clear the quote preview box UI
      renderPending();
      if (d && d.task_id) WB.toast(WB.t("Sent, task #") + d.task_id + " " + WB.t("created"));
      else WB.toast(WB.t("Sent"));
      // S41: show the sent bubble immediately — do not wait for the refresh
      // round-trip (a tunnel blip could drop it and leave "Sent" alone).
      echoSentMessage(peer, payload, d);
      // tasks cache first (loadConversations awaits loadTasks) so a fresh
      // dispatch's chip renders with a real status, not "…" (S18 chip race).
      loadConversations().then(async () => {
        renderThreadHead(peer); // S19-C: promote the virtual conversation to a real header after its first message
        const ok = await reloadThread(peer);
        // S41: one bounded retry — the first post-send GET often lands in the
        // same network blip that broke the refresh; a short backoff recovers
        // the server rows (real created_at / files / task chip) most of the time.
        if (!ok && chatState.selectedPeer === peer) {
          await new Promise((r) => setTimeout(r, 1200));
          await reloadThread(peer);
        }
      });
    })
    .catch((e) => WB.apiCatch(e, WB.t("Send")));
}

// ---------- S12: attachments ----------

const ATTACH_MAX_BYTES = 10 * 1024 * 1024; // matches the server cap

// pickFile opens a native file picker (accept: "image/*" or "*/*") and hands
// the chosen file to cb. The input is created on demand and removed after.
function pickFile(accept, cb) {
  const input = document.createElement("input");
  input.type = "file";
  input.accept = accept;
  input.style.display = "none";
  document.body.appendChild(input);
  input.onchange = () => {
    const f = input.files && input.files[0];
    input.remove();
    if (f) cb(f);
  };
  input.click();
}

// addPendingFile queues a file and uploads it immediately ("pick and send"); the
// item stays in the pending list until the message is sent or it is removed.
async function addPendingFile(file) {
  if (!file) return;
  if (file.size > ATTACH_MAX_BYTES) {
    WB.toast(WB.t("File exceeds the 10MB limit"), true);
    return;
  }
  const item = {
    tempId: "att" + Date.now().toString(16) + Math.random().toString(16).slice(2, 8),
    name: file.name || "file",
    isImage: (file.type || "").indexOf("image/") === 0,
    status: "uploading", // uploading | ready | error
    fileID: null,
  };
  if (item.isImage) item.thumbURL = URL.createObjectURL(file);
  chatState.attachments.push(item);
  renderPending();
  const form = new FormData();
  form.append("file", file, item.name);
  try {
    const d = await WB.upload("/api/files", form);
    item.status = "ready";
    item.fileID = d.id;
  } catch (e) {
    item.status = "error";
    WB.apiCatch(e, WB.t("Upload attachment"));
  }
  renderPending();
}

// renderPending redraws the attach-row preview (thumbnails / chips / status).
function renderPending() {
  const row = document.getElementById("attach-row");
  if (!row) return;
  if (chatState.attachments.length === 0) { row.innerHTML = ""; return; }
  row.innerHTML = chatState.attachments.map((a) => {
    const preview = (a.isImage && a.thumbURL)
      ? '<img class="attach-thumb" src="' + a.thumbURL + '" alt="">'
      : '<span class="attach-chip">' + PIX_ICON_FILE + "</span>";
    const status = a.status === "uploading"
      ? '<span class="attach-status">' + WB.t("Uploading…") + '</span>'
      : a.status === "error" ? '<span class="attach-status err">' + WB.t("Upload failed") + '</span>' : "";
    return (
      '<div class="attach-item" data-att="' + a.tempId + '">' +
      preview +
      '<span class="attach-name">' + WB.esc(a.name) + "</span>" +
      status +
      '<button type="button" class="attach-x" title="' + WB.t("Remove") + '">×</button>' +
      "</div>"
    );
  }).join("");
  row.querySelectorAll(".attach-x").forEach((b) => {
    b.onclick = () => {
      const id = b.closest(".attach-item").getAttribute("data-att");
      const i = chatState.attachments.findIndex((a) => a.tempId === id);
      if (i !== -1) {
        if (chatState.attachments[i].thumbURL) URL.revokeObjectURL(chatState.attachments[i].thumbURL);
        chatState.attachments.splice(i, 1);
      }
      renderPending();
    };
  });
}

// S16.1: the read-only quote preview box above the composer (mainstream IM-style quote bar).
// Shown when "Reply" is clicked: a "Reply to {sender}" label + the quoted text
// (truncation is left to the CSS single-line ellipsis; JS never truncates) + an
// × cancel button. The input itself stays empty; the quoted text is written back
// into the body as "> " prefixed lines on send (agent-readable), and reply_to drives the in-thread jump.
function renderComposerQuote() {
  const box = document.getElementById("composer-quote");
  const ta = document.getElementById("composer-body");
  if (!box) return;
  const q = chatState.replyTo;
  if (q == null) {
    box.hidden = true;
    box.innerHTML = "";
    return;
  }
  const sender = q.from_id == null ? "boss" : WB.empName(q.from_id);
  const text = String(q.body || "").trim() || ((q.files || []).length > 0 ? WB.t("(attachment)") : "");
  box.innerHTML =
    '<span class="composer-quote-label">' + WB.t("Reply to ") + WB.esc(sender) + '</span>' +
    '<span class="composer-quote-text">' + WB.esc(text) + '</span>' +
    '<button type="button" class="composer-quote-close" title="' + WB.t("Cancel quote") + '" aria-label="' + WB.t("Cancel quote") + '">×</button>';
  box.querySelector(".composer-quote-close").onclick = () => {
    chatState.replyTo = null;
    renderComposerQuote();
    if (ta) ta.focus();
  };
  box.hidden = false;
}

// filesHTML renders the attachments of a message bubble: image thumbnails
// (link to the original) and file chips (download).
function filesHTML(files) {
  if (!files || files.length === 0) return "";
  // P0-1: attachment urls arrive already signed (?exp=&sig=) from the server;
  // no client token is appended.
  const items = files.map((f) => {
    const url = WB.esc(WB.serverURL() + f.url); // F9: escape before HTML attr embedding
    if ((f.mime || "").indexOf("image/") === 0) {
      return (
        '<a class="attach-link" href="' + url + '" target="_blank" rel="noopener">' +
        '<img class="msg-thumb" src="' + url + '" alt="' + WB.esc(f.name) + '"></a>'
      );
    }
    const size = f.size != null ? " · " + fmtSize(f.size) : "";
    return (
      '<a class="msg-file-chip" href="' + url + '" download="' + WB.esc(f.name) + '">' +
      PIX_ICON_FILE +
      '<span class="chip-name">' + WB.esc(f.name) + "</span>" +
      '<span class="chip-size">' + size + "</span>" +
      "</a>"
    );
  });
  return '<div class="msg-files">' + items.join("") + "</div>";
}

function fmtSize(n) {
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
  return (n / 1024 / 1024).toFixed(1) + " MB";
}

// ---------- helpers ----------

function highlightMention(escapedBody) {
  return escapedBody.replace(/@([A-Za-z0-9_-]+)/g, '<span class="mention">@$1</span>');
}

function fmtTime(s) {
  if (!s) return "";
  // Server stores naive UTC wall time (SQLite datetime('now'): "YYYY-MM-DD HH:MM:SS",
  // no tz suffix). Parsing a naive string with new Date() assumes browser-local
  // time, shifting every displayed timestamp by the local UTC offset (e.g. -8h
  // in CST: a message sent at 13:59 local rendered as 05:59). Normalize naive
  // server timestamps to UTC (append "Z") so the browser renders them in the
  // viewer's local zone. Values that already carry a tz designator ("…Z",
  // "+08:00") or any other shape pass through untouched.
  const iso = /^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}(:\d{2})?(\.\d+)?$/.test(s)
    ? s.replace(" ", "T") + "Z"
    : s;
  const d = new Date(iso);
  if (isNaN(d.getTime())) return String(s);
  const p = (n) => (n < 10 ? "0" + n : String(n));
  return d.getMonth() + 1 + "-" + p(d.getDate()) + " " + p(d.getHours()) + ":" + p(d.getMinutes());
}

function truncate(s, n) {
  return s.length > n ? s.slice(0, n) + "…" : s;
}

// ---------- realtime ----------
// Listeners are registered once at module level so re-rendering the view
// does not stack duplicate handlers (S7-Minor1 pattern).
let lastWsRefresh = 0;
let trailingRefresh = null;
let trailingWsType = ""; // S19-B: a message.new swallowed by the debounce needs a markRead refresh
WB.on("message.new", (p) => onWsRefresh(p, "message.new"));
WB.on("task.state", (p) => { lastTaskStatePayload = p; onWsRefresh(p, "task.state"); }); // H-4: record the payload for board dedup
WB.on("agent.online", (p) => onWsRefresh(p, "agent.online"));
WB.on("agent.offline", (p) => onWsRefresh(p, "agent.offline"));
WB.on("group.members", (p) => {
  // S22: membership changed -> invalidate the target cache; if this group is the
  // open thread, redraw the dispatch-target dropdown in place (keeping the
  // selected value) instead of a full re-render that would wipe the draft.
  const gid = p && p.group_id;
  if (gid && chatState.groupMembers[gid]) delete chatState.groupMembers[gid];
  // S26: the viewer was just removed/left -> the group thread is no longer accessible
  // (further fetches would 403), so go straight back to the conversation list and
  // stop fetching group members.
  const removed = (p && p.removed) || [];
  const me = WB.state.me;
  const meId = me && me.admin ? 0 : (me ? me.employee_id : -1);
  if (gid && meId > 0 && removed.indexOf(meId) !== -1) {
    // S26 fix: if the settings modal is open on this group, close it too (further actions would 403/404).
    if (gsEl && gsGid === gid) closeGroupSettingsModal();
    if (chatState.selectedPeer === -(gid + 1)) closeThread();
    onWsRefresh(p, "group.members");
    return;
  }
  if (gid && chatState.selectedPeer === -(gid + 1)) {
    loadGroupMembers(gid).then(() => refreshDispatchTargets());
  }
  onWsRefresh(p, "group.members"); // S18.1
});
WB.on("group.updated", (p) => {
  // S26: rename -> the conversation list name updates on refresh; if the settings modal is open on this group, redraw it in place.
  const gid = p && p.group_id;
  if (gsEl && gsGid === gid) {
    WB.api("GET", "/api/groups/" + gid).then((d) => {
      if (gsEl && gsGid === gid) {
        renderGroupSettings(gsEl.querySelector(".gs-card"), gid, d);
      }
    }).catch(() => { /* the group may have been deleted; group.deleted will close the modal */ });
  }
  onWsRefresh(p, "group.updated");
});
WB.on("group.deleted", (p) => {
  // S26: group deleted -> close the modal if it is open on this group; if the current thread is this group, go back to the conversation list.
  const gid = p && p.group_id;
  if (gsEl && gsGid === gid) closeGroupSettingsModal();
  if (gid && chatState.selectedPeer === -(gid + 1)) closeThread();
  onWsRefresh(p, "group.deleted");
});
WB.on("message.read", (p) => {
  // S19-B: real-time read-status refresh; skip self-echo (reader===me) because
  // the local markRead already optimistically updated the conversation list, and
  // the read indicator only depends on others' cursors (1:1=peer; group=members
  // other than self, server filters reader != viewer). Own cursor advance has no
  // local display impact. Refreshing anyway would redraw the thread and erase
  // the flash added by search-jump.
  const me = WB.state.me;
  if (me && p && p.reader === me.employee_id) return;
  onWsRefresh(p, "message.read");
});

// Chain the thread re-render after the conversations+tasks load so chips
// re-render from a fresh task cache instead of a racing one (S18 chip race).
// S19-B loop fix: only message.new triggers auto markRead — a new message
// entering the open thread is the only case that needs the read-cursor advance;
// task.state/agent.*/message.read events only refresh display. Previously every
// event triggered markRead -> broadcast -> refresh again, causing infinite ping-pong
// between two open threads.
async function refreshAfterLoad(evType) {
  await loadConversations();
  if (chatState.selectedPeer != null) {
    reloadThread(chatState.selectedPeer);
    // S35: the await ensures the active thread's markRead zero-clear is effective
    // (mentions in the open thread do not chime); S19-B semantics unchanged —
    // only message.new auto-marks read.
    if (evType === "message.new") await markRead(chatState.selectedPeer);
    // S24: sync-refresh the board if it is open and matches the current group thread (preserve selected filter).
    // H-4: dedup by task.state payload — skip only when the local cache already
    // reflects that task state (WS echo of a local action); do NOT use a time window,
    // to avoid swallowing legitimate new events from another client right after
    // opening the board (s25 C6 regression lesson).
    if (gkEl && gkGid && chatState.selectedPeer === -(gkGid + 1)) {
      refreshGroupTasksBoard({ match: (evType === "task.state" && lastTaskStatePayload) || undefined });
    }
  }
  // S35: only new messages can add new mentions; the diff before/after refresh determines the title badge and chime.
  if (evType === "message.new") noteMentionChange();
}

function onWsRefresh(ev, evType) {
  if (WB.state.view !== "chat") {
    // S35 F1: a mention may have arrived while another view was active; mark
    // the baseline stale so loadConversations re-syncs it on return to chat
    // (otherwise the next diff false-fires for a mention already titled).
    if (evType === "message.new") mentionBaselineStale = true;
    return;
  }
  const now = Date.now();
  if (now - lastWsRefresh >= 800) {
    // Leading edge: refresh now (debounce bursts, e.g. dispatch's
    // message.new + task.state pair).
    lastWsRefresh = now;
    refreshAfterLoad(evType);
    return;
  }
  // Swallowed by the debounce: still guarantee one trailing refresh once the
  // burst settles. A fast agent claim delivers task.state queued + running
  // <800ms apart; without this the chip kept the pre-claim status until the
  // next unrelated event (S18 chip race, second half). If the swallowed event
  // is message.new, upgrade the catch-up refresh to include markRead, so new
  // messages are not left unread.
  if (evType === "message.new") trailingWsType = "message.new";
  if (trailingRefresh == null) {
    trailingRefresh = setTimeout(() => {
      const t = trailingWsType;
      trailingRefresh = null;
      trailingWsType = "";
      lastWsRefresh = Date.now();
      if (WB.state.view === "chat") refreshAfterLoad(t);
      else if (t === "message.new") mentionBaselineStale = true; // S35 F1: catch-up refresh was skipped by the view gate
    }, 800 - (now - lastWsRefresh));
  }
}

// ---------- S13: custom context menu on message bubbles ----------
// Right-click inside a .bubble suppresses the browser menu and shows a pixel
// menu: Forward (to another conversation, boss-only when other peers
// exist) / Reply (quote into composer) / Copy text (clipboard) / Save image
// (only when right-clicking an image thumbnail). Everywhere else the native
// browser menu is untouched.
const PIX_ICON_FWD =
  '<svg width="12" height="12" viewBox="0 0 12 12" shape-rendering="crispEdges" fill="none" stroke="currentColor" stroke-width="1">' +
  '<path d="M2 6h8M8 3l3 3-3 3"/></svg>';
const PIX_ICON_REPLY =
  '<svg width="12" height="12" viewBox="0 0 12 12" shape-rendering="crispEdges" fill="none" stroke="currentColor" stroke-width="1">' +
  '<path d="M10 6H2M4 3L1 6l3 3"/></svg>';
const PIX_ICON_COPY =
  '<svg width="12" height="12" viewBox="0 0 12 12" shape-rendering="crispEdges" fill="none" stroke="currentColor" stroke-width="1">' +
  '<rect x="4" y="4" width="6" height="6"/><path d="M2 8V2h6"/></svg>';

let ctxMenuEl = null;

function ctxMenuElement() {
  if (!ctxMenuEl) {
    ctxMenuEl = document.createElement("div");
    ctxMenuEl.className = "ctx-menu";
    ctxMenuEl.setAttribute("role", "menu");
    ctxMenuEl.style.display = "none";
    document.body.appendChild(ctxMenuEl);
    // Close on outside press / Esc / any scroll / resize (scroll does not
    // bubble, so listen in the capture phase on document).
    document.addEventListener("mousedown", (e) => {
      if (ctxMenuEl.classList.contains("open") && !ctxMenuEl.contains(e.target)) hideCtxMenu();
    }, true);
    document.addEventListener("keydown", (e) => {
      if (e.key === "Escape") hideCtxMenu();
    });
    document.addEventListener("scroll", hideCtxMenu, true);
    window.addEventListener("resize", hideCtxMenu);
  }
  return ctxMenuEl;
}

function ctxMenuItem(label, icon, fn) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = "ctx-item";
  b.setAttribute("role", "menuitem");
  b.innerHTML = (icon || "") + "<span>" + WB.esc(label) + "</span>";
  b.onclick = (ev) => {
    ev.stopPropagation();
    fn();
  };
  return b;
}

function positionCtxMenu(el, x, y) {
  el.style.display = "block";
  el.classList.add("open");
  const w = el.offsetWidth;
  const h = el.offsetHeight;
  const left = x + w > window.innerWidth - 8 ? Math.max(8, window.innerWidth - w - 8) : x;
  const top = y + h > window.innerHeight - 8 ? Math.max(8, window.innerHeight - h - 8) : y;
  el.style.left = left + "px";
  el.style.top = top + "px";
}

function hideCtxMenu() {
  if (ctxMenuEl) {
    ctxMenuEl.style.display = "none";
    ctxMenuEl.classList.remove("open");
  }
}

document.addEventListener("contextmenu", (ev) => {
  if (ctxMenuEl && ctxMenuEl.classList.contains("open") && ctxMenuEl.contains(ev.target)) {
    ev.preventDefault(); // never show the native menu over our own menu
    return;
  }
  if (!WB.state || WB.state.view !== "chat") return;
  // Only the bubble itself (body/thumbnails) triggers the menu; the row is
  // full-width, so blank space beside a bubble keeps the native menu.
  const bub = ev.target.closest ? ev.target.closest(".bubble") : null;
  const row = bub ? bub.closest(".bubble-row[data-mid]") : null;
  if (!row) return; // keep the native menu outside message bubbles
  ev.preventDefault();
  const msg = (chatState.messages || []).find((m) => m.id === Number(row.getAttribute("data-mid")));
  if (!msg) return;
  const img = ev.target.closest ? ev.target.closest(".msg-thumb") : null;
  showCtxMenu(ev.clientX, ev.clientY, msg, img);
});

// One-line preview shown at the top of the context menu so the user can tell
// which message the menu acts on. Whitespace-normalized, truncated to 42 chars.
function ctxPreview(msg) {
  const t = (msg.body || "").replace(/\s+/g, " ").trim();
  if (t) return truncate(t, 42);
  const files = (msg.files || []).map((f) => f.name).join(", ");
  return files ? truncate(files, 42) : WB.t("(attachment)");
}

function showCtxMenu(x, y, msg, img) {
  const el = ctxMenuElement();
  el.innerHTML = "";
  const title = document.createElement("div");
  title.className = "ctx-title";
  title.textContent = ctxPreview(msg);
  el.appendChild(title);
  if (img) {
    el.appendChild(ctxMenuItem(WB.t("Save image"), PIX_ICON_IMG, () => ctxSaveImage(img.src, img.alt || "image")));
  }
  // S37: forward targets = existing 1:1 targets (non-system kind, non-empty
  // allowedKinds, not the current conversation) + group targets (is_group rows in
  // the conversation list, i.e. groups the viewer belongs to — membership is
  // guaranteed server-side by ListGroupConversations; group forward kind is always
  // chat, and flow.SendToGroup is the final membership arbiter, non-members get 403).
  // Group conversation rows have negative peer_id (-(group_id+1)); the same rule
  // as 1:1 excludes the current conversation. System messages (kind=system) have
  // no forwardable content, so no "Forward" entry is shown (S37 decision 5).
  const targets = chatState.conversations.filter((c) => {
    if (c.peer_id === chatState.selectedPeer) return false;
    if (c.is_group) return true;
    if (c.kind === "system" || allowedKinds(c.peer_id).length === 0) return false;
    return true;
  });
  if (msg.kind !== "system" && targets.length > 0) {
    el.appendChild(ctxMenuItem(WB.t("Forward"), PIX_ICON_FWD, () => showCtxForward(x, y, msg, targets)));
  }
  // Reply: only when the open thread actually has a composer (route-allowed kinds).
  if (allowedKinds(chatState.selectedPeer).length > 0) {
    el.appendChild(ctxMenuItem(WB.t("Reply"), PIX_ICON_REPLY, () => ctxReply(msg)));
  }
  el.appendChild(ctxMenuItem(WB.t("Copy text"), PIX_ICON_COPY, () => ctxCopy(msg)));
  positionCtxMenu(el, x, y);
}

function showCtxForward(x, y, msg, targets) {
  const el = ctxMenuElement();
  el.innerHTML = "";
  const title = document.createElement("div");
  title.className = "ctx-title";
  title.textContent = WB.t("Forward to…");
  el.appendChild(title);
  targets.forEach((c) => {
    // S37: append a "(group)" suffix to group target names, to distinguish them from 1:1 targets.
    const label = c.is_group ? c.name + " (" + WB.t("Group") + ")" : c.name;
    el.appendChild(ctxMenuItem(label, "", () => doForward(c, msg)));
  });
  positionCtxMenu(el, x, y);
}

function doForward(target, msg) {
  hideCtxMenu();
  const fromName = msg.from_id == null ? "boss" : WB.empName(msg.from_id) || "?";
  const body = msg.body
    ? "Forwarded from " + fromName + ": " + msg.body
    : "Forwarded from " + fromName + " (" + (msg.files || []).length + " attachment(s))";
  // S37: group targets use the existing group contract {group, kind:"chat", body}
  // (server-side flow.SendToGroup is the final membership arbiter, non-members get 403);
  // 1:1 targets reuse the S13 logic (allowedKinds pick the kind, chat preferred;
  // some employee routes only allow report/answer/summary). Attachments do not
  // travel with the forward (S13 semantics: body only).
  let payload;
  if (target.is_group) {
    payload = { group: target.group_id, kind: "chat", body: body };
  } else {
    const kinds = allowedKinds(target.peer_id);
    const kind = kinds.indexOf("chat") !== -1 ? "chat" : kinds[0] || "chat";
    payload = { to: target.peer_id, kind: kind, body: body };
  }
  WB.api("POST", "/api/messages", payload)
    .then(() => {
      WB.toast(WB.t("Forwarded to") + " " + target.name);
      loadConversations();
    })
    .catch((e) => WB.apiCatch(e, WB.t("Forward")));
}

function ctxReply(msg) {
  hideCtxMenu();
  const ta = document.getElementById("composer-body");
  const sel = document.getElementById("composer-kind");
  if (!ta) return;
  // S16.1: the input stays empty; the quote renders in the standalone preview box
  // above the input. On send, sendMsg writes the quoted text back into the body as
  // "> " prefixed lines (agent-readable), and reply_to drives the jump.
  chatState.replyTo = msg;
  renderComposerQuote();
  if (sel) {
    const opts = [...sel.options].map((o) => o.value);
    const pref = WB.state.me && !WB.state.me.admin ? "answer" : "chat";
    sel.value = opts.indexOf(pref) !== -1 ? pref : opts[0];
  }
  ta.focus();
}

async function ctxCopy(msg) {
  hideCtxMenu();
  const text = msg.body || "";
  if (!text) {
    WB.toast(WB.t("This message has no text to copy"), true);
    return;
  }
  try {
    await navigator.clipboard.writeText(text);
    WB.toast(WB.t("Copied"));
  } catch (e) {
    try {
      const t = document.createElement("textarea");
      t.value = text;
      t.style.position = "fixed";
      t.style.opacity = "0";
      document.body.appendChild(t);
      t.select();
      document.execCommand("copy");
      t.remove();
      WB.toast(WB.t("Copied"));
    } catch (e2) {
      WB.toast(WB.t("Copy failed"), true);
    }
  }
}

async function ctxSaveImage(url, name) {
  hideCtxMenu();
  try {
    const r = await fetch(url);
    if (!r.ok) throw new Error("http " + r.status);
    const blob = await r.blob();
    const obj = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = obj;
    a.download = name || "image";
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(obj), 30000);
    WB.toast(WB.t("Download started: ") + (name || WB.t("Image")));
  } catch (e) {
    WB.apiCatch(e, WB.t("Save image"));
  }
}

// ---------- S14: new group ----------
// The "+" on the right of the search row opens the modal: enter the group name,
// pick members (self is pre-checked and locked; the boss also counts as a member),
// and on success POST /api/groups creates the group and opens its conversation directly
// (peer_id = -(gid+1)).
let newGroupEl = null;

function closeNewGroupModal() {
  if (!newGroupEl) return;
  newGroupEl.remove();
  newGroupEl = null;
}

// Esc closes the modal (registered once at module level, same listener pattern as the S13 context menu)
document.addEventListener("keydown", (ev) => {
  if (ev.key === "Escape" && newGroupEl) closeNewGroupModal();
});

async function openNewGroupModal() {
  if (newGroupEl) return; // already open
  const me = WB.state.me;
  if (!me) return;
  const myID = me.admin ? 0 : me.employee_id;

  const overlay = document.createElement("div");
  overlay.className = "ng-overlay";
  const card = document.createElement("div");
  card.className = "ng-card";
  overlay.appendChild(card);
  document.body.appendChild(overlay);
  newGroupEl = overlay;
  // Clicking the backdrop (outside the card) closes the modal
  overlay.addEventListener("mousedown", (ev) => {
    if (ev.target === overlay) closeNewGroupModal();
  });

  // Member list: self is pre-checked and disabled; the boss has no employee profile, so gets a pinned row on top
  let emps = [];
  try {
    const d = await WB.api("GET", "/api/employees");
    emps = d.employees || [];
  } catch (e) {
    closeNewGroupModal();
      WB.apiCatch(e, WB.t("Load employees"));
    return;
  }
  if (!newGroupEl) return; // modal was closed while loading
  const rows = [];
  if (me.admin) {
    rows.push(
      '<label class="ng-member">' +
      '<input type="checkbox" data-emp="0" checked disabled>' +
      WB.avatarSpan("boss", "ng-ava", 0) +
      "<span>boss" + WB.t(" (me)") + "</span></label>"
    );
  }
  emps.forEach((e) => {
    const self = e.id === myID;
    rows.push(
      '<label class="ng-member">' +
      '<input type="checkbox" data-emp="' + e.id + '"' + (self ? " checked disabled" : "") + ">" +
      WB.avatarSpan(e.name, "ng-ava", e.id) +
      "<span>" + WB.esc(e.name) + (self ? WB.t(" (me)") : "") + "</span></label>"
    );
  });
  if (rows.length === 0) rows.push('<p class="muted" style="padding:8px 0">' + WB.t("No employees") + '</p>');

  // S21: only the boss can choose between "Group / Project Channel"; the employee modal has no type row (same behavior as S14).
  const typeRow = me.admin
    ? '<div class="ng-type-row">' +
      '<label class="ng-type"><input type="radio" name="ng-type" value="group" checked>' + WB.t("Group chat") + '</label>' +
      '<label class="ng-type"><input type="radio" name="ng-type" value="channel">' + WB.t("Project channel") + '</label>' +
      '</div>'
    : "";
  card.innerHTML =
    '<div class="ng-title" id="ng-title">' + WB.t("New group chat") + '</div>' +
    typeRow +
    '<input id="ng-name" maxlength="64" placeholder="' + WB.t("Group name (1-64 characters)") + '">' +
    '<div class="ng-subtitle">' + WB.t("Select members (at least 2, including you)") + '</div>' +
    '<div class="ng-members">' + rows.join("") + "</div>" +
    '<div class="ng-actions">' +
    '<button type="button" class="btn secondary" id="ng-cancel">' + WB.t("Cancel") + '</button>' +
    '<button type="button" class="btn" id="ng-create">' + WB.t("Create group") + '</button>' +
    "</div>";
  card.querySelector("#ng-cancel").onclick = closeNewGroupModal;
  card.querySelector("#ng-create").onclick = createGroup;
  // S21: switching type updates the title / button text / name placeholder accordingly
  card.querySelectorAll('input[name="ng-type"]').forEach((r) => {
    r.onchange = () => applyNewGroupKind();
  });
}

// S21: read the type currently selected in the new-group modal (defaults to group).
function currentNewGroupKind() {
  const r = newGroupEl ? newGroupEl.querySelector('input[name="ng-type"]:checked') : null;
  return r ? r.value : "group";
}

// S21: refresh the modal text according to the current type.
function applyNewGroupKind() {
  if (!newGroupEl) return;
  const isChan = currentNewGroupKind() === "channel";
  const title = newGroupEl.querySelector("#ng-title");
  if (title) title.textContent = isChan ? WB.t("New project channel") : WB.t("New group chat");
  const btn = newGroupEl.querySelector("#ng-create");
  if (btn) btn.textContent = isChan ? WB.t("Create project channel") : WB.t("Create group");
  const nameInput = newGroupEl.querySelector("#ng-name");
  if (nameInput) nameInput.placeholder = isChan ? WB.t("Channel name (1-64 characters)") : WB.t("Group name (1-64 characters)");
}

async function createGroup() {
  const nameEl = document.getElementById("ng-name");
  const name = (nameEl ? nameEl.value : "").trim();
  const kind = currentNewGroupKind(); // S21: employees have no type row -> always "group"
  const isChan = kind === "channel";
  if (!name) {
    WB.toast(isChan ? WB.t("Please enter a channel name") : WB.t("Please enter a group name"), true);
    return;
  }
  const host = document.querySelector(".ng-members");
  const checked = host
    ? [...host.querySelectorAll("input[data-emp]")].filter((i) => i.checked)
    : [];
  if (checked.length < 2) {
    WB.toast(WB.t("At least 2 members required"), true);
    return;
  }
  // The server automatically includes the creator; only submit the remaining members here
  const me = WB.state.me;
  const myID = me.admin ? 0 : me.employee_id;
  const memberIDs = checked
    .map((i) => Number(i.getAttribute("data-emp")))
    .filter((id) => id !== myID);
  try {
    const d = await WB.api("POST", "/api/groups", { name: name, member_ids: memberIDs, kind: kind });
    closeNewGroupModal();
    WB.toast(isChan ? WB.t("Project channel created") : WB.t("Group chat created"));
    await loadConversations();
    openThread(-(d.id + 1));
  } catch (e) {
    WB.apiCatch(e, isChan ? WB.t("Create project channel") : WB.t("Create group"));
  }
}

// ---------- S17: group settings (members / add member / do-not-disturb / ignore; S26: rename / remove member / delete group) ----------
let gsEl = null;
let gsGid = 0; // S26: group id for the currently open settings modal (0 = not open)

function closeGroupSettingsModal() {
  if (!gsEl) return;
  gsEl.remove();
  gsEl = null;
  gsGid = 0;
}

// Esc closes the modal (same pattern as the new-group modal, registered once at module level)
document.addEventListener("keydown", (ev) => {
  if (ev.key === "Escape" && gsEl) closeGroupSettingsModal();
  if (ev.key === "Escape" && gkEl) closeGroupTasksModal(); // S24
  if (ev.key === "Escape" && tdEl) closeTaskDetailModal(); // task-duration-artifacts A
});

async function openGroupSettingsModal(gid) {
  if (gsEl) return; // already open
  const overlay = document.createElement("div");
  overlay.className = "gs-overlay";
  const card = document.createElement("div");
  card.className = "gs-card";
  overlay.appendChild(card);
  document.body.appendChild(overlay);
  gsEl = overlay;
  gsGid = gid; // S26: used by WS events to decide whether to redraw/close this modal
  // Clicking the backdrop (outside the card) closes the modal
  overlay.addEventListener("mousedown", (ev) => {
    if (ev.target === overlay) closeGroupSettingsModal();
  });
  card.innerHTML = '<p class="muted" style="padding:12px">' + WB.t("Loading…") + '</p>';

  let data;
  try {
    data = await WB.api("GET", "/api/groups/" + gid);
  } catch (e) {
    closeGroupSettingsModal();
    WB.apiCatch(e, WB.t("Load group settings"));
    return;
  }
  if (!gsEl) return; // modal was closed during the request; discard the stale result
  renderGroupSettings(card, gid, data);
}

function renderGroupSettings(card, gid, data) {
  const myID = WB.state.me && WB.state.me.admin ? 0 : (WB.state.me && WB.state.me.employee_id);
  const memberRows = (data.members || []).map((m) => {
    const label = (m.kind && m.kind !== "system")
      ? '<span class="tag">' + WB.esc(m.role || m.kind) + "</span>"
      : "";
    const creatorTag = m.is_creator ? '<span class="tag creator">' + WB.t("Creator") + '</span>' : "";
    // S26: boss row (employee_id=0) cannot be removed; for self show "Leave",
    // managers can show "Remove" for other members.
    let action = "";
    if (m.employee_id !== 0 && m.employee_id === myID) {
      action = '<button class="gs-remove-btn" type="button" data-eid="' +
        m.employee_id + '" title="' + WB.t("Leave group") + '">' + WB.t("Leave") + '</button>';
    } else if (m.employee_id !== 0 && data.can_manage) {
      action = '<button class="gs-remove-btn" type="button" data-eid="' +
        m.employee_id + '" title="' + WB.t("Remove from group") + '">' + WB.t("Remove") + '</button>';
    }
    return (
      '<div class="gs-member">' +
      WB.avatarSpan(m.name, "gs-ava", m.employee_id) +
      '<span class="gs-member-name">' + WB.esc(m.name) + "</span>" +
      label + creatorTag + action +
      "</div>"
    );
  }).join("");

  const readOnlyHint = data.can_manage ? "" :
    '<div class="gs-hint">' + WB.t("Only the creator or an admin can manage members") + '</div>';

  // S26: rename (visible to creator/boss only)
  let renameSection = "";
  if (data.can_manage) {
    renameSection =
      '<div class="gs-subtitle">' + WB.t("Group name") + '</div>' +
      '<div class="gs-rename-row">' +
      '<input type="text" id="gs-rename" maxlength="64" value="' + WB.esc(data.name) + '">' +
      '<button type="button" class="btn small" id="gs-rename-btn">' + WB.t("Rename") + '</button>' +
      "</div>";
  }

  let addSection = "";
  if (data.can_manage && data.candidates && data.candidates.length > 0) {
    const opts = data.candidates.map((c) =>
      '<option value="' + c.employee_id + '">' + WB.esc(c.name) + "</option>"
    ).join("");
    addSection =
      '<div class="gs-subtitle">' + WB.t("Add members") + '</div>' +
      '<div class="gs-add-row">' +
      '<select id="gs-add">' + opts + "</select>" +
      '<button type="button" class="btn small" id="gs-add-btn">' + WB.t("Add") + '</button>' +
      "</div>";
  }

  // S26: delete group (visible to creator/boss only; server returns 409 if unfinished tasks exist)
  let deleteSection = "";
  if (data.can_manage) {
    deleteSection =
      '<div class="gs-danger-zone">' +
      '<button type="button" class="btn small danger" id="gs-delete">' + WB.t("Delete this group") + '</button>' +
      '<div class="gs-hint">' + WB.t("Deleting will remove group messages and membership; task records are kept.") + '</div>' +
      "</div>";
  }

  // S30: task policy — group-level default priority (0-99) + SLA timeout (minutes, 0=off, ≤43200=30 days).
  // can_manage: show two inputs + save button; regular members see a read-only echo line.
  // Guard (S29 N2 lesson): clamp server values to valid integer range before echo, fallback to 0 if invalid.
  const dpEcho = Number(data.default_priority);
  const dp0 = Number.isInteger(dpEcho) && dpEcho >= 0 && dpEcho <= 99 ? dpEcho : 0;
  const slaEcho = Number(data.sla_timeout_seconds);
  const slaSec0 = Number.isInteger(slaEcho) && slaEcho >= 0 ? slaEcho : 0;
  const slaMin0 = Math.min(43200, Math.max(0, Math.round(slaSec0 / 60)));
  let policySection = "";
  if (data.can_manage) {
    policySection =
      '<div class="gs-subtitle">' + WB.t("Task policy") + '</div>' +
      '<div class="gs-policy-row">' +
      '<span class="gs-policy-label">' + WB.t("Default priority (0-99)") + '</span>' +
      '<input type="number" id="gs-default-priority" min="0" max="99" value="' + dp0 + '">' +
      "</div>" +
      '<div class="gs-policy-row">' +
      '<span class="gs-policy-label">' + WB.t("SLA timeout (minutes, 0=off)") + '</span>' +
      '<input type="number" id="gs-sla-minutes" min="0" max="43200" value="' + slaMin0 + '">' +
      "</div>" +
      '<div class="gs-policy-row">' +
      '<button type="button" class="btn small" id="gs-policy-save">' + WB.t("Save policy") + '</button>' +
      "</div>" +
      '<div class="gs-hint">' + WB.t("When a group dispatch has no explicit priority, the default priority is used; the system posts one reminder to the group when a task stays open beyond the SLA threshold.") + '</div>';
  } else {
    policySection =
      '<div class="gs-hint">' + WB.t("Default priority") + " " + dp0 + " · SLA " + slaMin0 + " " + WB.t("min (0=unset)") + "</div>";
  }

  // S21: group / project channel share the same settings modal, only title and subtitle differ
  const kindLabel = data.kind === "channel" ? WB.t("Project channel") : WB.t("Group chat");
  const settingsTitle = data.kind === "channel" ? WB.t("Channel settings") : WB.t("Group settings");
  card.innerHTML =
    '<div class="gs-title-row">' +
    '<span class="gs-title">' + settingsTitle + '</span>' +
    '<button class="gs-close" type="button" title="' + WB.t("Close") + '">×</button>' +
    "</div>" +
    '<div class="gs-subtitle">' + WB.esc(data.name) + " · " + kindLabel + " · " +
    ((data.members || []).length) + " " + WB.t("members") + " · " + WB.t("Created at") + " " + WB.esc(fmtTime(data.created_at)) + "</div>" +
    '<div class="gs-members">' + memberRows + "</div>" +
    readOnlyHint +
    renameSection +
    addSection +
    '<div class="gs-prefs">' +
    '<label class="gs-pref">' +
    "<span>" + WB.t("Mute messages") + "</span>" +
    '<input type="checkbox" id="gs-mute" ' + (data.muted ? "checked" : "") + ">" +
    '<span class="gs-switch"></span>' +
    "</label>" +
    '<label class="gs-pref">' +
    "<span>" + WB.t("Ignore this group") + "</span>" +
    '<input type="checkbox" id="gs-ignore" ' + (data.ignored ? "checked" : "") + ">" +
    '<span class="gs-switch"></span>' +
    "</label>" +
    '<div class="gs-hint">' + WB.t("Ignored groups move to the Ignored section at the bottom of the conversation list; messages are still saved.") + '</div>' +
    "</div>" +
    policySection +
    deleteSection;

  card.querySelector(".gs-close").onclick = closeGroupSettingsModal;

  // S26: rename
  const renameBtn = card.querySelector("#gs-rename-btn");
  if (renameBtn) {
    renameBtn.onclick = async () => {
      const input = card.querySelector("#gs-rename");
      const name = (input.value || "").trim();
      if (!name) { WB.toast(WB.t("Group name cannot be empty"), true); return; }
      try {
        await WB.api("PATCH", "/api/groups/" + gid, { name: name });
        WB.toast(WB.t("Group name updated"));
        const d = await WB.api("GET", "/api/groups/" + gid);
        if (!gsEl) return; // modal was closed during loading
        renderGroupSettings(card, gid, d);
        refreshChat();
      } catch (e) {
        WB.apiCatch(e, WB.t("Rename failed"));
      }
    };
  }

  // S26: remove member / leave group
  card.querySelectorAll(".gs-remove-btn").forEach((btn) => {
    btn.onclick = async () => {
      const eid = Number(btn.getAttribute("data-eid"));
      const isSelf = eid === myID;
      if (!confirm(isSelf ? WB.t("Leave this group?") : WB.t("Remove this member from the group?"))) return;
      try {
      await WB.api("DELETE", "/api/groups/" + gid + "/members/" + eid);
      WB.toast(isSelf ? WB.t("Left the group") : WB.t("Member removed"));
      if (isSelf) {
        // After self-leave, viewer is no longer a member; GET group details will 403: close modal directly,
        // close the thread first to avoid refreshChat reloading inaccessible group messages.
        closeGroupSettingsModal();
        if (chatState.selectedPeer === -(gid + 1)) closeThread();
        refreshChat();
        return;
      }
      const d = await WB.api("GET", "/api/groups/" + gid);
      if (!gsEl) return; // modal was closed during loading
      renderGroupSettings(card, gid, d);
      refreshChat();
      } catch (e) {
        WB.apiCatch(e, isSelf ? WB.t("Leave group failed") : WB.t("Remove member failed"));
      }
    };
  });

  // S26: delete group (with confirmation; on 409 the server returns the count of unfinished tasks)
  const delBtn = card.querySelector("#gs-delete");
  if (delBtn) {
    delBtn.onclick = async () => {
      if (!confirm(WB.t("Delete this group? All messages will be permanently deleted; task records are kept."))) return;
      try {
        await WB.api("DELETE", "/api/groups/" + gid);
        WB.toast(WB.t("Group deleted"));
        closeGroupSettingsModal();
        // Group no longer exists: close the thread first (set selectedPeer to null),
        // so refreshChat won't fetch /api/messages again for the deleted group and trigger a 404 toast.
        if (chatState.selectedPeer === -(gid + 1)) closeThread();
        refreshChat();
      } catch (e) {
        WB.apiCatch(e, WB.t("Delete group failed"));
      }
    };
  }

  const addBtn = card.querySelector("#gs-add-btn");
  if (addBtn) {
    addBtn.onclick = async () => {
      const sel = card.querySelector("#gs-add");
      if (!sel || !sel.value) return;
      const empId = Number(sel.value);
      try {
        await WB.api("POST", "/api/groups/" + gid + "/members", { employee_ids: [empId] });
        WB.toast(WB.t("Member added"));
        const d = await WB.api("GET", "/api/groups/" + gid);
        if (!gsEl) return; // modal was closed during loading
        renderGroupSettings(card, gid, d);
        refreshChat();
      } catch (e) {
        WB.apiCatch(e, WB.t("Add members"));
      }
    };
  }

  // S30: save task policy. Read two inputs (parseInt; missing/NaN or out-of-range -> toast block and
  // return, no request sent) -> POST /policy (convert SLA minutes to seconds) -> success toast +
  // re-GET + refreshChat (same refresh pattern as rename/add-member).
  const policySaveBtn = card.querySelector("#gs-policy-save");
  if (policySaveBtn) {
    policySaveBtn.onclick = async () => {
      const dpIn = card.querySelector("#gs-default-priority");
      const slaIn = card.querySelector("#gs-sla-minutes");
      const dp = dpIn ? parseInt(dpIn.value, 10) : NaN;
      const slaMin = slaIn ? parseInt(slaIn.value, 10) : NaN;
      if (isNaN(dp) || dp < 0 || dp > 99) {
        WB.toast(WB.t("Default priority must be between 0 and 99"), true);
        return;
      }
      if (isNaN(slaMin) || slaMin < 0 || slaMin > 43200) {
        WB.toast(WB.t("SLA timeout must be between 0 and 43200 minutes (0=off)"), true);
        return;
      }
      try {
        await WB.api("POST", "/api/groups/" + gid + "/policy", {
          default_priority: dp,
          sla_timeout_seconds: slaMin * 60,
        });
        WB.toast(WB.t("Policy saved"));
        const d = await WB.api("GET", "/api/groups/" + gid);
        if (!gsEl) return; // modal was closed during loading
        renderGroupSettings(card, gid, d);
        refreshChat();
      } catch (e) {
        WB.apiCatch(e, WB.t("Save policy"));
      }
    };
  }

  const muteChk = card.querySelector("#gs-mute");
  if (muteChk) {
    muteChk.onchange = async (ev) => {
      try {
        await WB.api("POST", "/api/groups/" + gid + "/prefs", { muted: ev.target.checked });
        refreshChat();
      } catch (e) {
        WB.apiCatch(e, WB.t("Set mute"));
        ev.target.checked = !ev.target.checked; // rollback toggle on failure
      }
    };
  }

  const ignoreChk = card.querySelector("#gs-ignore");
  if (ignoreChk) {
    ignoreChk.onchange = async (ev) => {
      try {
        await WB.api("POST", "/api/groups/" + gid + "/prefs", { ignored: ev.target.checked });
        refreshChat();
      } catch (e) {
        WB.apiCatch(e, WB.t("Set ignore"));
        ev.target.checked = !ev.target.checked; // rollback toggle on failure
      }
    };
  }
}

// ---------- S24: group task board (status summary + click row to jump back to thread message) ----------
let gkEl = null; // open board overlay (null = closed)
let gkGid = 0; // group id for the board
let gkFilter = "all"; // all | open | done | closed
let gkProject = 0; // S29: project filter (0=all, -1=no project only, >0=specific project id)
let gkTasks = []; // last fetched task rows
let gkRefreshInFlight = false; // H-4: whether a board GET is in-flight (dual-channel dedup)
// H-4: last task.state event payload {task_id, status}. Board dedup uses payload matching
// (local cache already has the task with same status -> local action WS echo -> skip), no time window
// (time window would wrongly swallow legitimate events from another client right after opening the board,
// see s25 C6 regression).
let lastTaskStatePayload = null;

const GK_FILTERS = [
  ["all", "All"],
  ["open", "In progress"],
  ["done", "Done"],
  ["closed", "Closed"],
];

function gkFilterMatch(status, f) {
  if (f === "all") return true;
  if (f === "open") return status === "queued" || status === "running";
  if (f === "done") return status === "done";
  return status === "failed" || status === "canceled";
}

function closeGroupTasksModal() {
  if (!gkEl) return;
  gkEl.remove();
  gkEl = null;
  gkGid = 0;
  gkProject = 0; // S29: reopening the board resets the project filter to "All"
  gkTasks = [];
}

async function openGroupTasksModal(gid) {
  if (gkEl) return; // already open
  const overlay = document.createElement("div");
  overlay.className = "gk-overlay";
  const card = document.createElement("div");
  card.className = "gk-card";
  overlay.appendChild(card);
  document.body.appendChild(overlay);
  gkEl = overlay;
  gkGid = gid;
  gkFilter = "all";
  gkProject = 0; // S29: reopening the board resets the project filter to "All"
  overlay.addEventListener("mousedown", (ev) => {
    if (ev.target === overlay) closeGroupTasksModal();
  });
  card.innerHTML = '<p class="muted" style="padding:12px">' + WB.t("Loading…") + '</p>';
  await loadProjects(); // S29: data source for the project filter dropdown (board is visible to all members)
  await refreshGroupTasksBoard({ force: true }); // H-4: opening the board must refresh
}

// H-4: dual-channel refetch dedup for the group board (payload matching, no time window).
//  * force=true (local actions: open board / filter / cancel / reassign): piggyback only
//    when a request is already in flight (the in-flight request carries the latest state —
//    cancel is persisted before the WS broadcast, see Broadcast before writeJSON in
//    handleTaskCancel).
//  * force=false with match (task.state event {task_id, status}): the local cache
//    already has the task with the same status -> WS echo of a local action, skip;
//    otherwise refresh as usual (s25 C6 regression lesson: a time window would
//    wrongly swallow legitimate new events from another client right after opening
//    the board).
async function refreshGroupTasksBoard(opts) {
  if (!gkEl || !gkGid) return;
  const force = !!(opts && opts.force);
  const match = opts && opts.match;
  if (gkRefreshInFlight) return;
  if (match && match.task_id != null) {
    const t = (gkTasks || []).find((x) => x.id === match.task_id);
    // H-4: both status and owner must match to count as "already reflected" (reassign
    // only swaps the owner, status stays the same; comparing status alone would
    // wrongly swallow reassign events, see s27 C7 regression).
    if (t && t.status === match.status && t.employee_id === match.employee_id) return;
  }
  gkRefreshInFlight = true;
  const gid = gkGid; // capture the group id at request time so a "close A, open B" doesn't mis-attach A's response to B
  // S29: project filter — 0=all (no param); -1=tasks without a project only (?project=0); >0=specific project
  const url = "/api/groups/" + gid + "/tasks" +
    (gkProject !== 0 ? "?project=" + (gkProject === -1 ? 0 : gkProject) : "");
  try {
    const data = await WB.api("GET", url);
    if (!gkEl || gkGid !== gid) return; // modal closed / group switched during the request; discard the stale result
    gkTasks = data.tasks || [];
    renderGroupTasksBoard();
  } catch (e) {
    if (gkGid === gid) closeGroupTasksModal(); // close only if the board still belongs to the group of this request
    WB.apiCatch(e, WB.t("Load group tasks"));
  } finally {
    gkRefreshInFlight = false;
  }
}

function renderGroupTasksBoard() {
  if (!gkEl) return;
  const card = gkEl.firstElementChild;
  if (!card) return;
  const c = chatState.conversations.find((x) => x.group_id === gkGid);
  const gname = c ? c.name : "Group " + gkGid;

  const counts = { all: gkTasks.length, open: 0, done: 0, closed: 0 };
  for (const t of gkTasks) {
    if (t.status === "queued" || t.status === "running") counts.open++;
    else if (t.status === "done") counts.done++;
    else counts.closed++;
  }
  const chips = GK_FILTERS.map(
    ([k, label]) =>
      '<button type="button" class="gk-chip' +
      (gkFilter === k ? " active" : "") +
      '" data-gk-filter="' + k + '">' + WB.t(label) + " " + counts[k] + "</button>"
  ).join("");

  // S29: project filter dropdown (all projects / no project / each project; -1=no project only, server maps to ?project=0)
  const projSelect =
    '<select id="gk-project" class="gk-project" title="' + WB.t("Project filter") + '">' +
    '<option value="0">' + WB.t("All projects") + '</option>' +
    '<option value="-1">' + WB.t("No project") + '</option>' +
    (chatState.projects || []).map(
      (p) => '<option value="' + p.id + '">' + WB.esc(p.name) + "</option>"
    ).join("") +
    "</select>";

  // S25: inline cancel button visibility = client-side heuristic (admin/initiator/owner, queued/running only);
  // S27: reassign button uses the same heuristic, but shows for queued/running/canceled/failed (done is terminal).
  // Server-side routes.Graph.CancelAllowed is still the final arbiter; up-chain leaders don't see the button but the API still allows it.
  const me = WB.state.me;
  const visible = gkTasks.filter((t) => gkFilterMatch(t.status, gkFilter));
  const rows = visible
    .map((t) => {
      const clickable = t.message_id != null;
      const canCancel =
        me &&
        (t.status === "queued" || t.status === "running") &&
        (me.admin ||
          (me.employee_id != null &&
            (me.employee_id === t.requested_by || me.employee_id === t.employee_id)));
      const canReassign =
        me &&
        (t.status === "queued" || t.status === "running" ||
          t.status === "canceled" || t.status === "failed") &&
        (me.admin ||
          (me.employee_id != null &&
            (me.employee_id === t.requested_by || me.employee_id === t.employee_id)));
      // task-duration-artifacts N2: GET /api/tasks/:id is owner-or-admin; non-owners don't see
      // the "Details" button, avoiding a 403 on click.
      const canDetail =
        me && (me.admin || (me.employee_id != null && me.employee_id === t.employee_id));
      // S29: project label (when the row's project_id is non-null; falls back to "project#id" if the name is missing)
      const projName = t.project_name || ("Project #" + t.project_id);
      const projTag =
        t.project_id != null
          ? '<span class="gk-proj" title="' + WB.esc(projName) + '">' +
            WB.esc(projName) + "</span>"
          : "";
      return (
        '<div class="gk-row" ' +
        (clickable ? 'data-gk-mid="' + t.message_id + '"' : "title=\"" + WB.t("Message missing") + "\"") +
        ' data-gk-clickable="' + (clickable ? "1" : "0") + '">' +
        '<span class="tag gk-tag gk-' + WB.esc(t.status) + '">' +
        WB.esc(WB.t(TASK_LABELS[t.status] || t.status)) +
        "</span>" +
        projTag +
        '<span class="gk-body">' + WB.esc(truncate(t.input || "", 42)) + "</span>" +
        '<span class="gk-meta">' +
        WB.esc(t.employee_name || ("Employee #" + t.employee_id)) +
        " · " +
        WB.esc(fmtTime(t.created_at)) +
        (t.duration ? " · " + WB.esc(t.duration) : "") +
        "</span>" +
        (canDetail
          ? '<button type="button" class="gk-detail" data-gk-detail="' + t.id + '" title="' + WB.t("Task detail") + '">' + WB.t("Detail") + "</button>"
          : "") +
        (canCancel
          ? '<button type="button" class="gk-cancel" data-gk-cancel="' + t.id + '">' + WB.t("Cancel") + '</button>'
          : "") +
        (canReassign
          ? '<button type="button" class="gk-reassign" data-gk-reassign="' + t.id + '">' + WB.t("Reassign") + '</button>'
          : "") +
        "</div>"
      );
    })
    .join("");

  // P2-3: when the group has tasks but the current filter shows 0, point the message at that filter, not the whole group.
  // S29: the message is project-filter aware (gkProject 0=any / -1=no project / >0=specific project).
  let emptyText;
  if (gkTasks.length === 0) {
    emptyText = WB.t("No tasks in this group");
  } else {
    const suffix = gkProject === 0
      ? ""
      : " (" + (gkProject === -1
          ? WB.t("Unassigned")
          : ((chatState.projects || []).find((p) => p.id === gkProject) || { name: WB.t("This project") }).name) + ")";
    emptyText =
      (gkFilter === "open" ? WB.t("No in-progress tasks in this group")
       : gkFilter === "done" ? WB.t("No done tasks in this group")
       : gkFilter === "closed" ? WB.t("No closed tasks in this group")
       : WB.t("No tasks in this group")) + suffix;
  }
  card.innerHTML =
    '<div class="gk-title-row">' +
    '<span class="gk-title">' + WB.t("Tasks") + ' · ' + WB.esc(gname) + "</span>" +
    '<button class="gk-close" type="button" title="' + WB.t("Close") + '">×</button>' +
    "</div>" +
    '<div class="gk-chips">' + chips + projSelect + "</div>" +
    (visible.length
      ? '<div class="gk-rows">' + rows + "</div>"
      : '<p class="gk-empty">' + WB.esc(emptyText) + "</p>");

  card.querySelector(".gk-close").onclick = closeGroupTasksModal;
  card.querySelectorAll(".gk-chip").forEach((b) => {
    b.onclick = () => {
      gkFilter = b.getAttribute("data-gk-filter");
      renderGroupTasksBoard();
    };
  });
  // S29: project filter dropdown — selection follows gkProject; switching re-fetches with the new param
  const gkProjSel = card.querySelector("#gk-project");
  if (gkProjSel) {
    gkProjSel.value = String(gkProject);
    // N2 guard: if the option no longer exists (project deleted mid-session),
    // fall back to "all" to avoid a hidden filter mismatch.
    if (gkProjSel.value !== String(gkProject)) {
      gkProject = 0;
      gkProjSel.value = "0";
    }
    gkProjSel.onchange = () => {
      gkProject = Number(gkProjSel.value);
      refreshGroupTasksBoard({ force: true }); // H-4: changing filter always re-fetches with the new params
    };
  }
  card.querySelectorAll(".gk-row").forEach((row) => {
    row.onclick = async (ev) => {
      // S25: inline "Cancel" button — intercepted before the row-jump branch so clicking it doesn't trigger the row jump.
      const cancelBtn = ev.target.closest(".gk-cancel");
      if (cancelBtn) {
        ev.stopPropagation();
        ev.preventDefault();
        if (!confirm(WB.t("Confirm cancel this task?"))) return;
        try {
          await WB.api("POST", "/api/tasks/" + cancelBtn.dataset.gkCancel + "/cancel", { reason: "Cancelled from group board" });
          WB.toast(WB.t("Task canceled"));
          await refreshGroupTasksBoard({ force: true }); // H-4: re-fetch (keep current filter); task moves to "Terminated"
        } catch (e) {
          WB.apiCatch(e, WB.t("Cancel task")); // 403/409 etc. only show a toast; the board is not re-fetched
        }
        return;
      }
      // S27: inline "Reassign" button / picker — intercepted before the row-jump branch so clicking it doesn't trigger the row jump.
      if (ev.target.closest(".gk-reassign-inline")) {
        ev.stopPropagation();
        ev.preventDefault();
        return;
      }
      const reassignBtn = ev.target.closest("[data-gk-reassign]");
      if (reassignBtn) {
        ev.stopPropagation();
        ev.preventDefault();
        const task = gkTasks.find((x) => x.id === Number(reassignBtn.dataset.gkReassign));
        if (!task) return;
        reassignPicker(reassignBtn, task.id, task.employee_id, "Reassigned from group board", () => refreshGroupTasksBoard({ force: true })); // H-4
        return;
      }
      // task-duration-artifacts A: detail button opens the task detail modal (the board
      // overlay comes first; the detail overlay has a lower z-index (131 < 135), so it stacks on top)
      const detailBtn = ev.target.closest("[data-gk-detail]");
      if (detailBtn) {
        ev.stopPropagation();
        ev.preventDefault();
        closeGroupTasksModal();
        openTaskDetail(Number(detailBtn.dataset.gkDetail));
        return;
      }
      if (row.getAttribute("data-gk-clickable") !== "1") return;
      const mid = Number(row.getAttribute("data-gk-mid"));
      closeGroupTasksModal();
      jumpToMessage(mid); // scroll to center + flash; toast if the message is out of the current window
    };
  });
}
