const $ = id => document.getElementById(id);

chrome.storage.local.get(["managerBaseURL"], r => {
  $("baseURL").value = (r && r.managerBaseURL) || "http://localhost:5179";
});

$("saveBase").addEventListener("click", () => {
  chrome.runtime.sendMessage({ type: "setBaseURL", url: $("baseURL").value.trim() }, () => {
    $("status").textContent = "saved.";
  });
});

$("send").addEventListener("click", () => {
  const payload = {
    value: $("keyValue").value.trim(),
    label: $("keyLabel").value.trim() || "from-extension",
    plan:  $("keyPlan").value,
  };
  if (!payload.value) { $("status").textContent = "key is empty"; return; }
  $("send").disabled = true; $("send").textContent = "sending…";
  chrome.runtime.sendMessage({ type: "submitKey", payload }, resp => {
    $("send").disabled = false; $("send").textContent = "Send to manager";
    $("status").textContent = resp && resp.ok ? "sent ✓" : ("failed: HTTP " + (resp ? resp.status : "?"));
  });
});
