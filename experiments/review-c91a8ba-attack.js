#!/usr/bin/env node
/**
 * 红队攻击测试 — commit c91a8ba（服务器面板：token 列 + 复制 + tfoot 添加行）。
 * hermetic：独立 18100 端口 + 全新 DB，不碰 demo.db。
 * 攻击面：
 *   A1 XSS：恶意服务器名称（HTML 注入 payload）
 *   A2 XSS：恶意 URL（双引号 + script）
 *   A3 token 本体不进 .servers-panel 的 DOM（含激活服务器的 token）
 *   A4 恶意 token 值（含引号/script）：存储不渲染 + 切换后 401 错误路径不崩
 *   A5 双击"添加" → 不产生重复行/同毫秒 id 碰撞
 *   A6 纯空白 token → 按无 token 处理（"—" 无图标）
 *   A7 clipboard 不可用（模拟非安全上下文）→ execCommand 回退 + toast
 *   A8 删除 ACTIVE 服务器 → bootServer 死代码 + 会话滞留 + 当前tag错位
 *   A9 双 .servers-panel 实例作用域（settings + admin 并存）
 *   A10 删除带 token 服务器 → 孤儿 token 滞留 localStorage
 * 用法：NODE_PATH=scratch-headless/node_modules node experiments/review-c91a8ba-attack.js
 */
const fs = require("fs");
const path = require("path");
const { spawn } = require("child_process");
const puppeteer = require("puppeteer-core");

const BIN = path.join(__dirname, "..", "bin", "whatbark");
const PORT = 18100;
const BASE = "http://127.0.0.1:" + PORT;
const CHROME = process.env.CHROME || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const ADM = "qasrvtoken123";
const ROOT = "/var/folders/7w/qmcknsz54cx8wdmrr457vy7r0000gn/T/opencode/review-c91a8ba-attack";
const DB = path.join(ROOT, "qa.db");
const PANEL = "#block-settings .servers-panel";
const ADMIN_PANEL = "#block-servers .servers-panel";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const results = [];
let serverProc = null;

function ok(cond, label, extra) {
  const pass = !!cond;
  results.push({ pass, label, extra });
  if (!pass) console.log(`FAIL  ${label}${extra ? "  [" + extra + "]" : ""}`);
  else console.log(`PASS  ${label}`);
}

function attachListeners(page, tag, problems) {
  page.on("pageerror", (e) => problems.push(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() !== "error") return;
    const t = m.text();
    if (/^Failed to load resource.*status of (400|403|404|409)/.test(t)) return;
    if (/ERR_NAME_NOT_RESOLVED|ERR_CONNECTION/.test(t)) return; // A4 故意切到不可达主机
    problems.push(`${tag} console: ${t}`);
  });
  page.on("response", (r) => {
    if (r.status() >= 400 && r.status() !== 404) problems.push(`${tag} http ${r.status()} ${r.url()}`);
  });
}

async function startServer() {
  fs.rmSync(ROOT, { recursive: true, force: true });
  fs.mkdirSync(ROOT, { recursive: true });
  const log = fs.openSync(path.join(ROOT, "serve.log"), "w");
  serverProc = spawn(BIN, ["serve", "--listen", `127.0.0.1:${PORT}`, "--data", DB, "--admin-token", ADM], {
    stdio: ["ignore", log, log],
  });
  const t0 = Date.now();
  while (Date.now() - t0 < 30000) {
    try {
      const r = await fetch(BASE + "/healthz");
      if (r.status === 200) return;
    } catch (_) {}
    await sleep(300);
  }
  throw new Error("临时服务未就绪: " + BASE);
}

// 工具：通过 UI 添加服务器（走 tfoot 输入框 + 点击添加按钮，覆盖完整路径）
async function addServerUI(page, sel, name, url, tok) {
  await page.evaluate((sel, name, url, tok) => {
    const c = document.querySelector(sel);
    c.querySelector(".srv-name").value = name;
    c.querySelector(".srv-url").value = url;
    c.querySelector(".srv-token").value = tok;
  }, sel, name, url, tok);
  await page.click(sel + " .srv-add");
  await sleep(150);
}

async function main() {
  const problems = [];
  await startServer();
  const browser = await puppeteer.launch({
    executablePath: CHROME,
    headless: "new",
    args: ["--no-sandbox", "--disable-setuid-sandbox"],
  });
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 900 });
  attachListeners(page, "main", problems);
  await page.goto(BASE + "/", { waitUntil: "networkidle2" });
  await page.waitForSelector("#login-token", { timeout: 15000 });
  await page.type("#login-token", ADM);
  await page.click("#login-btn");
  await page.waitForFunction(() => WB.state.me && WB.state.me.admin, { timeout: 15000 });
  await page.evaluate(() => WB.showView("settings"));
  await page.waitForSelector(PANEL + " table", { timeout: 10000 });
  await sleep(300);

  // ---------- A1: XSS 恶意名称 ----------
  const PAYLOAD_NAME = '<img src=x onerror="window.__xss1=1"><svg/onload=window.__xss1=2>';
  await addServerUI(page, PANEL, PAYLOAD_NAME, "https://xss.example:8080", "");
  const a1 = await page.evaluate((sel) => ({
    injectedImg: !!document.querySelector(sel + " img"),
    injectedSvg: !!document.querySelector(sel + " svg:not(.srv-copy svg)"),
    xss1: window.__xss1,
    textEscaped: [...document.querySelectorAll(sel + " .srv-rows td")].some(td => td.textContent.includes("<img")),
  }), PANEL);
  ok(!a1.injectedImg && !a1.injectedSvg && a1.xss1 === undefined, "A1 恶意名称未注入 DOM/未执行", JSON.stringify(a1));
  ok(a1.textEscaped, "A1 payload 以纯文本呈现（已转义）");

  // ---------- A2: XSS 恶意 URL（属性逃逸尝试） ----------
  const PAYLOAD_URL = 'https://evil.example"><script>window.__xss2=1</script>';
  await addServerUI(page, PANEL, "urlxss", PAYLOAD_URL, "");
  const a2 = await page.evaluate((sel) => ({
    xss2: window.__xss2,
    scriptEls: document.querySelectorAll(sel + " script").length,
    textEscaped: [...document.querySelectorAll(sel + " .srv-rows td")].some(td => td.textContent.includes('">')),
  }), PANEL);
  ok(a2.xss2 === undefined && a2.scriptEls === 0, "A2 恶意 URL 未执行/未注入 script", JSON.stringify(a2));
  ok(a2.textEscaped, "A2 URL payload 以纯文本呈现");

  // ---------- A3: token 本体不进面板 DOM（用合法 ADM token 加镜像服务器，可切换） ----------
  await addServerUI(page, PANEL, "sec", BASE, ADM);
  const a3 = await page.evaluate((sel, adm) => {
    const panel = document.querySelector(sel);
    return {
      inPanel: panel.innerHTML.includes(adm),
      inTbody: panel.querySelector(".srv-rows").innerHTML.includes(adm),
      stored: Object.values(WB.state.tokens).includes(adm),
    };
  }, PANEL, ADM);
  ok(!a3.inPanel && !a3.inTbody, "A3 token 本体不进 .servers-panel DOM", JSON.stringify(a3));
  ok(a3.stored, "A3 token 已正确存储");

  // 切换到该服务器（激活服务器的 token 在设置页账号区 readonly 框展示是既有行为，
  // 检查范围限定在 servers-panel 内部）
  await page.evaluate((sel) => {
    const rows = [...document.querySelectorAll(sel + " .srv-rows tr")];
    const tr = rows.find((r) => r.cells[0].textContent.includes("sec"));
    tr.querySelector(".srv-switch").click();
  }, PANEL);
  await page.waitForFunction(() => WB.state.me && WB.state.me.admin, { timeout: 15000 });
  await page.evaluate(() => WB.showView("settings"));
  await sleep(300);
  const a3b = await page.evaluate((sel, adm) => {
    return { inPanel: document.querySelector(sel).innerHTML.includes(adm) };
  }, PANEL, ADM);
  ok(!a3b.inPanel, "A3b 激活后 token 仍不进 servers-panel（账号区 readonly 框为既有展示面）", JSON.stringify(a3b));

  // ---------- A10: 删除带 token 服务器 → 孤儿 token（sec 此刻为激活服务器） ----------
  const secId = await page.evaluate(() => WB.state.servers.find((s) => s.name === "sec")?.id);
  await page.evaluate((sel) => {
    const rows = [...document.querySelectorAll(sel + " .srv-rows tr")];
    const tr = rows.find((r) => r.cells[0].textContent.includes("sec"));
    tr.querySelector(".srv-del").click();
  }, PANEL);
  await sleep(400);
  const a10 = await page.evaluate((sid) => {
    return {
      serverGone: !WB.state.servers.some((s) => s.id === sid),
      orphanKey: WB.state.tokens[sid] !== undefined,
      activeNow: WB.state.activeServer,
    };
  }, secId);
  ok(a10.serverGone, "A10 前置: sec 行已删除", JSON.stringify(a10));
  ok(a10.orphanKey, "A10[复现] 删除带 token 服务器后 token 键滞留 WB.state.tokens（无清理逻辑，孤儿凭证累积）", JSON.stringify(a10));

  // ---------- A4: 恶意 token 值 + 不可达服务器错误路径 ----------
  const EVIL_TOK = '"><script>window.__xss4=1</script>';
  await addServerUI(page, PANEL, "eviltok", "https://noexist.example:9999", EVIL_TOK);
  const a4a = await page.evaluate(() => ({
    xss4: window.__xss4,
    storedEvil: Object.values(WB.state.tokens).some((t) => t && t.includes("xss4")),
  }));
  ok(a4a.xss4 === undefined, "A4a 恶意 token 值未执行脚本", JSON.stringify(a4a));
  ok(a4a.storedEvil, "A4a 恶意 token 按原值存储（存储路径不渲染，无 DOM 暴露）");
  // 切换到不可达服务器 → 既有 bootServer 回退路径（预期：整表回落到 local 并重新登录）
  await page.evaluate((sel) => {
    const rows = [...document.querySelectorAll(sel + " .srv-rows tr")];
    const tr = rows.find((r) => r.cells[0].textContent.includes("eviltok"));
    tr.querySelector(".srv-switch").click();
  }, PANEL);
  const a4b = await page.evaluate(() => {
    return {
      servers: WB.state.servers.map((s) => s.id),
      active: WB.state.activeServer,
      meAdmin: !!(WB.state.me && WB.state.me.admin),
      view: WB.state.view,
    };
  });
  const a4bWait = await page.waitForFunction(
    () => WB.state.servers.length === 1 && WB.state.activeServer === "local" && WB.state.me && WB.state.me.admin,
    { timeout: 30000 }
  ).then(() => "fallback-ok").catch(() => "fallback-timeout");
  ok(a4bWait === "fallback-ok", "A4b 切换不可达服务器走既有回退路径（整表回落 local + 重新登录），不崩",
    JSON.stringify({ a4b, wait: a4bWait }));
  await page.evaluate(() => WB.showView("settings"));
  await sleep(500);

  // ---------- A5: 双击"添加" ----------
  const nBefore = await page.evaluate(() => WB.state.servers.length);
  await page.evaluate((sel) => {
    const c = document.querySelector(sel);
    c.querySelector(".srv-name").value = "dbl";
    c.querySelector(".srv-url").value = "https://dbl.example:8080";
  }, PANEL);
  await page.evaluate((sel) => {
    const btn = document.querySelector(sel + " .srv-add");
    btn.click();
    btn.click(); // 同步双击
  }, PANEL);
  await sleep(300);
  const a5 = await page.evaluate((sel, n) => {
    const rows = [...document.querySelectorAll(sel + " .srv-rows tr")];
    return { count: rows.filter((r) => r.cells[0].textContent.includes("dbl")).length, total: WB.state.servers.length };
  }, PANEL, nBefore);
  ok(a5.count === 1 && a5.total === nBefore + 1, "A5 双击添加只新增 1 行（第二次点击看到已清空输入框）", JSON.stringify(a5));

  // ---------- A6: 纯空白 token ----------
  await addServerUI(page, PANEL, "wstok", "https://ws.example:8080", "   ");
  const a6 = await page.evaluate((sel) => {
    const rows = [...document.querySelectorAll(sel + " .srv-rows tr")];
    const tr = rows.find((r) => r.cells[0].textContent.includes("wstok"));
    return {
      noneMark: tr.cells[2].textContent.trim() === "—",
      noCopy: !tr.cells[2].querySelector(".srv-copy"),
      emptyStored: Object.values(WB.state.tokens).includes("   "),
    };
  }, PANEL);
  ok(a6.noneMark && a6.noCopy && !a6.emptyStored, "A6 纯空白 token 按无 token 处理且不落库", JSON.stringify(a6));

  // ---------- A7: clipboard 不可用 → execCommand 回退 ----------
  await page.evaluate(() => {
    window.__execCmd = [];
    const orig = document.execCommand;
    document.execCommand = function (c, u, v) { window.__execCmd.push(c); return orig.apply(document, [c, u, v]); };
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText: () => Promise.reject(new Error("NotAllowedError (simulated)")) },
    });
  });
  await page.click(PANEL + " .srv-rows tr:first-child .srv-copy");
  await sleep(400);
  const a7 = await page.evaluate(() => {
    const t = document.querySelector("#toast");
    return { toast: t ? t.textContent.trim() : "", exec: window.__execCmd, leftTa: document.body.querySelectorAll("textarea").length };
  });
  ok(a7.exec.includes("copy"), "A7 clipboard 被拒后走到 execCommand 回退路径（无异常、textarea 已清理）", JSON.stringify(a7));
  ok(a7.toast.includes("已复制") || a7.toast.includes("复制失败"), "A7 回退后 toast 二选一且语义正确", a7.toast);

  // ---------- A8: 删除 ACTIVE 服务器 → bootServer 死代码 ----------
  await page.evaluate(() => {
    window.__bootCalls = 0;
    const orig = WB.bootServer;
    WB.bootServer = function () { window.__bootCalls++; return orig.apply(WB, arguments); };
  });
  const before = await page.evaluate(() => ({
    active: WB.state.activeServer,
    servers: WB.state.servers.map((s) => s.id),
    meAdmin: !!(WB.state.me && WB.state.me.admin),
  }));
  // 删除当前激活的那一行
  await page.evaluate((sel) => {
    const rows = [...document.querySelectorAll(sel + " .srv-rows tr")];
    const tr = rows.find((r) => !!r.querySelector(".tag.online"));
    tr.querySelector(".srv-del").click();
  }, PANEL);
  await sleep(1500);
  const a8 = await page.evaluate(() => ({
    bootCalls: window.__bootCalls,
    active: WB.state.activeServer,
    meAdmin: !!(WB.state.me && WB.state.me.admin),
    tagRow: (() => {
      const tr = [...document.querySelectorAll("#block-settings .servers-panel .srv-rows tr")].find((r) => r.querySelector(".tag.online"));
      return tr ? tr.cells[0].textContent.trim() : null;
    })(),
  }));
  ok(a8.bootCalls === 0, "A8[复现] 删除激活服务器后 bootServer 未被调用（死代码守卫 if(active!==id)return 在重指派后恒真）", JSON.stringify(a8));
  ok(a8.meAdmin === true && a8.active !== before.active, "A8[复现] 会话滞留：tag 已挂到新服务器但 me 仍是被删服务器的身份（面板与真实会话错位）",
    JSON.stringify({ before, ...a8 }));

  // ---------- A9: 双实例作用域 ----------
  await page.evaluate(() => { WB.showView("admin"); });
  await page.evaluate(() => WB.showTab("servers"));
  await sleep(400);
  const a9 = await page.evaluate(() => {
    const p1 = document.querySelector("#block-settings .servers-panel");
    const p2 = document.querySelector("#block-servers .servers-panel");
    if (!p1 || !p2) return { both: false };
    p2.querySelector(".srv-name").value = "P2-ONLY";
    p2.querySelector(".srv-url").value = "https://p2.example:8080";
    return {
      both: true,
      p1NameUntouched: p1.querySelector(".srv-name").value === "",
      p2NameSet: p2.querySelector(".srv-name").value === "P2-ONLY",
      distinctTables: p1.querySelector("table") !== p2.querySelector("table"),
    };
  });
  ok(a9.both && a9.distinctTables && a9.p1NameUntouched && a9.p2NameSet, "A9 双 .servers-panel 实例独立（DOM/输入互不串扰）", JSON.stringify(a9));
  // admin 面板添加 → 只清 admin 自己的输入框
  await page.click(ADMIN_PANEL + " .srv-add");
  await sleep(300);
  const a9b = await page.evaluate(() => {
    const p2 = document.querySelector("#block-servers .servers-panel");
    const vals = [...p2.querySelectorAll(".srv-add-row input")].map((i) => i.value);
    return { cleared: vals.every((v) => v === ""), rowAdded: WB.state.servers.some((s) => s.url === "https://p2.example:8080") };
  });
  ok(a9b.cleared && a9b.rowAdded, "A9b admin 面板添加走独立容器闭包，行正常新增", JSON.stringify(a9b));

  // ---------- A11: 累计孤儿 token（含 bootServer 回退清表后遗留） ----------
  const a11 = await page.evaluate(() => {
    const keys = Object.keys(WB.state.tokens);
    const ids = new Set(WB.state.servers.map((s) => s.id));
    return { orphans: keys.filter((k) => !ids.has(k)) };
  });
  console.log(`INFO  A11 累计孤儿 token keys: ${JSON.stringify(a11.orphans)}`);

  // 卫生
  ok(problems.length === 0, "H 全程无 pageerror / 4xx5xx（404 容忍）", problems.slice(0, 5).join(" | ") || "none");

  await browser.close();
  const fail = results.filter((r) => !r.pass).length;
  console.log(`\n===== ATTACK: PASS ${results.length - fail}/${results.length} (含 [复现] 项=缺陷确认) =====`);
  if (problems.length) { console.log("PROBLEMS:"); problems.slice(0, 10).forEach((p) => console.log("  - " + p)); }
  process.exit(0);
}

main().catch((e) => {
  console.error("ATTACK FATAL:", e);
  process.exit(2);
}).finally(() => {
  if (serverProc) { try { serverProc.kill("SIGTERM"); } catch (_) {} }
});
