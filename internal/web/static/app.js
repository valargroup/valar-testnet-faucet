// Valar Group testnet faucet — page logic. No dependencies; all untrusted text is
// written with textContent.
(() => {
  "use strict";

  const STATUS_EVERY_MS = 15000;
  const CLAIM_POLL_MS = 2500;
  const CLAIM_POLL_LIMIT_MS = 15 * 60 * 1000;
  const CLAIM_KEY = "valar-faucet-claim";

  const $ = (id) => document.getElementById(id);
  const form = $("claim-form");
  const input = $("address");
  const submit = $("submit");
  const alertBox = $("alert");
  const result = $("result");
  const resultMsg = $("result-msg");

  let status = null;
  let submitting = false;
  let pollTimer = null;

  // ---------- formatting ----------

  function duration(seconds) {
    seconds = Math.max(0, Math.round(seconds));
    const h = Math.floor(seconds / 3600);
    const m = Math.floor((seconds % 3600) / 60);
    if (h > 0) return m > 0 ? `${h}h ${m}m` : `${h}h`;
    if (m > 0) return `${m}m`;
    return `${seconds}s`;
  }

  function ago(unix) {
    const s = Date.now() / 1000 - unix;
    if (s < 60) return "just now";
    if (s < 3600) return `${Math.floor(s / 60)}m ago`;
    if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
    return `${Math.floor(s / 86400)}d ago`;
  }

  // "1850.375" -> "1,850.375": group the integer part without touching the decimals.
  function grouped(amount) {
    const [whole, frac] = String(amount).split(".");
    const g = whole.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
    return frac ? `${g}.${frac}` : g;
  }

  // Grouped and truncated (never rounded up) to dp decimals, for the compact stat tiles.
  function brief(amount, dp = 2) {
    const [whole, frac = ""] = String(amount).split(".");
    const f = frac.slice(0, dp).replace(/0+$/, "");
    return grouped(f ? `${whole}.${f}` : whole);
  }

  function explorerURL(txid) {
    if (!status || !status.explorerTxUrl || !/^[0-9a-f]{64}$/i.test(txid)) return null;
    return status.explorerTxUrl.replace("{txid}", txid);
  }

  // ---------- status ----------

  function setState(text, tone) {
    const el = $("faucet-state");
    el.textContent = text;
    el.dataset.tone = tone;
  }

  function renderStatus() {
    if (!status) return;
    document.querySelectorAll(".payout-amount").forEach((el) => { el.textContent = status.payout; });

    if (status.ready) setState("Online", "ok");
    else if (status.reason === "faucet is empty") setState("Empty", "bad");
    else if (status.reason === "wallet syncing") setState("Syncing", "warn");
    else setState("Unavailable", "bad");
    submit.disabled = !status.ready || submitting;

    $("stat-balance").textContent = brief(status.balance);
    $("stat-balance").title = `${status.balance} TAZ`;
    const notes = [];
    if (status.pendingBalance !== "0") notes.push(`+${brief(status.pendingBalance, 3)} confirming`);
    if (status.unshieldedBalance && status.unshieldedBalance !== "0") notes.push(`+${brief(status.unshieldedBalance)} unshielded`);
    $("stat-pending").textContent = notes.length ? notes.join(" · ") : "Shielded, spendable";
    $("stat-height").textContent = status.chainHeight ? status.chainHeight.toLocaleString("en-US") : "–";
    $("stat-paid").textContent = String(status.paidCount);
    $("stat-paid-amount").textContent = `${brief(status.capRemaining, 3)} TAZ left today`;
    $("stat-nodes").textContent = String(status.broadcastNodes);
    if (status.statusPageUrl) $("status-link").href = status.statusPageUrl;

    const list = $("recent");
    list.replaceChildren();
    if (!status.recent.length) {
      const li = document.createElement("li");
      li.className = "empty";
      li.textContent = "No payouts yet.";
      list.append(li);
    }
    for (const p of status.recent) {
      const li = document.createElement("li");
      const url = explorerURL(p.txid);
      const addr = document.createElement(url ? "a" : "span");
      addr.className = "r-addr";
      addr.textContent = p.address;
      if (url) {
        addr.href = url;
        addr.target = "_blank";
        addr.rel = "noopener";
        addr.title = p.txid;
      }
      const amount = document.createElement("span");
      amount.className = "r-amount";
      const unit = document.createElement("span");
      unit.className = "unit";
      unit.textContent = "TAZ";
      amount.append(document.createTextNode(p.amount), unit);
      const time = document.createElement("span");
      time.className = "r-time";
      time.textContent = ago(p.time);
      li.append(addr, amount, time);
      list.append(li);
    }
    $("recent-count").textContent = status.recent.length ? `Last ${status.recent.length} payouts` : "";

    if (status.donationAddress) {
      $("donation-address").textContent = status.donationAddress;
      $("give-back").hidden = false;
    }
  }

  async function refreshStatus() {
    try {
      const res = await fetch("/api/status", { cache: "no-store" });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      status = await res.json();
      renderStatus();
    } catch {
      setState("Offline", "bad");
      submit.disabled = true;
    }
  }

  // ---------- messages ----------

  function showAlert(message, tone) {
    alertBox.replaceChildren(document.createTextNode(message));
    alertBox.dataset.tone = tone || "bad";
    alertBox.hidden = false;
  }

  function showRetry(message, seconds) {
    alertBox.replaceChildren(document.createTextNode(`${message} `));
    if (seconds) {
      const strong = document.createElement("strong");
      strong.textContent = `Come back in ${duration(seconds)}.`;
      alertBox.append(strong);
    }
    alertBox.dataset.tone = "warn";
    alertBox.hidden = false;
  }

  function clearAlert() { alertBox.hidden = true; }

  // ---------- claim progress ----------

  const ORDER = ["queued", "sending", "sent"];

  function renderClaim(c) {
    result.hidden = false;
    const at = ORDER.indexOf(c.status);
    document.querySelectorAll("#steps li").forEach((li) => {
      const i = ORDER.indexOf(li.dataset.step);
      li.className = "";
      if (c.status === "sent") li.className = i === 2 ? "ok" : "done";
      else if (at >= 0) li.className = i < at ? "done" : i === at ? "active" : "";
    });

    const tx = $("tx");
    switch (c.status) {
      case "queued":
        resultMsg.textContent = `Your ${c.amount} TAZ is queued. Payouts go out one at a time.`;
        break;
      case "sending":
        resultMsg.textContent = "Building and proving your transaction…";
        break;
      case "sent":
        resultMsg.textContent = `Sent ${c.amount} TAZ. It will appear in your wallet once mined (about a minute or two).`;
        break;
      case "failed":
        resultMsg.textContent = `Not paid: ${c.message || "the payout failed"}. This did not use up your daily allowance.`;
        break;
      case "review":
        resultMsg.textContent = "We could not confirm whether this payout went through, so it is being checked by hand. Please don't resubmit.";
        break;
      default:
        resultMsg.textContent = "";
    }

    if (c.txid) {
      $("tx-id").textContent = c.txid;
      const link = $("tx-link");
      const url = explorerURL(c.txid);
      link.hidden = !url;
      if (url) link.href = url;
      $("tx-broadcast").textContent = c.broadcastOk > 0
        ? `Broadcast directly to ${c.broadcastOk} of ${c.broadcastNodes} Zakura nodes`
        : "Broadcast to the Zakura network";
      tx.hidden = false;
    } else {
      tx.hidden = true;
    }
  }

  function stopPolling() {
    if (pollTimer) clearTimeout(pollTimer);
    pollTimer = null;
  }

  function pollClaim(id, startedAt) {
    stopPolling();
    const tick = async () => {
      let c;
      try {
        const res = await fetch(`/api/claim/${encodeURIComponent(id)}`, { cache: "no-store" });
        if (res.status === 404) {
          sessionStorage.removeItem(CLAIM_KEY);
          result.hidden = true;
          return;
        }
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        c = await res.json();
      } catch {
        pollTimer = setTimeout(tick, CLAIM_POLL_MS * 2);
        return;
      }
      renderClaim(c);
      const settled = c.status === "sent" ? c.broadcastOk > 0 || Date.now() - startedAt > 20000
        : c.status === "failed" || c.status === "review";
      if (settled) {
        sessionStorage.removeItem(CLAIM_KEY);
        if (c.status === "sent") setTimeout(refreshStatus, 1500);
        return;
      }
      if (Date.now() - startedAt < CLAIM_POLL_LIMIT_MS) pollTimer = setTimeout(tick, CLAIM_POLL_MS);
    };
    tick();
  }

  // ---------- submit ----------

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const address = input.value.trim();
    clearAlert();
    if (!address) {
      showAlert("Paste a Zcash testnet address first.");
      input.focus();
      return;
    }
    submitting = true;
    submit.disabled = true;
    submit.classList.add("busy");
    try {
      const res = await fetch("/api/claim", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ address }),
      });
      const body = await res.json().catch(() => ({}));
      if (res.status === 202) {
        sessionStorage.setItem(CLAIM_KEY, JSON.stringify({ id: body.id, at: Date.now() }));
        renderClaim(body);
        pollClaim(body.id, Date.now());
      } else if (res.status === 429) {
        showRetry(body.message || "Limit reached.", body.retryAfterSeconds);
      } else {
        showAlert(body.message || `Request failed (HTTP ${res.status}).`);
      }
    } catch {
      showAlert("Could not reach the faucet. Check your connection and try again.");
    } finally {
      submitting = false;
      submit.classList.remove("busy");
      submit.disabled = !(status && status.ready);
    }
  });

  input.addEventListener("input", clearAlert);

  // ---------- copy buttons ----------

  document.addEventListener("click", async (event) => {
    const btn = event.target.closest("[data-copy-target]");
    if (!btn) return;
    const text = $(btn.dataset.copyTarget).textContent;
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      const range = document.createRange();
      range.selectNodeContents($(btn.dataset.copyTarget));
      const sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
      return;
    }
    const label = btn.textContent;
    btn.textContent = "Copied";
    btn.classList.add("done");
    setTimeout(() => { btn.textContent = label; btn.classList.remove("done"); }, 1500);
  });

  // ---------- start ----------

  refreshStatus();
  setInterval(refreshStatus, STATUS_EVERY_MS);
  try {
    const saved = JSON.parse(sessionStorage.getItem(CLAIM_KEY) || "null");
    if (saved && /^[0-9a-f]{32}$/.test(saved.id)) pollClaim(saved.id, saved.at || Date.now());
  } catch { /* ignore */ }
})();
