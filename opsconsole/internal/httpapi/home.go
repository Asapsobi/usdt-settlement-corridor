package httpapi

import (
	"context"
	"html/template"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const homeContent = `
<div class="page-head">
  <div>
    <h1 style="margin-bottom:2px">Home</h1>
    <div class="helptext" style="margin-bottom:0">Live status across every backend service, refreshed every 10s.</div>
  </div>
</div>
<div class="cards" id="oc-service-cards">
{{ range .Services }}
  <div class="card">
    <div class="card-head">
      {{ .Icon }}
      <h2>{{ .Name }}</h2>
      <span class="badge {{ if .Healthy }}badge-success{{ else }}badge-danger{{ end }}" style="margin-left:auto"><span class="dot {{ if .Healthy }}dot-green{{ else }}dot-red{{ end }}"></span>{{ if .Healthy }}Healthy{{ else }}Down{{ end }}</span>
    </div>
    {{ if .Error }}<div class="flash flash-error" style="margin:8px 0 0">` + iconAlert + `<span>{{ .Error }}</span></div>{{ end }}
    {{ range .Facts }}<div class="fact"><span class="fact-label">{{ .Label }}</span><span class="fact-value">{{ .Value }}</span></div>{{ end }}
  </div>
{{ end }}
</div>
`

type serviceFact struct {
	Label, Value string
}

type serviceCard struct {
	Name    string
	Icon    template.HTML
	Healthy bool
	Error   string
	Facts   []serviceFact
}

type homePageData struct {
	basePageData
	Services []serviceCard
}

// checkService runs healthz plus fn (a service's own invariants fetch)
// concurrently with every other service, each bounded by its own
// timeout -- invariant 4: one slow/dead service degrades only its own
// card, never the whole page.
func checkService(ctx context.Context, name string, icon template.HTML, healthz func(context.Context) error, facts func(context.Context) ([]serviceFact, error)) serviceCard {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	card := serviceCard{Name: name, Icon: icon}
	if err := healthz(ctx); err != nil {
		card.Error = err.Error()
		return card
	}
	card.Healthy = true
	if f, err := facts(ctx); err != nil {
		card.Error = err.Error()
	} else {
		card.Facts = f
	}
	return card
}

func (s *Server) collectServiceCards(r *http.Request) []serviceCard {
	type job struct {
		name    string
		icon    template.HTML
		healthz func(context.Context) error
		facts   func(context.Context) ([]serviceFact, error)
	}
	jobs := []job{
		{name: "ledger", icon: iconLedger, healthz: s.Ledger.Healthz, facts: func(ctx context.Context) ([]serviceFact, error) {
			inv, err := s.Ledger.GetInvariants(ctx)
			if err != nil {
				return nil, err
			}
			return []serviceFact{
				{"halted", boolStr(inv.Halted)},
				{"trial balance ok", boolStr(inv.TrialBalanceOK)},
				{"cache ok", boolStr(inv.CacheOK)},
			}, nil
		}},
		{name: "watcher", icon: iconWatcher, healthz: s.Watcher.Healthz, facts: func(ctx context.Context) ([]serviceFact, error) {
			inv, err := s.Watcher.GetInvariants(ctx)
			if err != nil {
				return nil, err
			}
			facts := []serviceFact{}
			if inv.CursorLagBlocks != nil {
				facts = append(facts, serviceFact{"cursor lag (blocks)", int64Str(*inv.CursorLagBlocks)})
			}
			if inv.PendingFinalityCount != nil {
				facts = append(facts, serviceFact{"pending finality", intStr(*inv.PendingFinalityCount)})
			}
			return facts, nil
		}},
		{name: "screening", icon: iconScreening, healthz: s.Screening.Healthz, facts: func(ctx context.Context) ([]serviceFact, error) {
			q, err := s.Screening.GetQueue(ctx)
			if err != nil {
				return nil, err
			}
			facts := []serviceFact{}
			for status, depth := range q.DepthByStatus {
				facts = append(facts, serviceFact{"queue: " + status, intStr(depth)})
			}
			return facts, nil
		}},
		{name: "broker", icon: iconBroker, healthz: s.Broker.Healthz, facts: func(ctx context.Context) ([]serviceFact, error) {
			inv, err := s.Broker.GetInvariants(ctx)
			if err != nil {
				return nil, err
			}
			return []serviceFact{
				{"buffer available/target", int64Str(inv.BufferAvailable) + " / " + int64Str(inv.BufferTarget)},
				{"open fallback events", int64Str(inv.OpenManualFallbackEvents)},
			}, nil
		}},
		{name: "dispatcher", icon: iconDispatcher, healthz: s.Dispatcher.Healthz, facts: func(ctx context.Context) ([]serviceFact, error) {
			inv, err := s.Dispatcher.GetInvariants(ctx)
			if err != nil {
				return nil, err
			}
			return []serviceFact{
				{"open dispatching orders", intStr(inv.OpenDispatchingOrders)},
				{"stuck pending reconciliation", intStr(inv.StuckPendingReconciliation)},
			}, nil
		}},
		{name: "s1", icon: iconKey, healthz: s.S1.Healthz, facts: func(ctx context.Context) ([]serviceFact, error) {
			pending, err := s.S1.ListPendingApprovals(ctx)
			if err != nil {
				return nil, err
			}
			return []serviceFact{{"pending approvals", intStr(len(pending))}}, nil
		}},
	}
	if s.Gateway != nil {
		jobs = append(jobs, job{name: "gateway", icon: iconSandbox, healthz: s.Gateway.Healthz, facts: func(ctx context.Context) ([]serviceFact, error) {
			orders, err := s.Gateway.ListSandboxOrders(ctx)
			if err != nil {
				return nil, err
			}
			return []serviceFact{{"sandbox orders", intStr(len(orders))}}, nil
		}})
	}
	if s.Proofrun != nil {
		jobs = append(jobs, job{name: "proofrun", icon: iconManual, healthz: s.Proofrun.Healthz, facts: func(ctx context.Context) ([]serviceFact, error) {
			return []serviceFact{{"manual flow", "ready"}}, nil
		}})
	}

	cards := make([]serviceCard, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			cards[i] = checkService(r.Context(), j.name, j.icon, j.healthz, j.facts)
		}(i, j)
	}
	wg.Wait()
	return cards
}

func (s *Server) getHome(w http.ResponseWriter, r *http.Request) {
	data := homePageData{basePageData: s.newBasePageData(r), Services: s.collectServiceCards(r)}
	s.Templates.Render(w, "home", data)
}

// getHomePartial serves the same cards without the surrounding page --
// the auto-refresh JS on the home page fetches this every 10s.
func (s *Server) getHomePartial(w http.ResponseWriter, r *http.Request) {
	s.getHome(w, r)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func intStr(i int) string     { return strconv.Itoa(i) }
func int64Str(i int64) string { return strconv.FormatInt(i, 10) }
