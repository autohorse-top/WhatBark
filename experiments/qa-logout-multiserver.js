#!/usr/bin/env node
/**
 * Red-team probe for logout-clear-token change (web/core.js L292-303).
 * Covers paths NOT covered by scratch-headless/qa-settings-cleanup.js:
 *   M1 real second server (in state.servers) with own token, same hermetic backend
 *   M2 switch to server2 -> auto-login (token preserved)
 *   M3 logout on server2 -> only server2 token cleared; server1 (local) token kept
 *   M4 switch back to server1 -> auto-login again (regression: switch-back after logout)
 *   M5 logout on server1 -> login screen; refresh -> stays on login (no re-login)
 *   M6 401 in-flight -> apiCatch -> logout(): idempotent, no recursion blowup, no pageerror
 *   M7 double logout() call -> idempotent, no crash
 */
const fs = require("fs");
const path = require("path");
const { spawn } = require("child_process");
const puppeteer = require("/Users/x/work/whatbark/scratch-headless/node_modules/puppeteer-core");

const ROOT = "/Users/x/work/whatbark";
const BIN = path.join(ROOT, "bin", "whatbark");
const PORT = 18097;
const BASE = "http://127.0.0.1:" + PORT;
const CHROME = process.env.CHROME || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const ADM = "qasetup123";
const QA_ROOT = "/var/folders/7w/qmcknsz54cx8wdmrr457vy7r0000gn/T/opencode/qa-logout-multiserver";
const DB = path.join(QA_ROOT, "qa.db");
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failCount = 0, serverProc = null;
const problems = [];

function ok(cond, label, extra) {
  const pass = !!cond;
  if (!pass) failCount++;
  console.log(`${pass ? "PASS" : "FAIL"}  ${label}${extra ? "  [" + JSON.stringify(extra) + "]" : ""}`);
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
    try { const r = await fetch(BASE + "/healthz"); if (r.status === 200) return; } catch (_) {}
    await sleep(300);
  }
  throw new Error("hermetic server not ready");
}

async function main() {
  await startServer();
  const browser = await puppeteer.launch({ executablePath: CHROME, headless: "new", args: ["--no-sandbox", "--disable-setuid-sandbox"] });
  const page = await browser.newPage();
  page.on("pageerror", (e) => problems.push("pageerror: " + e.message));
  await page.goto(BASE + "/", { waitUntil: "networkidle2" });
  await page.waitForSelector("#login-token", { timeout: 15000 });

  // M1: add a REAL second server (same backend, id s-srv2) with the admin token.
  await page.evaluate(() => {
    const st = WB.state;
    st.servers.push({ id: "s-srv2", name: "srv2", url: WB.serverURL() });
    st.tokens["s-srv2"] = "qasetup123";
    st.tokens[st.activeServer] = "qasetup123"; // make sure local has explicit token
    WB.saveServers(); WB.saveTokens(); WB.saveActive();
  });
  const ids = await page.evaluate(() => WB.state.servers.map(s => s.id));
  ok(ids.includes("s-srv2") && ids.length === 2, "M1 第二服务器加入 state.servers", ids);

  // M2: switch to srv2 -> bootServer auto-login via stored token
  await page.evaluate(() => switchServer("s-srv2"));
  await page.waitForFunction(() => WB.state.me && WB.state.me.admin, { timeout: 15000 });
  ok(await page.evaluate(() => WB.state.activeServer === "s-srv2"), "M2 切换到 srv2 自动登录成功");

  // M3: logout on srv2 -> only srv2 token cleared; local token kept
  await page.evaluate(() => WB.logout());
  const afterM3 = await page.evaluate(() => ({
    me: WB.state.me === null ? "null" : "set",
    localTok: WB.state.tokens["local"] || "",
    srv2Tok: WB.state.tokens["s-srv2"] || "",
    lsLocal: ((JSON.parse(localStorage.getItem("wb_tokens") || "{}"))["local"]) || "",
    lsSrv2: ((JSON.parse(localStorage.getItem("wb_tokens") || "{}"))["s-srv2"]) || "",
  }));
  ok(afterM3.me === "null" && afterM3.srv2Tok === "" && afterM3.lsSrv2 === "",
    "M3 登出清 srv2 token（内存+localStorage）", afterM3);
  ok(afterM3.localTok === "qasetup123" && afterM3.lsLocal === "qasetup123",
    "M3 local 服务器 token 保留", afterM3);

  // M4: switch back to local -> auto-login again (regression path)
  await page.evaluate(() => switchServer("local"));
  await page.waitForFunction(() => WB.state.me && WB.state.me.admin, { timeout: 15000 });
  ok(await page.evaluate(() => WB.state.activeServer === "local" && WB.state.me && WB.state.me.admin),
    "M4 登出后切回 local 自动重登成功（token 保留路径回归）");

  // M5: logout on local, refresh -> stays on login, no re-login
  await page.evaluate(() => WB.logout());
  await page.reload({ waitUntil: "networkidle2" });
  await page.waitForSelector("#login-token", { timeout: 20000 });
  const afterM5 = await page.evaluate(() => ({
    me: WB.state.me === null || WB.state.me === undefined ? "null" : "set",
    loginActive: document.getElementById("view-login").classList.contains("active"),
  }));
  ok(afterM5.me === "null" && afterM5.loginActive, "M5 登出 local 后刷新停在登录屏", afterM5);

  // M6: 401 in-flight -> apiCatch -> logout(): no recursion blowup / pageerror
  await page.type("#login-token", ADM);
  await page.click("#login-btn");
  await page.waitForFunction(() => WB.state.me && WB.state.me.admin, { timeout: 15000 });
  const m6 = await page.evaluate(async () => {
    const aid = WB.state.activeServer;
    WB.state.tokens[aid] = "bogus-token-401";
    WB.saveTokens();
    // in-flight request carrying the bogus token -> 401 -> (block catch) -> apiCatch -> logout()
    const err = await WB.api("GET", "/api/me").catch((e) => e);
    if (!err || err.status !== 401) return { err: "expected 401, got " + (err && err.status) };
    WB.apiCatch(err, "test-401");
    return {
      me: WB.state.me === null ? "null" : "set",
      tok: WB.state.tokens[aid] || "",
      lsTok: (JSON.parse(localStorage.getItem("wb_tokens") || "{}")[aid]) || "",
      loginActive: document.getElementById("view-login").classList.contains("active"),
    };
  });
  ok(m6.me === "null" && m6.tok === "" && m6.lsTok === "" && m6.loginActive,
    "M6 401→apiCatch→logout 幂等清 token 停登录屏", m6);
  await sleep(500); // let any (buggy) delayed recursion surface
  const m6b = await page.evaluate(() => ({ me: WB.state.me === null ? "null" : "set" }));
  ok(m6b.me === "null", "M6b 无延迟递归把 me 复活", m6b);

  // M7: double logout idempotency
  await page.evaluate(() => { WB.logout(); WB.logout(); });
  const m7 = await page.evaluate(() => ({
    me: WB.state.me === null ? "null" : "set",
    loginActive: document.getElementById("view-login").classList.contains("active"),
    loginInputs: document.querySelectorAll("#login-token").length,
  }));
  ok(m7.me === "null" && m7.loginActive && m7.loginInputs === 1, "M7 双重 logout 幂等（无重复 DOM/崩溃）", m7);

  // hygiene
  ok(problems.length === 0, "M8 无 pageerror", problems.slice(0, 3));

  await browser.close();
  console.log(`\n===== QA LOGOUT MULTISERVER: FAIL ${failCount} =====`);
  if (serverProc) {
    serverProc.kill("SIGTERM");
    await new Promise((res) => {
      const t = setTimeout(() => { try { serverProc.kill("SIGKILL"); } catch (_) {} res(); }, 3000);
      serverProc.on("exit", () => { clearTimeout(t); res(); });
    });
  }
  process.exit(failCount ? 1 : 0);
}

main().catch((e) => {
  console.error("QA FATAL:", e);
  try { if (serverProc) serverProc.kill("SIGKILL"); } catch (_) {}
  process.exit(2);
});
