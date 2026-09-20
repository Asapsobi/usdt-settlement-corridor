package httpapi

// designSystemCSS is the console's entire visual language: color/spacing/
// type tokens (light + dark, following system preference by default),
// sidebar layout, and every shared component (cards, badges, buttons,
// forms, tables, flash messages). Inlined, no build step -- one file to
// read top to bottom rather than a stylesheet pipeline, matching this
// project's "no new frontend tooling" decision.
const designSystemCSS = `
:root {
  --bg: #f7f8fa;
  --surface: #ffffff;
  --surface-2: #f1f3f6;
  --border: #e3e6eb;
  --text: #10131a;
  --text-muted: #6b7280;
  --primary: #4f46e5;
  --primary-hover: #4338ca;
  --primary-soft: #eef2ff;
  --success: #0f9d58;
  --success-soft: #e9f9ef;
  --danger: #d92d20;
  --danger-soft: #fef2f1;
  --warning: #b25e09;
  --warning-soft: #fef6e7;
  --radius-sm: 6px;
  --radius-md: 10px;
  --radius-lg: 14px;
  --shadow-sm: 0 1px 2px rgba(16, 19, 26, 0.06);
  --shadow-md: 0 4px 16px rgba(16, 19, 26, 0.08);
  --sidebar-w: 232px;
}

@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) {
    --bg: #0c0e13;
    --surface: #14171e;
    --surface-2: #1a1e27;
    --border: #262b36;
    --text: #e7e9ee;
    --text-muted: #8b93a3;
    --primary: #818cf8;
    --primary-hover: #a5b0fb;
    --primary-soft: #1e2247;
    --success: #34d399;
    --success-soft: #0e2e24;
    --danger: #f87171;
    --danger-soft: #3a1a19;
    --warning: #fbbf24;
    --warning-soft: #3a2c0e;
    --shadow-sm: 0 1px 2px rgba(0, 0, 0, 0.3);
    --shadow-md: 0 4px 20px rgba(0, 0, 0, 0.4);
  }
}
:root[data-theme="dark"] {
  --bg: #0c0e13;
  --surface: #14171e;
  --surface-2: #1a1e27;
  --border: #262b36;
  --text: #e7e9ee;
  --text-muted: #8b93a3;
  --primary: #818cf8;
  --primary-hover: #a5b0fb;
  --primary-soft: #1e2247;
  --success: #34d399;
  --success-soft: #0e2e24;
  --danger: #f87171;
  --danger-soft: #3a1a19;
  --warning: #fbbf24;
  --warning-soft: #3a2c0e;
  --shadow-sm: 0 1px 2px rgba(0, 0, 0, 0.3);
  --shadow-md: 0 4px 20px rgba(0, 0, 0, 0.4);
}

* { box-sizing: border-box; }
body {
  margin: 0;
  background: var(--bg);
  color: var(--text);
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Inter, Roboto, sans-serif;
  font-size: 14px;
  line-height: 1.5;
  -webkit-font-smoothing: antialiased;
}
h1 { font-size: 22px; font-weight: 650; margin: 0 0 20px; letter-spacing: -0.01em; }
h2 { font-size: 15px; font-weight: 650; margin: 0 0 4px; }
a { color: var(--primary); }

/* ---- shell / sidebar ---- */
.shell { display: flex; min-height: 100vh; }
.sidebar {
  width: var(--sidebar-w);
  flex-shrink: 0;
  background: var(--surface);
  border-right: 1px solid var(--border);
  display: flex;
  flex-direction: column;
  padding: 16px 12px;
  position: sticky;
  top: 0;
  height: 100vh;
}
.brand { display: flex; align-items: center; gap: 10px; padding: 8px 8px 20px; }
.brand-mark {
  width: 32px; height: 32px; border-radius: var(--radius-sm);
  background: var(--primary); color: #fff; font-weight: 700; font-size: 13px;
  display: flex; align-items: center; justify-content: center; flex-shrink: 0;
}
.brand-name { font-weight: 650; font-size: 15px; }
.nav { display: flex; flex-direction: column; gap: 2px; flex: 1; }
.nav-item {
  display: flex; align-items: center; gap: 10px;
  padding: 9px 10px; border-radius: var(--radius-sm);
  color: var(--text-muted); text-decoration: none; font-size: 13.5px; font-weight: 500;
  transition: background 0.12s, color 0.12s;
}
.nav-item svg { width: 18px; height: 18px; flex-shrink: 0; }
.nav-item:hover { background: var(--surface-2); color: var(--text); }
.nav-item.active { background: var(--primary-soft); color: var(--primary); }
.sidebar-footer { border-top: 1px solid var(--border); padding-top: 12px; margin-top: 8px; display: flex; align-items: center; gap: 10px; }
.theme-toggle {
  width: 32px; height: 32px; border-radius: var(--radius-sm); border: 1px solid var(--border);
  background: var(--surface); color: var(--text-muted); display: flex; align-items: center; justify-content: center;
  cursor: pointer; flex-shrink: 0;
}
.theme-toggle:hover { background: var(--surface-2); color: var(--text); }
.theme-toggle svg { width: 16px; height: 16px; }
.operator { flex: 1; min-width: 0; }
.operator-name { font-size: 13px; font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.link-button { background: none; border: none; padding: 0; color: var(--text-muted); font-size: 12px; cursor: pointer; text-decoration: underline; }
.link-button:hover { color: var(--text); }

.main-col { flex: 1; min-width: 0; display: flex; flex-direction: column; }
main { padding: 28px 32px; max-width: 1180px; width: 100%; margin: 0 auto; }
.auth-shell { min-height: 100vh; display: flex; align-items: center; justify-content: center; background: var(--bg); }

.halt-banner {
  background: var(--danger-soft); color: var(--danger); border-bottom: 1px solid var(--border);
  padding: 10px 32px; font-size: 13.5px; display: flex; align-items: center; gap: 8px;
}
.halt-banner svg { width: 16px; height: 16px; flex-shrink: 0; }
.halt-banner a { color: var(--danger); font-weight: 650; margin-left: auto; }

/* ---- cards ---- */
.cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(240px, 1fr)); gap: 14px; }
.card {
  background: var(--surface); border: 1px solid var(--border); border-radius: var(--radius-lg);
  padding: 18px; box-shadow: var(--shadow-sm);
}
.card-head { display: flex; align-items: center; gap: 8px; margin-bottom: 10px; }
.card-head svg { width: 18px; height: 18px; flex-shrink: 0; color: var(--text-muted); }
.fact { display: flex; justify-content: space-between; padding: 3px 0; font-size: 13px; }
.fact-label { color: var(--text-muted); }
.fact-value { font-weight: 600; font-variant-numeric: tabular-nums; }

/* ---- status dot + badges ---- */
.dot { display: inline-block; width: 9px; height: 9px; border-radius: 50%; flex-shrink: 0; }
.dot-green { background: var(--success); }
.dot-red { background: var(--danger); }
.badge {
  display: inline-flex; align-items: center; gap: 5px; padding: 2px 9px;
  border-radius: 999px; font-size: 12px; font-weight: 650;
}
.badge-success { background: var(--success-soft); color: var(--success); }
.badge-danger { background: var(--danger-soft); color: var(--danger); }
.badge-warning { background: var(--warning-soft); color: var(--warning); }
.badge-neutral { background: var(--surface-2); color: var(--text-muted); }

/* ---- buttons ---- */
.btn {
  display: inline-flex; align-items: center; gap: 6px; cursor: pointer;
  padding: 7px 14px; border-radius: var(--radius-sm); border: 1px solid var(--border);
  background: var(--surface); color: var(--text); font-size: 13px; font-weight: 600;
  font-family: inherit; transition: background 0.12s, border-color 0.12s, opacity 0.12s;
}
.btn:hover { background: var(--surface-2); }
.btn:disabled { opacity: 0.45; cursor: not-allowed; }
.btn-primary { background: var(--primary); border-color: var(--primary); color: #fff; }
.btn-primary:hover { background: var(--primary-hover); border-color: var(--primary-hover); }
.btn-danger { background: var(--danger); border-color: var(--danger); color: #fff; }
.btn-danger:hover { filter: brightness(1.08); }
.btn-sm { padding: 4px 10px; font-size: 12px; }
form.inline { display: inline-block; }
.actions { display: flex; gap: 8px; flex-wrap: wrap; align-items: center; }

/* ---- forms ---- */
label { display: block; margin: 14px 0 5px; font-weight: 600; font-size: 12.5px; color: var(--text-muted); }
input[type=text], input[type=password], input[type=number], textarea, select {
  padding: 8px 10px; border: 1px solid var(--border); border-radius: var(--radius-sm);
  width: 100%; box-sizing: border-box; background: var(--surface); color: var(--text);
  font-size: 13.5px; font-family: inherit; transition: border-color 0.12s, box-shadow 0.12s;
}
input:focus, textarea:focus, select:focus {
  outline: none; border-color: var(--primary); box-shadow: 0 0 0 3px var(--primary-soft);
}
.field-hint { font-size: 12px; color: var(--text-muted); margin-top: 4px; }
.panel-narrow { max-width: 460px; }

/* ---- flash / alerts ---- */
.flash {
  display: flex; align-items: flex-start; gap: 10px; padding: 11px 14px; margin-bottom: 16px;
  border-radius: var(--radius-md); font-size: 13.5px; border-left: 3px solid transparent;
}
.flash svg { width: 16px; height: 16px; flex-shrink: 0; margin-top: 1px; }
.flash-error { background: var(--danger-soft); color: var(--danger); border-color: var(--danger); }
.flash-ok { background: var(--success-soft); color: var(--success); border-color: var(--success); }

/* ---- tables ---- */
.table-wrap { background: var(--surface); border: 1px solid var(--border); border-radius: var(--radius-lg); overflow-x: auto; box-shadow: var(--shadow-sm); }
table { border-collapse: collapse; width: 100%; min-width: 640px; }
th {
  text-align: left; font-size: 11px; font-weight: 650; text-transform: uppercase; letter-spacing: 0.04em;
  color: var(--text-muted); background: var(--surface-2); padding: 10px 14px; border-bottom: 1px solid var(--border);
}
td { padding: 11px 14px; font-size: 13.5px; border-bottom: 1px solid var(--border); vertical-align: middle; }
tbody tr:last-child td { border-bottom: none; }
tbody tr:hover { background: var(--surface-2); }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12.5px; }
.empty-state { padding: 40px 20px; text-align: center; color: var(--text-muted); font-size: 13.5px; }

/* ---- page chrome ---- */
.page-head { display: flex; align-items: baseline; justify-content: space-between; margin-bottom: 18px; flex-wrap: wrap; gap: 10px; }
.tabs { display: flex; gap: 4px; margin-bottom: 18px; flex-wrap: wrap; }
.tab {
  padding: 6px 12px; border-radius: var(--radius-sm); font-size: 13px; font-weight: 600;
  color: var(--text-muted); text-decoration: none; border: 1px solid transparent;
}
.tab:hover { background: var(--surface-2); color: var(--text); }
.tab.active { background: var(--primary-soft); color: var(--primary); }
.section { margin-bottom: 28px; }
.helptext { color: var(--text-muted); font-size: 13px; margin-bottom: 16px; max-width: 640px; }

/* ---- login ---- */
.auth-card {
  background: var(--surface); border: 1px solid var(--border); border-radius: var(--radius-lg);
  box-shadow: var(--shadow-md); padding: 32px; width: 360px;
}
.auth-brand { display: flex; align-items: center; gap: 10px; margin-bottom: 22px; }
.auth-brand .brand-mark { width: 36px; height: 36px; font-size: 14px; }
.auth-brand-name { font-weight: 650; font-size: 17px; }
`

const themeScriptJS = `
(function() {
  try {
    var saved = localStorage.getItem('oc-theme');
    if (saved) document.documentElement.setAttribute('data-theme', saved);
  } catch (e) {}
})();
function ocToggleTheme() {
  var root = document.documentElement;
  var current = root.getAttribute('data-theme');
  var prefersDark = window.matchMedia('(prefers-color-scheme: dark)').matches;
  var effectiveIsDark = current ? current === 'dark' : prefersDark;
  var next = effectiveIsDark ? 'light' : 'dark';
  root.setAttribute('data-theme', next);
  try { localStorage.setItem('oc-theme', next); } catch (e) {}
}
`

const (
	iconHome       = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 11.5 12 4l9 7.5"/><path d="M5 10v9a1 1 0 0 0 1 1h4v-6h4v6h4a1 1 0 0 0 1-1v-9"/></svg>`
	iconLedger     = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 21h18"/><path d="M4 21V9l8-5 8 5v12"/><path d="M9 21v-6h6v6"/></svg>`
	iconWatcher    = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7Z"/><circle cx="12" cy="12" r="3"/></svg>`
	iconBroker     = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M13 2 3 14h7l-1 8 10-12h-7l1-8Z"/></svg>`
	iconScreening  = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3 4 6v6c0 4.5 3 7.7 8 9 5-1.3 8-4.5 8-9V6l-8-3Z"/></svg>`
	iconDispatcher = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="m3 11 18-8-8 18-2-8-8-2Z"/></svg>`
	iconKey        = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="8" cy="15" r="4"/><path d="m10.5 12.5 8-8"/><path d="m16 7 2 2"/><path d="m13.5 9.5 2 2"/></svg>`
	iconAudit      = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M8 3h8a2 2 0 0 1 2 2v14l-3-2-3 2-3-2-3 2V5a2 2 0 0 1 2-2Z"/><path d="M9 8h6M9 12h6"/></svg>`
	iconTheme      = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3a9 9 0 1 0 9 9c0-.46-.04-.92-.1-1.36A5.4 5.4 0 0 1 12 3Z"/></svg>`
	iconAlert      = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0Z"/><path d="M12 9v4M12 17h.01"/></svg>`
	iconCheck      = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="m20 6-11 11L4 12"/></svg>`
	iconSandbox    = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="3" width="18" height="18" rx="2"/><path d="M3 9h18M9 21V9" stroke-dasharray="2 2"/></svg>`
	iconManual     = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M9 11.5V6a2 2 0 1 1 4 0v4.5"/><path d="M13 10V5a2 2 0 1 1 4 0v6"/><path d="M17 11V8.5a2 2 0 1 1 4 0V15a7 7 0 0 1-7 7h-1.5a7 7 0 0 1-5.9-3.2l-2.9-4.5a1.7 1.7 0 0 1 2.6-2.2L9 15"/></svg>`
	iconOrders     = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="4" y="4" width="16" height="16" rx="2"/><path d="M8 9h8M8 13h8M8 17h4"/></svg>`
	iconWallet     = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7a2 2 0 0 1 2-2h13a1 1 0 0 1 1 1v3"/><path d="M3 7v11a2 2 0 0 0 2 2h14a1 1 0 0 0 1-1v-4"/><rect x="14" y="11" width="7" height="5" rx="1"/><circle cx="16.7" cy="13.5" r=".6" fill="currentColor" stroke="none"/></svg>`
	iconDeposit    = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3v11"/><path d="m7 10 5 5 5-5"/><path d="M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2"/></svg>`
	iconExposure   = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><circle cx="10.5" cy="10.5" r="6.5"/><path d="m20 20-4.35-4.35"/></svg>`
	iconPayout     = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M22 2 11 13"/><path d="M22 2 15 22l-4-9-9-4 20-7Z"/></svg>`
)
