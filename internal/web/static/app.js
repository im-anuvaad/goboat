// goboat dashboard: live log streams, status refresh, branch lists and confirmations.
// Everything also works without JavaScript except the live parts.
"use strict";

// <pre data-stream="/url"> shows a Server-Sent Events stream.
function stream(pre) {
  const status = document.querySelector("[data-stream-status]");
  const es = new EventSource(pre.dataset.stream);
  const maxLines = 3000;
  let lines = 0;
  const atBottom = () => pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
  es.onopen = () => { if (status) status.textContent = "live"; };
  es.onmessage = (e) => {
    const stick = atBottom();
    pre.appendChild(document.createTextNode(e.data + "\n"));
    lines += e.data.split("\n").length;
    while (lines > maxLines && pre.firstChild) {
      pre.removeChild(pre.firstChild);
      lines = pre.textContent.split("\n").length;
    }
    if (stick) pre.scrollTop = pre.scrollHeight;
  };
  es.addEventListener("done", (e) => {
    es.close();
    if (status) status.textContent = "finished: " + e.data;
    if (pre.hasAttribute("data-reload-on-done")) setTimeout(() => location.reload(), 800);
  });
  es.onerror = () => { if (status) status.textContent = "reconnecting…"; };
}

// Elements with data-refresh="N" and an id are re-fetched every N seconds,
// unless the user is typing in them.
function refresh(el) {
  const every = Math.max(2, parseInt(el.dataset.refresh, 10) || 5) * 1000;
  setInterval(async () => {
    if (document.hidden || el.contains(document.activeElement)) return;
    try {
      const r = await fetch(location.href, { credentials: "same-origin", headers: { "Accept": "text/html" } });
      if (!r.ok || r.redirected) return;
      const doc = new DOMParser().parseFromString(await r.text(), "text/html");
      const fresh = doc.getElementById(el.id);
      if (fresh) { el.innerHTML = fresh.innerHTML; wire(el); }
    } catch (_) { /* offline: try again later */ }
  }, every);
}

// <input data-branches="list-id"> (a repository): fill the datalist with
// its branches, and an empty branch field next to it with the default.
function branches(input) {
  const list = document.getElementById(input.dataset.branches);
  const field = input.form && input.form.elements.namedItem("branch");
  if (!list) return;
  let last = "";
  const load = async () => {
    const repo = input.value.trim();
    if (!repo || repo === last) return;
    last = repo;
    try {
      const r = await fetch("/github/branches?repo=" + encodeURIComponent(repo), { credentials: "same-origin" });
      if (!r.ok) return;
      const data = await r.json();
      list.replaceChildren(...data.branches.map((b) => { const o = document.createElement("option"); o.value = b; return o; }));
      if (field && !field.value && field.placeholder) field.placeholder = data.default + " (default branch)";
    } catch (_) { /* typing a branch by hand still works */ }
  };
  input.addEventListener("change", load);
  input.addEventListener("blur", load);
  load();
}

function wire(root) {
  root.querySelectorAll("form[data-confirm]").forEach((f) => {
    f.addEventListener("submit", (e) => { if (!confirm(f.dataset.confirm)) e.preventDefault(); });
  });
  root.querySelectorAll("[data-confirm-click]").forEach((b) => {
    b.addEventListener("click", (e) => { if (!confirm(b.dataset.confirmClick)) e.preventDefault(); });
  });
}

document.addEventListener("DOMContentLoaded", () => {
  document.querySelectorAll("pre[data-stream]").forEach(stream);
  document.querySelectorAll("[data-refresh][id]").forEach(refresh);
  document.querySelectorAll("input[data-branches]").forEach(branches);
  wire(document);
});
