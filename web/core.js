/* whatbark SPA core: state, api client, auth, views (chat/admin/settings),
 * admin sub-tabs, websocket, event bus. S9 chat-first layout. */
"use strict";
window.WB = (function () {
  const LS_SERVERS = "wb_servers";
  const LS_ACTIVE = "wb_active_server";
  const LS_TOKENS = "wb_tokens";
  const TABS = ["org", "wflow", "projects", "status", "inbox", "sched"];
  const VIEWS = ["chat", "admin", "settings"];

  const state = {
    servers: loadJSON(LS_SERVERS, []),
    activeServer: localStorage.getItem(LS_ACTIVE) || "",
    tokens: loadJSON(LS_TOKENS, {}),
    me: null,
    subordinates: [], // Route B: transitive subordinate empIDs for human employees
    empIndex: {},
    avatars: {}, // S12: empID (0 = boss) -> "/api/avatars/<name>" when custom
    ws: null,
    wsTimer: null,
    wsOpen: false,
    view: "chat",
    activeTab: "org", // admin sub-tab
    lang: localStorage.getItem("wb_lang") || "en",
    langs: ["en"],
  };
  const blocks = {};
  const listeners = {};

  // S11b blank-page diagnostics: #boot-wait (plain HTML, see index.html) is
  // removed as soon as core.js has executed; a top-level JS error is surfaced
  // in #toast instead of leaving a silent blank page.
  function removeBootWait() {
    const el = document.getElementById("boot-wait");
    if (el && el.parentNode) el.parentNode.removeChild(el);
  }
  function surfaceBootError(ev) {
    const el = document.getElementById("toast");
    if (!el) return;
    const msg = ev && ev.message ? ev.message : String(ev);
    el.textContent = t("Page script error: ") + msg;
    el.style.background = "#b91c1c";
    el.style.display = "block";
    el.style.maxWidth = "420px";
  }
  window.addEventListener("error", surfaceBootError);

  function loadJSON(k, def) {
    try { const v = JSON.parse(localStorage.getItem(k)); return v == null ? def : v; }
    catch (e) { return def; }
  }
  function saveServers() { localStorage.setItem(LS_SERVERS, JSON.stringify(state.servers)); }
  function saveActive() { localStorage.setItem(LS_ACTIVE, state.activeServer); }
  function saveTokens() { localStorage.setItem(LS_TOKENS, JSON.stringify(state.tokens)); }

  // Route B: can manage workflows/schedules = admin OR human with ≥1 subordinate.
  function canManage() {
    if (!state.me) return false;
    if (state.me.admin) return true;
    return state.me.employee && state.me.employee.kind === "human" && state.subordinates.length > 0;
  }

  function activeServerObj() {
    return state.servers.find(s => s.id === state.activeServer) || null;
  }
  function serverURL() { const s = activeServerObj(); return s ? s.url : ""; }
  function token() { return state.tokens[state.activeServer] || ""; }

  function esc(v) {
    return String(v == null ? "" : v)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;")
      .replace(/>/g, "&gt;").replace(/"/g, "&quot;");
  }

  let toastTimer = null;
  function toast(msg, isError) {
    const el = document.getElementById("toast");
    el.textContent = msg;
    el.style.background = isError ? "#b91c1c" : "#111827";
    el.style.display = "block";
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { el.style.display = "none"; }, 4000);
  }

  async function api(method, path, body) {
    if (!serverURL()) throw new Error("no server configured");
    const opts = { method: method, headers: {} };
    if (token()) opts.headers["Authorization"] = "Bearer " + token();
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    const res = await fetch(serverURL() + path, opts);
    let data = null;
    const ct = res.headers.get("content-type") || "";
    if (ct.indexOf("json") !== -1) {
      try { data = await res.json(); } catch (e) { data = null; }
    }
    if (!res.ok) {
      const msg = data && data.error ? data.error : ("HTTP " + res.status);
      const err = new Error(msg);
      err.status = res.status;
      throw err;
    }
    return data;
  }

  // S12: multipart upload (browser sets the Content-Type boundary itself;
  // no JSON header). Same auth + error shape as api().
  async function upload(path, form) {
    if (!serverURL()) throw new Error("no server configured");
    const opts = { method: "POST", body: form };
    if (token()) opts.headers = { Authorization: "Bearer " + token() };
    const res = await fetch(serverURL() + path, opts);
    let data = null;
    try { data = await res.json(); } catch (e) { data = null; }
    if (!res.ok) {
      const msg = data && data.error ? data.error : ("HTTP " + res.status);
      const err = new Error(msg);
      err.status = res.status;
      throw err;
    }
    return data;
  }

  function apiCatch(e, label) {
    if (e && e.status === 401) {
      logout();
      toast(t("Invalid token or not signed in. Please sign in again."), true);
    } else {
      // S39: server error messages (English source) go through tDyn so they
      // render in the active language when a dictionary/pattern covers them.
      toast((label ? label + ": " : "") + tDyn(e ? e.message : String(e)), true);
    }
  }

  function loginLabel() {
    if (state.me == null) return t("Login");
    if (state.me.admin) return "boss";
    const e = state.me.employee || {};
    return e.name || ("Employee #" + state.me.employee_id);
  }

  // Pixel avatar helpers: deterministic SVGs served from /avatars/<name>.svg.
  // Unknown names fall back to /avatars/default.svg.
  function avatarURL(name) {
    const n = String(name == null ? "" : name).toLowerCase().replace(/[^a-z0-9_-]/g, "");
    return "avatars/" + (n || "default") + ".svg";
  }
  // S12/P0-1: custom avatars are server-served short-lived signed URLs
  // (?exp=&sig=, minted server-side); <img> cannot set headers, so no client
  // token is appended here.
  function avatarSrc(name, empID) {
    const custom = empID != null ? state.avatars[empID] : "";
    if (custom) return serverURL() + custom;
    return avatarURL(name);
  }
  // S12a: avatar <img> failure used to be `onerror="this.remove()"` — one
  // transient failure (server restart, blip) left the initial letter forever.
  // Self-heal: retry once with a cache-buster, then fall back to the
  // deterministic pixel SVG (data-fb), and only then give up to the letter.
  function avaErr(img) {
    const n = Number(img.dataset.n || "0");
    if (n === 0) {
      img.dataset.n = "1";
      const base = img.src.split("#")[0];
      img.src = base + (base.indexOf("?") >= 0 ? "&" : "?") + "r=" + Date.now();
      return;
    }
    if (n === 1 && img.dataset.fb) {
      img.dataset.n = "2";
      img.src = img.dataset.fb;
      return;
    }
    img.remove();
  }
  function avatarSpan(name, cls, empID) {
    const initial = String(name == null ? "?" : name).slice(0, 1).toUpperCase();
    const c = cls ? " " + cls : "";
    return '<span class="ava' + c + '">' + esc(initial) +
      '<img data-fb="' + avatarURL(name) + '" src="' + avatarSrc(name, empID) + '" alt="" onerror="WB.avaErr(this)"></span>';
  }

  // S15: mobile breakpoint — must stay in lockstep with the style.css media
  // query "(max-width: 767px)".
  function isMobile() {
    return window.matchMedia("(max-width: 767px)").matches;
  }

  // renderChrome updates the left rail: avatar, admin button visibility, WS dot.
  function renderChrome() {
    const av = document.getElementById("rail-avatar");
    if (state.me == null) {
      av.innerHTML = "";
      av.textContent = "?";
      av.title = t("Not signed in");
    } else if (state.me.admin) {
      av.innerHTML = avatarSpan("boss", "rail-ava", 0);
      av.title = t("boss (admin)") + " " + t("(click to change avatar)");
    } else {
      const e = state.me.employee || {};
      av.innerHTML = avatarSpan(e.name, "rail-ava", state.me.employee_id);
      av.title = (e.name || ("Employee #" + state.me.employee_id)) + " " + t("(click to change avatar)");
    }
    // Route B: admin entry shown to admins and to human employees who have at
    // least one subordinate (canManage). Humans without subordinates never see
    // the admin entry; AI employees never see it.
    document.getElementById("rail-admin").hidden = !canManage();
    // S15: the mobile bottom nav mirrors the rail's admin visibility
    // (invisible on desktop — #bottom-nav is display:none there).
    document.getElementById("nav-admin").hidden = !canManage();
    updateWSDot();
  }

  function updateWSDot() {
    const dot = document.getElementById("ws-dot");
    dot.className = "ws-dot" + (state.wsOpen ? " on" : "");
    dot.title = state.wsOpen ? t("WS connected") : t("WS disconnected");
  }

  // showView switches between the top-level views (chat / admin / settings).
  function showView(v) {
    // Route B: admin view is reachable by admins and human employees with
    // subordinates; the tab strip below is filtered per role.
    if (v === "admin" && !canManage()) v = "chat";
    state.view = v;
    // The login view is not in VIEWS; deactivate it explicitly or it stays
    // visible after doLogin -> refreshMe -> showView (S11b: double-active bug).
    document.getElementById("view-login").classList.remove("active");
    VIEWS.forEach(name => {
      document.getElementById("view-" + name).classList.toggle("active", name === v);
    });
    // S15: the active highlight follows both the desktop rail and the mobile
    // bottom nav (bottom nav is display:none on desktop, so this is a no-op there).
    document.querySelectorAll("#rail-nav .rail-btn, #bottom-nav .nav-btn").forEach(b => {
      b.classList.toggle("active", b.getAttribute("data-view") === v);
    });
    if (v === "chat") {
      const el = document.getElementById("block-chat");
      // S15: on mobile the "chat" tab always returns to the conversation list;
      // desktop keeps the last thread (closeThread only runs on mobile).
      if (isMobile() && blocks.chat && typeof blocks.chat.closeThread === "function") {
        blocks.chat.closeThread();
      }
      // .block defaults to display:none; without .active the whole chat view
      // renders invisible (S11b round 3, caught by real-browser screenshot).
      el.classList.add("active");
      if (blocks.chat) blocks.chat.render(el);
      else el.innerHTML = '<p class="error">' + t("Chat module not loaded, please refresh the page.") + '</p>';
    } else if (v === "settings") {
      const el = document.getElementById("block-settings");
      el.classList.add("active");
      if (blocks.settings) blocks.settings.render(el);
      else el.innerHTML = '<p class="error">' + t("Settings module not loaded, please refresh the page.") + '</p>';
    } else if (v === "admin") {
      renderActive();
    }
  }

  function showLogin() {
    VIEWS.forEach(name => {
      document.getElementById("view-" + name).classList.remove("active");
    });
    document.getElementById("view-login").classList.add("active");
  }

  async function login() {
    const ta = document.getElementById("login-area");
    ta.innerHTML =
      '<input id="login-token" type="password" placeholder="token" size="24">' +
      '<button class="btn small" id="login-btn">' + t("Login") + '</button>';
    document.getElementById("login-btn").onclick = doLogin;
  }

  async function doLogin() {
    const input = document.getElementById("login-token");
    const tok = (input ? input.value : "").trim();
    if (!tok) { toast(t("Please enter a token"), true); return; }
    state.tokens[state.activeServer] = tok;
    saveTokens();
    await refreshMe();
  }

  async function refreshMe() {
    try {
      state.me = await api("GET", "/api/me");
      // Route B: transitive subordinate list for this caller (admin -> []).
      state.subordinates = Array.isArray(state.me.subordinates) ? state.me.subordinates : [];
      await loadEmployees();
      connectWS();
      // S12: seed own avatar (boss -> empID 0, employee -> own id).
      if (state.me && state.me.admin) state.avatars[0] = state.me.avatar || "";
      else if (state.me && state.me.employee) state.avatars[state.me.employee_id] = state.me.employee.avatar || "";
    } catch (e) {
      if (e.status === 401) {
        state.me = null;
        state.subordinates = [];
        disconnectWS();
        renderChrome();
        showLogin();
        login();
        toast(t("Login failed: invalid token"), true);
        return;
      }
      throw e;
    }
    // S39: i18n boot — strictly non-blocking; must not delay login/render.
    // Fired after successful auth (token guaranteed valid; the languages
    // endpoint requires it). Stale persisted lang not in the server list
    // resets to en.
    fetchLangs().then(function () {
      if (state.langs.indexOf(state.lang) === -1) {
        state.lang = "en";
        localStorage.removeItem(LS_LANG);
      }
      loadLangFile(state.lang, function () {
        document.documentElement.lang = state.lang === "zh" ? "zh-CN" : "en";
        applyStatic();
        // Re-render the active view so JS-rendered blocks (settings cards, etc.)
        // pick up the loaded dictionary; first render above may have used the
        // English fallback while the lang file was still loading.
        showView(state.view);
      });
    });
    renderChrome();
    showView(state.view);
  }

  function logout() {
    state.me = null;
    state.subordinates = [];
    // Clear the active server's stored token so a page reload does not auto
    // re-login (bootServer reads the token from localStorage and would call
    // refreshMe again). Other servers' tokens are intentionally preserved.
    delete state.tokens[state.activeServer];
    saveTokens();
    disconnectWS();
    renderChrome();
    showLogin();
    login();
  }

  async function loadEmployees() {
    const d = await api("GET", "/api/employees");
    state.empIndex = {};
    (d.employees || []).forEach(e => {
      state.empIndex[e.id] = e;
      if (e.avatar) state.avatars[e.id] = e.avatar;
    });
  }
  function empName(id) {
    const e = state.empIndex[id];
    return e ? e.name : ("Employee #" + id);
  }

  // ---------- S12: avatar change ----------
  const AVATAR_MAX_BYTES = 2 * 1024 * 1024; // matches server cap

  // prepareAvatar passes small files through; larger ones are downscaled to
  // <=512px JPEG 0.9 so users rarely hit the 2 MiB cap.
  function prepareAvatar(file) {
    if (file.size <= AVATAR_MAX_BYTES) return Promise.resolve(file);
    return new Promise((resolve, reject) => {
      const url = URL.createObjectURL(file);
      const img = new Image();
      img.onload = () => {
        URL.revokeObjectURL(url);
        const scale = Math.min(1, 512 / Math.max(img.width, img.height));
        const w = Math.max(1, Math.round(img.width * scale));
        const h = Math.max(1, Math.round(img.height * scale));
        const canvas = document.createElement("canvas");
        canvas.width = w;
        canvas.height = h;
        canvas.getContext("2d").drawImage(img, 0, 0, w, h);
        canvas.toBlob((blob) => {
          if (!blob) { reject(new Error(t("Image compression failed"))); return; }
          resolve(new File([blob], "avatar.jpg", { type: "image/jpeg" }));
        }, "image/jpeg", 0.9);
      };
      img.onerror = () => { URL.revokeObjectURL(url); reject(new Error(t("Image read failed"))); };
      img.src = url;
    });
  }

  // changeAvatar: rail avatar click -> file picker -> upload -> refresh.
  function changeAvatar() {
    if (!state.me) return;
    const input = document.createElement("input");
    input.type = "file";
    input.accept = "image/png,image/jpeg,image/gif,image/webp";
    input.style.display = "none";
    document.body.appendChild(input);
    input.onchange = async () => {
      const f = input.files && input.files[0];
      input.remove();
      if (!f) return;
      try {
        const ready = await prepareAvatar(f);
        const form = new FormData();
        form.append("avatar", ready);
        const d = await upload("/api/me/avatar", form);
        const meID = state.me && state.me.admin ? 0 : state.me.employee_id;
        state.avatars[meID] = d.avatar;
        renderChrome();
        if (state.view === "chat" && typeof refreshChat === "function") refreshChat();
        toast(t("Avatar updated"));
      } catch (e) {
        apiCatch(e, t("Change avatar"));
      }
    };
    input.click();
  }

  // ---------- admin sub-tabs ----------
  // Route B: admins see all admin tabs; human employees with subordinates see
  // only wflow + sched. AI employees never reach the admin view (canManage).
  function allowedTabs() {
    if (state.me && state.me.admin) return TABS;
    return ["wflow", "sched"];
  }

  function renderActive() {
    if (allowedTabs().indexOf(state.activeTab) === -1) state.activeTab = "wflow";
    TABS.forEach(t => {
      const sec = document.getElementById("block-" + t);
      const btn = document.querySelector('#tabs button[data-tab="' + t + '"]');
      const allowed = allowedTabs().indexOf(t) !== -1;
      if (btn) btn.style.display = allowed ? "" : "none";
      if (t === state.activeTab && allowed) {
        sec.classList.add("active");
        btn.classList.add("active");
        if (blocks[t] && typeof blocks[t].render === "function") {
          blocks[t].render(sec);
        }
      } else {
        sec.classList.remove("active");
        btn.classList.remove("active");
      }
    });
  }
  function showTab(name) {
    if (TABS.indexOf(name) === -1) return;
    if (allowedTabs().indexOf(name) === -1) return; // Route B: non-admin humans only wflow+sched
    state.activeTab = name;
    renderActive();
  }

  // ---------- event bus + ws ----------
  function on(ev, fn) {
    (listeners[ev] = listeners[ev] || []).push(fn);
  }
  function emit(ev, payload) {
    (listeners[ev] || []).forEach(fn => {
      try { fn(payload); } catch (e) { console.error(ev, e); }
    });
  }

  function disconnectWS() {
    if (state.wsTimer) { clearTimeout(state.wsTimer); state.wsTimer = null; }
    if (state.ws) {
      state.ws.onclose = null;
      state.ws.onmessage = null;
      try { state.ws.close(); } catch (e) {}
      state.ws = null;
    }
    state.wsOpen = false;
    updateWSDot();
  }

  // P0-1 (F2): server mints file/avatar URLs with a 1h TTL (fileURLTTL), but
  // the SPA only fetches them at boot / on events. A page left open >1h would
  // silently 401 on custom avatars / attachment links (img degrades to the
  // letter/pixel fallback). Re-mint cheaply via re-fetching /api/me +
  // /api/employees (both carry fresh signed avatar urls), then re-render.
  // Two triggers, both with margin against the 1h TTL:
  //   1. a 45-minute timer (15-min margin before URLs expire),
  //   2. WS reconnect success (onopen after a prior open) — proves the server
  //      is reachable and the session alive.
  // Failures are silent: the next timer tick / next onopen retries.
  let wsEverOpen = false;
  async function refreshAuthDependentData() {
    if (!state.me || !serverURL()) return;
    try {
      state.me = await api("GET", "/api/me");
      state.subordinates = Array.isArray(state.me.subordinates) ? state.me.subordinates : [];
      await loadEmployees();
      if (state.me && state.me.admin) state.avatars[0] = state.me.avatar || "";
      else if (state.me && state.me.employee) state.avatars[state.me.employee_id] = state.me.employee.avatar || "";
      renderChrome();
      // Re-fetch the open thread so message attachment urls are re-minted
      // (existing refresh path; no new protocol).
      if (state.view === "chat" && typeof refreshChat === "function") refreshChat();
    } catch (e) {
      // Silent by design (F2): transient outage/token blip; next tick retries.
      // state.me is left untouched so a failed refresh never logs the user out.
    }
  }

  function connectWS() {
    if (!serverURL() || !token() || !state.me) return;
    disconnectWS();
    let host = serverURL().replace(/^https:\/\//, "").replace(/^http:\/\//, "");
    const proto = serverURL().startsWith("https") ? "wss" : "ws";
    // P0-1 (F3): token rides the single Sec-WebSocket-Protocol subprotocol.
    // RFC 7230 token charset — a token with non-token chars (space, non-ASCII)
    // makes the WebSocket constructor throw *before* state.ws is assigned, so
    // onclose never runs and the reconnect loop never starts (permanently dead
    // socket). Fall back to the server's still-supported query-token path.
    const t = token();
    const tokOK = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/.test(t);
    let ws;
    if (tokOK) {
      ws = new WebSocket(proto + "://" + host + "/ws", [t]);
    } else {
      console.warn("WB: token not a valid WS subprotocol token; falling back to query-token /ws");
      ws = new WebSocket(proto + "://" + host + "/ws?" + "token=" + encodeURIComponent(t));
    }
    state.ws = ws;
    ws.onopen = () => {
      state.wsOpen = true; updateWSDot(); emit("ws.state", { open: true });
      // P0-1 (F2): a re-established connection = good moment to re-mint the
      // short-lived signed URLs (initial open right after boot is skipped:
      // refreshMe() just fetched everything).
      if (wsEverOpen) refreshAuthDependentData();
      wsEverOpen = true;
    };
    ws.onmessage = (ev) => {
      let e;
      try { e = JSON.parse(ev.data); } catch (err) { return; }
      if (!e || !e.type) return;
      emit(e.type, e.payload || {});
      emit("any", e);
    };
    ws.onclose = () => {
      state.wsOpen = false;
      updateWSDot();
      emit("ws.state", { open: false });
      if (state.me) state.wsTimer = setTimeout(connectWS, 3000);
    };
  }

  function wsStatusText() {
    if (!state.me) return t("Not signed in");
    return state.wsOpen ? t("WS connected") : t("WS disconnected, reconnecting…");
  }

  // ---------- i18n (S39) ----------
  const LS_LANG = "wb_lang";
  const LANG_NAMES = { en: "English", zh: "中文" };

  function t(str) {
    if (state.lang === "en" || !str) return str;
    const dict = (window.WB_I18N || {})[state.lang];
    return (dict && dict[str] != null) ? dict[str] : str;
  }

  function tDyn(str) {
    if (!str || state.lang === "en") return str;
    const dict = (window.WB_I18N || {})[state.lang];
    if (dict && dict[str] != null) return dict[str];
    const pats = (window.WB_I18N_SYS || {})[state.lang];
    if (pats) {
      for (const [reSrc, tpl] of pats) {
        const m = str.match(new RegExp(reSrc));
        if (m) {
          if (typeof tpl === "function") return tpl(m);
          // function replacer: avoids String.replace $-patterns ($&, $$, $')
          // from user-controlled capture values (F1 review fix).
          let out = tpl;
          for (let i = m.length - 1; i >= 1; i--) out = out.replace("$" + i, () => m[i]);
          return out;
        }
      }
    }
    return str;
  }

  function fetchLangs() {
    return api("GET", "/api/i18n/languages").then(function (d) {
      if (d && Array.isArray(d.languages)) state.langs = d.languages;
    }).catch(function () { state.langs = ["en"]; });
  }

  function loadLangFile(lang, cb) {
    if (lang === "en" || (window.WB_I18N || {})[lang]) { cb(); return; }
    const s = document.createElement("script");
    s.src = "i18n/" + lang + ".js";
    s.onload = cb;
    s.onerror = function () {
      console.warn("WB: failed to load i18n file for " + lang);
      state.lang = "en";
      localStorage.setItem(LS_LANG, "en");
      cb();
    };
    document.head.appendChild(s);
  }

  function setLang(lang) {
    if (state.langs.indexOf(lang) === -1) return;
    state.lang = lang;
    localStorage.setItem(LS_LANG, lang);
    loadLangFile(lang, function () {
      document.documentElement.lang = lang === "zh" ? "zh-CN" : "en";
      applyStatic();
      showView(state.view);
    });
  }

  function applyStatic() {
    document.querySelectorAll("[data-i18n]").forEach(function (el) {
      el.textContent = t(el.getAttribute("data-i18n"));
    });
    document.querySelectorAll("[data-i18n-title]").forEach(function (el) {
      el.title = t(el.getAttribute("data-i18n-title"));
      if (el.hasAttribute("aria-label")) el.setAttribute("aria-label", t(el.getAttribute("data-i18n-title")));
    });
  }

  // ---------- block registry ----------
  function register(name, impl) { blocks[name] = impl; }

  // ---------- boot ----------
  function init() {
    if (state.servers.length === 0) {
      const s = { id: "local", name: t("Local"), url: location.origin };
      state.servers.push(s);
      state.activeServer = s.id;
      saveServers();
      saveActive();
    }
    if (!state.activeServer || !activeServerObj()) {
      state.activeServer = state.servers[0] ? state.servers[0].id : "";
      saveActive();
    }

    document.querySelectorAll("#rail-nav .rail-btn").forEach(btn => {
      btn.onclick = () => showView(btn.getAttribute("data-view"));
    });
    // S15: mobile bottom nav shares the rail's wiring (#bottom-nav is invisible on desktop).
    document.querySelectorAll("#bottom-nav .nav-btn").forEach(btn => {
      btn.onclick = () => showView(btn.getAttribute("data-view"));
    });

    document.querySelectorAll("#tabs button").forEach(btn => {
      btn.onclick = () => showTab(btn.getAttribute("data-tab"));
    });

    // S12: rail avatar is the avatar-change entry point.
    const railAva = document.getElementById("rail-avatar");
    if (railAva) railAva.onclick = changeAvatar;

    // P0-1 (F2): re-mint short-lived signed avatar/file URLs every 45 minutes
    // (server fileURLTTL is 1h, so 45m keeps a 15-minute margin before expiry).
    // The re-fetch doubles as the reachability probe: if the server is down,
    // /api/me throws, is swallowed by the caller, and the next tick retries.
    // No-op until logged in (state.me guard inside).
    setInterval(refreshAuthDependentData, 45 * 60 * 1000);

    bootServer();
  }

  async function bootServer() {
    disconnectWS();
    state.me = null;
    state.subordinates = [];
    state.empIndex = {};
    state.avatars = {};
    renderChrome();
    if (!token()) {
      showLogin();
      login();
      return;
    }
    try {
      await refreshMe();
    } catch (e) {
      // The saved server may point at a dead demo instance (old port). If the
      // page is served from a different origin, that origin is most likely the
      // right one: fall back to it and retry once.
      if (e && e.status !== 401 && token() && serverURL() !== location.origin) {
        state.servers = [{ id: "local", name: t("Local"), url: location.origin }];
        state.activeServer = "local";
        saveServers();
        saveActive();
        try {
          await refreshMe();
          return;
        } catch (e2) {
          apiCatch(e2, t("Init"));
          if (!state.me) { showLogin(); login(); }
          return;
        }
      }
      apiCatch(e, t("Init"));
      if (!state.me) { showLogin(); login(); }
    }
  }

  // S12: someone changed an avatar (including this caller's) -> refresh the
  // avatar map and re-render chrome + the open chat view.
  on("employee.avatar", (p) => {
    if (p == null || p.employee_id == null || !p.avatar) return;
    state.avatars[p.employee_id] = p.avatar;
    renderChrome();
    if (state.view === "chat" && typeof refreshChat === "function") refreshChat();
  });

  removeBootWait();

  return {
    state: state,
    upload: upload,
    api: api,
    apiCatch: apiCatch,
    esc: esc,
    toast: toast,
    t: t,
    tDyn: tDyn,
    setLang: setLang,
    fetchLangs: fetchLangs,
    LANG_NAMES: LANG_NAMES,
    showTab: showTab,
    canManage: canManage,
    showView: showView,
    logout: logout,
    on: on,
    register: register,
    blocks: blocks,
    saveServers: saveServers,
    saveActive: saveActive,
    saveTokens: saveTokens,
    bootServer: bootServer,
    loadEmployees: loadEmployees,
    empName: empName,
    loginLabel: loginLabel,
    avatarSpan: avatarSpan,
    avatarURL: avatarURL,
    avatarSrc: avatarSrc,
    avaErr: avaErr,
    changeAvatar: changeAvatar,
    prepareAvatar: prepareAvatar,
    serverURL: serverURL,
    token: token,
    connectWS: connectWS,
    disconnectWS: disconnectWS,
    wsStatusText: wsStatusText,
    isMobile: isMobile,
    init: init,
  };
})();
