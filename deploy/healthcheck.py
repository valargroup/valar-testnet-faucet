#!/usr/bin/env python3
"""Valar testnet faucet health check -> Sentry -> Slack (#zakura-snapshots-alert).

Run every 5 minutes by valar-faucet-healthcheck.timer. Each run:

1. Checks the public site: GET /healthz (the app answers through Caddy and TLS), then
   GET /readyz (zecd reachable, synced, and funded; the body names the reason if not).
2. Sends a Sentry cron check-in, ok or error, for monitor `valar-faucet-health`. Every
   check-in also upserts the monitor's schedule and thresholds. Sentry opens an issue
   after 2 consecutive error or missed check-ins; a dead host or timer sends nothing,
   so the missed check-in is the dead-man signal. The Sentry project's existing Slack
   rule routes new and regressed issues to the channel.
3. Logs the failure reason locally. The cron incident is the only Sentry issue:
   healthy check-ins resolve it automatically, with no separate error issues to orphan.

Mirrors the zakura-snapshots check scripts, but talks to Sentry's HTTP APIs directly,
so the host needs no sentry-cli. Configuration comes from the environment:
SENTRY_DSN (required; unset means check and log only), FAUCET_PUBLIC_URL, STATE_DIRECTORY.

`healthcheck.py --self-test` runs the whole flow against local fake servers.
"""

from __future__ import annotations

import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

MONITOR_SLUG = "valar-faucet-health"
MONITOR_CONFIG = {
    "schedule": {"type": "crontab", "value": "*/5 * * * *"},
    "checkin_margin": 5,  # minutes of grace for a late check-in
    "max_runtime": 2,  # minutes
    "failure_issue_threshold": 2,  # consecutive bad check-ins before an issue
    "recovery_threshold": 1,
    "timezone": "UTC",
}
TIMEOUT = 15


def log(msg: str) -> None:
    print(f"[valar-faucet-healthcheck] {msg}", flush=True)


def http_get(url: str) -> tuple[int, str]:
    """Returns (status, body); status 0 means the request itself failed."""
    req = urllib.request.Request(url, headers={"User-Agent": "valar-faucet-healthcheck"})
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
            return resp.status, resp.read(2048).decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read(2048).decode("utf-8", "replace")
    except Exception as e:  # DNS, TLS, refused, timeout
        return 0, f"{type(e).__name__}: {e}"


def check(base: str) -> str | None:
    """Returns None when healthy, otherwise a short human-readable reason."""
    status, body = http_get(f"{base}/healthz")
    if status != 200:
        detail = f"HTTP {status}" if status else body
        return f"site down ({detail[:120]})"
    status, body = http_get(f"{base}/readyz")
    if status != 200:
        detail = body.strip().splitlines()[0] if body.strip() else f"HTTP {status}"
        return f"not accepting claims: {detail[:120]}"
    return None


class Sentry:
    def __init__(self, dsn: str):
        u = urllib.parse.urlsplit(dsn)
        if not (u.scheme and u.username and u.hostname and u.path.strip("/")):
            raise ValueError("malformed SENTRY_DSN")
        self.dsn = dsn
        self.key = u.username
        self.project = u.path.strip("/").split("/")[-1]
        port = f":{u.port}" if u.port else ""
        self.base = f"{u.scheme}://{u.hostname}{port}"

    def _post(self, url: str, data: bytes, headers: dict[str, str]) -> int:
        req = urllib.request.Request(url, data=data, headers=headers, method="POST")
        try:
            with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
                return resp.status
        except urllib.error.HTTPError as e:
            return e.code
        except Exception as e:
            log(f"sentry request failed: {type(e).__name__}")
            return 0

    def checkin(self, ok: bool) -> int:
        url = f"{self.base}/api/{self.project}/cron/{MONITOR_SLUG}/{self.key}/"
        body = json.dumps({"status": "ok" if ok else "error", "monitor_config": MONITOR_CONFIG}).encode()
        return self._post(url, body, {"Content-Type": "application/json"})



def load_state(path: str) -> dict:
    try:
        with open(path) as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def save_state(path: str, state: dict) -> None:
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(state, f)
    os.replace(tmp, path)


def run(base: str, sentry: Sentry | None, state_path: str) -> int:
    reason = check(base)
    state = load_state(state_path)
    failures = state.get("failures", 0)
    if reason is not None:
        failures += 1
        log(f"UNHEALTHY ({failures} in a row): {reason}")
        state = {"failures": failures}
    if sentry:
        status = sentry.checkin(reason is None)
        accepted = 200 <= status < 300
        log(f"check-in {'ok' if reason is None else 'error'} (HTTP {status})")
    else:
        accepted = True
        if reason is None:
            log("healthy (SENTRY_DSN unset; not reporting)")
    if reason is None and accepted:
        if failures:
            log(f"recovered after {failures} failed run(s)")
        state = {"failures": 0}
    elif reason is None:
        # Keep the outage evidence until Sentry accepts recovery. Every subsequent
        # healthy run sends another ok check-in, including after a process restart.
        log("faucet healthy; recovery check-in not accepted, will retry")
    save_state(state_path, state)
    return 0 if reason is None else 1


def self_test() -> int:
    """Runs the flow against fake faucet and Sentry servers on localhost."""
    import http.server
    import tempfile
    import threading

    faucet = {"health": 200, "ready": (200, "ready\n")}
    received: list[tuple[str, bytes]] = []
    response_status = [202]

    class Faucet(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            code, body = (faucet["health"], "ok\n") if self.path == "/healthz" else faucet["ready"]
            self.send_response(code)
            self.end_headers()
            self.wfile.write(body.encode())

        def log_message(self, *a):
            pass

    class FakeSentry(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            received.append((self.path, self.rfile.read(int(self.headers["Content-Length"]))))
            self.send_response(response_status[0])
            self.end_headers()

        def log_message(self, *a):
            pass

    servers = [http.server.ThreadingHTTPServer(("127.0.0.1", 0), h) for h in (Faucet, FakeSentry)]
    for s in servers:
        threading.Thread(target=s.serve_forever, daemon=True).start()
    base = f"http://127.0.0.1:{servers[0].server_port}"
    sentry = Sentry(f"http://publickey@127.0.0.1:{servers[1].server_port}/4242")
    state = os.path.join(tempfile.mkdtemp(), "state.json")

    def expect(cond: bool, what: str) -> None:
        if not cond:
            raise AssertionError(what)

    expect(run(base, sentry, state) == 0, "healthy run exits 0")
    expect(received[-1][0] == "/api/4242/cron/valar-faucet-health/publickey/", "check-in URL")
    body = json.loads(received[-1][1])
    expect(body["status"] == "ok" and body["monitor_config"]["failure_issue_threshold"] == 2, "ok check-in")

    faucet["ready"] = (503, "faucet is empty\n")
    received.clear()
    expect(run(base, sentry, state) == 1, "unhealthy run exits 1")
    expect([p for p, _ in received] == ["/api/4242/cron/valar-faucet-health/publickey/"], "first failure: check-in only")
    expect(json.loads(received[0][1])["status"] == "error", "error check-in")

    received.clear()
    run(base, sentry, state)
    expect(len(received) == 1 and received[0][0].endswith("/cron/valar-faucet-health/publickey/"),
           "second failure only reports a cron check-in, never a separate error issue")
    expect(load_state(state) == {"failures": 2}, "repeated failure is persisted")

    # Recovery must survive a rejected check-in and a new process/client instance.
    faucet["ready"] = (200, "ready\n")
    response_status[0] = 500
    received.clear()
    expect(run(base, sentry, state) == 0, "healthy faucet stays healthy during Sentry outage")
    expect(json.loads(received[-1][1])["status"] == "ok", "recovery sends ok")
    expect(load_state(state) == {"failures": 2}, "failed recovery delivery retains outage")
    response_status[0] = 202
    replacement = Sentry(sentry.dsn)
    expect(run(base, replacement, state) == 0 and load_state(state) == {"failures": 0},
           "next healthy run retries recovery after restart and resets state")

    faucet["health"] = 502
    received.clear()
    run(base, sentry, state)
    run(base, sentry, state)
    expect(len(received) == 2 and all(json.loads(body)["status"] == "error" for _, body in received),
           "site-down failures also use only the cron lifecycle")
    # Old deployed versions persisted outage_id/alerted; upgrades discard those only
    # after a successful recovery, without emitting or reopening old error events.
    save_state(state, {"failures": 3, "outage_id": "legacy", "alerted": True})
    faucet["health"] = 200
    expect(run(base, sentry, state) == 0 and load_state(state) == {"failures": 0},
           "recovery handles state written by the previous version")
    faucet["ready"] = (503, "wallet syncing\n")
    expect(run(base, None, state) == 1, "without DSN, failures still fail the check")
    faucet["ready"] = (200, "ready\n")
    expect(run(base, None, state) == 0 and load_state(state) == {"failures": 0},
           "without DSN, local recovery resets state")

    for s in servers:
        s.shutdown()
    print("self-test ok")
    return 0


def main() -> int:
    if sys.argv[1:] == ["--self-test"]:
        return self_test()
    base = os.environ.get("FAUCET_PUBLIC_URL", "https://faucet.testnet.valargroup.dev").rstrip("/")
    state_path = os.path.join(os.environ.get("STATE_DIRECTORY", "/tmp"), "healthcheck.json")
    dsn = os.environ.get("SENTRY_DSN", "").strip()
    sentry = Sentry(dsn) if dsn else None
    return run(base, sentry, state_path)


if __name__ == "__main__":
    sys.exit(main())
