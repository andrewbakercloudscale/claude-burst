// Audit view, shared by the dashboard and the support console: every alert
// shown, every action taken from either page, and for each one the log
// lines around it. Self-contained: no framework, styles scoped to .au-.
(function () {
  "use strict";
  const esc = (s) => String(s == null ? "" : s).replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const when = (iso) => {
    const d = new Date(iso);
    if (isNaN(d)) return esc(iso);
    return d.toLocaleDateString(undefined, { weekday: "short", day: "numeric", month: "short" }) +
      " " + d.toLocaleTimeString(undefined, { hour12: false });
  };
  const CSS = `
.au-bar{display:flex;gap:6px;flex-wrap:wrap;align-items:center;margin:0 0 10px}
.au-bar button{font:inherit;padding:3px 10px;border-radius:999px;border:1px solid var(--line,#8884);background:transparent;color:inherit;cursor:pointer}
.au-bar button.on{background:var(--accent,#3b82f6);border-color:var(--accent,#3b82f6);color:#fff}
.au-bar .au-n{opacity:.7;margin-left:auto;font-size:.9em}
.au-list{display:flex;flex-direction:column;gap:6px}
.au-row{border:1px solid var(--line,#8883);border-left-width:4px;border-radius:8px;padding:8px 10px}
.au-row.error{border-left-color:#dc2626}.au-row.warn{border-left-color:#d97706}
.au-row.ok{border-left-color:#16a34a}.au-row.info{border-left-color:#64748b}
.au-head{display:flex;gap:8px;flex-wrap:wrap;align-items:baseline}
.au-time{font-variant-numeric:tabular-nums;opacity:.75;white-space:nowrap}
.au-title{font-weight:600;flex:1;min-width:12em}
.au-kind{font-size:.8em;opacity:.7;border:1px solid var(--line,#8884);border-radius:4px;padding:0 5px}
.au-detail{margin:4px 0 0;white-space:pre-wrap;overflow-wrap:anywhere}
.au-log{margin:6px 0 0;padding:8px;max-height:340px;overflow:auto;background:var(--code-bg,#0f172a);color:#e2e8f0;border-radius:6px;font:12px/1.45 ui-monospace,Menlo,monospace;white-space:pre-wrap;overflow-wrap:anywhere}
.au-more{font:inherit;font-size:.9em;margin-top:6px;padding:2px 8px;border-radius:6px;border:1px solid var(--line,#8884);background:transparent;color:inherit;cursor:pointer}
.au-empty{opacity:.7}`;

  function addStyle() {
    if (document.getElementById("au-style")) return;
    const st = document.createElement("style");
    st.id = "au-style";
    st.textContent = CSS;
    document.head.appendChild(st);
  }

  const FILTERS = [
    ["all", "All", () => true],
    ["problems", "Problems", (e) => e.severity === "error" || e.severity === "warn"],
    ["alerts", "Alerts shown", (e) => !e.audit_only],
    ["actions", "Actions taken", (e) => e.audit_only],
  ];

  function mount(el, opts) {
    opts = opts || {};
    addStyle();
    let events = [], filter = "all";
    el.innerHTML = '<div class="au-bar"></div><div class="au-list"></div>';
    const bar = el.querySelector(".au-bar"), list = el.querySelector(".au-list");

    function renderBar(shown) {
      bar.innerHTML = FILTERS.map(([k, label]) =>
        `<button type="button" data-f="${k}" class="${k === filter ? "on" : ""}">${label}</button>`).join("") +
        `<span class="au-n">${shown} of ${events.length}</span>`;
    }
    function render() {
      const keep = FILTERS.find((f) => f[0] === filter)[2];
      const rows = events.filter(keep).slice(0, opts.limit || 200);
      renderBar(rows.length);
      if (!rows.length) {
        list.innerHTML = '<div class="au-empty">Nothing recorded yet.</div>';
        return;
      }
      list.innerHTML = rows.map((e) => `
        <div class="au-row ${esc(e.severity)}">
          <div class="au-head"><span class="au-time">${when(e.at)}</span>
            <span class="au-title">${esc(e.title)}</span>
            <span class="au-kind">${esc(e.audit_only ? "action" : e.kind)}</span></div>
          ${e.detail ? `<div class="au-detail">${esc(e.detail)}</div>` : ""}
          <button type="button" class="au-more" data-at="${Number(e.ts) || 0}">Log around this</button>
        </div>`).join("");
    }
    bar.addEventListener("click", (ev) => {
      const b = ev.target.closest("button[data-f]");
      if (!b) return;
      filter = b.dataset.f;
      render();
    });
    list.addEventListener("click", async (ev) => {
      const b = ev.target.closest("button.au-more");
      if (!b) return;
      const row = b.parentElement;
      const open = row.querySelector(".au-log");
      if (open) { open.remove(); b.textContent = "Log around this"; return; }
      b.textContent = "Loading...";
      let txt;
      try {
        const r = await fetch("/api/audit/context?at=" + b.dataset.at);
        if (!r.ok) throw new Error(await r.text());
        const j = await r.json();
        txt = j.lines.length ? j.lines.join("\n")
          : "No log lines in the 3 minutes before and 30 seconds after this (the log may have rotated).";
      } catch (e) {
        txt = "Could not read the log: " + e.message;
      }
      const pre = document.createElement("pre");
      pre.className = "au-log";
      pre.textContent = txt;
      row.appendChild(pre);
      b.textContent = "Hide log";
    });

    async function load() {
      try {
        const r = await fetch("/api/audit?limit=" + (opts.limit || 200));
        if (!r.ok) throw new Error("HTTP " + r.status);
        events = (await r.json()).events || [];
      } catch (e) {
        list.innerHTML = '<div class="au-empty">Could not load the audit: ' + esc(e.message) + "</div>";
        return;
      }
      if (!el.querySelector(".au-log")) render(); // never collapse a log being read
    }
    load();
    return { reload: load };
  }

  window.BurstAudit = { mount };
})();
