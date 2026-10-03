# Valar Group Zcash testnet faucet

**https://faucet.testnet.valargroup.dev**

This faucet hands out testnet ZEC (TAZ) on the public Zcash testnet:

- **0.125 TAZ per IP address per rolling 24 hours.** IPv6 clients are limited per /64.
- Each address can receive a payout once per 24 hours.
- A global cap of 12.5 TAZ per 24 hours stops a botnet from draining the wallet.

Payouts come from a [zecd](https://github.com/zecrocks/zecd) wallet. zecd syncs from, and broadcasts through, the archive `zakurad` on `zakura-testnet-1`. Each payout is also pushed straight to every Zakura testnet node with `sendrawtransaction`, so it doesn't have to wait for gossip.

```
browser ─https─► Caddy (X-Real-IP) ─► valar-faucet :8093 ─► zecd :18890 ─► zakurad :18232
                                            └─ sendrawtransaction ─► zakura-testnet-{1,eu,as}
```

## How it works

- `POST /api/claim` does three things:
  - Validates the address with zecd (`z_validateaddress`).
  - Checks every limit inside one SQLite transaction.
  - Queues the claim and returns `202 {id, status}`.
- A single worker pays queued claims one at a time with `sendtoaddress`, then fans the raw transaction out to each Zakura node.

A claim moves through these states:

| Status | Meaning |
|---|---|
| `queued` | Accepted and waiting for the worker. |
| `sending` | zecd has been asked to pay. |
| `sent` | zecd returned a txid. |
| `failed` | Definitively not paid, for example because the node rejected it. Doesn't count against any limit. |
| `review` | The outcome is unknown: a crash or timeout happened mid-send. Counts against limits and is **never** re-sent automatically. |

- If zecd reports `-6` (no spendable notes yet), the claim goes back to the queue and the worker retries after 30s.
- Claims that are still queued after 30 minutes are failed.
- zecd splits change into payout-sized notes (`[spend] target_note_count`), so several claims can be paid per block.

### API

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/status` | Balance, chain height, limits, recent payouts, donation address |
| `POST` | `/api/claim` | `{"address": "utest1…"}` returns `202`, `400` (invalid), `429` + `Retry-After` (limited), or `503` (empty or syncing) |
| `GET` | `/api/claim/{id}` | Claim status, txid, and how many Zakura nodes accepted the broadcast |
| `GET` | `/healthz` | Process liveness (used by deploys) |
| `GET` | `/readyz` | `200` only when the wallet is reachable, synced and funded |

## Repository layout

| Path | What |
|---|---|
| `cmd/valar-faucet` | Entry point; config from `FAUCET_*` env vars |
| `internal/faucet` | Claim rules, payout worker, status snapshot |
| `internal/store` | SQLite claims table and the limit checks |
| `internal/zecd`, `internal/jsonrpc` | zecd JSON-RPC client |
| `internal/broadcast` | Direct `sendrawtransaction` fan-out to Zakura nodes |
| `internal/web` | HTTP API and the embedded page (`static/`), styled after valargroup.dev |
| `deploy/` | systemd units, `zecd.toml`, `faucet.env`, Caddy snippet, pinned zecd version, `install.sh` |

## Development

```sh
go test -race ./...
```

To run the whole stack locally against the public testnet:

1. Start zecd with a throwaway wallet. Copy `deploy/zecd.toml` and change three things:
   - point `datadir` and the wallet `dir` at a local directory
   - point `password_file` at a local file
   - set `server = "zebra://167.99.103.111:18232"`
2. Then run:

   ```sh
   zecd --conf zecd.local.toml init --wallet default   # prints a mnemonic; throwaway only
   zecd --conf zecd.local.toml run
   ```

3. Start the faucet in another shell:

   ```sh
   set -a; . deploy/faucet.env; set +a
   FAUCET_DB=./faucet.db FAUCET_ZECD_PASSWORD_FILE=./rpc.password go run ./cmd/valar-faucet
   ```

4. Open http://127.0.0.1:8093.

## Deployment

The faucet runs on **zakura-testnet-1** (167.99.103.111, DO project `zakura-testnet`), next to the testnet archive node. **Every push to `main` deploys** through `.github/workflows/deploy.yml`:

1. Test and build a static linux/amd64 binary.
2. Stream the release to `/opt/valar-faucet/releases/<id>/` over SSH.
3. Run `deploy/install.sh <id>` as root on the host. It:
   - creates the users and directories and the host-local zecd RPC password
   - installs the pinned zecd, checked against the sha256 in `deploy/zecd.version`
   - validates `zecd.toml` with `zecd config check` before installing it
   - installs the units and the Caddy snippet, running `caddy validate` before reloading
   - flips `/opt/valar-faucet/current`, health-checks it, and **rolls back** on failure
4. Check `https://faucet.testnet.valargroup.dev/healthz` from the runner.

**Where things live on the host:**

| Path | Contents |
|---|---|
| `/opt/valar-faucet/{releases,current,zecd}` | Releases (the last 5 are kept) and zecd binaries |
| `/etc/valar-faucet/zecd.toml` | zecd config |
| `/etc/valar-faucet/zecd-rpc.password` | RPC password, `root:valar-zecd 0640`, host-local |
| `/mnt/data/valar-faucet/zecd` | Wallet (`valar-zecd`) |
| `/mnt/data/valar-faucet/app/faucet.db` | Claims database (`valar-faucet`) |
| `/etc/caddy/conf.d/valar-faucet.caddy` | Site block, imported by the managed Caddyfile |

**Services:** `valar-faucet-zecd.service` (zecd) and `valar-faucet.service` (the web app).

**GitHub configuration:** Environment `testnet`, with deployment branches limited to `main`.

| Kind | Name | Value |
|---|---|---|
| Secret | `DEPLOY_SSH_KEY` | ed25519 key authorized for root on the host. Source of truth: Infisical project "Zakura snapshots" (`c57a6889-6a7c-4d05-a54a-e4a4c0b14ee7`), env `prod`, path `/testnet-faucet`, `VALAR_FAUCET_DEPLOY_SSH_PRIVATE_KEY` |
| Variable | `DEPLOY_HOST` | `167.99.103.111` |
| Variable | `DEPLOY_KNOWN_HOSTS` | Output of `ssh-keyscan -t ed25519 167.99.103.111`, checked against the host's `/etc/ssh/ssh_host_ed25519_key.pub` |

### Caddy on a shared host

`/etc/caddy/Caddyfile` on zakura-testnet-1 is owned by `zakura-core/zakura` (`deploy/gateway/testnet/Caddyfile`, installed by `zakura-testnet-deploy.yml`). That file carries `import /etc/caddy/conf.d/*.caddy`, which is how this repo's snippet is served.

If a fleet deploy ever installs a Caddyfile without that line, the faucet drops off the public hostname. `install.sh` warns when this happens, and the deploy's public health check fails.

### NU7 readiness

The pinned zecd 0.9.0-rc1 release uses Zakura Common 2.2.0 and schedules public
Testnet NU7 at height 4,465,026 with branch ID `77190ad9`. The older 0.8.1
release does not schedule this activation. The faucet API also allows the
`https://zakura.com` dashboard origin on its three API routes; other origins do
not receive CORS permission. Payout limits and wallet ownership checks apply
unchanged.

Before upgrading a funded wallet, save a consistent SQLite backup and its
matching encrypted wallet files on the same host, preserve the old runtime and
configuration, and rehearse the new binary against an isolated copy. Check
there are no outstanding sends before the copy. Bind the rehearsal RPC to
loopback on a separate port, allow only read methods, and do not run faucet
workers against it. Verify the copied balance and sync state before restarting
the existing service. Rollback after a schema migration restores the matching
pre-upgrade database copy; do not run the old binary against a migrated database.

After upgrading, verify the service version, wallet balance and advancing scan
height, API readiness, an actual claim, and cross-origin preflight from
`https://zakura.com`. A real post-activation v6 payout must be checked after
activation; pre-activation claims cannot establish that result.

### Wallet

The faucet wallet is the **testnet miner's seed** (BIP-39, BIP-44 account 0).
- Its source of truth is Infisical `VALAR_FAUCET_ZECD_MNEMONIC` (project "Zakura snapshots", env `prod`, path `/testnet-faucet`).
- `zakura-testnet-mining` mines to its transparent address `tmK9XQyaisELfPsiGudwNXTRHo7G3z2uUwa` (external index 0).
- The same seed is also loaded in the `public` zecd wallet on the mining host. Spending from both at once can conflict: one transaction is rejected, and its notes stay locked until it expires.

To restore it on a host, stream the phrase over stdin so it never touches a command line. The first miner payout was at height 4,271,369, so start the scan just before it:

```sh
systemctl stop valar-faucet-zecd
infisical secrets get VALAR_FAUCET_ZECD_MNEMONIC --projectId=c57a6889-6a7c-4d05-a54a-e4a4c0b14ee7 \
    --env=prod --path=/testnet-faucet --plain --silent |
  ssh root@167.99.103.111 'runuser -u valar-zecd -- /opt/valar-faucet/zecd/current/zecd \
    --conf /etc/valar-faucet/zecd.toml init --restore --wallet default --birthday 4271000 >/dev/null'
ssh root@167.99.103.111 'systemctl start valar-faucet-zecd && systemctl restart valar-faucet'
```

### Shielding mined coins

Mined coins sit at the transparent address, and consensus lets them move only into a shielded pool. Until then they can't fund payouts.
- `getbalances` counts them under `mine.coinbase`.
- The page shows them as "unshielded", and they aren't counted as spendable.

To shield them into the wallet's own shielded address, 250 outputs per transaction, rerun this until `mine.coinbase` reaches 0:

```sh
ssh root@167.99.103.111
rpc() { curl -s -u "faucet:$(cat /etc/valar-faucet/zecd-rpc.password)" -H 'content-type: application/json' \
  -d "{\"jsonrpc\":\"1.0\",\"id\":1,\"method\":\"$1\",\"params\":$2}" http://127.0.0.1:18890/; }
to=$(curl -s 127.0.0.1:8093/api/status | python3 -c 'import sys,json;print(json.load(sys.stdin)["donationAddress"])')
rpc z_shieldcoinbase "[\"*\", \"$to\", null, 250]"   # -> {"opid": ...}
rpc z_waitforoperation '["<opid>", 300]'
```

Shielded funds become spendable after `[spend] trusted_confirmations` (3) blocks.

### Funding

Send TAZ to the donation address shown on the page or in `/api/status` (`donationAddress`). Any Orchard/Ironwood-capable testnet wallet works. The page reports "empty" when the shielded spendable plus pending balance is below one payout plus a fee reserve.

### Alerts

`valar-faucet-healthcheck.timer` runs `deploy/healthcheck.py` on the host every 5 minutes. It checks the public site (`/healthz`, then `/readyz`) and reports to the Sentry project **`zakura-snapshots`**. That project's existing `Notify zakura-snapshots-alert via Slack` rule posts to **#zakura-snapshots-alert**.

Sentry is told two things:
- **A cron check-in** for monitor `valar-faucet-health`, ok or error. Each check-in also upserts the monitor settings: schedule `*/5 * * * *`, a 5-minute margin, an issue after 2 consecutive bad check-ins, and recovery after 1. When the host or the timer dies, check-ins stop arriving, and the monitor alerts on the missing check-in.
- **One error event per outage** once a failure repeats, naming the reason:
  - `site down (HTTP 502)`
  - `not accepting claims: faucet is empty`
  - `not accepting claims: wallet syncing`
  - `not accepting claims: wallet unreachable`

The DSN comes from Infisical ("Zakura snapshots" / `prod` / `SENTRY_DSN`). Place it on the host once; `install.sh` enables the timer only when this file exists:

```sh
infisical secrets get SENTRY_DSN --projectId=c57a6889-6a7c-4d05-a54a-e4a4c0b14ee7 --env=prod --plain --silent |
  ssh root@167.99.103.111 'umask 077; { printf "SENTRY_DSN="; cat; } > /etc/valar-faucet/sentry.env'
```

To check the monitor: `journalctl -u valar-faucet-healthcheck -n 20`.

To run a check now: `systemctl start valar-faucet-healthcheck`.

### Operations

```sh
journalctl -u valar-faucet -f                                         # web + worker logs (JSON)
journalctl -u valar-faucet-zecd -f                                    # wallet logs
curl -s 127.0.0.1:9234/readyz                                         # zecd readiness
sqlite3 /mnt/data/valar-faucet/app/faucet.db \
  "select id, address, status, txid, error from claims where status='review'"
```

**Claims in `review`:** look the txid up with zecd `gettransaction`, or search the node's mempool and chain. Then set the claim to `sent` (with its txid) or `failed` by hand.

**Manual rollback:**

```sh
ln -sfn /opt/valar-faucet/releases/<previous-id> /opt/valar-faucet/current
systemctl restart valar-faucet
```

**Upgrading zecd:** bump both `ZECD_VERSION` and `ZECD_SHA256` in `deploy/zecd.version`; the next deploy installs it and restarts zecd.

### One public NU7 payout qualification

`deploy/install-qualification.sh` installs a separate unprivileged timer and preserves
an existing `/etc/valar-faucet/qualification.json`. The example config has
`enabled: false`. Enabling this payout check does not arm the network selector:
every new attempt requires the fresh dashboard to have selected public Testnet,
three validators to agree, and both the local node and independent Zebra 7
reference to have reached activation plus two blocks on branch `77190ad9` with a
common checkpoint. Until then the check sends no payout. The reference endpoint
must remain restricted to the existing observer host.

The authorized controlled recipient is the existing test address
`tmDCiNGTbRz1Y1eYWyrPBFCaH61JSffwzSr`; the check requests exactly 0.125 TAZ.
The persistent receipt is written and synced before POST. If the request is
ambiguous, no automatic second POST is possible: reconcile the API's request and
claim history manually before changing the receipt. An existing claim ID resumes
only polling. Acceptance requires v6 with NU7 branch, the exact recipient/amount,
two confirmations, and the mined block hash on the independent reference.
An accepted receipt makes subsequent timer runs a cheap local no-op.

The timer checks every two minutes, has a bounded 90-second service invocation,
and holds no wallet or SSH credentials. Check it with
`systemctl status valar-faucet-qualification.timer` and
`journalctl -u valar-faucet-qualification.service -n 20`.
The real post-activation payout remains pending until these gates pass.
