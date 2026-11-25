#!/usr/bin/env node
/**
 * 红队探针：设置页服务器渐进披露 diff 的边界攻击（review 2026-11-06）。
 * hermetic：临时 :18199 + 临时 db，不碰 demo.db。
 *  E1 servers 截空（length===0）→ 折叠态不崩，显示空摘要，可展开为完整面板
 *  E2 展开按钮双击 → 不产生重复卡片/重复面板，无 pageerror
 *  E3 2→1：删掉非激活服务器后切走再切回设置 → 按规格折叠（行为观察，非失败）
 *  用法：export CHROME=... && NODE_PATH=scratch-headless/node_modules node experiments/review-servers-collapse-probe.js
 */
const fs = require("fs");
const path = require("path");
const { spawn } = require("child_process");
const ROOT = path.join(__dirname, "..");
const puppeteer = require(path.join(ROOT, "scratch-headless", "node_modules", "puppeteer-core"));
const BIN = path.join(ROOT, "bin", "whatbark");
const PORT = 18199;
const BASE = "http://127.0.0.1:" + PORT;
const CHROME = process.env.CHROME || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const ADM = "probeadm123";
const QA_ROOT = "/var/folders/7w/qmcknsz54cx8wdmrr457vy7r0000gn/T/opencode/review-servers-collapse";
const DB = path.join(QA_ROOT, "probe.db");
const S = "#block-settings";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const results = [];
const problems = [];
let failCount = 0;
let serverProc = null;

function ok(cond, label, extra) {
  const pass = !!cond;
  results.push({ pass, label, extra });
  if (!pass) failCount++;
  console.log(`${pass ? "PASS" : "FAIL"}  ${label}${extra ? "  [" + extra + "]" : ""}`);
}

function attachListeners(page, tag) {
  page.on("pageerror", (e) => {
    problems.push(`${tag} pageerror: ${e.message}`);
    console.log(`  ! ${tag} pageerror: ${e.message}`);
  });
  page.on("console", (m) => {
    if (m.type() !== "error") return;
    const t = m.text();
    if (/^Failed to load resource.*status of (400|403|404|409)/.test(t)) return;
    problems.push(`${tag} console: ${t}`);
  });
  page.on("response", (r) => {
    if (r.status() >= 400 && r.status() !== 404) {
      problems.push(`${tag} http ${r.status()} ${r.url()}`);
      console.log(`  ! ${tag} http ${r.status()} ${r.url()}`);
    }
  });
}

async function startServer() {
  fs.rmSync(QA_ROOT, { recursive: true, force: true });
  fs.mkdirSync(QA_ROOT, { recursive: true });
  const log = fs.openSync(path.join(QA_ROOT, "serve.log"), "w");
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

async function login(page) {
  await page.waitForFunction(() =>
    (WB.state.me && WB.state.me.admin) || !!document.querySelector("#login-token"),
    { timeout: 15000 });
  if (await page.$("#login-token")) {
    await page.type("#login-token", ADM);
    await page.click("#login-btn");
  }
  await page.waitForFunction(() => WB.state.me && WB.state.me.admin, { timeout: 15000 });
}

async function main() {
  await startServer();
  const browser = await puppeteer.launch({
    executablePath: CHROME,
    headless: "new",
    args: ["--no-sandbox", "--disable-setuid-sandbox"],
  });

  // ---------- E1: servers 截空 → 折叠态安全 ----------
  {
    const page = await browser.newPage();
    await page.setViewport({ width: 1280, height: 900 });
    attachListeners(page, "E1");
    await page.goto(BASE + "/", { waitUntil: "networkidle2" });
    await login(page);
    // 运行时把 servers 截空（绕过 init 的 local 种子，模拟极端本地状态）
    await page.evaluate(() => { WB.state.servers.length = 0; });
    await page.evaluate(() => WB.showView("settings"));
    await sleep(400);
    const e1 = await page.evaluate((s) => {
      const card = [...document.querySelectorAll(s + " .card")].find((c) => c.querySelector("h3") && c.querySelector("h3").textContent.trim() === "服务器");
      return {
        hasTable: !!document.querySelector(s + " .servers-panel table"),
        hasExpand: !!document.querySelector(s + " #srv-expand"),
        cardText: card ? card.textContent : "(no card)",
      };
    }, S);
    ok(e1.hasExpand && !e1.hasTable, "E1 空列表：折叠态不崩，显示管理按钮", JSON.stringify(e1));
    // 展开 → 完整面板（空表 + 添加行），不崩
    await page.click(S + " #srv-expand");
    await sleep(400);
    const e1b = await page.evaluate((s) => ({
      hasTable: !!document.querySelector(s + " .servers-panel table"),
      hasAddRow: !!document.querySelector(s + " .servers-panel .srv-add-row"),
      rows: document.querySelectorAll(s + " .servers-panel .srv-rows tr").length,
    }), S);
    ok(e1b.hasTable && e1b.hasAddRow && e1b.rows === 0, "E1 空列表：展开后完整面板（0 行 + 添加行）", JSON.stringify(e1b));
    await page.close();
  }

  // ---------- E2: 展开按钮快速双击 ----------
  {
    const page = await browser.newPage();
    await page.setViewport({ width: 1280, height: 900 });
    attachListeners(page, "E2");
    await page.goto(BASE + "/", { waitUntil: "networkidle2" });
    await login(page);
    await page.evaluate(() => WB.showView("settings"));
    await sleep(400);
    // 连续两次触发点击（第二次时按钮可能已脱离 DOM）
    await page.evaluate((s) => {
      const b = document.querySelector(s + " #srv-expand");
      if (b) b.click();
      const b2 = document.querySelector(s + " #srv-expand");
      if (b2) b2.click();
    }, S);
    await sleep(400);
    const e2 = await page.evaluate((s) => ({
      expandBtns: document.querySelectorAll(s + " #srv-expand").length,
      tables: document.querySelectorAll(s + " .servers-panel table").length,
      srvCards: [...document.querySelectorAll(s + " .card")].filter((c) => {
        const h = c.querySelector("h3");
        return h && h.textContent.trim() === "服务器";
      }).length,
      hasAddRow: !!document.querySelector(s + " .servers-panel .srv-add-row"),
    }), S);
    ok(e2.tables === 1 && e2.hasAddRow && e2.expandBtns === 0 && e2.srvCards === 1,
      "E2 双击：无重复卡片/重复面板，按钮消失", JSON.stringify(e2));
    await page.close();
  }

  // ---------- E3: 2→1 删除后切走再切回（规格行为观察） ----------
  {
    const page = await browser.newPage();
    await page.setViewport({ width: 1280, height: 900 });
    attachListeners(page, "E3");
    await page.goto(BASE + "/", { waitUntil: "networkidle2" });
    await login(page);
    await page.evaluate(() => WB.showView("settings"));
    await sleep(300);
    // 确保完整面板（单服务器默认折叠 → 先展开）
    if (await page.$(S + " #srv-expand")) await page.click(S + " #srv-expand");
    await page.waitForSelector(S + " .servers-panel table", { timeout: 10000 });
    // 添加第二台（非激活）
    await page.evaluate((url, s) => {
      document.querySelector(s + " .srv-name").value = "远端";
      document.querySelector(s + " .srv-url").value = url;
      document.querySelector(s + " .srv-token").value = "probe-tok";
      document.querySelector(s + " .srv-add").click();
    }, BASE, S);
    await page.waitForFunction((s) => document.querySelectorAll(s + " .servers-panel .srv-rows tr").length === 2, { timeout: 5000 }, S);
    // 删除非激活（远端）
    await page.evaluate((s) => {
      const rows = [...document.querySelectorAll(s + " .servers-panel .srv-rows tr")];
      const target = rows.find((r) => !r.querySelector(".tag.online"));
      target.querySelector(".srv-del").click();
    }, S);
    await page.waitForFunction((s) => document.querySelectorAll(s + " .servers-panel .srv-rows tr").length === 1, { timeout: 5000 }, S);
    // 切走再切回 → 按规格应折叠（length===1 且从未手动… 注意：本会话已手动展开过 → 保持展开）
    await page.evaluate(() => { WB.showView("chat"); WB.showView("settings"); });
    await sleep(400);
    const e3 = await page.evaluate((s) => ({
      hasTable: !!document.querySelector(s + " .servers-panel table"),
      hasExpand: !!document.querySelector(s + " #srv-expand"),
      nServers: WB.state.servers.length,
    }), S);
    ok(e3.hasTable && !e3.hasExpand, "E3 手动展开过 → 删到 1 台后仍保持完整面板（flag 记忆）", JSON.stringify(e3));
    await page.close();
  }

  // ---------- E4: 从未手动展开、2 台删回 1 台 → 切走切回应折叠（规格 A1 语义） ----------
  {
    const page = await browser.newPage();
    await page.setViewport({ width: 1280, height: 900 });
    attachListeners(page, "E4");
    await page.goto(BASE + "/", { waitUntil: "networkidle2" });
    await login(page);
    // 直接预置第二台（未点过管理按钮 → serversExpanded 必为 false）
    await page.evaluate((url) => {
      WB.state.servers.push({ id: "s_probe2", name: "远端", url: url });
    }, BASE);
    await page.evaluate(() => WB.showView("settings"));
    await sleep(300);
    const base = await page.evaluate((s) => ({
      hasTable: !!document.querySelector(s + " .servers-panel table"),
      hasExpand: !!document.querySelector(s + " #srv-expand"),
      nServers: WB.state.servers.length,
    }), S);
    ok(base.hasTable && !base.hasExpand && base.nServers === 2, "E4 预置 2 台且未手动展开：直接完整面板（A3 路径）", JSON.stringify(base));
    // 删掉非激活（远端）→ 剩 1 台 → 切走切回
    await page.evaluate((s) => {
      const rows = [...document.querySelectorAll(s + " .servers-panel .srv-rows tr")];
      rows.find((r) => !r.querySelector(".tag.online")).querySelector(".srv-del").click();
    }, S);
    await page.waitForFunction((s) => document.querySelectorAll(s + " .servers-panel .srv-rows tr").length === 1, { timeout: 5000 }, S);
    await page.evaluate(() => { WB.showView("chat"); WB.showView("settings"); });
    await sleep(400);
    const e4 = await page.evaluate((s) => ({
      hasTable: !!document.querySelector(s + " .servers-panel table"),
      hasExpand: !!document.querySelector(s + " #srv-expand"),
      nServers: WB.state.servers.length,
    }), S);
    ok(e4.hasExpand && !e4.hasTable && e4.nServers === 1,
      "E4 从未展开 + 删回单台 → 切回折叠（规格 A1 语义）", JSON.stringify(e4));
    await page.close();
  }

  // ---------- 卫生 ----------
  ok(problems.length === 0, "E* 全程无 pageerror / 4xx5xx", problems.slice(0, 3).join(" | "));

  await browser.close();
  console.log(`\n===== PROBE SERVERS COLLAPSE: PASS ${results.length - failCount}/${results.length} FAIL ${failCount} =====`);
  console.log(problems.length ? "PROBLEMS=" + problems.length : "PROBLEMS=none");

  if (serverProc) {
    serverProc.kill("SIGTERM");
    await new Promise((res) => {
      const killTimer = setTimeout(() => { try { serverProc.kill("SIGKILL"); } catch (_) {} res(); }, 3000);
      serverProc.on("exit", () => { clearTimeout(killTimer); res(); });
    });
  }
  process.exit(failCount ? 1 : 0);
}

main().catch((e) => {
  console.error("PROBE FATAL:", e);
  try { if (serverProc) serverProc.kill("SIGKILL"); } catch (_) {}
  process.exit(2);
});
