#!/usr/bin/env bash
# Proof-only capture of the kata TUI run detail cost row, before and after the
# change. Each side builds kata at an exact commit, starts its own daemon in a
# synthetic home configured for a loopback fake AgentsView, records runs
# through the real CLI, drives the real TUI in tmux, and renders the raw pane
# with the docs screenshot primitives (normalize-ansi.py, freeze.json).
#
# Required environment: BEFORE_SHA, AFTER_SHA, OUT (artifact directory),
# FREEZE (freeze 0.2.2 binary). Nothing else is passed to kata, tmux or the
# fake: every child gets an explicit environment allowlist. DO_NOT_TRACK=1
# turns telemetry off.
set -euo pipefail

: "${BEFORE_SHA:?}" "${AFTER_SHA:?}" "${OUT:?}" "${FREEZE:?}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PROOF_DIR="$REPO_ROOT/.github/proof"
FREEZE_CFG="$REPO_ROOT/docs/screenshots/freeze.json"
NORMALIZE="$REPO_ROOT/docs/screenshots/normalize-ansi.py"
WIDTH=100
HEIGHT=30
SESSION="kata-cost"
SAFE_PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
TMP_ROOT="$(mktemp -d /tmp/kata-cost-XXXXXX)"
FAKE_PID=""
STARTED_HOMES=()
STARTED_BINS=()

mkdir -p "$OUT/before" "$OUT/after" "$OUT/logs" "$OUT/fixtures" "$TMP_ROOT/tmp"

# kenv HOME_DIR CMD... runs CMD with only these variables.
kenv() {
    local home="$1"
    shift
    env -i PATH="$SAFE_PATH" HOME="$home/user" TMPDIR="$TMP_ROOT/tmp" TZ=UTC \
        LANG=C.UTF-8 LC_ALL=C.UTF-8 DO_NOT_TRACK=1 \
        KATA_HOME="$home/kata" KATA_AUTHOR=demo-operator "$@"
}

tmux_cmd() {
    env -i PATH="$SAFE_PATH" HOME="$TMP_ROOT" TMPDIR="$TMP_ROOT/tmp" LANG=C.UTF-8 \
        tmux -L "$SESSION" -f /dev/null "$@"
}

pane() { tmux_cmd capture-pane -p -t "$SESSION"; }

# shows PATTERN reads the whole pane first, so grep -q cannot cut the pipe.
shows() {
    local text
    text="$(pane)"
    grep -Eq -- "$1" <<<"$text"
}

fail() {
    echo "ERROR: $*" >&2
    pane >"$OUT/failure-pane.txt" 2>/dev/null || true
    cat "$OUT/failure-pane.txt" >&2 || true
    exit 1
}

wait_until() {
    local pattern="$1"
    for _ in $(seq 1 300); do
        shows "$pattern" && return 0
        sleep 0.1
    done
    fail "timed out waiting for: $pattern"
}

wait_gone() {
    local pattern="$1"
    for _ in $(seq 1 300); do
        shows "$pattern" || return 0
        sleep 0.1
    done
    fail "timed out waiting for removal of: $pattern"
}

send() { tmux_cmd send-keys -t "$SESSION" "$@"; }

delay() { printf '%s\n' "$1" >"$TMP_ROOT/delay"; }

# capture NAME saves the raw pane ANSI, its plain text, and Freeze SVG/PNG.
capture() {
    local name="$1" base="$OUT/$1"
    tmux_cmd capture-pane -N -e -p -t "$SESSION" >"$base.ansi"
    pane >"$base.txt"
    python3 -I "$NORMALIZE" <"$base.ansi" >"$TMP_ROOT/normalized.ansi"
    "$FREEZE" --language ansi -c "$FREEZE_CFG" -o "$base.svg" <"$TMP_ROOT/normalized.ansi"
    if ! "$FREEZE" --language ansi -c "$FREEZE_CFG" -o "$base.png" <"$TMP_ROOT/normalized.ansi"; then
        echo "freeze PNG export failed for $name; converting its SVG with rsvg-convert" >>"$OUT/provenance.txt"
        rsvg-convert -o "$base.png" "$base.svg"
    fi
    echo "captured $name"
}

# open_run MESSAGE moves the Runs list cursor to the run with MESSAGE and opens it.
open_run() {
    local message="$1"
    send g
    for _ in $(seq 1 6); do
        sleep 0.3
        if shows "▶.*$message"; then
            send Enter
            wait_until "Sessions:"
            return 0
        fi
        send j
    done
    fail "run not found in list: $message"
}

cleanup() {
    set +e
    tmux_cmd kill-server >/dev/null 2>&1
    local i home log rel
    for i in "${!STARTED_HOMES[@]}"; do
        home="${STARTED_HOMES[$i]}"
        kenv "$home" "${STARTED_BINS[$i]}" daemon stop >/dev/null 2>&1
        while IFS= read -r log; do
            rel="${log#"$home/kata/"}"
            cp "$log" "$OUT/logs/$(basename "$home")/${rel//\//_}"
        done < <(find "$home/kata" -name '*.log' -type f)
    done
    if [[ -n "$FAKE_PID" ]]; then
        kill "$FAKE_PID" >/dev/null 2>&1
        wait "$FAKE_PID" >/dev/null 2>&1
    fi
    rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

# Synthetic AgentsView usage, in the producer's document shape. Run A is
# fully priced, run B has one session AgentsView has not indexed, and run C's
# prices exceed what the daemon can total, so its usage request fails.
cat >"$OUT/fixtures/agentsview-sessions.json" <<'EOF'
{
  "11111111-1111-4111-8111-111111111111": {"agent": "claude", "session_id": "11111111-1111-4111-8111-111111111111", "has_cost": true, "has_token_data": true, "cost": {"microdollars": 1205000}, "cost_source": "reported", "models": ["example-model"], "total_output_tokens": 1200, "subagent_count": 0},
  "22222222-2222-4222-8222-222222222222": {"agent": "codex", "session_id": "22222222-2222-4222-8222-222222222222", "has_cost": true, "has_token_data": true, "cost": {"microdollars": 1205000}, "cost_source": "reported", "models": ["example-model"], "total_output_tokens": 800, "subagent_count": 0},
  "33333333-3333-4333-8333-333333333333": {"agent": "claude", "session_id": "33333333-3333-4333-8333-333333333333", "has_cost": true, "has_token_data": true, "cost": {"microdollars": 2410000}, "cost_source": "computed", "models": ["example-model"], "total_output_tokens": 2000, "subagent_count": 0},
  "55555555-5555-4555-8555-555555555555": {"agent": "claude", "session_id": "55555555-5555-4555-8555-555555555555", "has_cost": true, "has_token_data": true, "cost": {"microdollars": 9223372036854775807}, "cost_source": "reported", "models": ["example-model"], "total_output_tokens": 10, "subagent_count": 0},
  "66666666-6666-4666-8666-666666666666": {"agent": "codex", "session_id": "66666666-6666-4666-8666-666666666666", "has_cost": true, "has_token_data": true, "cost": {"microdollars": 1}, "cost_source": "reported", "models": ["example-model"], "total_output_tokens": 10, "subagent_count": 0}
}
EOF

observation() {
    local message="$1" first_agent="$2" first_id="$3" second_agent="$4" second_id="$5" job_uid="$6" event_uid="$7"
    cat <<EOF
{"actor": "demo-operator", "job_uid": "$job_uid", "definition_event_uid": "$event_uid", "status": "succeeded", "started_at": "2026-11-12T07:00:00Z", "ended_at": "2026-11-12T07:04:00Z", "expected_revision": 0,
 "summary": {"version": 1, "message": "$message", "input_tokens": 30, "output_tokens": 10,
  "sessions": [{"agent": "$first_agent", "session_id": "$first_id", "step": "main"}, {"agent": "$second_agent", "session_id": "$second_id", "step": "review"}]}}
EOF
}
cat >"$OUT/fixtures/job.json" <<'EOF'
{"name":"Example cost","definition":{"version":1,"kind":"job","enabled":false,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Example task"},"issue":{"kind":"per-run","title":"Example task"},"overlap":"forbid","catchup":"latest","checkout_key":"example-project"}}
EOF

delay 0
env -i PATH="$SAFE_PATH" HOME="$TMP_ROOT" LANG=C.UTF-8 python3 -I "$PROOF_DIR/fake_agentsview.py" \
    "$OUT/fixtures/agentsview-sessions.json" "$TMP_ROOT/delay" "$TMP_ROOT/fake.port" \
    2>"$OUT/logs/fake-agentsview.log" &
FAKE_PID=$!
for _ in $(seq 1 100); do
    [[ -s "$TMP_ROOT/fake.port" ]] && break
    sleep 0.1
done
[[ -s "$TMP_ROOT/fake.port" ]] || fail "fake AgentsView did not start"
FAKE_PORT="$(cat "$TMP_ROOT/fake.port")"

{
    echo "proof_head=$(git -C "$REPO_ROOT" rev-parse HEAD)"
    echo "before_sha=$BEFORE_SHA"
    echo "after_sha=$AFTER_SHA"
    echo "terminal=${WIDTH}x${HEIGHT} tmux TERM=tmux-256color KATA_COLOR_MODE=dark COLORTERM=truecolor"
    echo "tmux=$(tmux -V)"
    echo "freeze=$("$FREEZE" --version 2>&1 | head -n 1)"
    echo "go=$(go version)"
    echo "python=$(python3 --version)"
    echo "child_env_allowlist=PATH HOME TMPDIR TZ LANG LC_ALL DO_NOT_TRACK KATA_HOME KATA_AUTHOR (TUI adds TERM COLORTERM KATA_COLOR_MODE)"
    echo "agentsview=fake loopback producer on 127.0.0.1, no token; fixtures/agentsview-sessions.json"
    echo "unavailable_state=run C's synthetic prices overflow the daemon total, so GET usage answers usage_total_overflow (502)"
} >"$OUT/provenance.txt"

build() {
    local name="$1" sha="$2" dir="$TMP_ROOT/src-$1"
    mkdir -p "$dir"
    git -C "$REPO_ROOT" archive "$sha" | tar -x -C "$dir"
    GOFLAGS=-buildvcs=false go -C "$dir" build -o "$TMP_ROOT/kata-$name" ./cmd/kata
    echo "kata_${name}_sha256=$(sha256sum "$TMP_ROOT/kata-$name" | cut -d' ' -f1)" >>"$OUT/provenance.txt"
}

seed() {
    local name="$1" bin="$2" home="$TMP_ROOT/$1"
    mkdir -p "$home/user" "$home/kata" "$home/ws" "$OUT/logs/$name"
    printf '[agentsview]\nurl = "http://127.0.0.1:%s"\n' "$FAKE_PORT" >"$home/kata/config.toml"
    STARTED_HOMES+=("$home")
    STARTED_BINS+=("$bin")
    kenv "$home" "$bin" --workspace "$home/ws" init --project demo-cost-project >/dev/null
    local run job_uid=01JD5000000000000000000010 event_uid
    kenv "$home" "$bin" --workspace "$home/ws" cron job create --uid "$job_uid" \
        --file "$OUT/fixtures/job.json" --json >"$OUT/fixtures/job-$name.json"
    event_uid="$(python3 -I -c 'import json,sys; j=json.load(open(sys.argv[1]))["job"]; assert j["uid"] == sys.argv[2]; print(j["definition_event_uid"])' "$OUT/fixtures/job-$name.json" "$job_uid")"
    observation "Complete cost run" claude 11111111-1111-4111-8111-111111111111 codex 22222222-2222-4222-8222-222222222222 "$job_uid" "$event_uid" >"$OUT/fixtures/run-$name-a.json"
    observation "Partial cost run" claude 33333333-3333-4333-8333-333333333333 codex 44444444-4444-4444-8444-444444444444 "$job_uid" "$event_uid" >"$OUT/fixtures/run-$name-b.json"
    observation "Unavailable cost run" claude 55555555-5555-4555-8555-555555555555 codex 66666666-6666-4666-8666-666666666666 "$job_uid" "$event_uid" >"$OUT/fixtures/run-$name-c.json"
    for run in a:01JD5000000000000000000001 b:01JD5000000000000000000002 c:01JD5000000000000000000003; do
        kenv "$home" "$bin" --workspace "$home/ws" cron run observe "${run#*:}" \
            --json-input "$OUT/fixtures/run-$name-${run%%:*}.json" --json >/dev/null
    done
}

start_tui() {
    local name="$1" bin="$2" home="$TMP_ROOT/$1"
    tmux_cmd kill-server >/dev/null 2>&1 || true
    tmux_cmd new-session -d -s "$SESSION" -x "$WIDTH" -y "$HEIGHT"
    tmux_cmd set-option -g default-terminal "tmux-256color"
    tmux_cmd set-option -ga terminal-overrides ",*:Tc"
    tmux_cmd set-option -g remain-on-exit on
    tmux_cmd respawn-pane -k -t "$SESSION" \
        "env -i PATH=$SAFE_PATH HOME=$home/user TMPDIR=$TMP_ROOT/tmp TZ=UTC LANG=C.UTF-8 LC_ALL=C.UTF-8 DO_NOT_TRACK=1 KATA_HOME=$home/kata KATA_AUTHOR=demo-operator TERM=tmux-256color COLORTERM=truecolor KATA_COLOR_MODE=dark $bin --workspace $home/ws tui"
    local _
    for _ in $(seq 1 30); do
        send 3
        sleep 1
        shows "loaded run" && break
    done
    wait_until "loaded run"
    wait_until "Unavailable cost run"
}

build before "$BEFORE_SHA"
build after "$AFTER_SHA"

# Before: the same runs and source, opened the same way, show no cost row.
seed before "$TMP_ROOT/kata-before"
start_tui before "$TMP_ROOT/kata-before"
for state in complete partial unavailable; do
    open_run "${state^} cost run"
    wait_gone "loading…"
    sleep 1.5
    capture "before/$state"
    send h
done

# After: first read pending, each answered state, then a held refresh.
seed after "$TMP_ROOT/kata-after"
start_tui after "$TMP_ROOT/kata-after"
delay 1.5
open_run "Complete cost run"
wait_until "Cost: +loading…"
capture "after/loading"
delay 0
wait_until 'Cost: +\$2\.41'
wait_gone "loading…"
capture "after/complete"
# The daemon reuses a lookup younger than ten seconds even on refresh.
sleep 11
delay 1.5
send r
wait_until 'Cost: +\$2\.41 · refreshing…'
capture "after/refreshing"
delay 0
wait_gone "refreshing…|loading…"
capture "after/refreshed"
send h
open_run "Partial cost run"
wait_until "1 of 2 sessions not priced"
wait_gone "loading…"
capture "after/partial"
send h
open_run "Unavailable cost run"
wait_until "Cost: +unavailable"
wait_gone "loading…"
capture "after/unavailable"
send h
echo "capture complete" >>"$OUT/provenance.txt"
