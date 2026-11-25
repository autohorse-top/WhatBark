#!/usr/bin/env bash
# whatbark S5 end-to-end smoke — machine-ruled acceptance for A1-A11 (+A14, +S12, +S14).
#
# Brings up a real `whatbark serve` (scratch port + scratch SQLite DB),
# spawns real `whatbark agent` processes (built-in mock tool / shell tools),
# drives the REST API and the `bark` CLI, and asserts every acceptance
# criterion in TASK.md section 2.
#
# Usage:  bash scripts/smoke.sh
# Exit:   0 = all checks passed, 1 = any check failed.
#
# Notes:
#   - bash 3.2 compatible (macOS default bash); deps: go, jq, sqlite3, curl.
#   - A7's WS event assertions live in internal/api tests; per TASK.md the
#     smoke falls back to API polling to confirm terminal states arrive.

set -uo pipefail
export LC_ALL=C

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

PASS=0
FAIL=0

pass() { PASS=$((PASS + 1)); printf 'PASS  %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf 'FAIL  %s\n' "$1"; }
die()  { printf 'ERROR %s\n' "$1" >&2; exit 1; }
section() { printf '\n--- %s\n' "$1"; }

PORT=18123
URL="http://127.0.0.1:${PORT}"
TMP="$(mktemp -d)"
DB="$TMP/smoke.db"
BIN="$TMP/wb"
ADMIN="smoke-admin-$$-$RANDOM"

SERVER_PID=""
SERVER2_PID=""
A_L=""
A_M=""
A_W=""
A_W2=""

# stop_pid: SIGTERM, brief grace, SIGKILL, then reap.
stop_pid() {
  local p="$1" i
  [ -n "$p" ] || return 0
  kill "$p" 2>"$TMP/null"
  for i in 1 2 3 4 5 6 7 8 9 10; do
    kill -0 "$p" 2>"$TMP/null" || break
    sleep 0.2
  done
  kill -9 "$p" 2>"$TMP/null"
  wait "$p" 2>"$TMP/null"
}

cleanup() {
  local p
  for p in "$A_L" "$A_M" "$A_W" "$A_W2" "$SERVER_PID" "$SERVER2_PID"; do
    stop_pid "$p"
  done
  rm -rf "$TMP"
}
trap cleanup EXIT

# req METHOD PATH CRED [JSON_BODY] -> prints HTTP status; body -> $TMP/last
req() {
  local m="$1" p="$2" a="$3" b="${4:-}"
  local args=(-s -X "$m" -o "$TMP/last" -w '%{http_code}')
  [ -n "$a" ] && args+=(-H "Authorization: Bearer $a")
  [ -n "$b" ] && args+=(-H "Content-Type: application/json" -d "$b")
  args+=("$URL$p")
  curl "${args[@]}"
}

jqget()  { jq -r "$1" "$TMP/last"; }
jqtest() { jq -e "$@" "$TMP/last" >"$TMP/null" 2>&1; }

check_http() { # desc code want
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (got http $2, want $3)"; fi
}

# wait_task ID STATUS TIMEOUT_SEC — poll until STATUS; fail fast on bad terminal.
wait_task() {
  local id="$1" want="$2" timeout="$3" i code s
  for i in $(seq 1 $((timeout * 2))); do
    code=$(req GET "/api/tasks/$id" "$ADMIN")
    s=$(jqget '.status')
    [ "$s" = "$want" ] && return 0
    if [ "$want" = "done" ] && { [ "$s" = "failed" ] || [ "$s" = "canceled" ]; }; then
      return 1
    fi
    sleep 0.5
  done
  return 1
}

# start_agent CRED LOGNAME — spawn an agent with fast intervals; echoes PID.
start_agent() {
  "$BIN" agent -server "$URL" -token "$1" \
    -poll 300ms -heartbeat 2s -cancel-poll 200ms -mock-sleep 100ms \
    -base-dir "$TMP/work" > "$TMP/$2.log" 2>&1 &
  echo $!
}

# wait_online TIMEOUT ID... — all listed AI employees report online.
wait_online() {
  local timeout="$1"
  shift
  local deadline=$(( $(date +%s) + timeout ))
  local id ok st
  while [ "$(date +%s)" -lt "$deadline" ]; do
    req GET /api/employees "$ADMIN" >"$TMP/null"
    ok=1
    for id in "$@"; do
      st=$(jq --argjson x "$id" -r '.employees[] | select(.id == $x) | .status' "$TMP/last")
      [ "$st" = "online" ] || { ok=0; break; }
    done
    [ "$ok" = 1 ] && return 0
    sleep 0.5
  done
  return 1
}

# wait_offline TIMEOUT ID — the employee reports offline.
wait_offline() {
  local timeout="$1"
  local id="$2"
  local deadline=$(( $(date +%s) + timeout ))
  local st
  while [ "$(date +%s)" -lt "$deadline" ]; do
    req GET /api/employees "$ADMIN" >"$TMP/null"
    st=$(jq --argjson x "$id" -r '.employees[] | select(.id == $x) | .status' "$TMP/last")
    [ "$st" = "offline" ] && return 0
    sleep 1
  done
  return 1
}

# bark CRED ARGS... — run the bark CLI against $URL.
bark() {
  local cred="$1"
  shift
  "$BIN" bark -server "$URL" -token "$cred" "$@"
}

# new_emp NAME ROLE KIND DEPTID -> prints "id cred" on success, 1 on failure.
new_emp() {
  local code
  code=$(req POST /api/employees "$ADMIN" \
    "{\"name\":\"$1\",\"role\":\"$2\",\"kind\":\"$3\",\"department_id\":$4}")
  if [ "$code" != "201" ]; then
    echo "emp create failed ($1): $(cat "$TMP/last")"
    return 1
  fi
  printf '%s %s\n' "$(jqget .id)" "$(jqget .token)"
}

section "0. preflight + build"
for c in go jq sqlite3 curl; do
  command -v "$c" >"$TMP/null" 2>&1 || die "required tool missing: $c"
done
( cd "$ROOT" && go build -o "$BIN" . ) >"$TMP/build.log" 2>&1 \
  || { cat "$TMP/build.log"; die "go build failed"; }
pass "build: single binary compiles (go build .)"

section "1. serve startup"
printf 'serve: %s  db: %s\n' "$URL" "$DB"
"$BIN" serve -listen "127.0.0.1:$PORT" -data "$DB" -admin-token "$ADMIN" -tick 1s \
  > "$TMP/serve.log" 2>&1 &
SERVER_PID=$!
up=""
i=0
while [ -z "$up" ] && [ $i -lt 40 ]; do
  code=$(curl -s -o "$TMP/last" -w '%{http_code}' "$URL/healthz" 2>"$TMP/null") || true
  [ "$code" = "200" ] && up=1
  i=$((i + 1))
  sleep 0.5
done
[ -n "$up" ] || { tail -20 "$TMP/serve.log"; die "server did not come up"; }
pass "serve: /healthz 200"

section "2. A11 — SPA + frontend assets"
code=$(curl -s -o "$TMP/last" -w '%{http_code}' "$URL/")
check_http "A11: GET / serves SPA (200)" "$code" 200
for blk in org wflow projects status inbox sched; do
  if grep -q "data-block=\"$blk\"" "$TMP/last"; then
    pass "A11: SPA block present: $blk"
  else
    fail "A11: SPA block missing: $blk"
  fi
done
for asset in core.js style.css blocks-org.js blocks-wflow.js blocks-projects.js \
             blocks-status.js blocks-inbox.js blocks-servers.js blocks-sched.js; do
  code=$(curl -s -o "$TMP/last" -w '%{http_code}' "$URL/$asset")
  if [ "$code" = "200" ]; then
    pass "A11: asset /$asset 200"
  else
    fail "A11: asset /$asset http $code"
  fi
done
code=$(curl -s -o "$TMP/core.js" -w '%{http_code}' "$URL/core.js")
if [ "$code" = "200" ] && grep -q "localStorage" "$TMP/core.js" \
     && grep -q "activeServer" "$TMP/core.js"; then
  pass "A11: core.js persists + switches servers (localStorage, activeServer)"
else
  fail "A11: core.js missing server persistence/switch logic"
fi
code=$(curl -s -o "$TMP/servers.js" -w '%{http_code}' "$URL/blocks-servers.js")
if [ "$code" = "200" ] && grep -q "addServer" "$TMP/servers.js" \
     && grep -q "removeServer" "$TMP/servers.js"; then
  pass "A11: servers block add/remove logic present"
else
  fail "A11: servers block add/remove logic missing"
fi
code=$(curl -s -o "$TMP/inbox.js" -w '%{http_code}' "$URL/blocks-inbox.js")
if [ "$code" = "200" ] && grep -q "data-reply" "$TMP/inbox.js" \
     && grep -q "data-finish" "$TMP/inbox.js" && grep -q "data-fail" "$TMP/inbox.js"; then
  pass "A11: human inbox has reply/finish/reject controls"
else
  fail "A11: inbox reply/finish/reject controls missing"
fi

section "3. org scaffolding (depts A/B; L/M/W ai, H human)"
code=$(req POST /api/departments "$ADMIN" '{"name":"eng-root"}')
check_http "org: root department created" "$code" 201
DEPA=$(jqget '.id')
code=$(req POST /api/departments "$ADMIN" "{\"name\":\"eng-dev\",\"parent_id\":$DEPA}")
check_http "org: child department created (parent A)" "$code" 201
DEPB=$(jqget '.id')

out=$(new_emp "lead-a" leader ai "$DEPA")       || die "create lead-a: $out"
L=${out%% *};  L_CRED=${out#* }
out=$(new_emp "lead-b" leader ai "$DEPB")       || die "create lead-b: $out"
M=${out%% *};  M_CRED=${out#* }
out=$(new_emp "worker-1" engineer ai "$DEPB")   || die "create worker-1: $out"
W=${out%% *};  W_CRED=${out#* }
out=$(new_emp "human-1" tester human "$DEPB")   || die "create human-1: $out"
H=${out%% *};  H_CRED=${out#* }

code=$(req PUT "/api/departments/$DEPA/leader" "$ADMIN" "{\"employee_id\":$L}")
check_http "org: department A leader = L" "$code" 200
code=$(req PUT "/api/departments/$DEPB/leader" "$ADMIN" "{\"employee_id\":$M}")
check_http "org: department B leader = M" "$code" 200

section "4. A2 — credential + auth"
code=$(req GET /api/employees "")
check_http "A2: no credential -> 401" "$code" 401
code=$(req GET /api/employees "bogus-credential")
check_http "A2: wrong credential -> 401" "$code" 401
out=$("$BIN" bark -server "$URL" -token "$W_CRED" status 2>&1)
name=$(printf '%s' "$out" | jq -r '.employee.name' 2>"$TMP/null")
if [ "$name" = "worker-1" ]; then
  pass "A2: bark CLI works with employee credential (status -> own identity)"
else
  fail "A2: bark status unexpected: $out"
fi
code=$(req GET /api/employees "$ADMIN")
if grep -qF "$L_CRED" "$TMP/last"; then
  fail "A2: plaintext credential leaked in employee list"
else
  pass "A2: plaintext credential returned once only (absent from list)"
fi

section "5. A14 — human identity + least privilege"
code=$(req GET /api/me "$H_CRED")
check_http "A14: /api/me with human credential -> 200" "$code" 200
k=$(jqget '.employee.kind'); n=$(jqget '.employee.name'); r=$(jqget '.employee.role')
if [ "$k" = "human" ] && [ "$n" = "human-1" ] && [ "$r" = "tester" ]; then
  pass "A14: /api/me returns own identity (kind/name/role)"
else
  fail "A14: /api/me identity wrong (kind=$k name=$n role=$r)"
fi
code=$(req POST /api/departments "$H_CRED" '{"name":"nope"}')
check_http "A14: human credential on admin endpoint -> 403" "$code" 403
code=$(req POST /api/departments "$ADMIN" '{"name":"eng-qa"}')
check_http "A14: admin credential on same endpoint -> 201" "$code" 201

section "6. A1 — org-derived routes"
code=$(req GET "/api/routes?employee_id=$W" "$ADMIN")
check_http "A1: routes endpoint for W -> 200" "$code" 200
if jqtest --argjson l "$L" --argjson m "$M" \
    '(.reporting == [$m, $l]) and (.dispatch == []) and (.chat == [])'; then
  pass "A1: W routes match org topology (reporting M,L; no dispatch; no chat for non-leader)"
else
  fail "A1: W routes unexpected: $(cat "$TMP/last")"
fi
code=$(req GET "/api/routes?employee_id=$L" "$ADMIN")
if jqtest --argjson m "$M" --argjson w "$W" --argjson h "$H" \
    '(.reporting == []) and (.dispatch == [$m, $w, $h]) and (.chat == [$m])'; then
  pass "A1: L routes match org topology (dispatch M/W/H; chat M only)"
else
  fail "A1: L routes unexpected: $(cat "$TMP/last")"
fi

section "7. A10 — route enforcement (before any workflow exists)"
if out=$(bark "$W_CRED" send --to "$L" --kind dispatch --body "must be rejected" 2>&1); then
  fail "A10: W->L dispatch unexpectedly allowed: $out"
else
  case "$out" in
    *403*) pass "A10: W->L dispatch rejected 403 (outside topology)" ;;
    *)     fail "A10: W->L dispatch rejected with non-403: $out" ;;
  esac
fi
if out=$(bark "$W_CRED" send --to "$H" --kind chat --body "hello" 2>&1); then
  fail "A10: W(non-leader)->H chat unexpectedly allowed: $out"
else
  case "$out" in
    *403*) pass "A10: W->H chat rejected 403 (non-leader cannot chat)" ;;
    *)     fail "A10: W->H chat rejected with non-403: $out" ;;
  esac
fi
out=$(bark "$L_CRED" send --to "$M" --kind chat --body "check-in" 2>&1)
mid=$(printf '%s' "$out" | jq -r '.message_id' 2>"$TMP/null")
if [ -n "$mid" ] && [ "$mid" != "null" ]; then
  pass "A10: leader->leader chat allowed (message $mid)"
else
  fail "A10: L->M chat failed: $out"
fi

section "8. A3 — agent attach / online"
A_L=$(start_agent "$L_CRED" agent-L)
A_M=$(start_agent "$M_CRED" agent-M)
A_W=$(start_agent "$W_CRED" agent-W)
if wait_online 20 "$L" "$M" "$W"; then
  pass "A3: agents L/M/W attach and report online"
else
  fail "A3: agents not online within 20s (see agent-*.log)"
fi

section "9. A4 — full project loop (mock agents + bark + human API)"
code=$(req POST /api/projects "$ADMIN" \
  "{\"name\":\"smoke-proj\",\"brief\":\"build the smoke feature end to end\",\"entry_employee_id\":$L}")
check_http "A4: project created with entry L" "$code" 201
P=$(jqget '.id')

seedst=""
i=0
while [ $i -lt 60 ]; do
  code=$(req GET "/api/projects/$P" "$ADMIN")
  seedst=$(jq --argjson l "$L" -r '[.tasks[] | select(.employee_id == $l)][0].status // "none"' "$TMP/last")
  case "$seedst" in done|failed|canceled) break ;; esac
  i=$((i + 1))
  sleep 0.5
done
if [ "$seedst" = "done" ]; then
  pass "A4: seed brief executed by leader L (mock agent)"
else
  fail "A4: seed task ended '$seedst' (want done)"
fi

out=$(bark "$L_CRED" send --to "$W" --kind dispatch --body "implement the smoke feature" --project "$P" 2>&1)
TW=$(printf '%s' "$out" | jq -r '.task_id' 2>"$TMP/null")
if [ -z "$TW" ] || [ "$TW" = "null" ]; then die "A4: L->W dispatch failed: $out"; fi
if wait_task "$TW" done 60; then
  pass "A4: worker W completed the dispatched task"
else
  fail "A4: W did not complete task $TW (state: $(jqget '.status'))"
fi

out=$(bark "$L_CRED" send --to "$H" --kind ask --body "please verify the smoke output" --project "$P" 2>&1)
TH=$(printf '%s' "$out" | jq -r '.task_id' 2>"$TMP/null")
if [ -z "$TH" ] || [ "$TH" = "null" ]; then die "A4: L->H ask failed: $out"; fi
# State machine: queued -> running -> done; humans start, then finish.
code=$(req POST "/api/tasks/$TH/start" "$H_CRED")
check_http "A4: H started the ask task via API" "$code" 200
code=$(req POST "/api/tasks/$TH/finish" "$H_CRED" '{"output":"verified ok"}')
check_http "A4: H closed the ask task via API" "$code" 200

pdone=0
i=0
while [ $i -lt 40 ]; do
  code=$(req GET "/api/projects/$P" "$ADMIN")
  if jqtest '(.tasks | length >= 3) and ([.tasks[].status] | all(. == "done"))'; then
    pdone=1
    break
  fi
  i=$((i + 1))
  sleep 0.5
done
if [ "$pdone" = 1 ]; then
  pass "A4: GET /api/projects/$P shows all tasks done"
else
  fail "A4: project tasks not all done: $(jq -c '[.tasks[] | {id, kind, status}]' "$TMP/last")"
fi

code=$(req GET "/api/messages?project=$P&limit=200" "$ADMIN")
if jqtest '([.messages[].kind] | index("dispatch") != null and index("ask") != null and index("report") != null and index("answer") != null)'; then
  pass "A4: project message chain has dispatch/ask/report/answer"
else
  fail "A4: project message chain incomplete: $(jq -c '[.messages[].kind]' "$TMP/last")"
fi

section "10. A3 — agent detach / offline"
stop_pid "$A_W"
A_W=""
if wait_offline 20 "$W"; then
  pass "A3: after agent process stops, W is marked offline"
else
  fail "A3: W still not offline after 20s"
fi

section "11. W2 setup (script tools for A5/A6)"
out=$(new_emp "worker-slow" engineer ai "$DEPB") || die "create worker-slow failed"
W2=${out%% *}; W2_CRED=${out#* }
cat > "$TMP/tool-slow.sh" <<'TOOL'
#!/bin/sh
echo "tool-slow: starting long job"
sleep 77
echo "tool-slow: done"
TOOL
chmod +x "$TMP/tool-slow.sh"
cat > "$TMP/tool-2s.sh" <<'TOOL'
#!/bin/sh
echo "tool-2s: tick"
sleep 2
echo "tool-2s: done"
TOOL
chmod +x "$TMP/tool-2s.sh"
A_W2=$(start_agent "$W2_CRED" agent-W2)
if wait_online 20 "$W2"; then
  pass "A3: W2 agent attaches and reports online"
else
  fail "A3: W2 agent not online within 20s"
fi
# Drain W2's initial selfreview before the tool switch in A5: wait until the
# scheduler-injected selfreview exists and is fully done. This arms the
# selfreview interval timer (so no further selfreview tasks appear during
# A5/A6) and leaves nothing in flight when the agent is restarted below —
# a stale-running task would never be re-claimed and could confuse A5.
drained=0
n=0
a=0
i=0
while [ $i -lt 40 ]; do
  req GET "/api/tasks?employee=$W2" "$ADMIN" >"$TMP/null"
  n=$(jqget '[.tasks[] | select(.kind == "selfreview" and .status == "done")] | length')
  a=$(jqget '[.tasks[] | select(.kind == "selfreview" and (.status == "queued" or .status == "running"))] | length')
  if [ "${n:-0}" -ge 1 ] && [ "${a:-0}" = "0" ]; then
    drained=1
    break
  fi
  i=$((i + 1))
  sleep 0.5
done
if [ "$drained" = 1 ]; then
  pass "A3: W2 initial selfreview drained before tool switch"
else
  fail "A3: W2 initial selfreview not drained (done=${n:-0} active=${a:-0})"
fi

section "12. A5 — cancel a running tool (process-group kill)"
code=$(req PUT "/api/employees/$W2/tool" "$ADMIN" \
  "{\"tool\":\"$TMP/tool-slow.sh\",\"tool_args\":\"\"}")
check_http "A5: W2 tool switched to slow script" "$code" 200
stop_pid "$A_W2"
A_W2=""
A_W2=$(start_agent "$W2_CRED" agent-W2)
if ! wait_online 20 "$W2"; then
  fail "A5: W2 agent not online after tool change"
fi

out=$(bark "$ADMIN" send --to "$W2" --kind dispatch --body "long running job" 2>&1)
T5=$(printf '%s' "$out" | jq -r '.task_id' 2>"$TMP/null")
if [ -z "$T5" ] || [ "$T5" = "null" ]; then
  fail "A5: admin dispatch to W2 failed: $out"
else
  running=0
  s=""
  i=0
  while [ $i -lt 40 ]; do
    code=$(req GET "/api/tasks/$T5" "$ADMIN")
    s=$(jqget '.status')
    [ "$s" = "running" ] && { running=1; break; }
    i=$((i + 1))
    sleep 0.5
  done
  if [ "$running" = 1 ] && pgrep -f "tool-slow" >"$TMP/null" 2>&1; then
    pass "A5: W2 tool running (task running, tool-slow process alive)"
  else
    fail "A5: tool-slow not observed running (status=$s)"
  fi

  out=$(bark "$ADMIN" cancel --reason "smoke cancel" "$T5" 2>&1)
  sleep 3
  code=$(req GET "/api/tasks/$T5" "$ADMIN")
  s=$(jqget '.status')
  if [ "$s" = "canceled" ]; then
    pass "A5: task $T5 status=canceled after bark cancel"
  else
    fail "A5: task $T5 status=$s (want canceled)"
  fi
  if pgrep -f "tool-slow" >"$TMP/null" 2>&1; then
    fail "A5: tool-slow process survived cancel: $(pgrep -fl 'tool-slow' 2>/dev/null)"
  else
    pass "A5: tool process group killed (no tool-slow left)"
  fi
fi

section "13. A6 — strictly serial execution (no task overlap)"
code=$(req PUT "/api/employees/$W2/tool" "$ADMIN" \
  "{\"tool\":\"$TMP/tool-2s.sh\",\"tool_args\":\"\"}")
check_http "A6: W2 tool switched to 2s script" "$code" 200
stop_pid "$A_W2"
A_W2=""
A_W2=$(start_agent "$W2_CRED" agent-W2)
if ! wait_online 20 "$W2"; then
  fail "A6: W2 agent not online after tool change"
fi

out=$(bark "$ADMIN" send --to "$W2" --kind dispatch --body "job 1" 2>&1)
T6=$(printf '%s' "$out" | jq -r '.task_id' 2>"$TMP/null")
out=$(bark "$ADMIN" send --to "$W2" --kind dispatch --body "job 2" 2>&1)
T7=$(printf '%s' "$out" | jq -r '.task_id' 2>"$TMP/null")
if [ -z "$T6" ] || [ "$T6" = "null" ] || [ -z "$T7" ] || [ "$T7" = "null" ]; then
  fail "A6: dispatch T6/T7 to W2 failed"
else
  if wait_task "$T6" done 90 && wait_task "$T7" done 90; then
    pass "A6: both 2s jobs completed"
  else
    fail "A6: T6/T7 did not both reach done"
  fi
  req GET "/api/tasks/$T6" "$ADMIN" >"$TMP/null"
  T6S=$(jqget '.started_at'); T6F=$(jqget '.finished_at')
  req GET "/api/tasks/$T7" "$ADMIN" >"$TMP/null"
  T7S=$(jqget '.started_at'); T7F=$(jqget '.finished_at')
  if [ -n "$T6S" ] && [ -n "$T6F" ] && [ -n "$T7S" ] && [ -n "$T7F" ]; then
    if [[ "$T7S" < "$T6F" ]] && [[ "$T6S" < "$T7F" ]]; then
      fail "A6: task windows overlap (T6 [$T6S..$T6F], T7 [$T7S..$T7F])"
    else
      pass "A6: no execution overlap, strictly serial (T6 [$T6S..$T6F], T7 [$T7S..$T7F])"
    fi
  else
    fail "A6: missing started_at/finished_at (T6S=$T6S T6F=$T6F T7S=$T7S T7F=$T7F)"
  fi
fi

section "14. A8 — selfreview interval via settings (sqlite3 write)"
summary_count() {
  local a b
  req GET "/api/messages?to=$M&limit=200" "$ADMIN" >"$TMP/null"
  a=$(jqget '[.messages[] | select(.kind == "summary")] | length')
  req GET "/api/messages?to=$L&limit=200" "$ADMIN" >"$TMP/null"
  b=$(jqget '[.messages[] | select(.kind == "summary")] | length')
  echo $(( ${a:-0} + ${b:-0} ))
}
S0=$(summary_count)
ok=""
i=0
while [ -z "$ok" ] && [ $i -lt 5 ]; do
  if sqlite3 "$DB" "INSERT INTO settings(key,value) VALUES('selfreview_interval_seconds','1')
ON CONFLICT(key) DO UPDATE SET value='1';" >"$TMP/null" 2>&1; then
    ok=1
  fi
  i=$((i + 1))
  sleep 1
done
[ -n "$ok" ] || fail "A8: sqlite3 write to settings failed (busy?)"
v=$(sqlite3 "$DB" "SELECT value FROM settings WHERE key='selfreview_interval_seconds';" 2>/dev/null)
if [ "$v" = "1" ]; then
  pass "A8: selfreview_interval_seconds read back as 1"
else
  fail "A8: settings readback got '$v' (want 1)"
fi
deadline=$(( $(date +%s) + 60 ))
S1=$S0
while [ "$(date +%s)" -lt "$deadline" ]; do
  S1=$(summary_count)
  [ "$S1" -gt "$S0" ] && break
  sleep 2
done
if [ "$S1" -gt "$S0" ]; then
  pass "A8: new selfreview summary appeared after interval=1s (count $S0 -> $S1)"
else
  fail "A8: no new summary within 60s of interval=1s (count stayed $S0)"
fi

section "15. A4 (final) — leader inbox holds report/answer/summary"
req GET "/api/messages?to=$L&limit=200" "$ADMIN" >"$TMP/null"
if jqtest --argjson w "$W" --argjson p "$P" \
    '[.messages[] | select(.from_id == $w and .kind == "report" and .project_id == $p)] | length > 0'; then
  pass "A4-final: L inbox has W's report (project P)"
else
  fail "A4-final: W->L report missing from L inbox"
fi
if jqtest --argjson h "$H" --argjson p "$P" \
    '[.messages[] | select(.from_id == $h and .kind == "answer" and .project_id == $p)] | length > 0'; then
  pass "A4-final: L inbox has H's answer (project P)"
else
  fail "A4-final: H->L answer missing from L inbox"
fi
if jqtest --argjson m "$M" \
    '[.messages[] | select(.from_id == $m and .kind == "summary")] | length > 0'; then
  pass "A4-final: L inbox has M's selfreview summary"
else
  fail "A4-final: M->L summary missing from L inbox"
fi

section "15b. S8 — conversations / thread / read cursor"
# Uses H (human): the selfreview scheduler (interval=1s since section 14)
# only injects into AI employees, so H's boss conversation stays quiet and
# the unread assertions are exact.
code=$(req POST "/api/messages/read" "$H_CRED" "{\"with\":0}")
check_http "S8: H marks boss conversation read" "$code" 200
code=$(req POST /api/messages "$ADMIN" "{\"to\":$H,\"kind\":\"chat\",\"body\":\"smoke s8 ping\"}")
check_http "S8: boss->H chat accepted" "$code" 201

code=$(req GET /api/conversations "$H_CRED")
unread=$(jqget '.conversations[] | select(.peer_id == 0) | .unread')
if [ "$unread" = "1" ]; then
  pass "S8: H conversation list shows boss unread=1"
else
  fail "S8: H boss-conversation unread=$unread, want 1"
fi

code=$(req GET "/api/messages?with=0" "$H_CRED")
check_http "S8: thread with=0 is 200" "$code" 200
if jqtest '.messages[-1].kind == "chat" and .messages[-1].from_id == null'; then
  pass "S8: thread last msg is boss chat (from_id null)"
else
  fail "S8: thread tail wrong: $(jq -c '.messages[-1]' "$TMP/last")"
fi

code=$(req POST "/api/messages/read" "$H_CRED" "{\"with\":0}")
check_http "S8: mark read returns 200" "$code" 200
code=$(req GET /api/conversations "$H_CRED")
unread=$(jqget '.conversations[] | select(.peer_id == 0) | .unread')
if [ "$unread" = "0" ]; then
  pass "S8: unread cleared after mark-read"
else
  fail "S8: unread after read = $unread, want 0"
fi

code=$(req GET "/api/messages?with=$H" "$H_CRED")
check_http "S8: with=self rejected (400)" "$code" 400
code=$(req GET /api/messages "$H_CRED")
check_http "S8: /api/messages without selector rejected (400)" "$code" 400
code=$(req POST "/api/messages/read" "$H_CRED" "{\"with\":$H}")
check_http "S8: read with=self rejected (400)" "$code" 400

code=$(req GET /api/conversations "$ADMIN")
check_http "S8: boss conversation list 200" "$code" 200
if jqtest --argjson h "$H" '[.conversations[] | select(.peer_id == $h and .name == "human-1")] | length == 1'; then
  pass "S8: boss list resolves H peer identity"
else
  fail "S8: boss list missing H peer: $(jq -c '.conversations' "$TMP/last")"
fi

section "16. A9 — workflow graph persistence across restart"
code=$(req POST /api/workflows "$ADMIN" '{"name":"wf1"}')
check_http "A9: workflow wf1 created" "$code" 201
WF1=$(jqget '.id')
code=$(req POST "/api/workflows/$WF1/nodes" "$ADMIN" "{\"employee_id\":$W,\"x\":10,\"y\":10}")
check_http "A9: wf1 node W added" "$code" 201
NW=$(jqget '.id')
code=$(req POST "/api/workflows/$WF1/nodes" "$ADMIN" "{\"employee_id\":$L,\"x\":120,\"y\":10}")
check_http "A9: wf1 node L added" "$code" 201
NL=$(jqget '.id')
code=$(req POST "/api/workflows/$WF1/nodes" "$ADMIN" "{\"employee_id\":$M,\"x\":230,\"y\":10}")
check_http "A9: wf1 node M added" "$code" 201
NM=$(jqget '.id')
code=$(req POST "/api/workflows/$WF1/edges" "$ADMIN" "{\"from_node_id\":$NW,\"to_node_id\":$NL}")
check_http "A9: wf1 edge W->L added" "$code" 201
code=$(req POST "/api/workflows/$WF1/edges" "$ADMIN" "{\"from_node_id\":$NL,\"to_node_id\":$NM}")
check_http "A9: wf1 edge L->M added" "$code" 201

# wf1's W->L edge now authorizes W->L dispatch (contrast with A10's 403).
out=$(bark "$W_CRED" send --to "$L" --kind dispatch --body "wf1 edge job" 2>&1)
TWFL=$(printf '%s' "$out" | jq -r '.task_id' 2>"$TMP/null")
if [ -n "$TWFL" ] && [ "$TWFL" != "null" ]; then
  pass "A9: W->L dispatch now allowed via workflow edge (task $TWFL)"
else
  fail "A9: W->L dispatch via workflow edge failed: $out"
fi

code=$(req POST /api/workflows "$ADMIN" '{"name":"wf2"}')
check_http "A9: workflow wf2 created" "$code" 201
WF2=$(jqget '.id')
code=$(req POST "/api/workflows/$WF2/nodes" "$ADMIN" "{\"employee_id\":$W,\"x\":10,\"y\":10}")
check_http "A9: wf2 node W added" "$code" 201
code=$(req DELETE "/api/workflows/$WF2" "$ADMIN")
check_http "A9: workflow wf2 deleted" "$code" 200
code=$(req POST /api/workflows "$ADMIN" '{"name":"wf2b"}')
check_http "A9: workflow wf2b recreated" "$code" 201
WF2B=$(jqget '.id')
code=$(req POST "/api/workflows/$WF2B/nodes" "$ADMIN" "{\"employee_id\":$H,\"x\":10,\"y\":10}")
check_http "A9: wf2b node H added" "$code" 201

# Kill the server (SIGKILL — no graceful shutdown), wait for port close.
kill -9 "$SERVER_PID" 2>"$TMP/null"
wait "$SERVER_PID" 2>"$TMP/null"
i=0
while [ $i -lt 40 ]; do
  curl -s -o "$TMP/null" "$URL/healthz" 2>"$TMP/null" || break
  i=$((i + 1))
  sleep 0.5
done

# Restart the same binary + same data dir.
"$BIN" serve -listen "127.0.0.1:$PORT" -data "$DB" -admin-token "$ADMIN" -tick 1s \
  > "$TMP/serve2.log" 2>&1 &
SERVER_PID=$!
up=""
i=0
while [ -z "$up" ] && [ $i -lt 40 ]; do
  code=$(curl -s -o "$TMP/last" -w '%{http_code}' "$URL/healthz" 2>"$TMP/null") || true
  [ "$code" = "200" ] && up=1
  i=$((i + 1))
  sleep 0.5
done
if [ -n "$up" ]; then
  pass "A9: server restarted on same DB"
else
  fail "A9: server did not restart"
fi

code=$(req GET /api/workflows "$ADMIN")
wf_ids=$(jqget '[.workflows[].id]')
if printf '%s' "$wf_ids" | jq -e --argjson a "$WF1" --argjson b "$WF2B" \
    'index($a) != null and index($b) != null' >"$TMP/null" 2>&1 \
   && ! printf '%s' "$wf_ids" | jq -e --argjson x "$WF2" \
      'index($x) != null' >"$TMP/null" 2>&1; then
  pass "A9: after restart wf1/wf2b present, deleted wf2 gone"
else
  fail "A9: workflow list after restart wrong: $wf_ids"
fi
code=$(req GET "/api/workflows/$WF1" "$ADMIN")
if jqtest '(.nodes | length == 3) and (.edges | length == 2)'; then
  pass "A9: wf1 graph intact after restart (3 nodes, 2 edges)"
else
  fail "A9: wf1 graph lost after restart: $(jq -c '{nodes: .nodes, edges: .edges}' "$TMP/last")"
fi

section "17. A7 — terminal states persist across restart"
s5=$(req GET "/api/tasks/$T5" "$ADMIN" >/dev/null; jqget '.status')
if [ "$s5" = "canceled" ]; then
  pass "A7: T5 still canceled after restart"
else
  fail "A7: T5 status=$s5 after restart (want canceled)"
fi
s6=$(req GET "/api/tasks/$T6" "$ADMIN" >/dev/null; jqget '.status')
if [ "$s6" = "done" ]; then
  pass "A7: T6 still done after restart"
else
  fail "A7: T6 status=$s6 after restart (want done)"
fi
s7=$(req GET "/api/tasks/$T7" "$ADMIN" >/dev/null; jqget '.status')
if [ "$s7" = "done" ]; then
  pass "A7: T7 still done after restart"
else
  fail "A7: T7 status=$s7 after restart (want done)"
fi

section "17b. S12 — file upload + avatar"

PNG_B64="iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="
PNGF="$TMP/shot.png"
printf '%s' "$PNG_B64" | ( base64 -D 2>"$TMP/null" || base64 --decode ) > "$PNGF"
[ -s "$PNGF" ] || die "cannot decode smoke png"

# 1) W uploads a file (multipart field "file").
code=$(curl -s -o "$TMP/last" -w '%{http_code}' -X POST \
  -F "file=@$PNGF" -H "Authorization: Bearer $W_CRED" "$URL/api/files")
check_http "S12: W file upload -> 201" "$code" 201
FURL=$(jqget '.url'); FID=$(jqget '.id')
if [ -n "$FURL" ] && [ "$FURL" != "null" ] && [ "$FID" != "null" ]; then
  pass "S12: upload returns url+id ($FURL)"
else
  fail "S12: upload url/id missing: $(cat "$TMP/last")"
fi

# 2) P0-1: fetch back via the signed URL (bare, no client token) -> 200 +
#    byte-identical.
fcode=$(curl -s -o "$TMP/fetch.png" -w '%{http_code}' "$URL$FURL")
if [ "$fcode" = "200" ] && cmp -s "$PNGF" "$TMP/fetch.png"; then
  pass "S12: file fetch via signed URL returns uploaded bytes"
else
  fail "S12: file fetch http $fcode or byte mismatch"
fi

# 3) Plain path without any credentials -> 401 (signed URL still required).
FPLAIN="${FURL%%\?*}"
fcode=$(curl -s -o "$TMP/null" -w '%{http_code}' "$URL$FPLAIN")
check_http "S12: plain path without token -> 401" "$fcode" 401
# 3b) P0-1: legacy ?token= fallback still works (server-side back-compat).
fcode=$(curl -s -o "$TMP/null" -w '%{http_code}' "$URL$FPLAIN?token=$W_CRED")
check_http "S12: legacy query-token fetch still -> 200" "$fcode" 200

# 4) Boss uploads a file too (owner 0).
code=$(curl -s -o "$TMP/last" -w '%{http_code}' -X POST \
  -F "file=@$PNGF" -H "Authorization: Bearer $ADMIN" "$URL/api/files")
check_http "S12: boss file upload -> 201" "$code" 201

# 5) W attaches its own file to a report up the chain (W->M).
code=$(req POST /api/messages "$W_CRED" \
  "{\"to\":$M,\"kind\":\"report\",\"body\":\"s12 attach\",\"file_ids\":[$FID]}")
check_http "S12: W->M report with file_ids -> 201" "$code" 201

# 6) M's thread with W carries the attachment.
code=$(req GET "/api/messages?with=$W" "$M_CRED")
check_http "S12: M thread with=W -> 200" "$code" 200
if jqtest '.messages[-1] | (.kind == "report" and (.files | length == 1) and .files[0].name == "shot.png")'; then
  pass "S12: thread message carries the attached file (shot.png)"
else
  fail "S12: thread tail has no attachment: $(jq -c '.messages[-1]' "$TMP/last")"
fi

# 7) Cross-owner attach is rejected (M cannot attach W's file).
code=$(req POST /api/messages "$M_CRED" \
  "{\"to\":$L,\"kind\":\"chat\",\"body\":\"x\",\"file_ids\":[$FID]}")
check_http "S12: M attaching W's file -> 400" "$code" 400

# 8) W uploads an avatar.
code=$(curl -s -o "$TMP/last" -w '%{http_code}' -X POST \
  -F "avatar=@$PNGF" -H "Authorization: Bearer $W_CRED" "$URL/api/me/avatar")
check_http "S12: W avatar upload -> 200" "$code" 200
AVURL=$(jqget '.avatar')
if [ -n "$AVURL" ] && [ "$AVURL" != "null" ]; then
  pass "S12: avatar upload returns url ($AVURL)"
else
  fail "S12: avatar url missing: $(cat "$TMP/last")"
fi

# 9) /api/me and /api/employees both reflect the avatar.
code=$(req GET /api/me "$W_CRED")
if [ "$code" = "200" ] && [ "$(jqget '.employee.avatar')" = "$AVURL" ]; then
  pass "S12: /api/me reflects W's avatar"
else
  fail "S12: /api/me avatar mismatch (got $(jqget '.employee.avatar'), want $AVURL)"
fi
code=$(req GET /api/employees "$ADMIN")
if jqtest --argjson w "$W" --arg av "$AVURL" \
    '[.employees[] | select(.id == $w and .avatar == $av)] | length == 1'; then
  pass "S12: /api/employees shows W's avatar"
else
  fail "S12: /api/employees avatar missing: $(jq -c '.employees' "$TMP/last")"
fi

# 10) P0-1: avatar fetch via signed URL (bare) returns the uploaded bytes.
fcode=$(curl -s -o "$TMP/ava.png" -w '%{http_code}' "$URL$AVURL")
if [ "$fcode" = "200" ] && cmp -s "$PNGF" "$TMP/ava.png"; then
  pass "S12: avatar fetch via signed URL returns uploaded bytes"
else
  fail "S12: avatar fetch http $fcode or byte mismatch"
fi
# 10b) Legacy query-token fallback still works.
AVPLAIN="${AVURL%%\?*}"
fcode=$(curl -s -o "$TMP/null" -w '%{http_code}' "$URL$AVPLAIN?token=$W_CRED")
check_http "S12: avatar legacy query-token fetch still -> 200" "$fcode" 200

# 11) Boss avatar upload + fetch (P0-1: url now signed, check base path + signed fetch).
code=$(curl -s -o "$TMP/last" -w '%{http_code}' -X POST \
  -F "avatar=@$PNGF" -H "Authorization: Bearer $ADMIN" "$URL/api/me/avatar")
check_http "S12: boss avatar upload -> 200" "$code" 200
BAV=$(jqget '.avatar')
if [ "${BAV%%\?*}" = "/api/avatars/boss.png" ] && [[ "$BAV" == *"sig="* ]]; then
  pass "S12: boss avatar url = signed /api/avatars/boss.png ($BAV)"
else
  fail "S12: boss avatar url unexpected = $BAV"
fi
fcode=$(curl -s -o "$TMP/null" -w '%{http_code}' "$URL$BAV")
check_http "S12: boss avatar fetch via signed URL -> 200" "$fcode" 200

section "17c. S14 — group chat (create / send / thread / list / isolation)"

# Group with L + M; boss (admin) is the creator and auto-joins.
code=$(req POST /api/groups "$ADMIN" "{\"name\":\"smoke-group\",\"member_ids\":[$L,$M]}")
check_http "S14: group created (L+M, boss auto-joins)" "$code" 201
G=$(jqget '.id')
[ -n "$G" ] && [ "$G" != "null" ] || die "S14: group id missing"

code=$(req POST /api/groups "$ADMIN" "{\"name\":\"   \",\"member_ids\":[$L]}")
check_http "S14: blank group name -> 400" "$code" 400
code=$(req POST /api/groups "$ADMIN" "{\"name\":\"solo\",\"member_ids\":[]}")
check_http "S14: creator-only group -> 400" "$code" 400

# W is not a member: cannot send, cannot list the thread.
code=$(req POST /api/messages "$W_CRED" "{\"group\":$G,\"kind\":\"chat\",\"body\":\"intruder\"}")
check_http "S14: non-member group send -> 403" "$code" 403
code=$(req GET "/api/messages?group=$G" "$W_CRED")
check_http "S14: non-member group thread -> 403" "$code" 403

code=$(req POST /api/messages "$L_CRED" "{\"group\":$G,\"kind\":\"chat\",\"body\":\"hello squad\"}")
check_http "S14: member group send -> 201" "$code" 201

code=$(req GET "/api/messages?group=$G" "$M_CRED")
check_http "S14: group thread visible to member M" "$code" 200
if jqtest --argjson l "$L" '[.messages[] | select(.body == "hello squad" and .from_id == $l and .kind == "chat")] | length == 1'; then
  pass "S14: group thread carries L's chat row"
else
  fail "S14: group thread content wrong: $(jq -c '.messages' "$TMP/last")"
fi

# M's conversation list: group entry (peer = -(G+1), member_count 3, unread 1).
code=$(req GET /api/conversations "$M_CRED")
gpeer=$(( -G - 1 ))
if jqtest --argjson g "$G" --argjson p "$gpeer" \
  '[.conversations[] | select(.is_group == true and .group_id == $g and .peer_id == $p
     and .name == "smoke-group" and .member_count == 3 and .unread == 1)] | length == 1'; then
  pass "S14: M conversation list has group entry (peer=$gpeer, unread 1)"
else
  fail "S14: M conversation list missing group entry: $(jq -c '.conversations' "$TMP/last")"
fi
# W (non-member) must not see the group at all.
code=$(req GET /api/conversations "$W_CRED")
check_http "S14: W conversation list" "$code" 200
if jqtest --argjson g "$G" '[.conversations[] | select(.group_id == $g)] | length == 0'; then
  pass "S14: non-member W does not see the group"
else
  fail "S14: W sees a group it is not a member of: $(jq -c '.conversations' "$TMP/last")"
fi

# M marks the group read -> unread clears.
code=$(req POST "/api/messages/read" "$M_CRED" "{\"group\":$G}")
check_http "S14: M marks group read" "$code" 200
code=$(req GET /api/conversations "$M_CRED")
unread=$(jq --argjson p "$gpeer" -r '.conversations[] | select(.peer_id == $p) | .unread' "$TMP/last")
if [ "$unread" = "0" ]; then
  pass "S14: group unread cleared after mark-read"
else
  fail "S14: group unread after read = $unread, want 0"
fi

# Group traffic must not leak into the L<->M 1:1 thread (and vice versa).
code=$(req POST /api/messages "$L_CRED" "{\"to\":$M,\"kind\":\"chat\",\"body\":\"smoke dm\"}")
check_http "S14: L->M 1:1 chat -> 201" "$code" 201
code=$(req GET "/api/messages?with=$M" "$L_CRED")
if jqtest '[.messages[] | select(.body == "hello squad")] | length == 0'; then
  pass "S14: group message absent from 1:1 L<->M thread"
else
  fail "S14: group message leaked into 1:1 thread: $(jq -c '.messages' "$TMP/last")"
fi

section "17d. S16 — reply quote (reply_to jump target)"

# 1:1: L chats to M, M quotes the message back (same conversation, reverse dir).
code=$(req POST /api/messages "$L_CRED" "{\"to\":$M,\"kind\":\"chat\",\"body\":\"s16 original\"}")
check_http "S16: L->M chat -> 201" "$code" 201
S16_A=$(jqget '.message_id')
code=$(req POST /api/messages "$M_CRED" \
  "{\"to\":$L,\"kind\":\"chat\",\"body\":\"> s16 original\\nmy reply\",\"reply_to\":$S16_A}")
check_http "S16: M reply with reply_to -> 201" "$code" 201
S16_B=$(jqget '.message_id')
code=$(req GET "/api/messages?with=$L" "$M_CRED")
check_http "S16: M thread with=L -> 200" "$code" 200
if jqtest --argjson a "$S16_A" \
  '[.messages[] | select(.id == $a)] | length == 1' \
  && jq -e --argjson b "$S16_B" --argjson a "$S16_A" \
      '[.messages[] | select(.id == $b and .reply_to == $a)] | length == 1' \
      "$TMP/last" >"$TMP/null" 2>&1; then
  pass "S16: reply row carries reply_to = original id"
else
  fail "S16: reply_to missing in thread: $(jq -c '.messages[-1]' "$TMP/last")"
fi
# The quoted original must not expose a reply_to key of its own.
if jqtest --argjson a "$S16_A" '[.messages[] | select(.id == $a and .reply_to != null)] | length == 0'; then
  pass "S16: original row has no reply_to"
else
  fail "S16: original row unexpectedly has reply_to"
fi

# Missing target -> 400.
code=$(req POST /api/messages "$M_CRED" \
  "{\"to\":$L,\"kind\":\"chat\",\"body\":\"x\",\"reply_to\":999999}")
check_http "S16: reply_to=999999 (missing) -> 400" "$code" 400

# Cross-conversation: W quotes L's 1:1 message inside its own (W,M) thread.
code=$(req POST /api/messages "$W_CRED" \
  "{\"to\":$M,\"kind\":\"report\",\"body\":\"s16 cross\",\"reply_to\":$S16_A}")
check_http "S16: cross-conversation reply_to -> 400" "$code" 400

# Group: L posts, M quotes inside the group; the 1:1 message is not quoteable there.
code=$(req POST /api/messages "$L_CRED" "{\"group\":$G,\"kind\":\"chat\",\"body\":\"s16 g original\"}")
check_http "S16: group original -> 201" "$code" 201
S16_C=$(jqget '.message_id')
code=$(req POST /api/messages "$M_CRED" \
  "{\"group\":$G,\"kind\":\"chat\",\"body\":\"> s16 g original\\nack\",\"reply_to\":$S16_C}")
check_http "S16: group reply with reply_to -> 201" "$code" 201
S16_D=$(jqget '.message_id')
code=$(req GET "/api/messages?group=$G" "$M_CRED")
check_http "S16: group thread -> 200" "$code" 200
if jqtest --argjson c "$S16_C" \
  '[.messages[] | select(.id == $c)] | length == 1' \
  && jq -e --argjson c "$S16_C" \
      '[.messages[] | select(.reply_to == $c)] | length == 1' "$TMP/last" >"$TMP/null" 2>&1; then
  pass "S16: group thread carries reply_to"
else
  fail "S16: group thread reply_to missing: $(jq -c '.messages' "$TMP/last")"
fi
code=$(req POST /api/messages "$L_CRED" \
  "{\"group\":$G,\"kind\":\"chat\",\"body\":\"s16 cross g\",\"reply_to\":$S16_A}")
check_http "S16: 1:1 message quoted in group -> 400" "$code" 400

# Regression: a plain message (no reply_to) is unchanged.
code=$(req POST /api/messages "$L_CRED" "{\"to\":$M,\"kind\":\"chat\",\"body\":\"s16 plain\"}")
check_http "S16: plain chat without reply_to -> 201" "$code" 201
S16_E=$(jqget '.message_id')
code=$(req GET "/api/messages?with=$M" "$L_CRED")
if jqtest --argjson e "$S16_E" \
  '[.messages[] | select(.id == $e and .reply_to != null)] | length == 0'; then
  pass "S16: plain message has no reply_to"
else
  fail "S16: plain message unexpectedly has reply_to"
fi

section "17e. S17 — group settings (members / prefs) + system create message"

# Q1: creating a group emits a system message (G from S14 was boss-created).
code=$(req GET "/api/messages?group=$G" "$ADMIN")
check_http "S17: group thread fetch" "$code" 200
if jqtest '[.messages[] | select(.kind == "system")] | length == 1' \
   && jqtest '[.messages[0] | select(.kind == "system" and .body == "boss created a group" and .from_id == null)] | length == 1'; then
  pass "S17: boss-created group has one system message (from_id null, exact body)"
else
  fail "S17: system create message wrong: $(jq -c '.messages' "$TMP/last")"
fi

# Boss (creator) adds W to G; duplicate / empty / invalid ids are rejected.
code=$(req POST "/api/groups/$G/members" "$ADMIN" "{\"employee_ids\":[$W]}")
check_http "S17: boss adds member W" "$code" 200
if jqtest --argjson w "$W" '.added == [$w]'; then
  pass "S17: add response lists exactly the new member"
else
  fail "S17: add response wrong: $(cat "$TMP/last")"
fi
code=$(req POST "/api/groups/$G/members" "$ADMIN" "{\"employee_ids\":[$W]}")
check_http "S17: duplicate add -> 400" "$code" 400
code=$(req POST "/api/groups/$G/members" "$ADMIN" '{"employee_ids":[]}')
check_http "S17: empty add list -> 400" "$code" 400
code=$(req POST "/api/groups/$G/members" "$ADMIN" '{"employee_ids":[99999]}')
check_http "S17: nonexistent employee id -> 400" "$code" 400
# Ordinary member (not creator/boss) cannot add members.
code=$(req POST "/api/groups/$G/members" "$L_CRED" "{\"employee_ids\":[$H]}")
check_http "S17: non-creator member add -> 403" "$code" 403
# Non-members cannot read the group.
code=$(req GET "/api/groups/$G" "$H_CRED")
check_http "S17: non-member GET group -> 403" "$code" 403

# Member view: can_manage=false, no candidates, boss creator row, own prefs.
code=$(req GET "/api/groups/$G" "$L_CRED")
check_http "S17: member GET group" "$code" 200
if jqtest --argjson w "$W" '
  (.can_manage == false and (.candidates | type) == "null"
   and .muted == false and .ignored == false
   and .creator_name == "boss"
   and ([.members[] | select(.employee_id == 0 and .name == "boss" and .kind == "system" and .is_creator == true)] | length == 1)
   and ([.members[] | select(.employee_id == $w and .is_creator == false)] | length == 1))'; then
  pass "S17: member view (can_manage=false, no candidates, boss row, new member listed)"
else
  fail "S17: member view wrong: $(cat "$TMP/last")"
fi
# Manager view: can_manage=true, candidates = employees outside the group
# (boss is already inside, so only H + W2 remain).
code=$(req GET "/api/groups/$G" "$ADMIN")
check_http "S17: boss GET group" "$code" 200
if jqtest --argjson h "$H" --argjson w2 "$W2" '
  (.can_manage == true
   and ([.candidates[] | select(.employee_id == $h)] | length == 1)
   and ([.candidates[] | select(.employee_id == $w2)] | length == 1)
   and ([.candidates[] | select(.employee_id == 0)] | length == 0))'; then
  pass "S17: manager view (can_manage=true, candidates = outside employees)"
else
  fail "S17: manager view wrong: $(cat "$TMP/last")"
fi

# Employee-created group: system message from_id = creator; a member who joins
# at creation sees unread=0 (the system message does not count as unread).
code=$(req POST /api/groups "$L_CRED" "{\"name\":\"s17-group\",\"member_ids\":[$M,$H]}")
check_http "S17: employee L creates group" "$code" 201
G2=$(jqget '.id')
[ -n "$G2" ] && [ "$G2" != "null" ] || die "S17: second group id missing"
code=$(req GET "/api/messages?group=$G2" "$L_CRED")
check_http "S17: G2 thread fetch" "$code" 200
if jqtest --argjson l "$L" '[.messages[] | select(.kind == "system" and .body == "lead-a created a group" and .from_id == $l)] | length == 1'; then
  pass "S17: employee-created group system message (from_id=creator, exact body)"
else
  fail "S17: G2 system message wrong: $(jq -c '.messages' "$TMP/last")"
fi
# M's conversation list: G2 entry is first (newest last_msg_id), unread 0,
# preview = the system create message.
code=$(req GET /api/conversations "$M_CRED")
check_http "S17: M conversations" "$code" 200
g2peer=$(( -G2 - 1 ))
if jqtest --argjson p "$g2peer" \
   '[.conversations[] | select(.peer_id == $p and .is_group == true and .unread == 0 and .last_msg_body == "lead-a created a group")] | length == 1'; then
  pass "S17: new member unread=0 (system excluded), preview = create message"
else
  fail "S17: M group entry wrong: $(jq -c '.conversations' "$TMP/last")"
fi
if jqtest --argjson p "$g2peer" '.conversations[0].peer_id == $p'; then
  pass "S17: new group pinned to top of conversation list"
else
  fail "S17: group not first: $(jq -c '[.conversations[] | {name, peer_id}]' "$TMP/last")"
fi

# Prefs: single-field updates, upsert preservation, round-trip, validation.
code=$(req POST "/api/groups/$G2/prefs" "$M_CRED" '{"muted":true}')
check_http "S17: member sets muted" "$code" 200
if jqtest '.muted == true and .ignored == false'; then
  pass "S17: prefs response is the full updated pair"
else
  fail "S17: prefs response wrong: $(cat "$TMP/last")"
fi
code=$(req GET /api/conversations "$M_CRED")
if jqtest --argjson p "$g2peer" '[.conversations[] | select(.peer_id == $p and .muted == true and .ignored == false)] | length == 1'; then
  pass "S17: conversations entry carries muted=true"
else
  fail "S17: conversations muted flag wrong: $(jq -c '.conversations' "$TMP/last")"
fi
code=$(req POST "/api/groups/$G2/prefs" "$M_CRED" '{"ignored":true}')
check_http "S17: member sets ignored (upsert keeps muted)" "$code" 200
if jqtest '.muted == true and .ignored == true'; then
  pass "S17: upsert preserves previously-set muted"
else
  fail "S17: upsert lost muted: $(cat "$TMP/last")"
fi
code=$(req GET /api/conversations "$M_CRED")
if jqtest --argjson p "$g2peer" '[.conversations[] | select(.peer_id == $p and .muted == true and .ignored == true)] | length == 1'; then
  pass "S17: conversations entry carries muted+ignored"
else
  fail "S17: conversations ignored flag wrong: $(jq -c '.conversations' "$TMP/last")"
fi
code=$(req POST "/api/groups/$G2/prefs" "$M_CRED" '{"muted":false}')
check_http "S17: clear muted" "$code" 200
if jqtest '.muted == false and .ignored == true'; then
  pass "S17: explicit false round-trips"
else
  fail "S17: muted clear failed: $(cat "$TMP/last")"
fi

# Prefs validation + scope.
code=$(req POST "/api/groups/$G2/prefs" "$M_CRED" '{}')
check_http "S17: prefs with no fields -> 400" "$code" 400
code=$(req POST "/api/groups/$G2/prefs" "$W_CRED" '{"muted":true}')
check_http "S17: non-member prefs -> 403" "$code" 403
# 1:1 conversation entries keep their shape (no muted/ignored keys).
code=$(req GET /api/conversations "$M_CRED")
if jqtest '[.conversations[] | select(.is_group != true)] | all(.muted == null and .ignored == null)'; then
  pass "S17: 1:1 entries unchanged (no muted/ignored keys)"
else
  fail "S17: 1:1 entries gained keys: $(jq -c '.conversations' "$TMP/last")"
fi

section "17f. S18 — task priority preemption + context files + orphan recovery"

# --- S18.0 — section 14 set selfreview_interval_seconds=1; reset to the
# default 1800 so the scheduler stops injecting selfreviews into W2's queue,
# then drain anything still active so preemption tests see a clean queue.
sqlite3 "$DB" "INSERT INTO settings(key,value) VALUES('selfreview_interval_seconds','1800')
ON CONFLICT(key) DO UPDATE SET value='1800';" >"$TMP/null" 2>&1
s18active=""
i=0
while [ $i -lt 60 ]; do
  req GET "/api/tasks?employee=$W2" "$ADMIN" >"$TMP/null"
  s18active=$(jqget '[.tasks[] | select(.kind == "selfreview" and (.status == "queued" or .status == "running"))] | length')
  [ "${s18active:-0}" = "0" ] && break
  i=$((i + 1))
  sleep 0.5
done
if [ "${s18active:-0}" = "0" ]; then
  pass "S18: selfreview interval reset to 1800s, W2 queue drained"
else
  fail "S18: W2 still has active selfreviews after reset (count=$s18active)"
fi

# One tool script covers every S18 tool invocation:
#   *s18-fast* -> instant marker   (the preempter / FIFO follower)
#   *s18-slow* -> 8s sleep         (the preempted / FIFO leader / orphan task)
#   *          -> echo full prompt + env markers (bootstrap/env assertions)
# NOTE: the prompt header contains "worker-slow" (employee name) but never
# "s18-slow"/"s18-fast", so the case branches only match the task body.
cat > "$TMP/tool-s18.sh" <<'TOOL'
#!/bin/sh
case "$1" in
  *s18-fast*) echo "s18: fast-done" ;;
  *s18-slow*) echo "s18: slow-start"; sleep 8; echo "s18: slow-done" ;;
  *) printf '%s\n' "$1"
     echo "S18_TOKEN=$BARK_TOKEN"
     echo "S18_SERVER=$BARK_SERVER"
     ;;
esac
TOOL
chmod +x "$TMP/tool-s18.sh"

code=$(req PUT "/api/employees/$W2/tool" "$ADMIN" \
  "{\"tool\":\"$TMP/tool-s18.sh\",\"tool_args\":\"\"}")
check_http "S18: W2 tool switched to s18 script" "$code" 200
stop_pid "$A_W2"
A_W2=""
A_W2=$(start_agent "$W2_CRED" agent-W2)
if wait_online 20 "$W2"; then
  pass "S18: W2 agent back online (initial attach)"
else
  fail "S18: W2 agent not online after restart"
fi

# --- S18.1 — context files (written on every initial attach)
W2DIR="$TMP/work/work/worker-slow"
ctxok=""
i=0
while [ -z "$ctxok" ] && [ $i -lt 20 ]; do
  [ -s "$W2DIR/whatbark/identity.md" ] && [ -s "$W2DIR/whatbark/platform.md" ] && ctxok=1
  i=$((i + 1))
  sleep 0.5
done
if [ -n "$ctxok" ]; then
  pass "S18: whatbark/identity.md + platform.md written to W2 workdir"
else
  fail "S18: context files missing under $W2DIR/whatbark"
fi
if grep -q "worker-slow" "$W2DIR/whatbark/identity.md" 2>/dev/null \
   && grep -q "whatbark/platform.md" "$W2DIR/whatbark/identity.md" 2>/dev/null; then
  pass "S18: identity.md names the employee and points to platform.md"
else
  fail "S18: identity.md content wrong: $(head -c 200 "$W2DIR/whatbark/identity.md" 2>/dev/null)"
fi
if grep -q "bark" "$W2DIR/whatbark/platform.md" 2>/dev/null; then
  pass "S18: platform.md carries the bark quick-reference"
else
  fail "S18: platform.md missing bark quick-reference"
fi
if [ ! -e "$W2DIR/role.md" ]; then
  pass "S18: agent did not create role.md (user-owned)"
else
  fail "S18: agent created role.md (must stay user-owned)"
fi

# --- S18.2 — bootstrap prompt header + tool env injection
out=$(bark "$ADMIN" send --to "$W2" --kind dispatch --body "s18 probe" 2>&1)
T18A=$(printf '%s' "$out" | jq -r '.task_id' 2>"$TMP/null")
if [ -z "$T18A" ] || [ "$T18A" = "null" ]; then
  fail "S18: probe dispatch failed: $out"
fi
if wait_task "$T18A" done 30; then
  pass "S18: probe task done"
else
  fail "S18: probe task not done in 30s"
fi
req GET "/api/tasks/$T18A" "$ADMIN" >"$TMP/null"
OUT18=$(jqget '.output')
case "$OUT18" in
  *"You are an AI employee of the whatbark platform, worker-slow"*) pass "S18: bootstrap header present in tool prompt" ;;
  *) fail "S18: bootstrap header missing; output=$OUT18" ;;
esac
case "$OUT18" in
  *"whatbark/identity.md"*"whatbark/platform.md"*) pass "S18: prompt references context files" ;;
  *) fail "S18: prompt lacks context-file pointers; output=$OUT18" ;;
esac
case "$OUT18" in
  *"s18 probe"*) pass "S18: original body passed through to tool" ;;
  *) fail "S18: body missing in prompt; output=$OUT18" ;;
esac
case "$OUT18" in
  *"S18_TOKEN=$W2_CRED"*) pass "S18: BARK_TOKEN injected into tool env" ;;
  *) fail "S18: BARK_TOKEN wrong/missing; output=$OUT18" ;;
esac
case "$OUT18" in
  *"S18_SERVER=$URL"*) pass "S18: BARK_SERVER injected into tool env" ;;
  *) fail "S18: BARK_SERVER wrong/missing; output=$OUT18" ;;
esac

# --- S18.3 — priority persistence + validation
out=$(bark "$ADMIN" send --to "$W2" --kind dispatch --priority 5 --body "s18 prio5" 2>&1)
T18B=$(printf '%s' "$out" | jq -r '.task_id' 2>"$TMP/null")
if wait_task "$T18B" done 30; then
  req GET "/api/tasks/$T18B" "$ADMIN" >"$TMP/null"
  if jqtest '.priority == 5'; then
    pass "S18: priority=5 persisted on task"
  else
    fail "S18: priority not persisted: $(cat "$TMP/last")"
  fi
else
  fail "S18: prio5 task not done in 30s"
fi

out=$(bark "$ADMIN" send --to "$W2" --kind dispatch --body "s18 prio default" 2>&1)
T18C=$(printf '%s' "$out" | jq -r '.task_id' 2>"$TMP/null")
if wait_task "$T18C" done 30; then
  req GET "/api/tasks/$T18C" "$ADMIN" >"$TMP/null"
  if jqtest '.priority == 0'; then
    pass "S18: default priority is 0"
  else
    fail "S18: default priority wrong: $(cat "$TMP/last")"
  fi
else
  fail "S18: default-prio task not done in 30s"
fi

code=$(req POST /api/messages "$ADMIN" "{\"to\":$W2,\"kind\":\"dispatch\",\"body\":\"x\",\"priority\":100}")
check_http "S18: priority=100 rejected (400)" "$code" 400
code=$(req POST /api/messages "$ADMIN" "{\"to\":$W2,\"kind\":\"dispatch\",\"body\":\"x\",\"priority\":-1}")
check_http "S18: priority=-1 rejected (400)" "$code" 400

code=$(req GET "/api/tasks?employee=$W2" "$ADMIN")
if [ "$code" = "200" ] && jqtest 'all(.tasks[]; has("priority"))'; then
  pass "S18: task list rows carry priority"
else
  fail "S18: task list rows missing priority (http $code)"
fi

# --- S18.4 — preemption: p9 queued task preempts the running p0 task
out=$(bark "$ADMIN" send --to "$W2" --kind dispatch --body "s18-slow" 2>&1)
T18S=$(printf '%s' "$out" | jq -r '.task_id' 2>"$TMP/null")
running=0; s=""; i=0
while [ $i -lt 40 ]; do
  req GET "/api/tasks/$T18S" "$ADMIN" >"$TMP/null"
  s=$(jqget '.status')
  [ "$s" = "running" ] && { running=1; break; }
  i=$((i + 1))
  sleep 0.5
done
if [ "$running" != "1" ]; then
  fail "S18: slow task never reached running (status=$s)"
fi

out=$(bark "$ADMIN" send --to "$W2" --kind dispatch --priority 9 --body "s18-fast" 2>&1)
T18F=$(printf '%s' "$out" | jq -r '.task_id' 2>"$TMP/null")
if [ -z "$T18F" ] || [ "$T18F" = "null" ]; then
  fail "S18: fast dispatch failed: $out"
fi

# Preemption proof by timing, not by polling the "queued" state: the agent
# re-claims the requeued slow task within one poll tick (~300ms), so the
# queued window is too short to poll reliably. Instead: with preemption the
# fast task (instant tool) finishes within ~2s of dispatch; without it the
# fast task must wait for the slow task's 8s first run (~7s after dispatch).
# A 4s window separates the two cases with margin.
fastdone=0; i=0
while [ $i -lt 8 ]; do
  req GET "/api/tasks/$T18F" "$ADMIN" >"$TMP/null"
  [ "$(jqget '.status')" = "done" ] && { fastdone=1; break; }
  i=$((i + 1))
  sleep 0.5
done
if [ "$fastdone" = "1" ]; then
  pass "S18: fast task done while slow first-run still in progress (preempted)"
else
  fail "S18: fast task not done within 4s of dispatch (no preemption?)"
fi

# The slow task carries the "preempted:" audit note until its re-run finishes
# (finish overwrites output). The re-run takes 8s, so right after the fast
# task landed the note is present whether the slow is queued or re-running.
req GET "/api/tasks/$T18S" "$ADMIN" >"$TMP/null"
SLOW_NOTE=$(jqget '.output')
case "$SLOW_NOTE" in
  *"preempted:"*"priority 9"*) pass "S18: preempt note recorded (names intruder priority)" ;;
  *) fail "S18: preempt note missing: $SLOW_NOTE" ;;
esac

if wait_task "$T18F" done 30; then
  req GET "/api/tasks/$T18F" "$ADMIN" >"$TMP/null"
  S18_F_OUT=$(jqget '.output')
  S18_F_START=$(jqget '.started_at')
  if printf '%s' "$S18_F_OUT" | grep -q "s18: fast-done"; then
    pass "S18: fast task ran to completion (fast-done)"
  else
    fail "S18: fast output wrong: $S18_F_OUT"
  fi
else
  fail "S18: fast task not done in 30s"
fi

if wait_task "$T18S" done 90; then
  req GET "/api/tasks/$T18S" "$ADMIN" >"$TMP/null"
  S18_S_OUT=$(jqget '.output')
  S18_S_START=$(jqget '.started_at')
  if printf '%s' "$S18_S_OUT" | grep -q "s18: slow-done"; then
    pass "S18: preempted task re-ran to completion (slow-done)"
  else
    fail "S18: slow final output wrong: $S18_S_OUT"
  fi
  # RFC3339 UTC strings compare lexicographically; the re-run must start at or
  # after the fast task's start (first run was killed, started_at reset).
  if [[ -n "$S18_S_START" && -n "$S18_F_START" && ! "$S18_S_START" < "$S18_F_START" ]]; then
    pass "S18: slow re-run started_at >= fast started_at (ordering)"
  else
    fail "S18: started_at order wrong (slow=$S18_S_START fast=$S18_F_START)"
  fi
else
  fail "S18: re-run of slow task not done in 90s"
fi

# --- S18.5 — equal priority never preempts; FIFO by id
out=$(bark "$ADMIN" send --to "$W2" --kind dispatch --priority 3 --body "s18-slow-a" 2>&1)
T18X=$(printf '%s' "$out" | jq -r '.task_id' 2>"$TMP/null")
running=0; s=""; i=0
while [ $i -lt 40 ]; do
  req GET "/api/tasks/$T18X" "$ADMIN" >"$TMP/null"
  s=$(jqget '.status')
  [ "$s" = "running" ] && { running=1; break; }
  i=$((i + 1))
  sleep 0.5
done
if [ "$running" != "1" ]; then
  fail "S18: equal-prio task X never reached running (status=$s)"
fi

out=$(bark "$ADMIN" send --to "$W2" --kind dispatch --priority 3 --body "s18-fast-b" 2>&1)
T18Y=$(printf '%s' "$out" | jq -r '.task_id' 2>"$TMP/null")
if [ -z "$T18Y" ] || [ "$T18Y" = "null" ]; then
  fail "S18: equal-prio dispatch Y failed: $out"
fi

if wait_task "$T18X" done 90; then
  req GET "/api/tasks/$T18X" "$ADMIN" >"$TMP/null"
  case "$(jqget '.output')" in
    *preempted*) fail "S18: equal-priority task X was preempted (must not)" ;;
    *) pass "S18: equal priority never preempts (X unaffected)" ;;
  esac
  S18_X_FIN=$(jqget '.finished_at')
else
  fail "S18: task X not done in 90s"
  S18_X_FIN=""
fi
if wait_task "$T18Y" done 30; then
  pass "S18: Y (queued during X) ran to completion"
else
  fail "S18: task Y not done in 30s"
fi
req GET "/api/tasks/$T18Y" "$ADMIN" >"$TMP/null"
S18_Y_START=$(jqget '.started_at')
if [[ -n "$S18_Y_START" && -n "$S18_X_FIN" && ! "$S18_Y_START" < "$S18_X_FIN" ]]; then
  pass "S18: serial order kept (Y started after X finished)"
else
  fail "S18: Y started before X finished (Y=$S18_Y_START Xfin=$S18_X_FIN)"
fi

# --- S18.6 — orphan recovery: agent dies mid-task, restart requeues it
out=$(bark "$ADMIN" send --to "$W2" --kind dispatch --body "s18-slow-z" 2>&1)
T18Z=$(printf '%s' "$out" | jq -r '.task_id' 2>"$TMP/null")
running=0; s=""; i=0
while [ $i -lt 40 ]; do
  req GET "/api/tasks/$T18Z" "$ADMIN" >"$TMP/null"
  s=$(jqget '.status')
  [ "$s" = "running" ] && { running=1; break; }
  i=$((i + 1))
  sleep 0.5
done
if [ "$running" != "1" ]; then
  fail "S18: orphan task never reached running (status=$s)"
fi

kill -9 "$A_W2" 2>"$TMP/null"
wait "$A_W2" 2>"$TMP/null" || true
A_W2=""
sleep 1
req GET "/api/tasks/$T18Z" "$ADMIN" >"$TMP/null"
if [ "$(jqget '.status')" = "running" ]; then
  pass "S18: task still running after agent SIGKILL (orphaned in DB)"
else
  fail "S18: orphan task status=$(jqget '.status') (want running)"
fi

A_W2=$(start_agent "$W2_CRED" agent-W2)
if wait_online 20 "$W2"; then
  pass "S18: restarted agent back online"
else
  fail "S18: restarted agent not online"
fi
# The "queued" window after requeue lasts only one poll tick (~300ms) before
# the agent re-claims the task, so polling it is racy. Instead assert on the
# requeue note: RequeueRunningTasks writes it to the task's output and it
# stays in the DB until the re-run's finish overwrites output (8s window).
noteok=""; s=""; i=0
while [ $i -lt 20 ]; do
  req GET "/api/tasks/$T18Z" "$ADMIN" >"$TMP/null"
  s=$(jqget '.status')
  case "$(jqget '.output')" in
    *"agent reattached"*) noteok=1; break ;;
  esac
  i=$((i + 1))
  sleep 0.5
done
if [ "$noteok" = "1" ]; then
  pass "S18: initial attach requeued the orphaned task (note recorded, saw status=$s)"
else
  fail "S18: orphaned task not requeued after attach (status=$s, no requeue note)"
fi
if wait_task "$T18Z" done 90; then
  req GET "/api/tasks/$T18Z" "$ADMIN" >"$TMP/null"
  if jqget '.output' | grep -q "s18: slow-done"; then
    pass "S18: requeued task re-ran to completion"
  else
    fail "S18: requeued task output wrong: $(jqget '.output')"
  fi
else
  fail "S18: requeued task not done in 90s"
fi

# --- S18.7 — heartbeat regression: empty-body attach must NOT requeue
out=$(bark "$ADMIN" send --to "$W2" --kind dispatch --body "s18-slow-h" 2>&1)
T18H=$(printf '%s' "$out" | jq -r '.task_id' 2>"$TMP/null")
running=0; s=""; i=0
while [ $i -lt 40 ]; do
  req GET "/api/tasks/$T18H" "$ADMIN" >"$TMP/null"
  s=$(jqget '.status')
  [ "$s" = "running" ] && { running=1; break; }
  i=$((i + 1))
  sleep 0.5
done
if [ "$running" != "1" ]; then
  fail "S18: heartbeat-probe task never reached running (status=$s)"
fi

code=$(req POST /api/agent/attach "$W2_CRED")
if [ "$code" = "200" ] && jqtest '(.ok == true) and (has("requeued") | not)'; then
  pass "S18: heartbeat attach (empty body) ok, no requeue"
else
  fail "S18: heartbeat attach wrong (http $code: $(cat "$TMP/last"))"
fi
req GET "/api/tasks/$T18H" "$ADMIN" >"$TMP/null"
if [ "$(jqget '.status')" = "running" ]; then
  pass "S18: heartbeat did not touch the running task"
else
  fail "S18: heartbeat touched running task (status=$(jqget '.status'))"
fi
if wait_task "$T18H" done 30; then
  pass "S18: heartbeat-probe task finished normally"
else
  fail "S18: heartbeat-probe task not done in 30s"
fi

section "17g. S20 — @mentions (1:1 / group / boss)"

# 1:1: L sends M a chat that @-mentions M by exact name.
code=$(req POST /api/messages "$L_CRED" "{\"to\":$M,\"kind\":\"chat\",\"body\":\"@lead-b review this\"}")
check_http "S20: L->M 1:1 with @lead-b -> 201" "$code" 201
S20_A=$(jqget '.message_id')
code=$(req GET "/api/messages?with=$L" "$M_CRED")
check_http "S20: M reads 1:1 thread" "$code" 200
if jqtest --argjson a "$S20_A" --argjson m "$M" \
   '[.messages[] | select(.id == $a and .mentions == [$m])] | length == 1'; then
  pass "S20: 1:1 message carries mentions=[M]"
else
  fail "S20: 1:1 mentions wrong: $(jq -c --argjson a "$S20_A" '.messages[] | select(.id == $a)' "$TMP/last")"
fi

# M's conversation list: unread_mentioned == 1 for peer L.
code=$(req GET /api/conversations "$M_CRED")
gpeerM=$(( -G - 1 ))  # group conversation peer id
if jqtest --argjson l "$L" \
   '[.conversations[] | select(.peer_id == $l and .unread_mentioned == 1)] | length == 1'; then
  pass "S20: M conversation row shows unread_mentioned=1 for L"
else
  fail "S20: M unread_mentioned wrong: $(jq -c '.conversations' "$TMP/last")"
fi

# Negatives: unknown token / non-participant / self-mention all yield empty mentions.
for body in "@nobodyxyz hi" "@worker-1 hi" "@lead-a hi"; do
  code=$(req POST /api/messages "$L_CRED" "{\"to\":$M,\"kind\":\"chat\",\"body\":\"$body\"}")
  [ "$code" = "201" ] || die "S20: negative mention send http $code: $(cat "$TMP/last")"
  S20_N=$(jqget '.message_id')
  code=$(req GET "/api/messages?with=$L" "$M_CRED")
  if jqtest --argjson a "$S20_N" \
     '[.messages[] | select(.id == $a and .mentions == [])] | length == 1'; then
    pass "S20: negative case (\"$body\") has empty mentions"
  else
    fail "S20: negative case (\"$body\") mentions not empty: $(jq -c --argjson a "$S20_N" '.messages[] | select(.id == $a)' "$TMP/last")"
  fi
done

# M marks the 1:1 read -> unread_mentioned clears.
code=$(req POST "/api/messages/read" "$M_CRED" "{\"with\":$L}")
check_http "S20: M marks 1:1 read" "$code" 200
code=$(req GET /api/conversations "$M_CRED")
if jqtest --argjson l "$L" \
   '[.conversations[] | select(.peer_id == $l and .unread_mentioned == 0)] | length == 1'; then
  pass "S20: 1:1 unread_mentioned cleared after read"
else
  fail "S20: 1:1 unread_mentioned not cleared: $(jq -c '.conversations' "$TMP/last")"
fi

# Group: M posts @lead-a; boss is an auto-joined member so @boss also resolves.
code=$(req POST /api/messages "$M_CRED" "{\"group\":$G,\"kind\":\"chat\",\"body\":\"@lead-a ping\"}")
check_http "S20: group @lead-a -> 201" "$code" 201
S20_G=$(jqget '.message_id')
code=$(req GET "/api/messages?group=$G" "$L_CRED")
check_http "S20: L reads group thread" "$code" 200
if jqtest --argjson a "$S20_G" --argjson l "$L" \
   '[.messages[] | select(.id == $a and .mentions == [$l])] | length == 1'; then
  pass "S20: group message carries mentions=[L]"
else
  fail "S20: group mentions wrong: $(jq -c --argjson a "$S20_G" '.messages[] | select(.id == $a)' "$TMP/last")"
fi
# L's group conversation row: unread_mentioned == 1.
code=$(req GET /api/conversations "$L_CRED")
if jqtest --argjson p "$gpeerM" \
   '[.conversations[] | select(.peer_id == $p and .unread_mentioned == 1)] | length == 1'; then
  pass "S20: L group row shows unread_mentioned=1"
else
  fail "S20: L group unread_mentioned wrong: $(jq -c '.conversations' "$TMP/last")"
fi

# @boss resolves to id 0 (boss auto-joined the group in S14).
code=$(req POST /api/messages "$M_CRED" "{\"group\":$G,\"kind\":\"chat\",\"body\":\"@boss check\"}")
check_http "S20: group @boss -> 201" "$code" 201
S20_B=$(jqget '.message_id')
code=$(req GET "/api/messages?group=$G" "$L_CRED")
if jqtest --argjson a "$S20_B" \
   '[.messages[] | select(.id == $a and .mentions == [0])] | length == 1'; then
  pass "S20: @boss resolves to mentions=[0]"
else
  fail "S20: @boss mentions wrong: $(jq -c --argjson a "$S20_B" '.messages[] | select(.id == $a)' "$TMP/last")"
fi

# Non-member token in group -> no mention (H was never added to G; W was in S17).
code=$(req POST /api/messages "$M_CRED" "{\"group\":$G,\"kind\":\"chat\",\"body\":\"@human-1 x\"}")
check_http "S20: group @human-1 (non-member) -> 201" "$code" 201
S20_C=$(jqget '.message_id')
code=$(req GET "/api/messages?group=$G" "$L_CRED")
if jqtest --argjson a "$S20_C" \
   '[.messages[] | select(.id == $a and .mentions == [])] | length == 1'; then
  pass "S20: non-member token yields empty mentions"
else
  fail "S20: non-member mentions not empty: $(jq -c --argjson a "$S20_C" '.messages[] | select(.id == $a)' "$TMP/last")"
fi

# L marks the group read -> unread_mentioned clears.
code=$(req POST "/api/messages/read" "$L_CRED" "{\"group\":$G}")
check_http "S20: L marks group read" "$code" 200
code=$(req GET /api/conversations "$L_CRED")
if jqtest --argjson p "$gpeerM" \
   '[.conversations[] | select(.peer_id == $p and .unread_mentioned == 0 and .unread == 0)] | length == 1'; then
  pass "S20: group unread_mentioned cleared after read"
else
  fail "S20: group unread_mentioned not cleared: $(jq -c '.conversations' "$TMP/last")"
fi

section "17h. S21 — project channel (kind=channel, boss-only)"

# Boss creates a channel: 201 + kind=channel echoed.
code=$(req POST /api/groups "$ADMIN" "{\"name\":\"smoke-chan\",\"member_ids\":[$L,$M],\"kind\":\"channel\"}")
check_http "S21: boss creates channel -> 201" "$code" 201
CH=$(jqget '.id')
[ -n "$CH" ] && [ "$CH" != "null" ] || die "S21: channel id missing"
if jqtest '.kind == "channel"'; then
  pass "S21: channel create echoes kind=channel"
else
  fail "S21: channel create kind = $(jqget '.kind'), want channel"
fi

# Employee creates a channel: 403 (boss-only surface).
code=$(req POST /api/groups "$L_CRED" "{\"name\":\"nope-chan\",\"member_ids\":[$M],\"kind\":\"channel\"}")
check_http "S21: employee channel -> 403" "$code" 403

# Employee creates a plain group (no kind): 201 + kind=group.
code=$(req POST /api/groups "$L_CRED" "{\"name\":\"s21-squad\",\"member_ids\":[$M]}")
check_http "S21: employee plain group -> 201" "$code" 201
PG=$(jqget '.id')
if jqtest '.kind == "group"'; then
  pass "S21: plain group echoes kind=group"
else
  fail "S21: plain group kind = $(jqget '.kind'), want group"
fi

# Invalid kind -> 400 (boss and employee).
code=$(req POST /api/groups "$ADMIN" "{\"name\":\"bad-chan\",\"member_ids\":[$M],\"kind\":\"bogus\"}")
check_http "S21: boss kind=bogus -> 400" "$code" 400
code=$(req POST /api/groups "$L_CRED" "{\"name\":\"bad-chan\",\"member_ids\":[$M],\"kind\":\"bogus\"}")
check_http "S21: employee kind=bogus -> 400" "$code" 400

# GET /api/groups/{id} echoes kind.
code=$(req GET "/api/groups/$CH" "$M_CRED")
check_http "S21: M GET channel" "$code" 200
if jqtest '.kind == "channel"'; then
  pass "S21: GET channel echoes kind=channel"
else
  fail "S21: GET channel kind = $(jqget '.kind'), want channel"
fi
code=$(req GET "/api/groups/$PG" "$M_CRED")
if jqtest '.kind == "group"'; then
  pass "S21: GET plain group echoes kind=group"
else
  fail "S21: GET plain group kind = $(jqget '.kind'), want group"
fi

# /api/conversations: group rows carry is_group=true + the real kind.
cpeer=$(( -CH - 1 ))
code=$(req GET /api/conversations "$M_CRED")
check_http "S21: M conversation list" "$code" 200
if jqtest --argjson ch "$CH" --argjson p "$cpeer" \
   '[.conversations[] | select(.group_id == $ch and .peer_id == $p
      and .is_group == true and .kind == "channel")] | length == 1'; then
  pass "S21: channel row is_group=true kind=channel"
else
  fail "S21: channel conversation row wrong: $(jq -c '.conversations' "$TMP/last")"
fi
ppeer=$(( -PG - 1 ))
code=$(req GET /api/conversations "$M_CRED")
if jqtest --argjson pg "$PG" --argjson p "$ppeer" \
   '[.conversations[] | select(.group_id == $pg and .peer_id == $p
      and .is_group == true and .kind == "group")] | length == 1'; then
  pass "S21: plain group row is_group=true kind=group"
else
  fail "S21: plain group row wrong: $(jq -c '.conversations' "$TMP/last")"
fi

# Channel messages reuse the group gate: member 201, non-member 403.
# (H = human-1 is not a member of this channel; W is in $G since S17.)
code=$(req POST /api/messages "$M_CRED" "{\"group\":$CH,\"kind\":\"chat\",\"body\":\"s21 hello\"}")
check_http "S21: member channel send -> 201" "$code" 201
code=$(req GET "/api/messages?group=$CH" "$M_CRED")
check_http "S21: channel thread visible to member" "$code" 200
code=$(req POST /api/messages "$H_CRED" "{\"group\":$CH,\"kind\":\"chat\",\"body\":\"intruder\"}")
check_http "S21: non-member channel send -> 403" "$code" 403
code=$(req GET "/api/messages?group=$CH" "$H_CRED")
check_http "S21: non-member channel thread -> 403" "$code" 403

section "17i. S22 — in-group task dispatch (kind=dispatch + to)"

# G (smoke-group) members: {boss, L, M, W}; H is not a member. W's agent was
# stopped in section 10, so W's group tasks are driven through the task API.

# Boss bark group-dispatches W: success + task_id; boss's from_id is null.
out=$(bark "$ADMIN" send --group "$G" --kind dispatch --to "$W" --body "s22 boss group task" 2>&1)
S22_B=$(printf '%s' "$out" | jq -r '.task_id' 2>"$TMP/null")
if [ -z "$S22_B" ] || [ "$S22_B" = "null" ]; then
  die "S22: bark group dispatch failed: $out"
fi
pass "S22: bark group dispatch created a task"

# Task row: owner W, boss requester (0), group-bound, queued.
code=$(req GET "/api/tasks/$S22_B" "$ADMIN")
check_http "S22: GET group task" "$code" 200
if jqtest --argjson w "$W" --argjson g "$G" \
    '.employee_id == $w and .group_id == $g and .requested_by == 0 and .status == "queued"'; then
  pass "S22: task bound to group (owner W, requested_by 0, queued)"
else
  fail "S22: group task row wrong: $(cat "$TMP/last")"
fi

# Group thread carries the dispatch row (task back-filled, to_id null, boss).
code=$(req GET "/api/messages?group=$G" "$W_CRED")
check_http "S22: group thread fetch (member)" "$code" 200
if jqtest --argjson t "$S22_B" '[.messages[] | select(.kind == "dispatch" and .task_id == $t
   and .to_id == null and .from_id == null and .body == "s22 boss group task")] | length == 1'; then
  pass "S22: group thread has the dispatch row (task_id back-filled, to_id null)"
else
  fail "S22: dispatch row missing/wrong: $(jq -c '.messages' "$TMP/last")"
fi

# Conversation list: the group row now shows open_tasks=1.
code=$(req GET /api/conversations "$L_CRED")
check_http "S22: L conversations" "$code" 200
gpeer22=$(( -G - 1 ))
if jqtest --argjson g "$G" --argjson p "$gpeer22" \
   '[.conversations[] | select(.group_id == $g and .peer_id == $p and .open_tasks == 1)] | length == 1'; then
  pass "S22: group conversation row shows open_tasks=1"
else
  fail "S22: group open_tasks wrong: $(jq -c '.conversations' "$TMP/last")"
fi

# Negative matrix (API).
code=$(req POST /api/messages "$H_CRED" "{\"group\":$G,\"kind\":\"dispatch\",\"to\":$W,\"body\":\"intruder\"}")
check_http "S22: non-member dispatch -> 403" "$code" 403
code=$(req POST /api/messages "$ADMIN" "{\"group\":$G,\"kind\":\"dispatch\",\"to\":$H,\"body\":\"x\"}")
check_http "S22: target not a member -> 400" "$code" 400
code=$(req POST /api/messages "$L_CRED" "{\"group\":$G,\"kind\":\"dispatch\",\"to\":$L,\"body\":\"x\"}")
check_http "S22: self dispatch -> 400" "$code" 400
code=$(req POST /api/messages "$L_CRED" "{\"group\":$G,\"kind\":\"dispatch\",\"body\":\"no target\"}")
check_http "S22: dispatch without to -> 400" "$code" 400
code=$(req POST /api/messages "$L_CRED" "{\"group\":$G,\"kind\":\"ask\",\"to\":$W,\"body\":\"x\"}")
check_http "S22: kind=ask in group -> 400" "$code" 400
# S29 changed this contract: group kind=dispatch now accepts project_id (201,
# covered in 17o). The surviving negative here is kind=chat, which still rejects.
code=$(req POST /api/messages "$ADMIN" "{\"group\":$G,\"kind\":\"chat\",\"project_id\":$P,\"body\":\"x\"}")
check_http "S22: group chat with project_id -> 400 (S29: chat still rejects)" "$code" 400
code=$(req POST /api/messages "$L_CRED" "{\"group\":$G,\"kind\":\"chat\",\"to\":$W,\"body\":\"x\"}")
check_http "S22: group chat with to -> 400" "$code" 400

# bark CLI rejects a non-member group dispatch (non-zero exit).
if out=$(bark "$H_CRED" send --group "$G" --kind dispatch --to "$W" --body "nope" 2>&1); then
  fail "S22: bark group dispatch as non-member should fail: $out"
else
  pass "S22: bark group dispatch as non-member rejected"
fi

# Leader group-dispatches W (API path): 201 + task.
code=$(req POST /api/messages "$L_CRED" "{\"group\":$G,\"kind\":\"dispatch\",\"to\":$W,\"body\":\"s22 lead group task\"}")
check_http "S22: leader group dispatch -> 201" "$code" 201
S22_L=$(jqget '.task_id')
[ -n "$S22_L" ] && [ "$S22_L" != "null" ] || die "S22: leader group dispatch task_id missing"

# Group chat (no to) still works and creates no task.
code=$(req POST /api/messages "$M_CRED" "{\"group\":$G,\"kind\":\"chat\",\"body\":\"s22 chat\"}")
check_http "S22: group chat still 201" "$code" 201
if jqtest 'has("message_id") and (has("task_id") | not)'; then
  pass "S22: group chat response has no task_id"
else
  fail "S22: group chat response wrong: $(cat "$TMP/last")"
fi

# Drive both group tasks to done through the task API (W's agent is offline).
code=$(req POST "/api/tasks/$S22_B/start" "$W_CRED")
check_http "S22: W started boss group task" "$code" 200
code=$(req POST "/api/tasks/$S22_B/finish" "$W_CRED" '{"output":"s22 boss done"}')
check_http "S22: W finished boss group task" "$code" 200
code=$(req POST "/api/tasks/$S22_L/start" "$W_CRED")
check_http "S22: W started leader group task" "$code" 200
code=$(req POST "/api/tasks/$S22_L/finish" "$W_CRED" '{"output":"s22 lead done"}')
check_http "S22: W finished leader group task" "$code" 200

# Both reports land in the group thread, not in the 1:1 threads.
code=$(req GET "/api/messages?group=$G" "$ADMIN")
check_http "S22: group thread after reports" "$code" 200
if jqtest --argjson b "$S22_B" --argjson l "$S22_L" --argjson w "$W" \
   '([.messages[] | select(.kind == "report" and .task_id == $b and .from_id == $w and .body == "s22 boss done")] | length == 1)
    and ([.messages[] | select(.kind == "report" and .task_id == $l and .from_id == $w and .body == "s22 lead done")] | length == 1)'; then
  pass "S22: both reports are in the group thread"
else
  fail "S22: group reports missing: $(jq -c '[.messages[] | {kind, body, task_id}]' "$TMP/last")"
fi
code=$(req GET "/api/messages?with=$W" "$L_CRED")
check_http "S22: L<->W 1:1 thread fetch" "$code" 200
if jqtest '[.messages[] | select(.body == "s22 lead done")] | length == 0'; then
  pass "S22: leader's report did not leak into the 1:1 thread"
else
  fail "S22: report leaked into 1:1 thread: $(jq -c '.messages' "$TMP/last")"
fi

# open_tasks back to 0 once both group tasks are done.
code=$(req GET /api/conversations "$L_CRED")
if jqtest --argjson g "$G" --argjson p "$gpeer22" \
   '[.conversations[] | select(.group_id == $g and .peer_id == $p and .open_tasks == 0)] | length == 1'; then
  pass "S22: group open_tasks back to 0"
else
  fail "S22: group open_tasks not 0: $(jq -c '.conversations' "$TMP/last")"
fi

section "17j. S24 — group task kanban (GET /api/groups/{id}/tasks)"

# Create 2 group tasks via boss dispatch (fresh tasks for kanban assertions).
code=$(req POST /api/messages "$ADMIN" "{\"group\":$G,\"kind\":\"dispatch\",\"to\":$W,\"body\":\"s24 kanban task A\"}")
check_http "S24: boss group dispatch A -> 201" "$code" 201
S24_A=$(jqget '.task_id')
[ -n "$S24_A" ] && [ "$S24_A" != "null" ] || die "S24: task A id missing"

code=$(req POST /api/messages "$ADMIN" "{\"group\":$G,\"kind\":\"dispatch\",\"to\":$W,\"body\":\"s24 kanban task B\"}")
check_http "S24: boss group dispatch B -> 201" "$code" 201
S24_B=$(jqget '.task_id')
[ -n "$S24_B" ] && [ "$S24_B" != "null" ] || die "S24: task B id missing"

# Boss (admin) reads the kanban: 2 rows (at minimum), id DESC, fields correct.
code=$(req GET "/api/groups/$G/tasks" "$ADMIN")
check_http "S24: boss GET group tasks -> 200" "$code" 200
if jqtest --argjson a "$S24_A" --argjson b "$S24_B" --argjson w "$W" \
  '.tasks as $ts | ($ts | length >= 2)
   and ($ts[0].id > $ts[1].id)
   and ([ $ts[] | select(.id == $a or .id == $b) ]
     | all(.status == "queued"
       and .employee_id == $w and .employee_name != null
       and .requested_by_name == "boss" and .message_id > 0))'; then
  pass "S24: kanban rows correct (id desc, status, names, message_id)"
else
  fail "S24: kanban rows wrong: $(jq -c '.tasks' "$TMP/last" | head -c 500)"
fi

# Rows must NOT contain output field.
if jqtest '.tasks | all(has("output") | not)'; then
  pass "S24: kanban rows omit output field"
else
  fail "S24: kanban rows should not expose output"
fi

# Start + finish task A, verify status transitions in kanban.
code=$(req POST "/api/tasks/$S24_A/start" "$W_CRED")
check_http "S24: start task A" "$code" 200
code=$(req GET "/api/groups/$G/tasks" "$ADMIN")
if jqtest --argjson a "$S24_A" '.tasks[] | select(.id == $a) | .status == "running"'; then
  pass "S24: task A shows running after start"
else
  fail "S24: task A not running: $(jq -c '.tasks' "$TMP/last" | head -c 300)"
fi
code=$(req POST "/api/tasks/$S24_A/finish" "$W_CRED" '{"output":"s24 done"}')
check_http "S24: finish task A" "$code" 200
code=$(req GET "/api/groups/$G/tasks" "$ADMIN")
if jqtest --argjson a "$S24_A" '.tasks[] | select(.id == $a) | .status == "done"'; then
  pass "S24: task A shows done after finish"
else
  fail "S24: task A not done: $(jq -c '.tasks' "$TMP/last" | head -c 300)"
fi

# Limit parameter.
code=$(req GET "/api/groups/$G/tasks?limit=1" "$ADMIN")
check_http "S24: limit=1 -> 200" "$code" 200
if jqtest '.tasks | length == 1'; then
  pass "S24: limit=1 returns 1 row"
else
  fail "S24: limit=1 wrong count: $(jq '.tasks | length' "$TMP/last")"
fi

# Invalid limit values.
code=$(req GET "/api/groups/$G/tasks?limit=0" "$ADMIN")
check_http "S24: limit=0 -> 400" "$code" 400
code=$(req GET "/api/groups/$G/tasks?limit=abc" "$ADMIN")
check_http "S24: limit=abc -> 400" "$code" 400
code=$(req GET "/api/groups/$G/tasks?limit=501" "$ADMIN")
check_http "S24: limit=501 -> 400" "$code" 400

# Non-member (H) cannot access.
code=$(req GET "/api/groups/$G/tasks" "$H_CRED")
check_http "S24: non-member H -> 403" "$code" 403

# Member non-owner (L) can see the kanban.
code=$(req GET "/api/groups/$G/tasks" "$L_CRED")
check_http "S24: member L (non-owner) -> 200" "$code" 200

# Non-existent group.
code=$(req GET "/api/groups/99999/tasks" "$ADMIN")
check_http "S24: non-existent group -> 404" "$code" 404

section "17k. S25 — group task cancel (requested_by field + board-path cancel)"

# W's agent was stopped in section 10, so these tasks stay queued and the
# cancel outcome is deterministic. L has a dispatch route to W (17i).

# 1) Boss dispatches C to W (requested_by = 0).
code=$(req POST /api/messages "$ADMIN" "{\"group\":$G,\"kind\":\"dispatch\",\"to\":$W,\"body\":\"s25 boss cancel task\"}")
check_http "S25: boss group dispatch C -> 201" "$code" 201
S25_C=$(jqget '.task_id')
[ -n "$S25_C" ] && [ "$S25_C" != "null" ] || die "S25: task C id missing"

# 2) Leader L dispatches D to W (requested_by = L).
code=$(req POST /api/messages "$L_CRED" "{\"group\":$G,\"kind\":\"dispatch\",\"to\":$W,\"body\":\"s25 leader cancel task\"}")
check_http "S25: leader L group dispatch D -> 201" "$code" 201
S25_D=$(jqget '.task_id')
[ -n "$S25_D" ] && [ "$S25_D" != "null" ] || die "S25: task D id missing"

# 3) Board rows expose requested_by (C=0 boss, D=L) and both rows exist.
code=$(req GET "/api/groups/$G/tasks" "$ADMIN")
check_http "S25: board fetch (boss)" "$code" 200
if jqtest --argjson c "$S25_C" --argjson d "$S25_D" --argjson l "$L" \
  '([.tasks[] | select(.id == $c)] | length == 1 and all(.requested_by == 0))
   and ([.tasks[] | select(.id == $d)] | length == 1 and all(.requested_by == $l))'; then
  pass "S25: board rows carry requested_by (C=0, D=L)"
else
  fail "S25: requested_by wrong: $(jq -c '.tasks' "$TMP/last" | head -c 500)"
fi

# 4) H (not boss/requester/owner, not on W's reporting line) cannot cancel C.
code=$(req POST "/api/tasks/$S25_C/cancel" "$H_CRED" '{"reason":"s25 intruder"}')
check_http "S25: unauthorized H cancel C -> 403" "$code" 403

# 5) Boss cancels C -> 200 {ok,canceled}; board row flips to canceled.
code=$(req POST "/api/tasks/$S25_C/cancel" "$ADMIN" '{"reason":"smoke 17k"}')
check_http "S25: boss cancel C -> 200" "$code" 200
if jqtest '.ok == true and .status == "canceled"'; then
  pass "S25: cancel C response ok+canceled"
else
  fail "S25: cancel C response wrong: $(cat "$TMP/last")"
fi
code=$(req GET "/api/groups/$G/tasks" "$ADMIN")
if jqtest --argjson c "$S25_C" '[.tasks[] | select(.id == $c) | .status] | length == 1 and all(. == "canceled")'; then
  pass "S25: board row C flipped to canceled"
else
  fail "S25: board row C not canceled: $(jq -c '.tasks' "$TMP/last" | head -c 300)"
fi

# 6) Re-cancel of a terminal task -> 409.
code=$(req POST "/api/tasks/$S25_C/cancel" "$ADMIN" '{"reason":"again"}')
check_http "S25: re-cancel terminal C -> 409" "$code" 409

# 7) L (requester) cancels D -> 200; board row flips (requester path).
code=$(req POST "/api/tasks/$S25_D/cancel" "$L_CRED" '{"reason":"smoke 17k"}')
check_http "S25: requester L cancel D -> 200" "$code" 200
code=$(req GET "/api/groups/$G/tasks" "$ADMIN")
if jqtest --argjson d "$S25_D" '[.tasks[] | select(.id == $d) | .status] | length == 1 and all(. == "canceled")'; then
  pass "S25: board row D flipped to canceled (requester path)"
else
  fail "S25: board row D not canceled: $(jq -c '.tasks' "$TMP/last" | head -c 300)"
fi

# 8) M (W's direct leader on the reporting chain) cancels E -> 200 (chain path),
# the one CancelAllowed branch not exercised end-to-end elsewhere.
code=$(req POST /api/messages "$ADMIN" "{\"group\":$G,\"kind\":\"dispatch\",\"to\":$W,\"body\":\"s25 chain cancel task\"}")
check_http "S25: boss group dispatch E -> 201" "$code" 201
S25_E=$(jqget '.task_id')
[ -n "$S25_E" ] && [ "$S25_E" != "null" ] || die "S25: task E id missing"
code=$(req POST "/api/tasks/$S25_E/cancel" "$M_CRED" '{"reason":"smoke 17k chain"}')
check_http "S25: direct leader M cancel E -> 200 (chain path)" "$code" 200

section "17l. S26 — group manage (rename / remove member / delete group)"

# Fresh group: boss + L + M, then + W. W's agent is stopped (section 10), so
# a task dispatched to W stays queued (deterministic 409 for delete).
code=$(req POST /api/groups "$ADMIN" "{\"name\":\"s26-grp\",\"member_ids\":[$L,$M]}")
check_http "S26: create group (boss+L+M)" "$code" 201
S26_G=$(jqget '.id')
[ -n "$S26_G" ] && [ "$S26_G" != "null" ] || die "S26: group id missing"

# Add W and dispatch a task to W before any removals: the task must stay
# queued for the delete-409 check below.
code=$(req POST "/api/groups/$S26_G/members" "$ADMIN" "{\"employee_ids\":[$W]}")
check_http "S26: add W to group" "$code" 200
code=$(req POST /api/messages "$ADMIN" "{\"group\":$S26_G,\"kind\":\"dispatch\",\"to\":$W,\"body\":\"s26 delete guard\"}")
check_http "S26: dispatch task into group (W offline -> queued)" "$code" 201
S26_T=$(jqget '.task_id')
[ -n "$S26_T" ] && [ "$S26_T" != "null" ] || die "S26: task id missing"

# --- rename ---
code=$(req PATCH "/api/groups/$S26_G" "$ADMIN" '{"name":"  s26-renamed  "}')
check_http "S26: boss rename -> 200" "$code" 200
if jqtest '.ok == true'; then
  pass "S26: rename response ok"
else
  fail "S26: rename response wrong: $(cat "$TMP/last")"
fi
code=$(req GET "/api/groups/$S26_G" "$ADMIN")
if jqtest '.name == "s26-renamed"'; then
  pass "S26: group name updated (trimmed)"
else
  fail "S26: group name not updated: $(cat "$TMP/last")"
fi

S26_LONG=$(printf 'x%.0s' $(seq 1 65))
code=$(req PATCH "/api/groups/$S26_G" "$ADMIN" '{"name":"   "}')
check_http "S26: rename blank name -> 400" "$code" 400
code=$(req PATCH "/api/groups/$S26_G" "$ADMIN" "{\"name\":\"$S26_LONG\"}")
check_http "S26: rename 65-char name -> 400" "$code" 400
code=$(req PATCH "/api/groups/$S26_G" "$L_CRED" '{"name":"sneak"}')
check_http "S26: member L (non-manager) rename -> 403" "$code" 403
code=$(req PATCH "/api/groups/$S26_G" "$H_CRED" '{"name":"sneak"}')
check_http "S26: non-member H rename -> 403" "$code" 403
code=$(req PATCH "/api/groups/99999" "$ADMIN" '{"name":"x"}')
check_http "S26: rename unknown group -> 404" "$code" 404

# --- delete: guarded by open task + permission checks ---
code=$(req DELETE "/api/groups/$S26_G" "$ADMIN")
check_http "S26: delete with open task -> 409" "$code" 409
if jqtest '.error | contains("unfinished task")'; then
  pass "S26: 409 body mentions open task"
else
  fail "S26: 409 body wrong: $(cat "$TMP/last")"
fi
code=$(req DELETE "/api/groups/$S26_G" "$L_CRED")
check_http "S26: non-manager L delete -> 403" "$code" 403
code=$(req DELETE "/api/groups/$S26_G" "$H_CRED")
check_http "S26: non-member H delete -> 403" "$code" 403

# --- member removal ---
code=$(req DELETE "/api/groups/$S26_G/members/$W" "$ADMIN")
check_http "S26: boss removes W -> 200" "$code" 200
code=$(req GET "/api/groups/$S26_G" "$ADMIN")
if jqtest --argjson w "$W" '[.members[] | select(.employee_id == $w)] | length == 0'; then
  pass "S26: W no longer in members"
else
  fail "S26: W still a member: $(jq -c '.members' "$TMP/last")"
fi
code=$(req DELETE "/api/groups/$S26_G/members/0" "$ADMIN")
check_http "S26: remove boss (eid 0) -> 403" "$code" 403
code=$(req DELETE "/api/groups/$S26_G/members/$L" "$H_CRED")
check_http "S26: non-member H removes L -> 403" "$code" 403
code=$(req DELETE "/api/groups/$S26_G/members/$M" "$L_CRED")
check_http "S26: non-manager L removes M -> 403" "$code" 403
code=$(req DELETE "/api/groups/$S26_G/members/99999" "$ADMIN")
check_http "S26: remove unknown member -> 404" "$code" 404
code=$(req DELETE "/api/groups/$S26_G/members/$M" "$M_CRED")
check_http "S26: M self-leave -> 200" "$code" 200

# Group is now boss+L; removing L would leave 1 member.
code=$(req DELETE "/api/groups/$S26_G/members/$L" "$ADMIN")
check_http "S26: remove leaving <2 members -> 400" "$code" 400

# --- delete: now unblocked ---
code=$(req POST "/api/tasks/$S26_T/cancel" "$ADMIN" '{"reason":"s26 cleanup"}')
check_http "S26: cancel open task" "$code" 200
code=$(req DELETE "/api/groups/$S26_G" "$ADMIN")
check_http "S26: delete group -> 200" "$code" 200
if jqtest '.ok == true'; then
  pass "S26: delete response ok"
else
  fail "S26: delete response wrong: $(cat "$TMP/last")"
fi
code=$(req GET "/api/groups/$S26_G" "$ADMIN")
check_http "S26: deleted group -> 404" "$code" 404
code=$(req GET "/api/tasks/$S26_T" "$ADMIN")
if jqtest --argjson t "$S26_T" '.id == $t and .status == "canceled"'; then
  pass "S26: task survives group deletion (canceled)"
else
  fail "S26: task after delete wrong: $(cat "$TMP/last")"
fi

section "17m. S27 — task reassign + atomic status guard"

# X: fresh AI employee (DEPB) with NO agent process: its tasks stay queued
# deterministically and can be driven through the task API. Reassign targets:
# M (agent online, for the end-to-end completion path) and W (agent stopped in
# section 10, so reassigned-to-W tasks stay queued for deterministic asserts).
out=$(new_emp "s27x" "engineer" "ai" "$DEPB") || die "S27: create s27x failed"
S27_X=${out%% *}; S27_X_CRED=${out#* }

# --- T1: queued -> reassign to W (agent offline: stays queued, deterministic) ---
# Spec item: reassign queued -> 200, owner change, status=queued, output has note.
# Note: finish overwrites output (SetTaskStatus), so the note assertion is made
# in the queued state, before any agent can complete the task.
code=$(req POST /api/messages "$ADMIN" "{\"to\":$S27_X,\"kind\":\"dispatch\",\"body\":\"s27 t1\"}")
check_http "S27: boss 1:1 dispatch T1 to X -> 201" "$code" 201
S27_T1=$(jqget '.task_id')
[ -n "$S27_T1" ] && [ "$S27_T1" != "null" ] || die "S27: T1 task_id missing"

code=$(req POST "/api/tasks/$S27_T1/reassign" "$ADMIN" "{\"to\":$W,\"reason\":\"s27 queued\"}")
check_http "S27: reassign T1 X->W (queued) -> 200" "$code" 200
if jqtest --argjson w "$W" '.ok == true and .status == "queued" and .employee_id == $w'; then
  pass "S27: reassign T1 response (ok, status=queued, employee_id=W)"
else
  fail "S27: reassign T1 response wrong: $(cat "$TMP/last")"
fi
code=$(req GET "/api/tasks/$S27_T1" "$ADMIN")
if jqtest --argjson w "$W" '.employee_id == $w and .status == "queued" and .started_at == null and (.output | contains("reassigned"))'; then
  pass "S27: T1 queued->queued (owner W, started_at null, output has reassigned note)"
else
  fail "S27: T1 state after reassign wrong: $(cat "$TMP/last")"
fi

# --- T1-newowner: new owner (X, after W->X) can start+finish -> done ---
code=$(req POST "/api/tasks/$S27_T1/reassign" "$ADMIN" "{\"to\":$S27_X}")
check_http "S27: reassign T1 W->X -> 200" "$code" 200
code=$(req POST "/api/tasks/$S27_T1/start" "$S27_X_CRED")
check_http "S27: new owner X starts T1 -> 200" "$code" 200
code=$(req POST "/api/tasks/$S27_T1/finish" "$S27_X_CRED" '{"output":"s27 t1 by new owner"}')
check_http "S27: new owner X finish T1 -> 200 done" "$code" 200

# --- T1E: end-to-end — reassign to M (agent online), M's agent completes it ---
code=$(req POST /api/messages "$ADMIN" "{\"to\":$S27_X,\"kind\":\"dispatch\",\"body\":\"s27 t1e\"}")
check_http "S27: boss 1:1 dispatch T1E to X -> 201" "$code" 201
S27_T1E=$(jqget '.task_id')
[ -n "$S27_T1E" ] && [ "$S27_T1E" != "null" ] || die "S27: T1E task_id missing"

code=$(req POST "/api/tasks/$S27_T1E/reassign" "$ADMIN" "{\"to\":$M,\"reason\":\"s27 e2e\"}")
check_http "S27: reassign T1E X->M -> 200" "$code" 200
if jqtest --argjson m "$M" '.ok == true and .status == "queued" and .employee_id == $m'; then
  pass "S27: reassign T1E response (ok, status=queued, employee_id=M)"
else
  fail "S27: reassign T1E response wrong: $(cat "$TMP/last")"
fi

if wait_task "$S27_T1E" done 20; then
  pass "S27: new owner M's agent completed the reassigned task (end-to-end)"
else
  fail "S27: T1E not done after reassign to M"
fi
code=$(req GET "/api/tasks/$S27_T1E" "$ADMIN")
if jqtest --argjson m "$M" '.employee_id == $m and .status == "done"'; then
  pass "S27: T1E owner=M, status=done"
else
  fail "S27: T1E owner/status wrong: $(cat "$TMP/last")"
fi

# --- T2: running -> reassign to W (agent offline: deterministic) ---
code=$(req POST /api/messages "$ADMIN" "{\"to\":$S27_X,\"kind\":\"dispatch\",\"body\":\"s27 t2\"}")
check_http "S27: boss 1:1 dispatch T2 to X -> 201" "$code" 201
S27_T2=$(jqget '.task_id')
[ -n "$S27_T2" ] && [ "$S27_T2" != "null" ] || die "S27: T2 task_id missing"

code=$(req POST "/api/tasks/$S27_T2/start" "$S27_X_CRED")
check_http "S27: X starts T2 -> 200" "$code" 200

code=$(req POST "/api/tasks/$S27_T2/reassign" "$ADMIN" "{\"to\":$W,\"reason\":\"s27 running\"}")
check_http "S27: reassign T2 X->W (running) -> 200" "$code" 200
code=$(req GET "/api/tasks/$S27_T2" "$ADMIN")
if jqtest --argjson w "$W" '.employee_id == $w and .status == "queued" and .started_at == null and (.output | contains("s27 running"))'; then
  pass "S27: T2 running->queued (owner W, started_at cleared, reason in output)"
else
  fail "S27: T2 state after reassign wrong: $(cat "$TMP/last")"
fi

code=$(req POST "/api/tasks/$S27_T2/finish" "$S27_X_CRED" '{"output":"stale"}')
check_http "S27: old owner X finish T2 after reassign -> 403" "$code" 403

# --- T3: canceled -> reassign back to queued ---
code=$(req POST /api/messages "$ADMIN" "{\"to\":$S27_X,\"kind\":\"dispatch\",\"body\":\"s27 t3\"}")
check_http "S27: boss 1:1 dispatch T3 to X -> 201" "$code" 201
S27_T3=$(jqget '.task_id')
[ -n "$S27_T3" ] && [ "$S27_T3" != "null" ] || die "S27: T3 task_id missing"

code=$(req POST "/api/tasks/$S27_T3/cancel" "$ADMIN" '{"reason":"s27 pre-cancel"}')
check_http "S27: cancel T3 -> 200" "$code" 200
code=$(req POST "/api/tasks/$S27_T3/reassign" "$ADMIN" "{\"to\":$W}")
check_http "S27: reassign T3 X->W (canceled) -> 200" "$code" 200
code=$(req GET "/api/tasks/$S27_T3" "$ADMIN")
if jqtest --argjson w "$W" '.employee_id == $w and .status == "queued" and .finished_at == null and (.output | contains("reassigned"))'; then
  pass "S27: T3 canceled->queued (owner W, finished_at cleared, audit note)"
else
  fail "S27: T3 state after reassign wrong: $(cat "$TMP/last")"
fi

# --- T4: failed -> reassign back to queued ---
code=$(req POST /api/messages "$ADMIN" "{\"to\":$S27_X,\"kind\":\"dispatch\",\"body\":\"s27 t4\"}")
check_http "S27: boss 1:1 dispatch T4 to X -> 201" "$code" 201
S27_T4=$(jqget '.task_id')
[ -n "$S27_T4" ] && [ "$S27_T4" != "null" ] || die "S27: T4 task_id missing"

code=$(req POST "/api/tasks/$S27_T4/start" "$S27_X_CRED")
check_http "S27: X starts T4 -> 200" "$code" 200
code=$(req POST "/api/tasks/$S27_T4/fail" "$S27_X_CRED" '{"output":"s27 fail"}')
check_http "S27: X fails T4 -> 200" "$code" 200
code=$(req POST "/api/tasks/$S27_T4/reassign" "$ADMIN" "{\"to\":$W}")
check_http "S27: reassign T4 X->W (failed) -> 200" "$code" 200
code=$(req GET "/api/tasks/$S27_T4" "$ADMIN")
if jqtest --argjson w "$W" '.employee_id == $w and .status == "queued" and (.output | contains("s27 fail"))'; then
  pass "S27: T4 failed->queued (owner W, old output preserved)"
else
  fail "S27: T4 state after reassign wrong: $(cat "$TMP/last")"
fi

# --- T5: done is terminal -> reassign rejected (409) ---
code=$(req POST /api/messages "$ADMIN" "{\"to\":$S27_X,\"kind\":\"dispatch\",\"body\":\"s27 t5\"}")
check_http "S27: boss 1:1 dispatch T5 to X -> 201" "$code" 201
S27_T5=$(jqget '.task_id')
[ -n "$S27_T5" ] && [ "$S27_T5" != "null" ] || die "S27: T5 task_id missing"

code=$(req POST "/api/tasks/$S27_T5/start" "$S27_X_CRED")
check_http "S27: X starts T5 -> 200" "$code" 200
code=$(req POST "/api/tasks/$S27_T5/finish" "$S27_X_CRED" '{"output":"s27 done"}')
check_http "S27: X finishes T5 -> 200" "$code" 200
code=$(req POST "/api/tasks/$S27_T5/reassign" "$ADMIN" "{\"to\":$W}")
check_http "S27: reassign T5 (done) -> 409" "$code" 409

# --- negative matrix (T3 is W-owned + queued at this point) ---
code=$(req POST "/api/tasks/$S27_T3/reassign" "$ADMIN" "{\"to\":$W}")
check_http "S27: reassign to same owner -> 400" "$code" 400
msg=$(jqget '.error')
case "$msg" in
  *same\ owner*) pass "S27: same-owner 400 body explains no-op" ;;
  *)            fail "S27: same-owner 400 body wrong: $msg" ;;
esac

code=$(req POST "/api/tasks/$S27_T3/reassign" "$ADMIN" '{"to":0}')
check_http "S27: reassign to 0 -> 400" "$code" 400

code=$(req POST "/api/tasks/$S27_T3/reassign" "$ADMIN" '{"to":99999}')
check_http "S27: reassign to unknown employee -> 404" "$code" 404

code=$(req POST "/api/tasks/$S27_T3/reassign" "$H_CRED" "{\"to\":$W}")
check_http "S27: unauthorized H reassign -> 403" "$code" 403

code=$(req POST "/api/tasks/99999/reassign" "$ADMIN" "{\"to\":$W}")
check_http "S27: reassign unknown task -> 404" "$code" 404

# --- regression: cancel path unchanged (T4 is W-owned + queued) ---
code=$(req POST "/api/tasks/$S27_T4/cancel" "$ADMIN" '{"reason":"smoke 17m"}')
check_http "S27: cancel reassigned queued T4 -> 200" "$code" 200
code=$(req POST "/api/tasks/$S27_T4/cancel" "$ADMIN" '{"reason":"again"}')
check_http "S27: re-cancel terminal T4 -> 409" "$code" 409

section "17n. S28 — group task_status visibility (non-owner live status)"

# X: fresh AI employee with NO agent process: its tasks stay queued
# deterministically and can be driven through the task API. M (non-owner group
# member) never owns T, so its online agent is irrelevant. H: non-member.
out=$(new_emp "s28x" "engineer" "ai" "$DEPB") || die "S28: create s28x failed"
S28_X=${out%% *}; S28_X_CRED=${out#* }

code=$(req POST /api/groups "$ADMIN" "{\"name\":\"s28-grp\",\"member_ids\":[$S28_X,$M]}")
check_http "S28: create group (boss+X+M)" "$code" 201
S28_G=$(jqget '.id')
[ -n "$S28_G" ] && [ "$S28_G" != "null" ] || die "S28: group id missing"

code=$(req POST /api/messages "$ADMIN" "{\"group\":$S28_G,\"kind\":\"dispatch\",\"to\":$S28_X,\"body\":\"s28 live status\"}")
check_http "S28: boss dispatches T into group (queued)" "$code" 201
S28_T=$(jqget '.task_id')
[ -n "$S28_T" ] && [ "$S28_T" != "null" ] || die "S28: task id missing"

# --- 1) non-owner M: dispatch row carries task_id + live task_status=queued ---
code=$(req GET "/api/messages?group=$S28_G" "$M_CRED")
check_http "S28: M (member) reads group thread -> 200" "$code" 200
if jqtest --argjson t "$S28_T" '[.messages[] | select(.task_id == $t and .kind == "dispatch")] | length == 1 and .[0].task_status == "queued"'; then
  pass "S28: M sees dispatch row (task_id=T, task_status=queued)"
else
  fail "S28: M dispatch row wrong: $(cat "$TMP/last")"
fi

# --- 2) M's self-scoped task list must not contain T ---
code=$(req GET "/api/tasks?employee=$M" "$M_CRED")
check_http "S28: M self-scoped task list -> 200" "$code" 200
if jqtest --argjson t "$S28_T" '[.tasks[]? | select(.id == $t)] | length == 0'; then
  pass "S28: M self-scope excludes T (visibility unchanged)"
else
  fail "S28: M self-scope leaks T: $(cat "$TMP/last")"
fi

# --- 3) boss starts T -> M re-fetch shows running ---
code=$(req POST "/api/tasks/$S28_T/start" "$ADMIN")
check_http "S28: boss starts T -> 200" "$code" 200
code=$(req GET "/api/messages?group=$S28_G" "$M_CRED")
if jqtest --argjson t "$S28_T" '[.messages[] | select(.task_id == $t)] | all(.[]; .task_status == "running")'; then
  pass "S28: M re-fetch shows task_status=running"
else
  fail "S28: M running state wrong: $(cat "$TMP/last")"
fi

# --- 4) owner X finishes T -> M re-fetch shows done (dispatch + report rows) ---
code=$(req POST "/api/tasks/$S28_T/finish" "$S28_X_CRED" '{"output":"s28 done"}')
check_http "S28: owner X finishes T -> 200" "$code" 200
code=$(req GET "/api/messages?group=$S28_G" "$M_CRED")
if jqtest --argjson t "$S28_T" '[.messages[] | select(.task_id == $t)] | length >= 2 and all(.[]; .task_status == "done")'; then
  pass "S28: M re-fetch shows task_status=done (dispatch + report)"
else
  fail "S28: M done state wrong: $(cat "$TMP/last")"
fi

# --- 5) 1:1 regression: boss 1:1 dispatch row carries task_id, NO task_status ---
code=$(req POST /api/messages "$ADMIN" "{\"to\":$S28_X,\"kind\":\"dispatch\",\"body\":\"s28 1to1\"}")
check_http "S28: boss 1:1 dispatch T2 to X -> 201" "$code" 201
S28_T2=$(jqget '.task_id')
[ -n "$S28_T2" ] && [ "$S28_T2" != "null" ] || die "S28: T2 task_id missing"
code=$(req GET "/api/messages?with=$S28_X" "$ADMIN")
if jqtest --argjson t "$S28_T2" '[.messages[] | select(.task_id == $t)] | length == 1 and all(.[]; has("task_status") | not)'; then
  pass "S28: 1:1 dispatch row carries no task_status key"
else
  fail "S28: 1:1 row should have no task_status: $(cat "$TMP/last")"
fi

# --- 6) non-member H is locked out of the group thread ---
code=$(req GET "/api/messages?group=$S28_G" "$H_CRED")
check_http "S28: non-member H reads group thread -> 403" "$code" 403

section "17o. S29 — group task project binding + board project filter"

# X: fresh AI employee with NO agent process (s28x pattern). M reused as
# non-owner member; dedicated group s29-grp keeps the board assertions isolated.
code=$(req POST /api/projects "$ADMIN" '{"name":"s29-proj"}')
check_http "S29: create project s29-proj" "$code" 201
S29_P=$(jqget '.id')
[ -n "$S29_P" ] && [ "$S29_P" != "null" ] || die "S29: project id missing"

out=$(new_emp "s29x" "engineer" "ai" "$DEPB") || die "S29: create s29x failed"
S29_X=${out%% *}; S29_X_CRED=${out#* }

code=$(req POST /api/groups "$ADMIN" "{\"name\":\"s29-grp\",\"member_ids\":[$S29_X,$M]}")
check_http "S29: create group (boss+x+M)" "$code" 201
S29_G=$(jqget '.id')
[ -n "$S29_G" ] && [ "$S29_G" != "null" ] || die "S29: group id missing"

# --- 1) boss group dispatch with project_id -> 201, task carries the project ---
code=$(req POST /api/messages "$ADMIN" "{\"group\":$S29_G,\"kind\":\"dispatch\",\"to\":$S29_X,\"project_id\":$S29_P,\"body\":\"s29 dispatch with project\"}")
check_http "S29: boss group dispatch with project_id -> 201" "$code" 201
S29_T=$(jqget '.task_id')
[ -n "$S29_T" ] && [ "$S29_T" != "null" ] || die "S29: task id missing"
code=$(req GET "/api/tasks/$S29_T" "$ADMIN")
check_http "S29: GET task detail -> 200" "$code" 200
if jqtest --argjson p "$S29_P" '.project_id == $p'; then
  pass "S29: task project_id persisted"
else
  fail "S29: task project_id wrong: $(cat "$TMP/last")"
fi

# --- 2) boss group dispatch without project -> 201 (unchanged default) ---
code=$(req POST /api/messages "$ADMIN" "{\"group\":$S29_G,\"kind\":\"dispatch\",\"to\":$S29_X,\"body\":\"s29 dispatch no project\"}")
check_http "S29: boss group dispatch without project -> 201" "$code" 201
S29_T2=$(jqget '.task_id')
[ -n "$S29_T2" ] && [ "$S29_T2" != "null" ] || die "S29: task2 id missing"

# --- 3) board no param -> both rows ---
code=$(req GET "/api/groups/$S29_G/tasks" "$ADMIN")
check_http "S29: board (no filter) -> 200" "$code" 200
if jqtest --argjson t "$S29_T" --argjson t2 "$S29_T2" \
  '(.tasks | length == 2) and ([.tasks[].id] | index($t)) != null and ([.tasks[].id] | index($t2)) != null'; then
  pass "S29: board (all) shows both rows"
else
  fail "S29: board (all) rows wrong: $(jq -c '.tasks' "$TMP/last" | head -c 400)"
fi

# --- 4) ?project=P -> only the bound task, project_id echoed ---
code=$(req GET "/api/groups/$S29_G/tasks?project=$S29_P" "$ADMIN")
check_http "S29: board ?project=P -> 200" "$code" 200
if jqtest --argjson t "$S29_T" --argjson p "$S29_P" \
  '(.tasks | length == 1) and .tasks[0].id == $t and .tasks[0].project_id == $p'; then
  pass "S29: board ?project=P filters to that project (project_id echoed)"
else
  fail "S29: board ?project=P wrong: $(jq -c '.tasks' "$TMP/last" | head -c 400)"
fi

# --- 5) ?project=0 -> only the unbound task, project_id null, no project_name key ---
code=$(req GET "/api/groups/$S29_G/tasks?project=0" "$ADMIN")
check_http "S29: board ?project=0 -> 200" "$code" 200
if jqtest --argjson t2 "$S29_T2" \
  '(.tasks | length == 1) and .tasks[0].id == $t2 and .tasks[0].project_id == null and ((.tasks[0] | has("project_name")) | not)'; then
  pass "S29: board ?project=0 -> unbound task only, project_id null, no project_name key"
else
  fail "S29: board ?project=0 wrong: $(jq -c '.tasks' "$TMP/last" | head -c 400)"
fi

# --- 6) row project_name == "s29-proj" (from the ?project=P fetch above) ---
code=$(req GET "/api/groups/$S29_G/tasks?project=$S29_P" "$ADMIN")
if jqtest --arg n "s29-proj" '.tasks[0].project_name == $n'; then
  pass "S29: board row carries project_name"
else
  fail "S29: project_name wrong: $(jq -c '.tasks' "$TMP/last" | head -c 400)"
fi

# --- 7) unknown project -> empty list ---
code=$(req GET "/api/groups/$S29_G/tasks?project=99999" "$ADMIN")
check_http "S29: board ?project=99999 -> 200" "$code" 200
if jqtest '(.tasks | length == 0)'; then
  pass "S29: board ?project=unknown -> empty list"
else
  fail "S29: board ?project=unknown not empty: $(jq -c '.tasks' "$TMP/last" | head -c 400)"
fi

# --- 8) invalid project params -> 400 ---
code=$(req GET "/api/groups/$S29_G/tasks?project=abc" "$ADMIN")
check_http "S29: board ?project=abc -> 400" "$code" 400
code=$(req GET "/api/groups/$S29_G/tasks?project=-1" "$ADMIN")
check_http "S29: board ?project=-1 -> 400" "$code" 400

# --- 9) group kind=chat with project_id -> 400 (unchanged) ---
code=$(req POST /api/messages "$ADMIN" "{\"group\":$S29_G,\"kind\":\"chat\",\"project_id\":$S29_P,\"body\":\"s29 chat with project\"}")
check_http "S29: group chat with project_id -> 400" "$code" 400

# --- 10) group dispatch with unknown project_id -> 400 ---
code=$(req POST /api/messages "$ADMIN" "{\"group\":$S29_G,\"kind\":\"dispatch\",\"to\":$S29_X,\"project_id\":99999,\"body\":\"s29 unknown project\"}")
check_http "S29: group dispatch unknown project_id -> 400" "$code" 400

# --- 11) regression: 1:1 dispatch with project_id -> 201, project persisted ---
code=$(req POST /api/messages "$ADMIN" "{\"to\":$S29_X,\"kind\":\"dispatch\",\"project_id\":$S29_P,\"body\":\"s29 1to1 with project\"}")
check_http "S29: 1:1 dispatch with project_id -> 201" "$code" 201
S29_T3=$(jqget '.task_id')
[ -n "$S29_T3" ] && [ "$S29_T3" != "null" ] || die "S29: 1:1 task id missing"
code=$(req GET "/api/tasks/$S29_T3" "$ADMIN")
if jqtest --argjson p "$S29_P" '.project_id == $p'; then
  pass "S29: 1:1 task project_id persisted (existing semantics)"
else
  fail "S29: 1:1 task project_id wrong: $(cat "$TMP/last")"
fi

# --- 12) regression: group message with task_id still 400 ---
code=$(req POST /api/messages "$ADMIN" "{\"group\":$S29_G,\"kind\":\"chat\",\"task_id\":$S29_T,\"body\":\"s29 group with task_id\"}")
check_http "S29: group message with task_id -> 400 (unchanged)" "$code" 400

section "17p. S30 — group SLA/priority policy"

# X: fresh HUMAN employee with NO agent process (tasks stay queued forever,
# so the scheduler's SLA scan can observe them). M reused as non-creator
# member; dedicated group s30-grp isolates the policy assertions.
out=$(new_emp "s30x" "engineer" "human" "$DEPB") || die "S30: create s30x failed"
S30_X=${out%% *}; S30_X_CRED=${out#* }

code=$(req POST /api/groups "$ADMIN" "{\"name\":\"s30-grp\",\"member_ids\":[$S30_X,$M]}")
check_http "S30: create group (boss+x+M)" "$code" 201
S30_G=$(jqget '.id')
[ -n "$S30_G" ] && [ "$S30_G" != "null" ] || die "S30: group id missing"

# --- 1) default_priority only -> 200; readback echoes full policy ---
code=$(req POST "/api/groups/$S30_G/policy" "$ADMIN" '{"default_priority":7}')
check_http "S30: POST /policy {default_priority:7} -> 200" "$code" 200
if jqtest '.ok == true and .default_priority == 7 and .sla_timeout_seconds == 0'; then
  pass "S30: policy readback (ok, default_priority=7, sla_timeout_seconds=0)"
else
  fail "S30: policy readback wrong: $(cat "$TMP/last")"
fi

# --- 2) GET group detail carries the new keys ---
code=$(req GET "/api/groups/$S30_G" "$ADMIN")
check_http "S30: GET group detail -> 200" "$code" 200
if jqtest '.default_priority == 7 and .sla_timeout_seconds == 0'; then
  pass "S30: GET group detail carries default_priority=7, sla_timeout_seconds=0"
else
  fail "S30: GET group detail policy keys wrong: $(cat "$TMP/last")"
fi

# --- 3) group dispatch without priority -> task inherits group default (7) ---
code=$(req POST /api/messages "$ADMIN" "{\"group\":$S30_G,\"kind\":\"dispatch\",\"to\":$S30_X,\"body\":\"s30 inherit default\"}")
check_http "S30: group dispatch without priority -> 201" "$code" 201
S30_T1=$(jqget '.task_id')
[ -n "$S30_T1" ] && [ "$S30_T1" != "null" ] || die "S30: task1 id missing"
code=$(req GET "/api/tasks/$S30_T1" "$ADMIN")
if jqtest '.priority == 7'; then
  pass "S30: group task inherits group default priority=7"
else
  fail "S30: group task priority wrong (want 7): $(cat "$TMP/last")"
fi

# --- 4) group dispatch explicit priority=2 -> overrides group default ---
code=$(req POST /api/messages "$ADMIN" "{\"group\":$S30_G,\"kind\":\"dispatch\",\"to\":$S30_X,\"priority\":2,\"body\":\"s30 explicit priority\"}")
check_http "S30: group dispatch explicit priority=2 -> 201" "$code" 201
S30_T2=$(jqget '.task_id')
[ -n "$S30_T2" ] && [ "$S30_T2" != "null" ] || die "S30: task2 id missing"
code=$(req GET "/api/tasks/$S30_T2" "$ADMIN")
if jqtest '.priority == 2'; then
  pass "S30: explicit priority=2 overrides group default"
else
  fail "S30: explicit priority wrong (want 2): $(cat "$TMP/last")"
fi

# --- 5) regression: 1:1 dispatch without priority -> 0 (no group default) ---
code=$(req POST /api/messages "$ADMIN" "{\"to\":$S30_X,\"kind\":\"dispatch\",\"body\":\"s30 1to1 regression\"}")
check_http "S30: 1:1 dispatch without priority -> 201" "$code" 201
S30_T3=$(jqget '.task_id')
[ -n "$S30_T3" ] && [ "$S30_T3" != "null" ] || die "S30: 1:1 task id missing"
code=$(req GET "/api/tasks/$S30_T3" "$ADMIN")
if jqtest '.priority == 0'; then
  pass "S30: 1:1 task priority=0 (group default does not apply)"
else
  fail "S30: 1:1 task priority wrong (want 0): $(cat "$TMP/last")"
fi

# --- 6) negative matrix on /policy ---
code=$(req POST "/api/groups/$S30_G/policy" "$ADMIN" '{}')
check_http "S30: POST /policy {} (no field) -> 400" "$code" 400
code=$(req POST "/api/groups/$S30_G/policy" "$ADMIN" '{"default_priority":100}')
check_http "S30: default_priority=100 -> 400" "$code" 400
code=$(req POST "/api/groups/$S30_G/policy" "$ADMIN" '{"sla_timeout_seconds":-1}')
check_http "S30: sla_timeout_seconds=-1 -> 400" "$code" 400
code=$(req POST "/api/groups/$S30_G/policy" "$ADMIN" '{"sla_timeout_seconds":99999999}')
check_http "S30: sla_timeout_seconds=99999999 -> 400" "$code" 400
code=$(req POST "/api/groups/$S30_G/policy" "$M_CRED" '{"default_priority":3}')
check_http "S30: non-creator member POST /policy -> 403" "$code" 403
code=$(req POST "/api/groups/99999/policy" "$ADMIN" '{"default_priority":1}')
check_http "S30: unknown group POST /policy -> 404" "$code" 404

# --- 7) SLA end-to-end: 1s threshold, scheduler tick 1s -> exactly one system
#       reminder per due open task, in the group thread, not duplicated.
#       (T1/T2 are also open+due and each get their own one-time reminder;
#       the assertion below is scoped to the fresh task T4.)
code=$(req POST "/api/groups/$S30_G/policy" "$ADMIN" '{"sla_timeout_seconds":1}')
check_http "S30: POST /policy {sla_timeout_seconds:1} -> 200" "$code" 200
code=$(req POST /api/messages "$ADMIN" "{\"group\":$S30_G,\"kind\":\"dispatch\",\"to\":$S30_X,\"body\":\"s30 sla watch\"}")
check_http "S30: group dispatch for SLA watch -> 201" "$code" 201
S30_T4=$(jqget '.task_id')
[ -n "$S30_T4" ] && [ "$S30_T4" != "null" ] || die "S30: sla task id missing"
sla_count() { # group, task -> count of system reminders matching "Task #<id> ("
  req GET "/api/messages?group=$1" "$ADMIN" >"$TMP/null"
  jq --argjson t "$2" --arg p "Task #" --arg s " (" \
    '[.messages[] | select(.kind == "system" and (.body | contains($p + ($t | tostring) + $s)))] | length' \
    "$TMP/last"
}
found=""
i=0
while [ -z "$found" ] && [ $i -lt 24 ]; do
  found=$(sla_count "$S30_G" "$S30_T4")
  [ "$found" = "0" ] && found=""
  [ -n "$found" ] && break
  i=$((i + 1))
  sleep 0.5
done
if [ "$found" != "0" ] && [ -n "$found" ]; then
  pass "S30: system SLA reminder appears in group thread (task #$S30_T4, within 12s)"
else
  fail "S30: no system SLA reminder for task $S30_T4 within 12s"
fi
# one reminder per task, lifetime: count must stay stable 3s later.
sleep 3
count_stable=$(sla_count "$S30_G" "$S30_T4")
if [ "$count_stable" = "1" ]; then
  pass "S30: SLA reminder not duplicated 3s later (count stable at 1)"
else
  fail "S30: SLA reminder count changed (got $count_stable, want 1)"
fi

# --- 8) disable SLA -> 200; a fresh task stays silent ---
code=$(req POST "/api/groups/$S30_G/policy" "$ADMIN" '{"sla_timeout_seconds":0}')
check_http "S30: POST /policy {sla_timeout_seconds:0} (off) -> 200" "$code" 200
code=$(req POST /api/messages "$ADMIN" "{\"group\":$S30_G,\"kind\":\"dispatch\",\"to\":$S30_X,\"body\":\"s30 sla off\"}")
check_http "S30: group dispatch with SLA off -> 201" "$code" 201
S30_T5=$(jqget '.task_id')
[ -n "$S30_T5" ] && [ "$S30_T5" != "null" ] || die "S30: sla-off task id missing"
sleep 3
count_off=$(sla_count "$S30_G" "$S30_T5")
if [ "$count_off" = "0" ]; then
  pass "S30: no SLA reminder after SLA disabled (task #$S30_T5 silent 3s)"
else
  fail "S30: unexpected SLA reminder after disable (got $count_off, want 0)"
fi

section "17q. S31 — department channel (auto-create / rename / cascade delete)"

# s31e: fresh HUMAN employee — the scheduler only injects selfreview tasks
# for kind=ai, so s31e stays task-free and is deterministically deletable for
# the clean-delete path (store DeleteEmployee blocks on ANY task row,
# pre-existing contract). The department 409 open-task guard is exercised by
# s31w (ai, step 14) instead. W (DEPB) is a known non-member of the
# s31dept channel.

# --- 1) create top-level dept -> 201 {id, group_id} ---
code=$(req POST /api/departments "$ADMIN" '{"name":"s31dept"}')
check_http "S31: create dept s31dept -> 201" "$code" 201
S31_DEPT=$(jqget '.id')
S31_G=$(jqget '.group_id')
[ -n "$S31_DEPT" ] && [ "$S31_DEPT" != "null" ] || die "S31: dept id missing"
[ -n "$S31_G" ] && [ "$S31_G" != "null" ] || die "S31: group_id missing"

# --- 2) channel detail: kind=channel, created_by=0, name=dept name, boss only ---
code=$(req GET "/api/groups/$S31_G" "$ADMIN")
check_http "S31: GET dept channel" "$code" 200
if jqtest '.kind == "channel" and .created_by == 0 and .name == "s31dept"'; then
  pass "S31: channel kind=channel created_by=0 name=s31dept"
else
  fail "S31: channel detail wrong: $(cat "$TMP/last")"
fi
if jqtest '[.members[].employee_id] | sort == [0]'; then
  pass "S31: channel initial members == [0] (boss only)"
else
  fail "S31: channel members wrong: $(jq -c '.members' "$TMP/last")"
fi

# --- 3) system message in the channel thread ---
code=$(req GET "/api/messages?group=$S31_G" "$ADMIN")
check_http "S31: channel thread fetch (boss)" "$code" 200
if jqtest '[.messages[] | select(.kind == "system" and (.body | contains("department channel") and contains("s31dept")))] | length == 1'; then
  pass "S31: system msg 'boss created a department channel \"s31dept\"'"
else
  fail "S31: system msg missing/wrong: $(jq -c '.messages' "$TMP/last")"
fi

# --- 4) create negatives: duplicate / too long / blank ---
code=$(req POST /api/departments "$ADMIN" '{"name":"s31dept"}')
check_http "S31: same-level duplicate name -> 400" "$code" 400
S31_LONG=$(printf 'x%.0s' $(seq 1 65))
code=$(req POST /api/departments "$ADMIN" "{\"name\":\"$S31_LONG\"}")
check_http "S31: 65-rune name -> 400" "$code" 400
code=$(req POST /api/departments "$ADMIN" '{"name":"   "}')
check_http "S31: blank name -> 400" "$code" 400

# --- 5) adminOnly regression: employee cannot create a dept ---
code=$(req POST /api/departments "$M_CRED" '{"name":"sneak"}')
check_http "S31: employee create dept -> 403" "$code" 403

# --- 6) new employee in the dept auto-joins its channel ---
out=$(new_emp "s31e" engineer human "$S31_DEPT") || die "S31: create s31e failed"
S31_E=${out%% *}; S31_E_CRED=${out#* }
code=$(req GET "/api/groups/$S31_G" "$ADMIN")
if jqtest --argjson e "$S31_E" \
    '([.members[] | select(.employee_id == 0 or .employee_id == $e)] | length == 2) and ([.members[]] | length == 2)'; then
  pass "S31: new employee auto-joined dept channel (members == [0, s31e])"
else
  fail "S31: s31e not auto-joined: $(jq -c '.members' "$TMP/last")"
fi

# --- 7) auto-joined employee can send in the channel ---
code=$(req POST /api/messages "$S31_E_CRED" "{\"group\":$S31_G,\"kind\":\"chat\",\"body\":\"s31 auto-join check\"}")
check_http "S31: s31e sends in dept channel -> 201" "$code" 201

# --- 8) non-member (W, eng-dev) is blocked from the channel ---
code=$(req POST /api/messages "$W_CRED" "{\"group\":$S31_G,\"kind\":\"chat\",\"body\":\"intruder\"}")
check_http "S31: non-member W sends -> 403" "$code" 403

# --- 9) GET /api/departments carries group_id (fresh + setup regression) ---
code=$(req GET /api/departments "$ADMIN")
check_http "S31: GET /api/departments" "$code" 200
if jqtest --argjson d "$S31_DEPT" --argjson g "$S31_G" \
    '[.departments[] | select(.id == $d and .group_id == $g)] | length == 1'; then
  pass "S31: fresh dept row carries group_id"
else
  fail "S31: dept row group_id wrong: $(jq -c --argjson d "$S31_DEPT" '.departments[] | select(.id == $d)' "$TMP/last")"
fi
if jqtest --argjson d "$DEPA" \
    '[.departments[] | select(.id == $d and .group_id != null)] | length == 1'; then
  pass "S31: setup dept eng-root carries group_id (auto-channel regression)"
else
  fail "S31: eng-root group_id missing: $(jq -c --argjson d "$DEPA" '.departments[] | select(.id == $d)' "$TMP/last")"
fi

# --- 10) rename dept -> 200 {ok,name,group_id} + channel name synced ---
code=$(req PUT "/api/departments/$S31_DEPT" "$ADMIN" '{"name":"  s31dept2  "}')
check_http "S31: rename dept -> 200" "$code" 200
if jqtest --argjson g "$S31_G" '.ok == true and .name == "s31dept2" and .group_id == $g'; then
  pass "S31: rename response (ok, trimmed name, group_id)"
else
  fail "S31: rename response wrong: $(cat "$TMP/last")"
fi
code=$(req GET "/api/groups/$S31_G" "$ADMIN")
if jqtest '.name == "s31dept2"'; then
  pass "S31: channel name synced to s31dept2"
else
  fail "S31: channel name not synced: $(cat "$TMP/last")"
fi

# --- 11) rename negatives: blank / too long / same-level dup / unknown / non-admin ---
code=$(req PUT "/api/departments/$S31_DEPT" "$ADMIN" '{"name":"   "}')
check_http "S31: rename blank -> 400" "$code" 400
code=$(req PUT "/api/departments/$S31_DEPT" "$ADMIN" "{\"name\":\"$S31_LONG\"}")
check_http "S31: rename 65-rune -> 400" "$code" 400
code=$(req PUT "/api/departments/$S31_DEPT" "$ADMIN" '{"name":"eng-root"}')
check_http "S31: rename to same-level dup (eng-root) -> 400" "$code" 400
code=$(req PUT "/api/departments/99999" "$ADMIN" '{"name":"x"}')
check_http "S31: rename unknown dept -> 404" "$code" 404
code=$(req PUT "/api/departments/$S31_DEPT" "$M_CRED" '{"name":"sneak"}')
check_http "S31: employee rename -> 403" "$code" 403

# --- 12) delete blocked while the dept still has s31e -> 400 ---
code=$(req DELETE "/api/departments/$S31_DEPT" "$ADMIN")
check_http "S31: delete dept with employee -> 400" "$code" 400

# --- 13) clean delete path: fresh dept + message-less employee -> 200 ---
# NOTE: s31e itself is NOT deletable here — it sent a chat message in step 7,
# and messages.from_id/to_id hold bare FKs to employees (no ON DELETE action,
# pre-existing schema design), so the employee-delete fails on the FK. The
# clean cascade is therefore verified with a fresh, message-less employee.
# (s31e/s31dept stay in the throwaway smoke DB; harmless.)
code=$(req POST /api/departments "$ADMIN" '{"name":"s31clean"}')
check_http "S31: create s31clean dept" "$code" 201
S31_CLEAN=$(jqget '.id')
S31_GC=$(jqget '.group_id')
[ -n "$S31_CLEAN" ] && [ "$S31_CLEAN" != "null" ] || die "S31: s31clean dept id missing"
[ -n "$S31_GC" ] && [ "$S31_GC" != "null" ] || die "S31: s31clean group_id missing"
out=$(new_emp "s31c" engineer human "$S31_CLEAN") || die "S31: create s31c failed"
S31_C=${out%% *}
code=$(req DELETE "/api/employees/$S31_C" "$ADMIN")
check_http "S31: delete employee s31c (no task/message history)" "$code" 200
code=$(req DELETE "/api/departments/$S31_CLEAN" "$ADMIN")
check_http "S31: delete s31clean (no emp, no open tasks) -> 200" "$code" 200
if jqtest '.ok == true'; then
  pass "S31: delete response ok"
else
  fail "S31: delete response wrong: $(cat "$TMP/last")"
fi
code=$(req GET "/api/groups/$S31_GC" "$ADMIN")
check_http "S31: deleted channel -> 404" "$code" 404
code=$(req GET /api/departments "$ADMIN")
if jqtest '[.departments[] | select(.name == "s31clean")] | length == 0'; then
  pass "S31: s31clean no longer in list"
else
  fail "S31: s31clean still listed: $(jq -c '.departments' "$TMP/last")"
fi

# --- 14) open-task guard: dept WITHOUT employees but with a channel task ---
# Spec check order is child-depts -> employees -> open tasks, so the 409 case
# needs a task anchored to the channel whose target is NOT a department
# employee: boss adds s31w (lives in eng-dev) to the channel, then dispatches.
code=$(req POST /api/departments "$ADMIN" '{"name":"s31task"}')
check_http "S31: create s31task dept" "$code" 201
S31_DEPT2=$(jqget '.id')
S31_G2=$(jqget '.group_id')
[ -n "$S31_DEPT2" ] && [ "$S31_DEPT2" != "null" ] || die "S31: s31task dept id missing"
[ -n "$S31_G2" ] && [ "$S31_G2" != "null" ] || die "S31: s31task group_id missing"
out=$(new_emp "s31w" engineer ai "$DEPB") || die "S31: create s31w failed"
S31_W=${out%% *}
code=$(req POST "/api/groups/$S31_G2/members" "$ADMIN" "{\"employee_ids\":[$S31_W]}")
check_http "S31: add s31w (other dept) to channel" "$code" 200
code=$(req POST /api/messages "$ADMIN" "{\"group\":$S31_G2,\"kind\":\"dispatch\",\"to\":$S31_W,\"body\":\"s31 dept task\"}")
check_http "S31: in-channel dispatch (no agent -> queued) -> 201" "$code" 201
S31_T=$(jqget '.task_id')
[ -n "$S31_T" ] && [ "$S31_T" != "null" ] || die "S31: task id missing"

# --- 15) delete blocked by open task -> 409 + S26 wording ---
code=$(req DELETE "/api/departments/$S31_DEPT2" "$ADMIN")
check_http "S31: delete with open task -> 409" "$code" 409
if jqtest '.error | contains("unfinished task")'; then
  pass "S31: 409 body mentions open task"
else
  fail "S31: 409 body wrong: $(cat "$TMP/last")"
fi

# --- 16) cancel the task, then delete -> 200; channel gone; task preserved ---
code=$(req POST "/api/tasks/$S31_T/cancel" "$ADMIN" '{"reason":"s31 cleanup"}')
check_http "S31: cancel open task" "$code" 200
code=$(req DELETE "/api/departments/$S31_DEPT2" "$ADMIN")
check_http "S31: delete s31task (after cancel) -> 200" "$code" 200
code=$(req GET "/api/groups/$S31_G2" "$ADMIN")
check_http "S31: s31task channel gone -> 404" "$code" 404
code=$(req GET /api/departments "$ADMIN")
if jqtest '[.departments[] | select(.name == "s31task")] | length == 0'; then
  pass "S31: s31task no longer in list"
else
  fail "S31: s31task still listed: $(jq -c '.departments' "$TMP/last")"
fi
code=$(req GET "/api/tasks/$S31_T" "$ADMIN")
if jqtest --argjson t "$S31_T" '.id == $t and .status == "canceled" and .group_id == null'; then
  pass "S31: task survives dept deletion (canceled, group_id null)"
else
  fail "S31: task after delete wrong: $(cat "$TMP/last")"
fi

# --- 17) legacy dept (group_id NULL): employee create + rename + delete ---
sqlite3 "$DB" "INSERT INTO departments (name) VALUES ('s31legacy');"
S31_LEG=$(sqlite3 "$DB" "SELECT id FROM departments WHERE name = 's31legacy';")
[ -n "$S31_LEG" ] || die "S31: legacy dept insert failed"
out=$(new_emp "s31g" engineer human "$S31_LEG") || die "S31: create s31g in legacy dept failed"
S31_EMP3=${out%% *}
code=$(req PUT "/api/departments/$S31_LEG" "$ADMIN" '{"name":"s31legacy2"}')
check_http "S31: legacy dept rename -> 200" "$code" 200
if jqtest '.ok == true and .name == "s31legacy2" and .group_id == null'; then
  pass "S31: legacy rename response group_id=null"
else
  fail "S31: legacy rename wrong: $(cat "$TMP/last")"
fi
code=$(req DELETE "/api/employees/$S31_EMP3" "$ADMIN")
check_http "S31: delete employee s31g" "$code" 200
code=$(req DELETE "/api/departments/$S31_LEG" "$ADMIN")
check_http "S31: legacy dept delete -> 200" "$code" 200
code=$(req GET /api/departments "$ADMIN")
if jqtest '[.departments[] | select(.name == "s31legacy2")] | length == 0'; then
  pass "S31: legacy dept gone from list"
else
  fail "S31: legacy dept still listed: $(jq -c '.departments' "$TMP/last")"
fi

# ============================================================
# 17r) S32 — group/channel name search (GET /api/search .groups)
# ============================================================
section "17r. S32 — group name search"

# --- 1) setup: group + channel the caller is a member of ---
code=$(req POST /api/groups "$ADMIN" "{\"name\":\"s32alpha\",\"member_ids\":[$L,$M]}")
check_http "S32: create group s32alpha -> 201" "$code" 201
S32_G1=$(jqget '.id')
[ -n "$S32_G1" ] || die "S32: create s32alpha failed"
code=$(req POST /api/groups "$ADMIN" "{\"name\":\"s32chan\",\"kind\":\"channel\",\"member_ids\":[$W]}")
check_http "S32: create channel s32chan -> 201" "$code" 201
S32_G2=$(jqget '.id')
[ -n "$S32_G2" ] || die "S32: create s32chan failed"

# --- 2) boss search: both names hit, kind + member_count correct ---
code=$(req GET "/api/search?q=s32alpha" "$ADMIN")
check_http "S32: search s32alpha -> 200" "$code" 200
if jqtest --argjson g "$S32_G1" '[.groups[] | select(.group_id == $g)] | length == 1'; then
  pass "S32: s32alpha in groups"
else
  fail "S32: s32alpha missing: $(jq -c '.groups' "$TMP/last")"
fi
if jqtest --argjson g "$S32_G1" '.groups[] | select(.group_id == $g) | .name == "s32alpha" and .kind == "group" and .member_count == 3'; then
  pass "S32: s32alpha row name/kind/member_count"
else
  fail "S32: s32alpha row wrong: $(jq -c '.groups[] | select(.group_id == $S32_G1)' "$TMP/last" 2>/dev/null || cat "$TMP/last")"
fi
code=$(req GET "/api/search?q=s32chan" "$ADMIN")
check_http "S32: search s32chan -> 200" "$code" 200
if jqtest --argjson g "$S32_G2" '.groups[] | select(.group_id == $g) | .kind == "channel" and .member_count == 2'; then
  pass "S32: s32chan kind=channel member_count=2"
else
  fail "S32: s32chan row wrong: $(cat "$TMP/last")"
fi

# --- 3) member employee sees own groups ---
code=$(req GET "/api/search?q=s32alpha" "$L_CRED")
check_http "S32: member L search -> 200" "$code" 200
if jqtest --argjson g "$S32_G1" '[.groups[] | select(.group_id == $g)] | length == 1'; then
  pass "S32: member L sees s32alpha"
else
  fail "S32: member L missing group: $(jq -c '.groups' "$TMP/last")"
fi
code=$(req GET "/api/search?q=s32chan" "$W_CRED")
check_http "S32: member W search -> 200" "$code" 200
if jqtest --argjson g "$S32_G2" '[.groups[] | select(.group_id == $g)] | length == 1'; then
  pass "S32: member W sees s32chan"
else
  fail "S32: member W missing channel: $(jq -c '.groups' "$TMP/last")"
fi

# --- 4) non-member employee sees nothing (visibility scoping) ---
code=$(req GET "/api/search?q=s32alpha" "$H_CRED")
check_http "S32: non-member H search -> 200" "$code" 200
if jqtest '.groups | length == 0'; then
  pass "S32: non-member sees no groups"
else
  fail "S32: non-member leak: $(jq -c '.groups' "$TMP/last")"
fi

# --- 5) % stays literal (LIKE escape): s32wild% must not wildcard-match s32wildA ---
req POST /api/groups "$ADMIN" "{\"name\":\"s32wildA\",\"member_ids\":[$L]}" >/dev/null
S32_GA=$(jqget '.id')
req POST /api/groups "$ADMIN" '{"name":"s32wild%","member_ids":['"$L"']}' >/dev/null
S32_GP=$(jqget '.id')
[ -n "$S32_GA" ] && [ -n "$S32_GP" ] || die "S32: wildcard groups create failed"
code=$(req GET "/api/search?q=s32wild%25" "$L_CRED")   # q = s32wild%
check_http "S32: literal-% search -> 200" "$code" 200
if jqtest --argjson p "$S32_GP" '(.groups | length) == 1 and ((.groups | map(select(.group_id == $p))) | length) == 1'; then
  pass "S32: % is literal (only s32wild% matches)"
else
  fail "S32: % wildcard leak: $(jq -c '.groups' "$TMP/last")"
fi

# --- 6) M1: boss message search must not surface messages from groups the
#     boss is not a member of (their thread would 403 on open) ---
code=$(req POST /api/groups "$L_CRED" "{\"name\":\"s32priv\",\"member_ids\":[$M]}")
check_http "S32: L creates private group -> 201" "$code" 201
S32_G3=$(jqget '.id')
[ -n "$S32_G3" ] || die "S32: create s32priv failed"
code=$(req POST /api/messages "$M_CRED" "{\"group\":$S32_G3,\"kind\":\"chat\",\"body\":\"s32m1secret\"}")
check_http "S32: M posts in s32priv -> 201" "$code" 201
code=$(req GET "/api/search?q=s32m1secret" "$M_CRED")
check_http "S32: member M searches own group message -> 200" "$code" 200
if jqtest '[.messages[] | select(.body == "s32m1secret")] | length == 1'; then
  pass "S32: M sees group message in search"
else
  fail "S32: M missing group message: $(jq -c '.messages' "$TMP/last")"
fi
code=$(req GET "/api/search?q=s32m1secret" "$ADMIN")
check_http "S32: boss searches same body -> 200" "$code" 200
if jqtest '[.messages[] | select(.body == "s32m1secret")] | length == 0'; then
  pass "S32/M1: boss does not see non-member group message"
else
  fail "S32/M1: boss leak: $(jq -c '.messages' "$TMP/last")"
fi

# --- 7) q < 2 chars still 400; response shape keeps old keys ---
code=$(req GET "/api/search?q=a" "$ADMIN")
check_http "S32: 1-char q -> 400" "$code" 400
code=$(req GET "/api/search?q=s32alpha" "$ADMIN")
if jqtest '(.groups | type == "array") and (.contacts | type == "array") and (.messages | type == "array")'; then
  pass "S32: response has groups+contacts+messages arrays"
else
  fail "S32: response shape: $(jq -c 'keys' "$TMP/last")"
fi

# ============================================================
# 17s) S33 — conversation pin (per-viewer pin: GET /api/conversations
#      pinned key + pinned-first partition; POST /api/conversations/{peer_id}/pin)
#      peer_id convention: >=0 = 1:1 peer (0 = boss side), <=-1 = group
#      (-(group_id+1)).
# ============================================================
section "17s. S33 — conversation pin (per-viewer)"

# --- 1) setup: group s33pin (boss+L+M), channel s33chan (boss+W),
#     each with a message so all three appear in the boss list ---
code=$(req POST /api/groups "$ADMIN" "{\"name\":\"s33pin\",\"member_ids\":[$L,$M]}")
check_http "S33: create group s33pin -> 201" "$code" 201
S33_G=$(jqget '.id')
[ -n "$S33_G" ] || die "S33: create s33pin failed"
code=$(req POST /api/groups "$ADMIN" "{\"name\":\"s33chan\",\"kind\":\"channel\",\"member_ids\":[$W]}")
check_http "S33: create channel s33chan -> 201" "$code" 201
S33_C=$(jqget '.id')
[ -n "$S33_C" ] || die "S33: create s33chan failed"
S33_GPEER=$((-S33_G - 1))
S33_CPEER=$((-S33_C - 1))
code=$(req POST /api/messages "$M_CRED" "{\"group\":$S33_G,\"kind\":\"chat\",\"body\":\"s33 m1\"}")
check_http "S33: M posts in s33pin -> 201" "$code" 201
code=$(req POST /api/messages "$ADMIN" "{\"to\":$L,\"kind\":\"chat\",\"body\":\"s33 ping L\"}")
check_http "S33: boss posts 1:1 to L -> 201" "$code" 201
code=$(req POST /api/messages "$ADMIN" "{\"group\":$S33_C,\"kind\":\"chat\",\"body\":\"s33 m3\"}")
check_http "S33: boss posts in s33chan -> 201" "$code" 201

# --- 2) 1:1 pin: boss pins L -> 200, body shape, row pinned=true and first ---
code=$(req POST "/api/conversations/$L/pin" "$ADMIN" '{"pinned":true}')
check_http "S33: boss pins 1:1 L -> 200" "$code" 200
if jqtest --argjson l "$L" '.ok == true and .peer_id == $l and .pinned == true'; then
  pass "S33: pin response {ok, peer_id, pinned:true}"
else
  fail "S33: pin response wrong: $(cat "$TMP/last")"
fi
code=$(req GET /api/conversations "$ADMIN")
check_http "S33: conversations 200" "$code" 200
if jqtest 'all(.conversations[]; has("pinned"))'; then
  pass "S33: every row carries pinned key"
else
  fail "S33: row missing pinned key: $(jq -c '[.conversations[] | {peer_id, pinned}]' "$TMP/last")"
fi
if jqtest --argjson l "$L" '.conversations[0].peer_id == $l and .conversations[0].pinned == true'; then
  pass "S33: pinned 1:1 row at top with pinned=true"
else
  fail "S33: pinned 1:1 not first: $(jq -c '[.conversations[] | {peer_id, pinned}] | .[0:5]' "$TMP/last")"
fi

# --- 3) group pin: pinned group jumps to the pinned block (before the
#     still-unpinned, more-recent s33chan row) ---
code=$(req POST "/api/conversations/$S33_GPEER/pin" "$ADMIN" '{"pinned":true}')
check_http "S33: boss pins group s33pin -> 200" "$code" 200
code=$(req GET /api/conversations "$ADMIN")
if jqtest --argjson g "$S33_GPEER" --argjson c "$S33_CPEER" '([.conversations[] | .peer_id] | index($g)) < ([.conversations[] | .peer_id] | index($c)) and (.conversations[] | select(.peer_id == $g) | .pinned) == true'; then
  pass "S33: pinned group before unpinned newer group, pinned=true"
else
  fail "S33: group pin ordering: $(jq -c '[.conversations[] | {peer_id, pinned}] | .[0:6]' "$TMP/last")"
fi
if jqtest '[.conversations[] | .pinned] as $p | $p == ($p | sort | reverse)'; then
  pass "S33: pinned rows partitioned first"
else
  fail "S33: partition violated: $(jq -c '[.conversations[] | .pinned]' "$TMP/last")"
fi

# --- 4) channel pin: kind=channel row pinned=true ---
code=$(req POST "/api/conversations/$S33_CPEER/pin" "$ADMIN" '{"pinned":true}')
check_http "S33: boss pins channel s33chan -> 200" "$code" 200
code=$(req GET /api/conversations "$ADMIN")
if jqtest --argjson c "$S33_CPEER" \
  '(.conversations[] | select(.peer_id == $c) | .pinned == true and .kind == "channel" and .is_group == true)'; then
  pass "S33: channel row pinned=true kind=channel"
else
  fail "S33: channel pin: $(jq -c '.conversations[] | select(.name == "s33chan")' "$TMP/last")"
fi

# --- 5) new activity in an unpinned conversation must not break the pin ---
code=$(req POST /api/groups "$ADMIN" "{\"name\":\"s33new\",\"member_ids\":[$M]}")
check_http "S33: create group s33new -> 201" "$code" 201
S33_N=$(jqget '.id')
S33_NPEER=$((-S33_N - 1))
code=$(req POST /api/messages "$ADMIN" "{\"group\":$S33_N,\"kind\":\"chat\",\"body\":\"s33 m4\"}")
check_http "S33: boss posts in s33new -> 201" "$code" 201
code=$(req GET /api/conversations "$ADMIN")
if jqtest --argjson n "$S33_NPEER" '(.conversations[0].pinned == true) and (.conversations[] | select(.peer_id == $n) | .pinned == false) and ([.conversations[] | .peer_id] | index($n)) > 0'; then
  pass "S33: new unpinned activity stays below pinned block"
else
  fail "S33: pin displaced by activity: $(jq -c '[.conversations[] | {peer_id, pinned}] | .[0:6]' "$TMP/last")"
fi

# --- 6) unpin: 200, row back to pinned=false, activity order restored ---
code=$(req POST "/api/conversations/$L/pin" "$ADMIN" '{"pinned":false}')
check_http "S33: boss unpins 1:1 L -> 200" "$code" 200
code=$(req GET /api/conversations "$ADMIN")
if jqtest --argjson l "$L" --argjson n "$S33_NPEER" '(.conversations[] | select(.peer_id == $l) | .pinned) == false and ([.conversations[] | .peer_id] | index($n)) < ([.conversations[] | .peer_id] | index($l))'; then
  pass "S33: unpin -> pinned=false, newest activity ahead"
else
  fail "S33: unpin order: $(jq -c '[.conversations[] | {peer_id, pinned}] | .[0:6]' "$TMP/last")"
fi

# --- 7) unpin when not pinned: idempotent 200 ---
code=$(req POST "/api/conversations/$H/pin" "$ADMIN" '{"pinned":false}')
check_http "S33: unpin never-pinned H -> 200 (idempotent)" "$code" 200

# --- 8) per-viewer: L pins the boss conversation; boss view unaffected ---
code=$(req POST "/api/conversations/0/pin" "$L_CRED" '{"pinned":true}')
check_http "S33: L pins boss conversation -> 200" "$code" 200
code=$(req GET /api/conversations "$L_CRED")
if jqtest '.conversations[] | select(.peer_id == 0) | .pinned == true'; then
  pass "S33: L's view shows peer0 pinned=true"
else
  fail "S33: L view: $(jq -c '[.conversations[] | {peer_id, pinned}] | .[0:6]' "$TMP/last")"
fi
code=$(req GET /api/conversations "$ADMIN")
if jqtest --argjson l "$L" '(.conversations[] | select(.peer_id == $l) | .pinned) == false'; then
  pass "S33: per-viewer — boss's row for L still unpinned"
else
  fail "S33: cross-viewer leak: $(jq -c '[.conversations[] | {peer_id, pinned}] | .[0:6]' "$TMP/last")"
fi

# --- 9) boss and employee independently pin the same 1:1 thread ---
code=$(req POST "/api/conversations/$L/pin" "$ADMIN" '{"pinned":true}')
check_http "S33: boss pins L again -> 200" "$code" 200
code=$(req GET /api/conversations "$ADMIN")
if jqtest --argjson l "$L" '(.conversations[] | select(.peer_id == $l) | .pinned) == true'; then
  pass "S33: boss view L pinned=true"
else
  fail "S33: boss re-pin missing: $(jq -c '[.conversations[] | {peer_id, pinned}] | .[0:6]' "$TMP/last")"
fi
code=$(req GET /api/conversations "$L_CRED")
if jqtest '.conversations[] | select(.peer_id == 0) | .pinned == true'; then
  pass "S33: L view peer0 still pinned=true (independent)"
else
  fail "S33: L view lost pin: $(jq -c '[.conversations[] | {peer_id, pinned}] | .[0:6]' "$TMP/last")"
fi

# --- 10) validation errors ---
code=$(req POST /api/groups "$ADMIN" "{\"name\":\"s33noin\",\"member_ids\":[$M]}")
S33_X=$(jqget '.id')
S33_XPEER=$((-S33_X - 1))
[ -n "$S33_X" ] || die "S33: create s33noin failed"
code=$(req POST "/api/conversations/$S33_XPEER/pin" "$L_CRED" '{"pinned":true}')
check_http "S33: non-member group pin -> 403" "$code" 403
code=$(req POST "/api/conversations/99999/pin" "$ADMIN" '{"pinned":true}')
check_http "S33: unknown employee pin -> 400" "$code" 400
code=$(req POST "/api/conversations/$L/pin" "$L_CRED" '{"pinned":true}')
check_http "S33: self-pin -> 400" "$code" 400
code=$(req POST "/api/conversations/$L/pin" "$ADMIN" '{}')
check_http "S33: missing pinned -> 400" "$code" 400
code=$(req POST "/api/conversations/$L/pin" "$ADMIN" '{"pinned":"yes"}')
check_http "S33: non-bool pinned -> 400" "$code" 400

# --- 11) ignored group: pin via API keeps ignored=true, row pinned=true ---
code=$(req POST "/api/groups/$S33_G/prefs" "$M_CRED" '{"ignored":true}')
check_http "S33: M ignores s33pin -> 200" "$code" 200
code=$(req POST "/api/conversations/$S33_GPEER/pin" "$M_CRED" '{"pinned":true}')
check_http "S33: M pins ignored group via API -> 200" "$code" 200
code=$(req GET /api/conversations "$M_CRED")
if jqtest --argjson g "$S33_GPEER" \
  '(.conversations[] | select(.peer_id == $g) | .ignored == true and .pinned == true)'; then
  pass "S33: ignored group row stays ignored=true with pinned=true"
else
  fail "S33: ignored+pin row: $(jq -c '.conversations[] | select(.name == "s33pin")' "$TMP/last")"
fi

# ============================================================
# 17t) S36 — mention history search (GET /api/search .mentions)
# ============================================================
section "17t. S36 — mention history search"

# --- 1) setup: s36grp (boss creator + L + M) ---
code=$(req POST /api/groups "$ADMIN" "{\"name\":\"s36grp\",\"member_ids\":[$L,$M]}")
check_http "S36: create group s36grp -> 201" "$code" 201
S36_G=$(jqget '.id')
[ -n "$S36_G" ] || die "S36: create s36grp failed"

# --- 2) M posts @lead-a s36tok1 in group: L sees exactly 1 mention with group_id ---
code=$(req POST /api/messages "$M_CRED" "{\"group\":$S36_G,\"kind\":\"chat\",\"body\":\"@lead-a s36tok1\"}")
check_http "S36: M posts @lead-a in s36grp -> 201" "$code" 201
S36_M1=$(jqget '.message_id')
[ -n "$S36_M1" ] || die "S36: missing message_id"
code=$(req GET "/api/search?q=s36tok1" "$L_CRED")
check_http "S36: L searches s36tok1 -> 200" "$code" 200
if jqtest --argjson g "$S36_G" --argjson m "$S36_M1" \
  '(.mentions | length == 1) and (.mentions[0].id == $m) and (.mentions[0].group_id == $g) and (.mentions[0].body == "@lead-a s36tok1")'; then
  pass "S36: L sees @lead-a mention row with group_id"
else
  fail "S36: L mention row: $(jq -c '.mentions' "$TMP/last")"
fi
if jqtest --argjson m "$S36_M1" '[.messages[] | select(.id == $m)] | length == 1'; then
  pass "S36: same row still in messages facet (no regression)"
else
  fail "S36: messages facet lost row: $(jq -c '.messages' "$TMP/last")"
fi

# --- 3) sender M: no mention row for own @lead-a, still sees it in messages ---
code=$(req GET "/api/search?q=s36tok1" "$M_CRED")
check_http "S36: M searches s36tok1 -> 200" "$code" 200
if jqtest --argjson m "$S36_M1" '(.mentions | length == 0) and ([.messages[] | select(.id == $m)] | length == 1)'; then
  pass "S36: sender has no mention row; messages facet intact"
else
  fail "S36: M view: $(jq -c '.mentions' "$TMP/last")"
fi

# --- 4) non-member H: no leak of mention or message ---
code=$(req GET "/api/search?q=s36tok1" "$H_CRED")
check_http "S36: non-member H searches s36tok1 -> 200" "$code" 200
if jqtest --argjson m "$S36_M1" '(.mentions | length == 0) and ([.messages[] | select(.id == $m)] | length == 0)'; then
  pass "S36: non-member sees neither mention nor message"
else
  fail "S36: H leak: $(jq -c '{messages:.messages, mentions:.mentions}' "$TMP/last")"
fi

# --- 5) L posts @boss in group: boss sees 1 mention ---
code=$(req POST /api/messages "$L_CRED" "{\"group\":$S36_G,\"kind\":\"chat\",\"body\":\"@boss s36tok2\"}")
check_http "S36: L posts @boss in s36grp -> 201" "$code" 201
S36_M2=$(jqget '.message_id')
code=$(req GET "/api/search?q=s36tok2" "$ADMIN")
check_http "S36: boss searches s36tok2 -> 200" "$code" 200
if jqtest --argjson g "$S36_G" --argjson m "$S36_M2" --argjson l "$L" \
  '(.mentions | length == 1) and (.mentions[0].id == $m) and (.mentions[0].group_id == $g) and (.mentions[0].from_id == $l)'; then
  pass "S36: boss sees @boss mention row with from_id=L"
else
  fail "S36: boss mention row: $(jq -c '.mentions' "$TMP/last")"
fi

# --- 6) boss 1:1 -> M @lead-b: M sees mention without group_id, from_id null ---
code=$(req POST /api/messages "$ADMIN" "{\"to\":$M,\"kind\":\"chat\",\"body\":\"@lead-b s36tok3\"}")
check_http "S36: boss 1:1 @lead-b -> 201" "$code" 201
S36_M3=$(jqget '.message_id')
code=$(req GET "/api/search?q=s36tok3" "$M_CRED")
check_http "S36: M searches s36tok3 -> 200" "$code" 200
if jqtest --argjson m "$S36_M3" \
  '(.mentions | length == 1) and (.mentions[0].id == $m) and (.mentions[0].group_id == null) and (.mentions[0].from_id == null)'; then
  pass "S36: boss 1:1 mention row has no group_id, null from_id"
else
  fail "S36: boss 1:1 mention row: $(jq -c '.mentions' "$TMP/last")"
fi

# --- 7) plain message (no @): mentions empty, messages facet intact ---
code=$(req POST /api/messages "$L_CRED" "{\"group\":$S36_G,\"kind\":\"chat\",\"body\":\"s36plain\"}")
check_http "S36: L posts plain message -> 201" "$code" 201
code=$(req GET "/api/search?q=s36plain" "$L_CRED")
check_http "S36: L searches s36plain -> 200" "$code" 200
if jqtest '(.mentions | length == 0) and ((.messages | length) >= 1)'; then
  pass "S36: plain message -> no mention row, messages intact"
else
  fail "S36: plain message facets: $(jq -c '{messages:(.messages|length), mentions:.mentions}' "$TMP/last")"
fi

# --- 8) self-mention: L posts @lead-a -> L gets no mention row ---
code=$(req POST /api/messages "$L_CRED" "{\"group\":$S36_G,\"kind\":\"chat\",\"body\":\"@lead-a s36self\"}")
check_http "S36: L posts self-mention -> 201" "$code" 201
code=$(req GET "/api/search?q=s36self" "$L_CRED")
check_http "S36: L searches s36self -> 200" "$code" 200
if jqtest '(.mentions | length == 0) and ((.messages | length) >= 1)'; then
  pass "S36: self-mention yields no mention row"
else
  fail "S36: self-mention: $(jq -c '.mentions' "$TMP/last")"
fi

# --- 9) limit=2 -> two newest in id DESC; default limit keeps all three ---
code=$(req POST /api/messages "$M_CRED" "{\"group\":$S36_G,\"kind\":\"chat\",\"body\":\"@lead-a s36limA\"}")
check_http "S36: post limA -> 201" "$code" 201
code=$(req POST /api/messages "$M_CRED" "{\"group\":$S36_G,\"kind\":\"chat\",\"body\":\"@lead-a s36limB\"}")
check_http "S36: post limB -> 201" "$code" 201
code=$(req POST /api/messages "$M_CRED" "{\"group\":$S36_G,\"kind\":\"chat\",\"body\":\"@lead-a s36limC\"}")
check_http "S36: post limC -> 201" "$code" 201
code=$(req GET "/api/search?q=s36lim&limit=2" "$L_CRED")
check_http "S36: limit=2 search -> 200" "$code" 200
if jqtest '[.mentions[].body] == ["@lead-a s36limC", "@lead-a s36limB"]'; then
  pass "S36: limit=2 returns two newest (id DESC)"
else
  fail "S36: limit order: $(jq -c '[.mentions[].body]' "$TMP/last")"
fi
code=$(req GET "/api/search?q=s36lim" "$L_CRED")
check_http "S36: default-limit search -> 200" "$code" 200
if jqtest '(.mentions | length) == 3'; then
  pass "S36: default limit keeps all 3 mentions"
else
  fail "S36: default limit count: $(jq -c '[.mentions[].body]' "$TMP/last")"
fi

# --- 10) validation + always-array shape ---
code=$(req GET "/api/search?q=s" "$L_CRED")
check_http "S36: q<2 chars -> 400" "$code" 400
code=$(req GET "/api/search?q=s36tok1&limit=0" "$L_CRED")
check_http "S36: limit=0 -> 400" "$code" 400
code=$(req GET "/api/search?q=s36tok1&limit=51" "$L_CRED")
check_http "S36: limit=51 -> 400" "$code" 400
code=$(req GET "/api/search?q=s36tok1" "$H_CRED")
check_http "S36: no-hit search still 200" "$code" 200
if jqtest '(.mentions | type == "array") and (.mentions | length == 0)'; then
  pass "S36: mentions is always an array (empty on no hits)"
else
  fail "S36: mentions shape: $(jq -c 'keys' "$TMP/last")"
fi

section "17u. S38/S39 — P0-1 WS subprotocol auth + signed file URLs"

WS_KEY="dGhlIHNhbXBsZSBub25jZQ=="

# S38: token as the single Sec-WebSocket-Protocol subprotocol -> 101 + echo.
# The 101 hijacks the conn (server holds it); curl times out, headers captured.
curl -s -i --max-time 3 -o "$TMP/s38_hdr" \
  -H "Connection: Upgrade" -H "Upgrade: websocket" \
  -H "Sec-WebSocket-Version: 13" \
  -H "Sec-WebSocket-Key: $WS_KEY" \
  -H "Origin: http://127.0.0.1" \
  -H "Sec-WebSocket-Protocol: $ADMIN" \
  "$URL/ws" >/dev/null 2>&1 || true
if grep -q "101 Switching Protocols" "$TMP/s38_hdr" \
   && grep -Fq "Sec-WebSocket-Protocol: $ADMIN" "$TMP/s38_hdr"; then
  pass "S38: WS subprotocol token -> 101 + protocol echo"
else
  fail "S38: WS subprotocol handshake: $(tr -d '\r' < "$TMP/s38_hdr" | head -5 | tr '\n' ' ')"
fi

# S38b: no credentials at all -> 401.
wcode=$(curl -s -i --max-time 3 -o "$TMP/s38b_hdr" -w '%{http_code}' \
  -H "Connection: Upgrade" -H "Upgrade: websocket" \
  -H "Sec-WebSocket-Version: 13" \
  -H "Sec-WebSocket-Key: $WS_KEY" \
  -H "Origin: http://127.0.0.1" \
  "$URL/ws" || true)
if [ "$wcode" = "401" ]; then
  pass "S38b: WS without credentials -> 401"
else
  fail "S38b: WS no-creds expected 401, got $wcode"
fi

# S38c: legacy ?token= query fallback still -> 101 (server back-compat).
curl -s -i --max-time 3 -o "$TMP/s38c_hdr" \
  -H "Connection: Upgrade" -H "Upgrade: websocket" \
  -H "Sec-WebSocket-Version: 13" \
  -H "Sec-WebSocket-Key: $WS_KEY" \
  -H "Origin: http://127.0.0.1" \
  "$URL/ws?token=$ADMIN" >/dev/null 2>&1 || true
if grep -q "101 Switching Protocols" "$TMP/s38c_hdr"; then
  pass "S38c: WS legacy query-token -> 101"
else
  fail "S38c: WS legacy fallback: $(tr -d '\r' < "$TMP/s38c_hdr" | head -5 | tr '\n' ' ')"
fi

# S39: admin upload -> signed URL; bare GET 200 + bytes; tampered sig -> 401.
code=$(curl -s --max-time 5 -o "$TMP/last" -w '%{http_code}' -X POST \
  -F "file=@$PNGF" -H "Authorization: Bearer $ADMIN" "$URL/api/files")
check_http "S39: admin file upload -> 201" "$code" 201
S39URL=$(jqget '.url')
case "$S39URL" in
  *"sig="*) : ;;
  *) fail "S39: upload url not signed: $S39URL"; S39URL="" ;;
esac
if [ -n "$S39URL" ]; then
  code=$(curl -s --max-time 5 -o "$TMP/s39.png" -w '%{http_code}' "$URL$S39URL")
  if [ "$code" = "200" ] && cmp -s "$PNGF" "$TMP/s39.png"; then
    pass "S39: bare GET on signed URL -> 200 + uploaded bytes"
  else
    fail "S39: signed URL fetch http $code or byte mismatch"
  fi
  # Tamper: keep path+exp, zero out the sig.
  S39BASE="${S39URL%%\?*}"
  S39EXP="${S39URL#*\?exp=}"; S39EXP="${S39EXP%%\&*}"
  TAMPERED="$S39BASE?exp=$S39EXP&sig=0000000000000000000000000000000000000000000000000000000000000000"
  code=$(curl -s --max-time 5 -o "$TMP/null" -w '%{http_code}' "$URL$TAMPERED")
  check_http "S39: tampered sig -> 401" "$code" 401
fi

# S40: /metrics — unauthenticated Prometheus text snapshot (P1b). Values drift,
# so only assert the status code and the presence of the expected gauge names.
code=$(req GET "/metrics" "")
check_http "S40: /metrics unauthenticated -> 200" "$code" 200
if grep -q "whatbark_uptime_seconds " "$TMP/last" \
  && grep -q "whatbark_goroutines " "$TMP/last" \
  && grep -q "whatbark_ws_clients " "$TMP/last" \
  && grep -q "whatbark_agents_online " "$TMP/last" \
   && grep -q "whatbark_employees_total " "$TMP/last" \
   && grep -q "whatbark_files_total " "$TMP/last" \
   && grep -q "whatbark_tasks_total{status=" "$TMP/last"; then
  pass "S40: /metrics exposes expected gauge names"
else
  fail "S40: /metrics missing expected gauges: $(tr '\n' ' ' < "$TMP/last" | head -c 300)"
fi

# ============================================================
# 17v) A16 — leader orchestration: review wake. Leader dispatches,
#      the worker finishes, and the leader's queue gains a kind=review
#      task plus a review message in the boss thread. Also pins the
#      preset-import collaboration graph (S2) through the CLI.
#      Fresh hermetic org (lead-a16/worker-a16, no attached agents)
#      so every wake is driven purely through the API.
# ============================================================
section "17v. A16 — leader orchestration (review wake) + preset collab graph"

# --- 1) preset import carries the leadership collaboration graph ---
# Hermetic: own scratch DB, no serve involved. The CLI prints one line
# per imported workflow; assert the line, then the graph shape straight
# from SQLite (1 workflow / 5 leader nodes / 20 directed edges = K5).
A16P_DB="$TMP/a16preset.db"
if "$BIN" preset import software -data "$A16P_DB" >"$TMP/a16import.log" 2>&1 \
   && grep -q "workflow Leadership Collab" "$TMP/a16import.log"; then
  pass "A16: preset import software exits 0, prints 'workflow Leadership Collab'"
else
  fail "A16: preset import workflow line missing (exit/log): $(tail -3 "$TMP/a16import.log")"
fi
a16_wf=$(sqlite3 "$A16P_DB" "SELECT COUNT(*) FROM workflows WHERE name='Leadership Collab';")
a16_nd=$(sqlite3 "$A16P_DB" "SELECT COUNT(*) FROM wflow_nodes WHERE workflow_id IN (SELECT id FROM workflows WHERE name='Leadership Collab');")
a16_ed=$(sqlite3 "$A16P_DB" "SELECT COUNT(*) FROM wflow_edges WHERE workflow_id IN (SELECT id FROM workflows WHERE name='Leadership Collab');")
if [ "$a16_wf" = "1" ] && [ "$a16_nd" = "5" ] && [ "$a16_ed" = "20" ]; then
  pass "A16: collab graph = 1 workflow / 5 nodes / 20 edges (K5 complete)"
else
  fail "A16: collab graph shape wf=$a16_wf n=$a16_nd e=$a16_ed (want 1/5/20)"
fi

# --- 2) fresh org: lead-a16 (AI leader) + worker-a16 (AI worker) ---
code=$(req POST /api/departments "$ADMIN" '{"name":"a16-dept"}')
check_http "A16: department a16-dept created" "$code" 201
A16_DEPT=$(jqget '.id')
out=$(new_emp "lead-a16" leader ai "$A16_DEPT") || die "create lead-a16: $out"
A16_L=${out%% *}; A16_L_CRED=${out#* }
out=$(new_emp "worker-a16" engineer ai "$A16_DEPT") || die "create worker-a16: $out"
A16_W=${out%% *}; A16_W_CRED=${out#* }
code=$(req PUT "/api/departments/$A16_DEPT/leader" "$ADMIN" "{\"employee_id\":$A16_L}")
check_http "A16: lead-a16 set as a16-dept leader" "$code" 200

# Drain the scheduler's one-time selfreview injection for lead-a16 before
# anything pins thread lengths: the first tick after creation delivers one
# kind=selfreview message into the boss thread, then the pending-guard
# blocks re-injection for good (lead-a16 has no agent, the task stays
# queued) — after this wait the boss thread is frozen for the section.
up=""
i=0
while [ -z "$up" ] && [ $i -lt 20 ]; do
  req GET "/api/messages?with=0" "$A16_L_CRED" >"$TMP/null"
  jqtest '[.messages[] | select(.kind == "selfreview")] | length >= 1' && up=1
  i=$((i + 1))
  sleep 0.5
done
if [ -n "$up" ]; then
  pass "A16: lead-a16 boss thread drained (one-time selfreview seen)"
else
  fail "A16: lead-a16 selfreview injection not observed within 10s"
fi

# a16_lq_reviews: kind=review count in lead-a16's queue (admin view).
a16_lq_reviews() {
  req GET "/api/tasks?employee=$A16_L&status=queued" "$ADMIN" >"$TMP/null"
  jqget '[.tasks[] | select(.kind == "review")] | length'
}

# --- 3) dispatch -> worker finish -> review wakes in the leader queue ---
code=$(req POST /api/messages "$A16_L_CRED" "{\"to\":$A16_W,\"kind\":\"dispatch\",\"body\":\"do A16\"}")
check_http "A16: leader dispatch to worker-a16 -> 201" "$code" 201
A16_T=$(jqget '.task_id')
{ [ -n "$A16_T" ] && [ "$A16_T" != "null" ]; } || die "A16: dispatch missing task_id: $(cat "$TMP/last")"
code=$(req GET "/api/tasks?employee=$A16_W&status=queued" "$A16_W_CRED")
check_http "A16: worker reads own queued list -> 200" "$code" 200
if jqtest --argjson t "$A16_T" \
    '[.tasks[] | select(.id == $t and .kind == "dispatch" and .status == "queued")] | length == 1'; then
  pass "A16: dispatched task $A16_T queued for worker-a16"
else
  fail "A16: task $A16_T not in worker queue: $(jq -c '[.tasks[] | {id, kind, status}]' "$TMP/last")"
fi
code=$(req POST "/api/tasks/$A16_T/start" "$A16_W_CRED")
check_http "A16: worker starts task $A16_T" "$code" 200
code=$(req POST "/api/tasks/$A16_T/finish" "$A16_W_CRED" '{"output":"A16 done"}')
check_http "A16: worker finishes task $A16_T (output 'A16 done')" "$code" 200
rv=$(a16_lq_reviews)
if [ "$rv" = "1" ]; then
  pass "A16: leader queue holds exactly 1 kind=review task after the finish"
else
  fail "A16: leader queue review count = $rv (want 1)"
fi
req GET "/api/tasks?employee=$A16_L&status=queued" "$ADMIN" >"$TMP/null"
if jqtest --argjson l "$A16_L" \
    '[.tasks[] | select(.kind == "review")][0] | (.requested_by == 0) and (.priority == 0) and (.employee_id == $l)'; then
  pass "A16: review task shape: requested_by=0, priority=0, owner=lead-a16"
else
  fail "A16: review task shape wrong: $(jq -c '[.tasks[] | select(.kind=="review")][0]' "$TMP/last")"
fi
A16_R=$(jqget '[.tasks[] | select(.kind == "review")][0].id')
code=$(req GET "/api/messages?with=0" "$A16_L_CRED")
check_http "A16: leader reads boss thread (with=0) -> 200" "$code" 200
A16_NEEDLE="#$A16_T by worker-a16 is done."
if jqtest --arg n "$A16_NEEDLE" \
    '[.messages[] | select(.kind == "review" and (.body | contains($n)) and (.body | contains("A16 done")) and (.body | contains("Guidance:")))] | length == 1'; then
  pass "A16: review wake message in leader boss thread (#task, worker, output, guidance)"
else
  fail "A16: review wake message missing: $(jq -c '[.messages[] | select(.kind=="review") | .body]' "$TMP/last")"
fi

# --- 4) the ordinary kind=report receipt still lands (wake eats nothing) ---
code=$(req GET "/api/messages?with=$A16_W" "$A16_L_CRED")
check_http "A16: leader reads worker thread -> 200" "$code" 200
if jqtest --argjson t "$A16_T" \
    '[.messages[] | select(.kind == "report" and .task_id == $t and .body == "A16 done")] | length == 1'; then
  pass "A16: kind=report receipt for task $A16_T still delivered to leader"
else
  fail "A16: report receipt missing: $(jq -c --argjson t "$A16_T" '[.messages[] | select(.task_id == $t)]' "$TMP/last")"
fi

# --- 5) canceled dispatch never wakes (cancel path skips FinishTask) ---
code=$(req POST /api/messages "$A16_L_CRED" "{\"to\":$A16_W,\"kind\":\"dispatch\",\"body\":\"do A16 cancel case\"}")
check_http "A16: second dispatch -> 201" "$code" 201
A16_T2=$(jqget '.task_id')
code=$(req POST "/api/tasks/$A16_T2/cancel" "$A16_W_CRED" '{"reason":"a16 not needed"}')
check_http "A16: worker cancels task $A16_T2 -> 200" "$code" 200
rv=$(a16_lq_reviews)
if [ "$rv" = "1" ]; then
  pass "A16: cancel does not wake (leader review count stays 1)"
else
  fail "A16: cancel woke a review? count = $rv (want 1)"
fi

# --- 6) throttle: review_max_per_hour=0 disables the wake, 30 restores it ---
code=$(req POST /api/settings "$ADMIN" '{"review_max_per_hour":"0"}')
check_http "A16: settings review_max_per_hour=0 -> 200" "$code" 200
code=$(req GET /api/settings "$ADMIN")
if [ "$code" = "200" ] && [ "$(jqget '.review_max_per_hour')" = "0" ]; then
  pass "A16: GET /api/settings echoes review_max_per_hour=0"
else
  fail "A16: settings readback: $(cat "$TMP/last")"
fi
code=$(req POST /api/messages "$A16_L_CRED" "{\"to\":$A16_W,\"kind\":\"dispatch\",\"body\":\"do A16 throttled\"}")
check_http "A16: third dispatch (throttle=0) -> 201" "$code" 201
A16_T3=$(jqget '.task_id')
code=$(req POST "/api/tasks/$A16_T3/start" "$A16_W_CRED")
check_http "A16: worker starts task $A16_T3" "$code" 200
code=$(req POST "/api/tasks/$A16_T3/finish" "$A16_W_CRED" '{"output":"A16 throttled"}')
check_http "A16: worker finishes task $A16_T3" "$code" 200
rv=$(a16_lq_reviews)
if [ "$rv" = "1" ]; then
  pass "A16: throttle=0 disabled the wake (review count stays 1)"
else
  fail "A16: throttle=0 not honored? review count = $rv (want 1)"
fi
code=$(req POST /api/settings "$ADMIN" '{"review_max_per_hour":"30"}')
check_http "A16: settings review_max_per_hour=30 -> 200" "$code" 200
code=$(req POST /api/messages "$A16_L_CRED" "{\"to\":$A16_W,\"kind\":\"dispatch\",\"body\":\"do A16 restored\"}")
check_http "A16: fourth dispatch (throttle=30) -> 201" "$code" 201
A16_T4=$(jqget '.task_id')
code=$(req POST "/api/tasks/$A16_T4/start" "$A16_W_CRED")
check_http "A16: worker starts task $A16_T4" "$code" 200
code=$(req POST "/api/tasks/$A16_T4/finish" "$A16_W_CRED" '{"output":"A16 restored"}')
check_http "A16: worker finishes task $A16_T4" "$code" 200
rv=$(a16_lq_reviews)
if [ "$rv" = "2" ]; then
  pass "A16: throttle=30 restored the wake (review count 1 -> 2)"
else
  fail "A16: restored throttle did not wake? count = $rv (want 2)"
fi

# --- 7) finishing a review neither re-wakes nor emits a receipt (F7 pin) ---
req GET "/api/messages?with=0" "$A16_L_CRED" >"$TMP/null"
A16_BT0=$(jqget '.messages | length')
A16_REV0=$(sqlite3 "$DB" "SELECT COUNT(*) FROM tasks WHERE kind='review';")
code=$(req POST "/api/tasks/$A16_R/start" "$A16_L_CRED")
check_http "A16: leader starts the review task $A16_R" "$code" 200
code=$(req POST "/api/tasks/$A16_R/finish" "$A16_L_CRED" '{"output":"reviewed, accepted"}')
check_http "A16: leader finishes the review task" "$code" 200
code=$(req GET "/api/tasks/$A16_R" "$ADMIN")
if [ "$code" = "200" ] && [ "$(jqget '.status')" = "done" ]; then
  pass "A16: review task $A16_R reached done (owner start+finish)"
else
  fail "A16: review task state after finish: http $code, $(cat "$TMP/last")"
fi
A16_REV1=$(sqlite3 "$DB" "SELECT COUNT(*) FROM tasks WHERE kind='review';")
if [ "$A16_REV0" = "$A16_REV1" ]; then
  pass "A16: finishing a review spawns no new review (no wake loop)"
else
  fail "A16: review loop? global review rows $A16_REV0 -> $A16_REV1"
fi
code=$(req GET "/api/messages?with=0" "$A16_L_CRED")
A16_BT1=$(jqget '.messages | length')
if [ "$code" = "200" ] && [ "$A16_BT0" = "$A16_BT1" ]; then
  pass "A16: review finish left no receipt message in the boss thread"
else
  fail "A16: boss thread grew on review finish ($A16_BT0 -> $A16_BT1, http $code)"
fi

# ============================================================
# 17w) F2 real-binary — pre-review legacy DB migration link. A DB
#      hand-built with the old kind CHECKs (the exact pre-S1 shape
#      carried in internal/store/review_migrate_test.go) is booted by
#      the new binary: store.Open rebuilds both tables, legacy rows
#      survive, staff still cannot forge review, and a live
#      dispatch->finish wake inserts review rows into the migrated
#      tables through the running serve. Own scratch DB + scratch
#      port; fully hermetic (shared serve untouched).
# ============================================================
section "17w. F2 real-binary — pre-review legacy DB migration + wake"

A16W_PORT=18125
A16W_URL="http://127.0.0.1:$A16W_PORT"
A16W_DB="$TMP/a16legacy.db"

# 1) hand-build a pre-review database (CHECKs without 'review').
sqlite3 "$A16W_DB" <<'SQL'
CREATE TABLE employees (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	role TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT 'ai' CHECK (kind IN ('ai', 'human')),
	department_id INTEGER NOT NULL,
	tool TEXT NOT NULL DEFAULT 'mock',
	tool_args TEXT NOT NULL DEFAULT '{}',
	workdir TEXT NOT NULL DEFAULT '',
	token_hash TEXT NOT NULL DEFAULT '',
	last_seen_at TEXT,
	avatar TEXT NOT NULL DEFAULT '',
	email TEXT,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE tasks (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
	employee_id INTEGER NOT NULL REFERENCES employees(id),
	workflow_id INTEGER REFERENCES workflows(id) ON DELETE SET NULL,
	group_id INTEGER REFERENCES groups(id) ON DELETE SET NULL,
	requested_by INTEGER NOT NULL DEFAULT 0,
	kind TEXT NOT NULL DEFAULT 'dispatch'
		CHECK (kind IN ('dispatch', 'ask', 'scheduled', 'selfreview')),
	status TEXT NOT NULL DEFAULT 'queued'
		CHECK (status IN ('queued', 'running', 'done', 'failed', 'canceled')),
	priority INTEGER NOT NULL DEFAULT 0,
	input TEXT NOT NULL DEFAULT '',
	output TEXT NOT NULL DEFAULT '',
	started_at TEXT,
	finished_at TEXT,
	sla_reminded_at TEXT,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	from_id INTEGER REFERENCES employees(id),
	to_id INTEGER REFERENCES employees(id),
	kind TEXT NOT NULL CHECK (kind IN ('report', 'dispatch', 'ask', 'answer', 'chat', 'summary', 'system', 'scheduled', 'selfreview')),
	body TEXT NOT NULL,
	project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
	task_id INTEGER REFERENCES tasks(id) ON DELETE SET NULL,
	group_id INTEGER REFERENCES groups(id) ON DELETE SET NULL,
	reply_to INTEGER,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO employees (id, name, role, kind, department_id) VALUES (1, 'alice', 'leader', 'ai', 1);
INSERT INTO tasks (id, employee_id, requested_by, kind, status, priority, input, output)
	VALUES (1, 1, 0, 'dispatch', 'done', 5, 'legacy input', 'legacy output');
INSERT INTO tasks (id, employee_id, requested_by, kind, status, input)
	VALUES (2, 1, 0, 'ask', 'queued', 'legacy ask');
INSERT INTO messages (id, from_id, to_id, kind, body, task_id)
	VALUES (1, 1, 1, 'chat', 'legacy chat', NULL);
INSERT INTO messages (id, from_id, to_id, kind, body, task_id)
	VALUES (2, NULL, 1, 'system', 'legacy system', 1);
SQL
# The needle is the QUOTED literal 'review' (not a bare substring): the
# pre-review DDL already contains 'selfreview', so an unquoted instr()
# would false-match and void both this precondition and the post-boot
# DDL check below (same guard as the unit test's ddlLacksReview).
p_t=$(sqlite3 "$A16W_DB" "SELECT COUNT(*) FROM sqlite_master WHERE name='tasks' AND instr(sql,'''review''')>0;")
p_m=$(sqlite3 "$A16W_DB" "SELECT COUNT(*) FROM sqlite_master WHERE name='messages' AND instr(sql,'''review''')>0;")
if [ "$p_t" = "0" ] && [ "$p_m" = "0" ]; then
  pass "A16w: legacy fixture built (no 'review' in either CHECK)"
else
  fail "A16w: legacy fixture already carries review (fixture wrong)"
fi

# 2) boot the new binary on the legacy DB (store.Open runs the migration).
"$BIN" serve -listen "127.0.0.1:$A16W_PORT" -data "$A16W_DB" \
  -admin-token "$ADMIN" -tick 1s > "$TMP/serve-a16w.log" 2>&1 &
SERVER2_PID=$!
up=""
i=0
while [ -z "$up" ] && [ $i -lt 40 ]; do
  code=$(curl -s -o "$TMP/null" -w '%{http_code}' "$A16W_URL/healthz" 2>"$TMP/null") || true
  [ "$code" = "200" ] && up=1
  i=$((i + 1))
  sleep 0.5
done
if [ -n "$up" ]; then
  pass "A16w: serve boots on the pre-review DB (migration ran at Open)"
else
  tail -20 "$TMP/serve-a16w.log"
  die "A16w: serve did not come up on the legacy DB"
fi

# 3) migrated DDL carries 'review'; legacy rows survived the rebuild.
ddl_t=$(sqlite3 "$A16W_DB" "SELECT COUNT(*) FROM sqlite_master WHERE name='tasks' AND instr(sql,'''review''')>0;")
ddl_m=$(sqlite3 "$A16W_DB" "SELECT COUNT(*) FROM sqlite_master WHERE name='messages' AND instr(sql,'''review''')>0;")
if [ "$ddl_t" = "1" ] && [ "$ddl_m" = "1" ]; then
  pass "A16w: tasks+messages DDL rebuilt with 'review' in both CHECKs"
else
  fail "A16w: DDL migration incomplete (tasks=$ddl_t messages=$ddl_m, want 1/1)"
fi
# Note: right after boot the scheduler injects one kind=selfreview task
# + message for alice (AI employee, default interval), so raw table counts
# drift by +1 on both tables. The survival pin is therefore id-anchored:
# exactly the seeded rows, with ids and payloads intact.
n_emp=$(sqlite3 "$A16W_DB" "SELECT COUNT(*) FROM employees WHERE id=1 AND name='alice' AND kind='ai';")
n_tsk=$(sqlite3 "$A16W_DB" "SELECT COUNT(*) FROM tasks WHERE id IN (1,2);")
t1k=$(sqlite3 "$A16W_DB" "SELECT kind||'/'||status||'/'||priority FROM tasks WHERE id=1;")
t2k=$(sqlite3 "$A16W_DB" "SELECT kind||'/'||input FROM tasks WHERE id=2;")
n_msg=$(sqlite3 "$A16W_DB" "SELECT COUNT(*) FROM messages WHERE id IN (1,2);")
m1k=$(sqlite3 "$A16W_DB" "SELECT kind||'/'||body FROM messages WHERE id=1;")
m2k=$(sqlite3 "$A16W_DB" "SELECT kind||'/'||body||'/'||task_id FROM messages WHERE id=2;")
if [ "$n_emp" = "1" ] && [ "$n_tsk" = "2" ] && [ "$n_msg" = "2" ] \
   && [ "$t1k" = "dispatch/done/5" ] && [ "$t2k" = "ask/legacy ask" ] \
   && [ "$m1k" = "chat/legacy chat" ] && [ "$m2k" = "system/legacy system/1" ]; then
  pass "A16w: legacy rows survived (1 emp, tasks 1-2, msgs 1-2, payloads intact)"
else
  fail "A16w: legacy rows changed (emp=$n_emp t=$n_tsk/$t1k/$t2k m=$n_msg/$m1k/$m2k)"
fi

# 4) live org on the migrated DB: staff forging still 403, wake still works.
# a16w_* mirror req/jqget/jqtest against the scratch serve ($TMP/last2).
a16w_req() {
  local m="$1" p="$2" a="$3" b="${4:-}"
  local args=(-s -X "$m" -o "$TMP/last2" -w '%{http_code}')
  [ -n "$a" ] && args+=(-H "Authorization: Bearer $a")
  [ -n "$b" ] && args+=(-H "Content-Type: application/json" -d "$b")
  args+=("$A16W_URL$p")
  curl "${args[@]}"
}
a16w_jqget()  { jq -r "$1" "$TMP/last2"; }
a16w_jqtest() { jq -e "$@" "$TMP/last2" >"$TMP/null" 2>&1; }

code=$(a16w_req POST /api/departments "$ADMIN" '{"name":"a16w-dept"}')
check_http "A16w: department a16w-dept created on migrated DB" "$code" 201
A16W_DEPT=$(a16w_jqget '.id')
code=$(a16w_req POST /api/employees "$ADMIN" \
  "{\"name\":\"lead-a16w\",\"role\":\"leader\",\"kind\":\"ai\",\"department_id\":$A16W_DEPT}")
[ "$code" = "201" ] || die "A16w: create lead-a16w failed: $(cat "$TMP/last2")"
A16W_L=$(a16w_jqget '.id'); A16W_L_CRED=$(a16w_jqget '.token')
code=$(a16w_req POST /api/employees "$ADMIN" \
  "{\"name\":\"worker-a16w\",\"role\":\"engineer\",\"kind\":\"ai\",\"department_id\":$A16W_DEPT}")
[ "$code" = "201" ] || die "A16w: create worker-a16w failed: $(cat "$TMP/last2")"
A16W_W=$(a16w_jqget '.id'); A16W_W_CRED=$(a16w_jqget '.token')
code=$(a16w_req PUT "/api/departments/$A16W_DEPT/leader" "$ADMIN" "{\"employee_id\":$A16W_L}")
check_http "A16w: lead-a16w set as a16w-dept leader" "$code" 200

code=$(a16w_req POST /api/messages "$A16W_L_CRED" "{\"to\":$A16W_W,\"kind\":\"review\",\"body\":\"forged review\"}")
check_http "A16w: leader forging kind=review -> 403" "$code" 403
code=$(a16w_req POST /api/messages "$A16W_W_CRED" "{\"to\":$A16W_L,\"kind\":\"review\",\"body\":\"forged review 2\"}")
check_http "A16w: worker forging kind=review -> 403" "$code" 403
code=$(a16w_req GET "/api/tasks?employee=$A16W_L&status=queued" "$ADMIN")
if [ "$code" = "200" ] && a16w_jqtest '[.tasks[] | select(.kind == "review")] | length == 0'; then
  pass "A16w: forged reviews left zero review rows"
else
  fail "A16w: forged review created rows: $(cat "$TMP/last2")"
fi

code=$(a16w_req POST /api/messages "$A16W_L_CRED" "{\"to\":$A16W_W,\"kind\":\"dispatch\",\"body\":\"legacy wake drill\"}")
check_http "A16w: dispatch on migrated DB -> 201" "$code" 201
A16W_T=$(a16w_jqget '.task_id')
code=$(a16w_req POST "/api/tasks/$A16W_T/start" "$A16W_W_CRED")
check_http "A16w: worker starts task $A16W_T" "$code" 200
code=$(a16w_req POST "/api/tasks/$A16W_T/finish" "$A16W_W_CRED" '{"output":"migrated db wake ok"}')
check_http "A16w: worker finishes task $A16W_T" "$code" 200
code=$(a16w_req GET "/api/tasks?employee=$A16W_L&status=queued" "$ADMIN")
if [ "$code" = "200" ] && a16w_jqtest '[.tasks[] | select(.kind == "review")] | length == 1'; then
  pass "A16w: review task materialized through the live serve (migrated tables)"
else
  fail "A16w: no review after wake on migrated DB: $(cat "$TMP/last2")"
fi
a16w_rev=$(sqlite3 "$A16W_DB" "SELECT COUNT(*) FROM tasks WHERE kind='review';")
a16w_msgrev=$(sqlite3 "$A16W_DB" "SELECT COUNT(*) FROM messages WHERE kind='review';")
if [ "$a16w_rev" -ge 1 ] && [ "$a16w_msgrev" -ge 1 ]; then
  pass "A16w: sqlite3 confirms review rows landed in both migrated tables"
else
  fail "A16w: review rows missing on disk (tasks=$a16w_rev messages=$a16w_msgrev)"
fi

# 5) teardown: stop the scratch serve and confirm the port closed.
stop_pid "$SERVER2_PID"
SERVER2_PID=""
code=$(curl -s -o "$TMP/null" -w '%{http_code}' "$A16W_URL/healthz" 2>"$TMP/null") || true
if [ "$code" != "200" ]; then
  pass "A16w: scratch serve stopped (port closed)"
else
  fail "A16w: scratch serve still answering after stop"
fi

section "18. summary"
printf '================ SUMMARY ================\n'
printf 'PASS: %d\nFAIL: %d\n' "$PASS" "$FAIL"
if [ "$FAIL" -gt 0 ]; then
  echo
  echo "--- serve.log (tail) ---"
  tail -30 "$TMP/serve.log" 2>/dev/null || true
  if [ -f "$TMP/serve2.log" ]; then
    echo "--- serve2.log (tail) ---"
    tail -30 "$TMP/serve2.log"
  fi
  for lg in agent-L agent-M agent-W agent-W2; do
    if [ -f "$TMP/$lg.log" ]; then
      echo "--- $lg.log (tail) ---"
      tail -10 "$TMP/$lg.log"
    fi
  done
fi
[ "$FAIL" -eq 0 ]
