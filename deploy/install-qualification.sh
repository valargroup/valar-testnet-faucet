#!/usr/bin/env bash
set -euo pipefail
qualification_source=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
if ! id valar-faucet-qualifier >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin valar-faucet-qualifier
fi
install -d -m 0755 /opt/valar-faucet-qualification /etc/valar-faucet
install -m 0755 "$qualification_source/qualify_public_faucet.py" /opt/valar-faucet-qualification/qualify.py
if [[ ! -e /etc/valar-faucet/qualification.json ]]; then
  install -m 0644 "$qualification_source/qualification.example.json" /etc/valar-faucet/qualification.json
fi
install -m 0644 "$qualification_source/valar-faucet-qualification.service" /etc/systemd/system/
install -m 0644 "$qualification_source/valar-faucet-qualification.timer" /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now valar-faucet-qualification.timer
