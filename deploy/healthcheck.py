#!/usr/bin/env python3
"""Valar testnet faucet health check -> Sentry -> Slack (#zakura-snapshots-alerts).

Run every 5 minutes by valar-faucet-healthcheck.timer. Each run:

1. Checks the public site: GET /healthz (the app answers through Caddy and TLS), then
   GET /readyz (zecd reachable, synced, and funded; the body names the reason if not).
2. Sends a Sentry cron check-in, ok or error, for monitor `valar-faucet-health`. Every
   check-in also upserts the monitor's schedule and thresholds. Sentry opens an issue
   after 2 consecutive error or missed check-ins; a dead host or timer sends nothing,
   so the missed check-in is the dead-man signal. The Sentry project's existing Slack
   rule routes new and regressed issues to the channel.
3. When a failure reaches the second consecutive run, sends one error event that
   names the reason, fingerprinted per outage, so Slack says *why* once per outage
   rather than every five minutes.

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
import uuid

MONITOR_SLUG = "valar-faucet-health"
MONITOR_CONFIG = {
    "schedule": {"type": "crontab", "value": "*/5 * * * *"},
    "checkin_margin": 5,  # minutes of grace for a late check-in
    "max_runtime": 2,  # minutes
    "failure_issue_threshold": 2,  # consecutive bad check-ins before an issue
    "recovery_threshold": 1,
    "timezone": "UTC",
}
ALERT_AFTER = 2  # consecutive failed runs before the reason event (matches the monitor)
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

    def event(self, message: str, reason: str, outage_id: str) -> int:
        event_id = uuid.uuid4().hex
        event = {
            "event_id": event_id,
            "timestamp": time.time(),
            "platform": "other",
            "level": "error",
            "logger": "valar-faucet-healthcheck",
            "message": {"formatted": message},
            "tags": {"alert": "valar_faucet", "check": "health", "network": "testnet"},
            # One issue per outage and reason: the Slack rule fires on new issues.
            "fingerprint": ["valar-faucet-health", reason.split(":")[0], outage_id],
        }
        envelope = "\n".join([
            json.dumps({"event_id": event_id, "dsn": self.dsn}),
            json.dumps({"type": "event"}),
            json.dumps(event),
        ]).encode() + b"\n"
        auth = f"Sentry sentry_version=7, sentry_key={self.key}, sentry_client=valar-faucet-healthcheck/1"
        return self._post(f"{self.base}/api/{self.project}/envelope/", envelope,
                          {"Content-Type": "application/x-sentry-envelope", "X-Sentry-Auth": auth})


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
    if reason is None:
        if state.get("failures"):
            log(f"recovered after {state['failures']} failed run(s)")
        state = {"failures": 0}
    else:
        failures = state.get("failures", 0) + 1
        outage_id = state.get("outage_id") or time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
        state = {"failures": failures, "outage_id": outage_id, "alerted": state.get("alerted", False)}
        log(f"UNHEALTHY ({failures} in a row): {reason}")
        if sentry and failures >= ALERT_AFTER and not state["alerted"]:
            msg = f"Valar testnet faucet {reason} — {base}"
            status = sentry.event(msg, reason, outage_id)
            log(f"sent Sentry event (HTTP {status})")
            state["alerted"] = 200 <= status < 300
    if sentry:
        status = sentry.checkin(reason is None)
        log(f"check-in {'ok' if reason is None else 'error'} (HTTP {status})")
    elif reason is None:
        log("healthy (SENTRY_DSN unset; not reporting)")
    save_state(state_path, state)
    return 0 if reason is None else 1


def self_test() -> int:
    """Runs the flow against fake faucet and Sentry servers on localhost."""
    import http.server
    import tempfile
    import threading

    faucet = {"health": 200, "ready": (200, "ready\n")}
    received: list[tuple[str, bytes]] = []

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
            self.send_response(202)
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
    paths = [p for p, _ in received]
    expect(paths == ["/api/4242/envelope/", "/api/4242/cron/valar-faucet-health/publickey/"], "second failure: event + check-in")
    event = json.loads(received[0][1].split(b"\n")[2])
    expect("faucet is empty" in event["message"]["formatted"], "event names the reason")
    expect(event["level"] == "error" and event["fingerprint"][1] == "not accepting claims", "event fingerprint")

    received.clear()
    run(base, sentry, state)
    expect([p for p, _ in received] == ["/api/4242/cron/valar-faucet-health/publickey/"], "one event per outage")

    faucet["ready"] = (200, "ready\n")
    received.clear()
    expect(run(base, sentry, state) == 0 and load_state(state) == {"failures": 0}, "recovery resets state")

    faucet["health"] = 502
    run(base, sentry, state)
    received.clear()
    run(base, sentry, state)
    event = json.loads(received[0][1].split(b"\n")[2])
    expect(event["message"]["formatted"].startswith("Valar testnet faucet site down (HTTP 502)"), "site-down reason")

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
