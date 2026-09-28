#!/usr/bin/env bash
# install.sh <release-id>
#
# Activates /opt/valar-faucet/releases/<release-id> on the faucet host. Run as root; CI
# runs it over SSH after unpacking a release. Idempotent: every step checks current state
# first, so re-running a release is safe.
#
#   1. users, directories and the host-local zecd RPC password
#   2. the pinned zecd binary (checksum-verified)
#   3. zecd config and systemd units
#   4. the Caddy site snippet (validated before reload; restored on failure)
#   5. zecd restart when its binary or config changed (only once a wallet exists)
#   6. switch the faucet to this release, health-check, roll back on failure
#   7. prune old releases
set -euo pipefail

REL_ID=${1:?usage: install.sh <release-id>}
ROOT=/opt/valar-faucet
REL=$ROOT/releases/$REL_ID
DATA=/mnt/data/valar-faucet
ETC=/etc/valar-faucet
UNIT_DIR=/etc/systemd/system
CADDYFILE=/etc/caddy/Caddyfile
CADDY_SNIPPET=/etc/caddy/conf.d/valar-faucet.caddy
HEALTH_URL=http://127.0.0.1:8093/healthz
WALLET_KEYS=$DATA/zecd/default/keys.toml
KEEP_RELEASES=5

log() { printf '[install] %s\n' "$*"; }
warn() { printf '[install] WARNING: %s\n' "$*" >&2; }
die() { printf '[install] ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "must run as root"
[[ $(uname -m) == x86_64 ]] || die "unsupported architecture $(uname -m)"
[[ -x $REL/valar-faucet ]] || die "release $REL has no valar-faucet binary"

# install_if_changed <src> <dst> <mode> <owner:group>: installs src at dst when the
# content differs; returns 0 if it changed dst, 1 if dst was already current.
install_if_changed() {
	local src=$1 dst=$2 mode=$3 owner=$4
	if [[ -f $dst ]] && cmp -s "$src" "$dst"; then
		return 1
	fi
	install -m "$mode" -o "${owner%%:*}" -g "${owner##*:}" "$src" "$dst"
	log "updated $dst"
	return 0
}

# ---------------------------------------------------------------- 1. users and dirs
for user in valar-zecd valar-faucet; do
	if ! id -u "$user" >/dev/null 2>&1; then
		useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$user"
		log "created system user $user"
	fi
done
[[ -d /mnt/data ]] || die "/mnt/data is not mounted"
install -d -m 755 "$ROOT" "$ROOT/releases" "$ROOT/zecd" "$DATA" /etc/caddy/conf.d
install -d -m 750 -o root -g valar-zecd "$ETC"
install -d -m 700 -o valar-zecd -g valar-zecd "$DATA/zecd"
install -d -m 700 -o valar-faucet -g valar-faucet "$DATA/app"

if [[ ! -s $ETC/zecd-rpc.password ]]; then
	# Host-local loopback credential; it never leaves this machine.
	(umask 077 && openssl rand -hex 32 >"$ETC/zecd-rpc.password")
	log "generated zecd RPC password"
fi
chown root:valar-zecd "$ETC/zecd-rpc.password"
chmod 640 "$ETC/zecd-rpc.password"

# ---------------------------------------------------------------- 2. zecd binary
ZECD_VERSION=$(sed -n 's/^ZECD_VERSION=//p' "$REL/deploy/zecd.version")
ZECD_SHA256=$(sed -n 's/^ZECD_SHA256=//p' "$REL/deploy/zecd.version")
[[ $ZECD_VERSION =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "bad ZECD_VERSION '$ZECD_VERSION'"
[[ $ZECD_SHA256 =~ ^[0-9a-f]{64}$ ]] || die "bad ZECD_SHA256"
ZECD_DIR=$ROOT/zecd/$ZECD_VERSION
zecd_changed=0

if [[ ! -x $ZECD_DIR/zecd ]]; then
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	tarball=zecd-$ZECD_VERSION-linux-amd64.tar.gz
	log "downloading zecd $ZECD_VERSION"
	curl -fsSL --retry 3 -o "$tmp/$tarball" \
		"https://github.com/zecrocks/zecd/releases/download/v$ZECD_VERSION/$tarball"
	echo "$ZECD_SHA256  $tmp/$tarball" | sha256sum -c --quiet - || die "zecd checksum mismatch"
	tar -xzf "$tmp/$tarball" -C "$tmp"
	install -d -m 755 "$ZECD_DIR"
	install -m 755 "$tmp/zecd-$ZECD_VERSION-linux-amd64/zecd" "$ZECD_DIR/zecd"
	log "installed zecd $ZECD_VERSION"
fi
if [[ $(readlink "$ROOT/zecd/current" || true) != "$ZECD_DIR" ]]; then
	ln -sfn "$ZECD_DIR" "$ROOT/zecd/current"
	zecd_changed=1
	log "zecd now $ZECD_VERSION"
fi
ZECD=$ROOT/zecd/current/zecd

# ---------------------------------------------------------------- 3. config and units
# Validate the candidate zecd config before it replaces the live one.
install -m 640 -o root -g valar-zecd "$REL/deploy/zecd.toml" "$ETC/zecd.toml.candidate"
if ! runuser -u valar-zecd -- "$ZECD" --conf "$ETC/zecd.toml.candidate" config check; then
	rm -f "$ETC/zecd.toml.candidate"
	die "zecd config check failed"
fi
if install_if_changed "$ETC/zecd.toml.candidate" "$ETC/zecd.toml" 640 root:valar-zecd; then
	zecd_changed=1
fi
rm -f "$ETC/zecd.toml.candidate"

units_changed=0
for unit in valar-faucet.service valar-faucet-zecd.service; do
	if install_if_changed "$REL/deploy/$unit" "$UNIT_DIR/$unit" 644 root:root; then
		units_changed=1
		if [[ $unit == valar-faucet-zecd.service ]]; then
			zecd_changed=1
		fi
	fi
done
if ((units_changed)); then
	systemctl daemon-reload
fi
systemctl enable --quiet valar-faucet-zecd.service valar-faucet.service

# ---------------------------------------------------------------- 4. Caddy
if ! grep -Eq '^[[:space:]]*import[[:space:]]+/etc/caddy/conf\.d/\*\.caddy' "$CADDYFILE"; then
	warn "$CADDYFILE does not import /etc/caddy/conf.d/*.caddy; the faucet is not publicly routed."
	warn "Add the import to zakura-core/zakura deploy/gateway/testnet/Caddyfile (see README)."
fi
snippet_backup=
if [[ -f $CADDY_SNIPPET ]]; then
	snippet_backup=$(mktemp)
	cp -p "$CADDY_SNIPPET" "$snippet_backup"
fi
if install_if_changed "$REL/deploy/valar-faucet.caddy" "$CADDY_SNIPPET" 644 root:root; then
	if ! caddy validate --config "$CADDYFILE" --adapter caddyfile >/dev/null 2>&1; then
		if [[ -n $snippet_backup ]]; then
			cp -p "$snippet_backup" "$CADDY_SNIPPET"
		else
			rm -f "$CADDY_SNIPPET"
		fi
		caddy validate --config "$CADDYFILE" --adapter caddyfile || true
		die "Caddy rejected the faucet snippet; restored the previous one"
	fi
	systemctl reload caddy.service
	log "reloaded Caddy"
fi
if [[ -n $snippet_backup ]]; then
	rm -f "$snippet_backup"
fi

# ---------------------------------------------------------------- 5. zecd
if [[ -f $WALLET_KEYS ]]; then
	if ((zecd_changed)) || ! systemctl is-active --quiet valar-faucet-zecd.service; then
		systemctl restart valar-faucet-zecd.service
		log "restarted valar-faucet-zecd"
	fi
else
	warn "no wallet at $WALLET_KEYS yet; zecd stays stopped until it is initialized (README: Wallet setup)."
fi

# ---------------------------------------------------------------- 6. faucet release
previous=$(readlink "$ROOT/current" || true)
activate() {
	ln -sfn "$1" "$ROOT/current.next"
	mv -T "$ROOT/current.next" "$ROOT/current"
	systemctl restart valar-faucet.service
}
healthy() {
	for _ in $(seq 1 30); do
		if curl -fsS --max-time 2 "$HEALTH_URL" >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	return 1
}

activate "$REL"
if ! healthy; then
	journalctl -u valar-faucet.service -n 40 --no-pager || true
	if [[ -n $previous && -d $previous && $previous != "$REL" ]]; then
		warn "release $REL_ID failed its health check; rolling back to $(basename "$previous")"
		activate "$previous"
		healthy || warn "previous release is not healthy either"
	fi
	die "release $REL_ID is not healthy"
fi
log "valar-faucet $("$ROOT/current/valar-faucet" -version) is live"

# ---------------------------------------------------------------- 7. prune
# Release ids start with a UTC timestamp, so name order is age order.
mapfile -t old < <(find "$ROOT/releases" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' |
	sort -r | tail -n +$((KEEP_RELEASES + 1)))
for name in "${old[@]}"; do
	dir=$ROOT/releases/$name
	if [[ $dir == "$REL" || $dir == "$previous" ]]; then
		continue
	fi
	rm -rf -- "$dir"
	log "pruned $(basename "$dir")"
done
