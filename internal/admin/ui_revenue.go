package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// The revenue page: what the exchange earned and what it paid the chain, per
// asset, over a period (docs/plan-v1.0.md 23.5). Read-only -- it is the one
// page in the back office with nothing to submit.

type revenueData struct {
	Lines  []RevenueLine
	Assets []registry.Asset
	Filter revenueFilter
	// Query is the filter re-encoded, so the CSV link asks for exactly what
	// the table shows rather than the default window.
	Query string
}

// revenueFilter is the form as the operator typed it: two dates and an asset.
// The dates are kept as the strings the form submitted so the inputs render
// back what was entered even when one of them is nonsense.
type revenueFilter struct {
	From  string
	To    string
	Asset string
}

// dayLayout is what an <input type="date"> submits. The page takes dates
// rather than timestamps because an operator reporting on revenue thinks in
// days; the API keeps RFC 3339 because a machine integration should not have
// to guess a timezone.
const dayLayout = "2006-01-02"

func (u *UI) revenue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l := langFrom(ctx)
	f := revenueFilter{
		From:  strings.TrimSpace(r.URL.Query().Get("from")),
		To:    strings.TrimSpace(r.URL.Query().Get("to")),
		Asset: strings.TrimSpace(r.URL.Query().Get("asset")),
	}
	period, err := f.period(time.Now().UTC())
	if err != nil {
		u.tpl.render(w, r, http.StatusBadRequest, "revenue", view{
			Title: l.T("page.revenue"), Flash: &flash{Kind: "err", Text: l.T("revenue.bad_period")},
			Data: revenueData{Filter: f},
		})
		return
	}
	d := revenueData{Filter: f, Query: f.query()}
	if d.Lines, err = u.h.Revenue(ctx, period, f.Asset); err != nil {
		u.fail(w, r, "revenue", "revenue report", err)
		return
	}
	if d.Assets, err = u.h.registry.ListAssets(ctx, u.h.tenant); err != nil {
		u.fail(w, r, "revenue", "assets", err)
		return
	}
	u.tpl.render(w, r, http.StatusOK, "revenue", view{Title: l.T("page.revenue"), Data: d})
}

// revenueCSV is the browser's download. It cannot use the spec'd
// /admin/v1/reports/revenue.csv: that path is behind the API key header, and a
// browser following a link sends no headers of its own. Same numbers, same
// writer, session authentication instead.
func (u *UI) revenueCSV(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f := revenueFilter{
		From:  strings.TrimSpace(r.URL.Query().Get("from")),
		To:    strings.TrimSpace(r.URL.Query().Get("to")),
		Asset: strings.TrimSpace(r.URL.Query().Get("asset")),
	}
	period, err := f.period(time.Now().UTC())
	if err != nil {
		http.Error(w, "bad period", http.StatusBadRequest)
		return
	}
	lines, err := u.h.Revenue(ctx, period, f.Asset)
	if err != nil {
		u.fail(w, r, "revenue", "revenue report", err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="revenue-`+
		period.From.Format(dayLayout)+"_"+period.To.Format(dayLayout)+`.csv"`)
	if err := writeRevenueCSV(w, lines); err != nil {
		// The status is already sent, so there is nothing to say to the
		// browser; the operator sees a truncated file and the log says why.
		telemetry.Logger(ctx).Error("admin: revenue csv", "err", err.Error())
	}
}

// period turns the two submitted dates into the half-open window the query
// takes. A date means the whole UTC day: from is that day's midnight, to is
// the midnight after the day named, so both ends of the form are inclusive of
// the day the operator typed even though the query is half-open.
func (f revenueFilter) period(now time.Time) (RevenuePeriod, error) {
	var from, to *time.Time
	if f.From != "" {
		t, err := time.ParseInLocation(dayLayout, f.From, time.UTC)
		if err != nil {
			return RevenuePeriod{}, err
		}
		from = &t
	}
	if f.To != "" {
		t, err := time.ParseInLocation(dayLayout, f.To, time.UTC)
		if err != nil {
			return RevenuePeriod{}, err
		}
		end := t.AddDate(0, 0, 1)
		to = &end
	}
	return revenuePeriod(from, to, now)
}

// query re-encodes the filter for the CSV link.
func (f revenueFilter) query() string {
	v := make([]string, 0, 3)
	for _, kv := range [][2]string{{"from", f.From}, {"to", f.To}, {"asset", f.Asset}} {
		if kv[1] != "" {
			v = append(v, kv[0]+"="+kv[1])
		}
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + strings.Join(v, "&")
}
