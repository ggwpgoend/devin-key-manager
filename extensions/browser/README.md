# devin-key-manager browser extension (scaffold)

A minimal Chrome / Edge extension for the **devin-key-manager** local
service. It does one thing: lets you POST a Devin API key to your local
manager without copy-pasting between tabs.

## Install (developer mode)

1. Clone or download this folder somewhere stable.
2. Open `chrome://extensions` (or `edge://extensions`).
3. Toggle **Developer mode** (top right).
4. Click **Load unpacked** → select `extensions/browser`.
5. Pin the extension to the toolbar for quick access.

## Use

- Click the extension icon to open the popup. Paste a key, optionally
  set a label and plan, click **Send to manager**.
- On `app.devin.ai` pages, any element matching the key shape gets a
  small `→ devinmgr` button next to it. One click sends the key.
- The manager URL defaults to `http://localhost:5179`. Change it in the
  popup if your manager runs elsewhere; the value persists in
  `chrome.storage.local`.

## Implementation notes

- Manifest V3 service worker (`background.js`) handles the actual fetch.
- The content script (`content.js`) does conservative key detection
  (24-80 chars `[A-Za-z0-9_\-]`) — it never auto-submits.
- The manager's POST endpoint is `POST /api/keys` accepting JSON
  `{value, label, plan}`. CORS is **not** enabled by default in
  `internal/web/web.go`; add a permissive policy or run the extension
  on the same origin.

## Status

This is a **scaffold**, not a finished extension:

- Icons are PNG placeholders (replace with real 16/48/128 px assets).
- No `chrome.notifications` hookup yet for round-trip confirmation.
- No native messaging (the fetch path is enough for localhost).
- Untested on Firefox; the MV3 manifest needs a few tweaks
  (`browser_specific_settings`).
