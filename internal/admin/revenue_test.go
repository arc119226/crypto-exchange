package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

func amt(s string) money.Amount { return money.MustParse(s) }

// The window an operator gets when they ask for nothing has to be a real
// window, not everything and not nothing.
func TestRevenuePeriodDefaultsToTheLastThirtyDays(t *testing.T) {
	now := time.Date(2026, 9, 9, 15, 4, 5, 0, time.UTC)

	p, err := revenuePeriod(nil, nil, now)
	require.NoError(t, err)
	assert.Equal(t, now, p.To)
	assert.Equal(t, now.Add(-DefaultRevenueWindow), p.From)

	// One bound given, the other derived from it -- not from now, or a
	// report of last January would silently reach forward to today.
	to := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	p, err = revenuePeriod(nil, &to, now)
	require.NoError(t, err)
	assert.Equal(t, to, p.To)
	assert.Equal(t, to.Add(-DefaultRevenueWindow), p.From)
}

// An inverted window would report zero of everything, which is
// indistinguishable on screen from a quiet month.
func TestRevenuePeriodRefusesAnEmptyWindow(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	from := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)

	_, err := revenuePeriod(&from, &to, now)
	require.Error(t, err)

	same := from
	_, err = revenuePeriod(&from, &same, now)
	require.Error(t, err, "a zero-length window is a mistake, not an empty report")
}

// The page takes days and the query takes instants. Both ends of the form are
// inclusive of the day typed, which means `to` widens to the following
// midnight -- get this wrong and the last day of the month goes missing.
func TestRevenueFilterTreatsBothDatesAsWholeUTCDays(t *testing.T) {
	now := time.Date(2026, 9, 9, 15, 0, 0, 0, time.UTC)
	p, err := revenueFilter{From: "2026-09-01", To: "2026-09-08"}.period(now)
	require.NoError(t, err)

	assert.Equal(t, "2026-09-01T00:00:00Z", p.From.Format(time.RFC3339))
	assert.Equal(t, "2026-09-09T00:00:00Z", p.To.Format(time.RFC3339),
		"the 8th is included, so the exclusive end is the 9th")

	_, err = revenueFilter{From: "the first"}.period(now)
	require.Error(t, err)
}

func TestRevenueFilterQueryCarriesOnlyWhatWasAsked(t *testing.T) {
	assert.Empty(t, revenueFilter{}.query())
	assert.Equal(t, "?asset=ETH", revenueFilter{Asset: "ETH"}.query())
	assert.Equal(t, "?from=2026-09-01&to=2026-09-08&asset=ETH",
		revenueFilter{From: "2026-09-01", To: "2026-09-08", Asset: "ETH"}.query())
}

// The CSV is read by position in somebody's spreadsheet, so the header and
// the rows have to stay the same width and the same order, and every amount
// has to be the decimal string -- never a float, never a thousands separator.
func TestRevenueCSVIsPositionalAndDecimal(t *testing.T) {
	var b strings.Builder
	require.NoError(t, writeRevenueCSV(&b, []RevenueLine{{
		Asset: "ETH", MakerFees: amt("0.796"), TakerFees: amt("0.0008"),
		WithdrawalFees: amt("0.002"), DepositFees: money.Zero, OtherFees: money.Zero,
		GasExpense: amt("0.000042"), Net: amt("0.798758"),
		Trades: 12, Withdrawals: 2, Deposits: 0,
	}}))

	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	require.Len(t, lines, 2)
	assert.Equal(t, strings.Join(revenueCSVHeader, ","), strings.TrimRight(lines[0], "\r"))
	assert.Equal(t, "ETH,0.796,0.0008,0.002,0,0,0.000042,0.798758,12,2,0",
		strings.TrimRight(lines[1], "\r"))
	assert.Len(t, revenueCSVHeader, 11)
}

func TestRevenuePageRenders(t *testing.T) {
	tpl, err := loadTemplates()
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	tpl.render(rec, httptest.NewRequest(http.MethodGet, "/admin/revenue", nil), http.StatusOK, "revenue",
		view{Title: "revenue", Session: &auth.AdminSession{Email: "ops@example.com", TOTPEnabled: true, TOTPVerified: true},
			Data: revenueData{
				Filter: revenueFilter{From: "2026-09-01", To: "2026-09-08", Asset: "ETH"},
				Query:  "?from=2026-09-01&to=2026-09-08&asset=ETH",
				Assets: []registry.Asset{{Symbol: "ETH"}, {Symbol: "USDC"}},
				Lines: []RevenueLine{{
					Asset: "ETH", MakerFees: amt("0.796"), TakerFees: amt("0.0008"),
					WithdrawalFees: amt("0.002"), DepositFees: money.Zero, OtherFees: money.Zero,
					GasExpense: amt("0.000042"), Net: amt("0.798758"), Trades: 12, Withdrawals: 2,
				}},
			}})

	body := rec.Body.String()
	require.Contains(t, body, "</html>")
	assert.Contains(t, body, `value="2026-09-01"`)
	assert.Contains(t, body, `<option value="ETH" selected>`)
	assert.Contains(t, body, "<strong>0.798758</strong>", "the net is the number an operator came for")
	assert.Contains(t, body, `href="/admin/revenue.csv?from=2026-09-01&amp;to=2026-09-08&amp;asset=ETH"`,
		"the download asks for what the table shows, not the default window")

	rec = httptest.NewRecorder()
	tpl.render(rec, httptest.NewRequest(http.MethodGet, "/admin/revenue", nil), http.StatusOK, "revenue",
		view{Title: "revenue", Session: &auth.AdminSession{Email: "ops@example.com", TOTPEnabled: true, TOTPVerified: true},
			Data: revenueData{}})
	assert.Contains(t, rec.Body.String(), "Nothing happened in this period.")
}
