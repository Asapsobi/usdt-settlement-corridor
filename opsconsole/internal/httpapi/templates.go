package httpapi

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
)

// Templates holds every parsed page template. Server-rendered HTML via
// html/template only -- no frontend build step, matching this
// project's convention of introducing no new tooling for one
// operator-facing dashboard (see §0's own STACK note). The design
// system below (tokens.css, layout.css, components.css -- all inlined,
// no build step) is a from-scratch visual pass over the original
// functional-but-plain version; every route, form, and data flow is
// unchanged.
type Templates struct {
	pages map[string]*template.Template
}

// navItem is one sidebar entry. Icon is trusted, author-written inline
// SVG (never user input), so it's template.HTML rather than a plain
// string -- otherwise html/template would escape the markup into
// visible tag text.
type navItem struct {
	Href, Label string
	Icon        template.HTML
}

var navItems = []navItem{
	{"/", "Home", iconHome},
	{"/orders", "Orders", iconOrders},
	{"/alerts", "Alerts", iconAlert},
	{"/ledger/halt", "Ledger", iconLedger},
	{"/watcher/cursor", "Watcher", iconWatcher},
	{"/broker/reservations?status=FAILED", "Broker", iconBroker},
	{"/screening/holds", "Screening", iconScreening},
	{"/dispatcher/slots", "Dispatcher", iconDispatcher},
	{"/s1/approvals", "S1", iconKey},
	{"/sandbox/orders", "Sandbox", iconSandbox},
	{"/manual/payout", "Manual Flow", iconManual},
	{"/audit", "Audit", iconAudit},
}

const layout = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Ops Console</title>
<style>` + designSystemCSS + `</style>
</head>
<body>
{{ if .Session }}
<div class="shell">
  <aside class="sidebar">
    <div class="brand">
      <span class="brand-mark">OC</span>
      <span class="brand-name">Ops Console</span>
    </div>
    <nav class="nav">
      {{ $path := .Path }}
      {{ range nav }}
      <a href="{{ .Href }}" class="nav-item {{ if isActive $path .Href }}active{{ end }}">{{ .Icon }}<span>{{ .Label }}</span></a>
      {{ end }}
    </nav>
    <div class="sidebar-footer">
      <button type="button" class="theme-toggle" onclick="ocToggleTheme()" title="Toggle color theme" aria-label="Toggle color theme">` + iconTheme + `</button>
      <div class="operator">
        <div class="operator-name">{{ .Session.DisplayName }}</div>
        <form method="post" action="/logout"><button class="link-button" type="submit">Log out</button></form>
      </div>
    </div>
  </aside>
  <div class="main-col">
    {{ if .Halted }}
    <div class="halt-banner">
      ` + iconAlert + `
      <span><strong>Ledger halted:</strong> {{ .HaltReason }}</span>
      <a href="/ledger/halt">Manage</a>
    </div>
    {{ end }}
    <main>
    {{ template "content" . }}
    </main>
  </div>
</div>
{{ else }}
<main class="auth-shell">
{{ template "content" . }}
</main>
{{ end }}
<script>` + themeScriptJS + `</script>
</body>
</html>`

// MustLoadTemplates parses every page template against layout, panicking
// on a template error -- this happens once at startup, the same "fail
// loud before serving traffic" posture every required-config check in
// this project takes.
func MustLoadTemplates() *Templates {
	pages := map[string]string{
		"login":               loginContent,
		"home":                homeContent,
		"ledger_halt":         ledgerHaltContent,
		"watcher_cursor":      watcherCursorContent,
		"watcher_sweep":       watcherSweepContent,
		"broker_reservations": brokerReservationsContent,
		"broker_reconcile":    brokerReconcileContent,
		"broker_fallback":     brokerFallbackContent,
		"broker_providers":    brokerProvidersContent,
		"screening_holds":     screeningHoldsContent,
		"dispatcher_slots":    dispatcherSlotsContent,
		"s1_approvals":        s1ApprovalsContent,
		"sandbox_orders":      sandboxOrdersContent,
		"manual_flow":         manualFlowContent,
		"order_list":          orderListContent,
		"order_detail":        orderDetailContent,
		"alerts":              alertsContent,
		"audit":               auditContent,
	}
	funcs := template.FuncMap{
		"nav": func() []navItem { return navItems },
		"isActive": func(currentPath, href string) bool {
			hrefPath, _, _ := strings.Cut(href, "?")
			return currentPath == hrefPath
		},
	}
	t := &Templates{pages: make(map[string]*template.Template, len(pages))}
	for name, content := range pages {
		tmpl := template.New(name).Funcs(funcs)
		tmpl = template.Must(tmpl.Parse(layout))
		tmpl = template.Must(tmpl.Parse(`{{ define "content" }}` + content + `{{ end }}`))
		t.pages[name] = tmpl
	}
	return t
}

// Render writes page's template to w with data.
func (t *Templates) Render(w http.ResponseWriter, page string, data any) {
	tmpl, ok := t.pages[page]
	if !ok {
		http.Error(w, fmt.Sprintf("opsconsole: unknown template %q", page), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
