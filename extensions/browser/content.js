// content.js — runs on app.devin.ai pages, exposes a small "send to
// manager" button next to any element that looks like it could carry an
// API key (i.e. visible monospace text that matches the expected token
// shape). This is intentionally conservative: we never auto-submit, we
// just give the user a single-click hand-off.

(function () {
  const TOKEN_RE = /[A-Za-z0-9_\-]{24,80}/;

  function looksLikeKey(text) {
    return TOKEN_RE.test(text) && text.length < 200;
  }

  function tagCandidates() {
    document.querySelectorAll("code, pre, .token, [data-key]").forEach(el => {
      if (el.dataset.devinmgrTagged === "1") return;
      const text = el.textContent || "";
      if (!looksLikeKey(text)) return;
      el.dataset.devinmgrTagged = "1";
      const btn = document.createElement("button");
      btn.textContent = "→ devinmgr";
      btn.title = "Send this key to your local devin-key-manager";
      btn.style.cssText = "margin-left:.5rem;padding:.125rem .375rem;border-radius:.25rem;background:#4a7a9b;color:#fff;font-size:.75rem;border:0;cursor:pointer;";
      btn.addEventListener("click", e => {
        e.preventDefault(); e.stopPropagation();
        const value = (text.match(TOKEN_RE) || [text])[0].trim();
        chrome.runtime.sendMessage({
          type: "submitKey",
          payload: { value, label: location.hostname, plan: "free" },
        }, resp => {
          btn.textContent = resp && resp.ok ? "sent ✓" : "failed";
        });
      });
      el.parentElement && el.parentElement.appendChild(btn);
    });
  }

  // Initial pass + observe DOM for SPA navigations.
  tagCandidates();
  new MutationObserver(tagCandidates).observe(document.body, { childList: true, subtree: true });
})();
