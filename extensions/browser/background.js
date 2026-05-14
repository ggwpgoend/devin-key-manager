// devin-key-manager browser-extension scaffold (PR-16 / roadmap A5).
//
// Service-worker entry point. Listens for runtime messages from the
// popup/content script and forwards them to the local manager's HTTP
// API. The manager URL is configurable via storage (default
// http://localhost:5179) so multi-port setups work without rebuilding.

const DEFAULT_BASE = "http://localhost:5179";

async function getBaseURL() {
  return new Promise(resolve => {
    chrome.storage.local.get(["managerBaseURL"], r => {
      resolve((r && r.managerBaseURL) || DEFAULT_BASE);
    });
  });
}

async function postKey(payload) {
  const base = await getBaseURL();
  try {
    const r = await fetch(base + "/api/keys", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    return { ok: r.ok, status: r.status };
  } catch (e) {
    return { ok: false, status: 0, error: String(e) };
  }
}

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  if (msg && msg.type === "submitKey") {
    postKey(msg.payload).then(sendResponse);
    return true; // keep the message channel open for the async response
  }
  if (msg && msg.type === "setBaseURL") {
    chrome.storage.local.set({ managerBaseURL: msg.url }, () => sendResponse({ ok: true }));
    return true;
  }
});
