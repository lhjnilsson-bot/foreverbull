package command_test

import (
	"context"

	"github.com/lhjnilsson/foreverbull/pkg/backtest/internal/repository"
	financeServicePB "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
)

const fbISIN = "US30303M1027"

// laterTickerNotRecorded is the whole step message when create can't record
// a later-ticker resolution and the backtest was removed.
const laterTickerNotRecorded = "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't save which " +
	"later tickers were used for some symbols. The backtest wasn't saved. Run `fbenv status` to check that the " +
	"environment is running, then run `fbull backtest create` again."

// fbReadAsMeta stores FB and then META as tickers of one ISIN and META's
// bars for 2024 Q1, which the check reads for FB.
func (test *BacktestCommandTest) fbReadAsMeta() {
	ctx := context.Background()
	_, err := test.db.Exec(ctx, `INSERT INTO instrument (id, isin, name) VALUES ($1, $1, 'Meta Platforms')`, fbISIN)
	test.Require().NoError(err)
	_, err = test.db.Exec(ctx, `
		INSERT INTO asset_symbol (isin, symbol, from_date, to_date)
		VALUES ($1, 'FB', '2012-05-18', '2022-06-09'), ($1, 'META', '2022-06-09', NULL)`, fbISIN)
	test.Require().NoError(err)
	test.storeBars("META", "2024-01-01", "2024-03-31")
}

func readAs(symbol, later string) *financeServicePB.DownloadHistoricalDataResponse {
	return &financeServicePB.DownloadHistoricalDataResponse{
		Symbol:      symbol,
		Status:      financeServicePB.DownloadHistoricalDataResponse_STATUS_OK,
		LaterTicker: &financeServicePB.DownloadHistoricalDataResponse_LaterTicker{Symbol: later},
	}
}

func (test *BacktestCommandTest) storedResolutions() []string {
	stored, err := (&repository.Backtest{Conn: test.db}).Get(context.Background(), quarterBacktest)
	test.Require().NoError(err)
	lines := make([]string, 0, len(stored.GetTickerResolutions()))
	for _, resolution := range stored.GetTickerResolutions() {
		lines = append(lines, resolution.GetSymbol()+" read as "+resolution.GetReadAs())
	}
	return lines
}

// FB's bars came from META, whether META was found by FB's ISIN or the
// provider answered FB's lookup as META: the response is the same. With FB
// and META stored as tickers of one ISIN, create keeps the backtest and
// stores the resolution on it, with no day.
func (test *BacktestCommandTest) TestDownloadFinancialDataStoresALaterTicker() {
	test.createQuarter("FB")
	test.fbReadAsMeta()

	_, err := test.download(quarterEnd, readAs("FB", "META"))
	test.Require().NoError(err)
	test.requireKept()
	test.Equal([]string{"FB read as META"}, test.storedResolutions())
}

// The provider answered FB's lookup as META, so the finance service stored
// META's bars and named META. asset_symbol has no FB alias, so nothing ties
// META's bars to FB: the missing-session check finds none for FB and create
// fails. It never keeps a backtest that reads META's prices as FB's. With
// the alias stored, TestDownloadFinancialDataStoresALaterTicker is the same
// response, kept and named.
func (test *BacktestCommandTest) TestDownloadFinancialDataFailsForTheTickerTheProviderAnsweredAsWithoutAnAlias() {
	test.createQuarter("FB")
	test.storeBars("META", "2024-01-01", "2024-03-31")

	_, err := test.download(quarterEnd, readAs("FB", "META"))
	test.Require().EqualError(err, "Couldn't load data for backtest 'oef-2024q1': yahoo has no prices for FB between "+
		"2024-01-02 and 2024-03-28. The backtest wasn't saved. Check the symbol and dates, or try another provider "+
		"with --provider, then run `fbull backtest create` again.")
	test.requireNothingSaved()
}

// A download that read every symbol under its own ticker stores none.
func (test *BacktestCommandTest) TestDownloadFinancialDataStoresNoLaterTickerWithoutOne() {
	test.createQuarter("XYZ")
	test.storeBars("XYZ", "2024-01-01", "2024-03-31")

	_, err := test.download(quarterEnd, &financeServicePB.DownloadHistoricalDataResponse{
		Symbol: "XYZ", Status: financeServicePB.DownloadHistoricalDataResponse_STATUS_OK,
	})
	test.Require().NoError(err)
	test.requireKept()
	test.Empty(test.storedResolutions())
}

// A later ticker create can't use fails create and saves nothing, rather
// than reading a symbol under another ticker without a word.
func (test *BacktestCommandTest) TestDownloadFinancialDataFailsOnAnUnusableLaterTicker() {
	cases := []struct {
		name   string
		answer *financeServicePB.DownloadHistoricalDataResponse
	}{
		{"no ticker", readAs("FB", "")},
		{"outside the ticker pattern", readAs("FB", "ME TA")},
		{"the asked ticker", readAs("FB", "FB")},
	}
	for _, c := range cases {
		test.Run(c.name, func() {
			test.SetupTest()
			test.createQuarter("FB")
			test.fbReadAsMeta()

			_, err := test.download(quarterEnd, c.answer)
			test.Require().EqualError(err, laterTickerNotRecorded)
			test.requireNothingSaved()
		})
	}
}

// The resolution can't be stored, so create fails and the backtest is
// removed. It never passes with the resolution left out.
func (test *BacktestCommandTest) TestDownloadFinancialDataFailsWhenALaterTickerIsNotStored() {
	test.createQuarter("FB")
	test.fbReadAsMeta()
	_, err := test.db.Exec(context.Background(), `
CREATE OR REPLACE FUNCTION refuse_ticker_resolutions() RETURNS trigger AS $$
BEGIN
	IF NEW.ticker_resolutions <> OLD.ticker_resolutions THEN
		RAISE EXCEPTION 'ticker_resolutions refused';
	END IF;
	RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER refuse_ticker_resolutions BEFORE UPDATE ON backtest FOR EACH ROW EXECUTE FUNCTION refuse_ticker_resolutions();`)
	test.Require().NoError(err)
	defer func() {
		_, err := test.db.Exec(context.Background(), `DROP TRIGGER IF EXISTS refuse_ticker_resolutions ON backtest`)
		test.Require().NoError(err)
	}()

	_, err = test.download(quarterEnd, readAs("FB", "META"))
	test.Require().EqualError(err, laterTickerNotRecorded)
	test.NotContains(err.Error(), "refused")
	test.requireNothingSaved()
}
