package command

import (
	"testing"
	"time"

	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	"github.com/stretchr/testify/require"
)

func copyDay(t *testing.T, value string) time.Time {
	t.Helper()
	day, err := time.Parse(time.DateOnly, value)
	require.NoError(t, err)
	return day
}

// The copy for a gap at the start of the period, word for word.
func TestMissingSessionsMessageStartGap(t *testing.T) {
	t.Parallel()
	gaps := []symbolGaps{{symbol: "NEWCO", noneBefore: copyDay(t, "2024-02-15")}}
	require.Equal(t, "Couldn't load data for backtest 'demo': NEWCO has no prices before 2024-02-15, and "+
		"yahoo didn't confirm it was listed then, so foreverbull treats it as missing data. "+
		"The backtest wasn't saved. "+
		"Choose a start on or after 2024-02-15, or remove NEWCO, then run `fbull backtest create` again.",
		missingSessionsMessage("demo", "yahoo", nil, gaps, true))
}

// The copy for a gap at the end of the period, word for word.
func TestMissingSessionsMessageEndGap(t *testing.T) {
	t.Parallel()
	gaps := []symbolGaps{{symbol: "OLDCO", noneAfter: copyDay(t, "2024-03-27")}}
	require.Equal(t, "Couldn't load data for backtest 'demo': OLDCO has no prices after 2024-03-27, and "+
		"yahoo didn't confirm it was delisted then, so foreverbull treats it as missing data. "+
		"The backtest wasn't saved. "+
		"Choose an end on or before 2024-03-27, or remove OLDCO, then run `fbull backtest create` again.",
		missingSessionsMessage("demo", "yahoo", nil, gaps, true))
}

// When the backtest couldn't be removed, it was partly saved, and the step
// is its own sentence to delete it first, then the error's own step as it
// reads when the backtest was removed.
func TestMissingSessionsMessageNotRemoved(t *testing.T) {
	t.Parallel()
	start := []symbolGaps{{symbol: "NEWCO", noneBefore: copyDay(t, "2024-02-15")}}
	require.Equal(t, "Couldn't load data for backtest 'oef-2024q1': NEWCO has no prices before 2024-02-15, and "+
		"yahoo didn't confirm it was listed then, so foreverbull treats it as missing data. "+
		"The backtest was partly saved and couldn't be removed. Run `fbull backtest delete oef-2024q1` first. "+
		"Choose a start on or after 2024-02-15, or remove NEWCO, then run `fbull backtest create` again.",
		missingSessionsMessage("oef-2024q1", "yahoo", nil, start, false))

	end := []symbolGaps{{symbol: "OLDCO", noneAfter: copyDay(t, "2024-03-27")}}
	require.Equal(t, "Couldn't load data for backtest 'oef-2024q1': OLDCO has no prices after 2024-03-27, and "+
		"yahoo didn't confirm it was delisted then, so foreverbull treats it as missing data. "+
		"The backtest was partly saved and couldn't be removed. Run `fbull backtest delete oef-2024q1` first. "+
		"Choose an end on or before 2024-03-27, or remove OLDCO, then run `fbull backtest create` again.",
		missingSessionsMessage("oef-2024q1", "yahoo", nil, end, false))

	days := []time.Time{copyDay(t, "2024-02-14"), copyDay(t, "2024-02-20")}

	several := []symbolGaps{{symbol: "AAA", sessions: []int{1}}, {symbol: "NEWCO", noneBefore: days[1]}}
	require.Equal(t, "Couldn't load data for backtest 'oef-2024q1': yahoo is missing prices on trading days for "+
		"2 symbols. The backtest was partly saved and couldn't be removed.\n"+
		"  AAA: 2024-02-20\n"+
		"  NEWCO: no prices before 2024-02-20.\n"+
		"yahoo didn't confirm a listing, so foreverbull treats it as missing data. "+
		"Run `fbull backtest delete oef-2024q1` first. Try another provider with --provider, or change the "+
		"backtest dates, then run `fbull backtest create` again.",
		missingSessionsMessage("oef-2024q1", "yahoo", days, several, false))

	noBars := []symbolGaps{{symbol: "XYZ", noBars: true, from: days[0], to: days[1]}}
	require.Equal(t, "Couldn't load data for backtest 'oef-2024q1': yahoo has no prices for XYZ between 2024-02-14 "+
		"and 2024-02-20. The backtest was partly saved and couldn't be removed. Run `fbull backtest delete oef-2024q1` "+
		"first. Check the symbol and dates, or try another provider with --provider, then run "+
		"`fbull backtest create` again.",
		missingSessionsMessage("oef-2024q1", "yahoo", days, noBars, false))
}

// Request symbols of one instrument are one unit, named by its latest
// ticker; any other symbol is read alone, named as requested.
func TestCheckUnits(t *testing.T) {
	t.Parallel()
	fb := repository.Ticker{Symbol: "FB", From: copyDay(t, "2012-05-18"), To: copyDay(t, "2022-06-09")}
	meta := repository.Ticker{Symbol: "META", From: copyDay(t, "2012-05-18")}
	units, alias := checkUnits([]string{"OEF", "BRK.B", "FB", "META"},
		map[string][]repository.Ticker{"FB": {fb, meta}, "META": {fb, meta}})
	require.Equal(t, []checkUnit{
		{name: "OEF", tickers: []repository.Ticker{{Symbol: "OEF"}}},
		{name: "BRK.B", tickers: []repository.Ticker{{Symbol: "BRKB"}}},
		{name: "META (earlier FB)", tickers: []repository.Ticker{fb, meta}},
	}, units)
	require.Equal(t, "META (earlier FB)", alias["META"])
}

// The session check sees an edge gap only for a session of the period:
// a symbol with bars over the whole period has none, and one whose bars
// start late or end early has it at its first or last bar.
func TestMissingSessionsFindsEdgeGaps(t *testing.T) {
	t.Parallel()
	sessions := []time.Time{copyDay(t, "2024-02-13"), copyDay(t, "2024-02-14"), copyDay(t, "2024-02-15")}
	_, gaps := missingSessions(sessions, []string{"FULL", "LATE", "EARLY"}, map[string]*repository.SymbolBars{
		"FULL":  {First: sessions[0], Last: sessions[2], Days: sessions},
		"LATE":  {First: sessions[1], Last: sessions[2], Days: sessions[1:]},
		"EARLY": {First: sessions[0], Last: sessions[1], Days: sessions[:2]},
	}, nil, true)
	require.Len(t, gaps, 2)
	require.Equal(t, "LATE", gaps[0].symbol)
	require.Equal(t, sessions[1], gaps[0].noneBefore)
	require.Empty(t, gaps[0].sessions)
	require.Equal(t, "EARLY", gaps[1].symbol)
	require.Equal(t, sessions[1], gaps[1].noneAfter)
	require.Empty(t, gaps[1].sessions)
}

// An empty period that couldn't be removed says so the usual way: partly
// saved, delete first, then the step as it reads.
func TestEmptyPeriodMessages(t *testing.T) {
	t.Parallel()
	today := copyDay(t, "2026-10-30")
	require.Equal(t, "Couldn't load data for backtest 'oef-2024q1': no trading session between 2026-10-30 and "+
		"2026-10-30 has closed yet (sessions count as closed at 16:30 New York time). The backtest wasn't saved. "+
		"Choose a start on or before 2026-10-29, then run `fbull backtest create` again.",
		notClosedMessage("oef-2024q1", today, today, copyDay(t, "2026-10-29"), true))
	require.Equal(t, "Couldn't load data for backtest 'oef-2024q1': no trading session between 2026-10-30 and "+
		"2026-10-30 has closed yet (sessions count as closed at 16:30 New York time). The backtest was partly "+
		"saved and couldn't be removed. Run `fbull backtest delete oef-2024q1` first. Choose a start on or before "+
		"2026-10-29, then run `fbull backtest create` again.",
		notClosedMessage("oef-2024q1", today, today, copyDay(t, "2026-10-29"), false))
	require.Equal(t, "Couldn't load data for backtest 'oef-2024q1': there's no trading session between 2024-03-30 "+
		"and 2024-03-31. The backtest wasn't saved. Choose dates that include a trading day, then run "+
		"`fbull backtest create` again.",
		noSessionMessage("oef-2024q1", copyDay(t, "2024-03-30"), copyDay(t, "2024-03-31"), true))
	require.Equal(t, "Couldn't load data for backtest 'oef-2024q1': there's no trading session between 2024-03-30 "+
		"and 2024-03-31. The backtest was partly saved and couldn't be removed. Run `fbull backtest delete "+
		"oef-2024q1` first. Choose dates that include a trading day, then run `fbull backtest create` again.",
		noSessionMessage("oef-2024q1", copyDay(t, "2024-03-30"), copyDay(t, "2024-03-31"), false))
}
