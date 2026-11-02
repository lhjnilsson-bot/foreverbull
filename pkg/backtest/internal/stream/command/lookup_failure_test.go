package command

import (
	"errors"
	"testing"
	"time"

	"github.com/lhjnilsson/foreverbull/pkg/finance"
	"github.com/stretchr/testify/require"
)

func lookupErr(kind finance.LookupKind, provider string, status int) *finance.LookupError {
	return &finance.LookupError{
		Kind: kind, Provider: provider, Symbol: "XYZ", Status: status,
		Err: errors.New("https://provider.example/secret {\"body\":\"hidden\"}"),
	}
}

func miss(symbol string, listing bool, cause *finance.LookupError) lookupMiss {
	cause.Symbol = symbol
	return lookupMiss{symbol: symbol, listing: listing, cause: cause}
}

func TestLookupFailureMessageNamesEveryCause(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		misses []lookupMiss
		want   string
	}{
		{
			name:   "network delisting",
			misses: []lookupMiss{miss("XYZ", false, lookupErr(finance.LookupNetwork, "yahoo", 0))},
			want: "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't check with yahoo whether " +
				"XYZ was delisted: yahoo didn't answer. The backtest wasn't saved. Check your internet connection, " +
				"then run `fbull backtest create` again.",
		},
		{
			name:   "network listing",
			misses: []lookupMiss{miss("XYZ", true, lookupErr(finance.LookupNetwork, "yahoo", 0))},
			want: "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't check with yahoo when XYZ " +
				"was listed: yahoo didn't answer. The backtest wasn't saved. Check your internet connection, " +
				"then run `fbull backtest create` again.",
		},
		{
			name:   "server error",
			misses: []lookupMiss{miss("XYZ", false, lookupErr(finance.LookupHTTP, "alpaca", 503))},
			want: "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't check with alpaca whether " +
				"XYZ was delisted: alpaca had a server error (HTTP 503). The backtest wasn't saved. " +
				"Run `fbull backtest create` again in a few minutes, or try another provider with --provider.",
		},
		{
			name:   "rate limit",
			misses: []lookupMiss{miss("XYZ", true, lookupErr(finance.LookupHTTP, "yahoo", 429))},
			want: "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't check with yahoo when XYZ " +
				"was listed: yahoo limited how many requests foreverbull can make. The backtest wasn't saved. " +
				"Wait a few minutes, then run `fbull backtest create` again, or try another provider with --provider.",
		},
		{
			name:   "alpaca key",
			misses: []lookupMiss{miss("XYZ", false, lookupErr(finance.LookupHTTP, "alpaca", 401))},
			want: "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't check with alpaca whether " +
				"XYZ was delisted: alpaca's paper trading API didn't accept foreverbull's API key (HTTP 401). The backtest wasn't saved. " +
				"Check that your alpaca API key and secret work for paper trading, or try another provider with --provider, then run " +
				"`fbull backtest create` again.",
		},
		{
			name:   "alpaca forbidden",
			misses: []lookupMiss{miss("XYZ", false, lookupErr(finance.LookupHTTP, "alpaca", 403))},
			want: "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't check with alpaca whether " +
				"XYZ was delisted: alpaca's paper trading API didn't accept foreverbull's API key (HTTP 403). The backtest wasn't saved. " +
				"Check that your alpaca API key and secret work for paper trading, or try another provider with --provider, then run " +
				"`fbull backtest create` again.",
		},
		{
			name:   "yahoo refused",
			misses: []lookupMiss{miss("XYZ", true, lookupErr(finance.LookupHTTP, "yahoo", 403))},
			want: "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't check with yahoo when XYZ " +
				"was listed: yahoo refused the request (HTTP 403). The backtest wasn't saved. " +
				"Try another provider with --provider, then run `fbull backtest create` again.",
		},
		{
			name:   "yahoo not found",
			misses: []lookupMiss{miss("XYZ", false, lookupErr(finance.LookupHTTP, "yahoo", 404))},
			want: "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't check with yahoo whether " +
				"XYZ was delisted: yahoo refused the request (HTTP 404). The backtest wasn't saved. " +
				"Try another provider with --provider, then run `fbull backtest create` again.",
		},
		{
			name:   "alpaca unprocessable",
			misses: []lookupMiss{miss("XYZ", true, lookupErr(finance.LookupHTTP, "alpaca", 422))},
			want: "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't check with alpaca when XYZ " +
				"was listed: alpaca refused the request (HTTP 422). The backtest wasn't saved. " +
				"Try another provider with --provider, then run `fbull backtest create` again.",
		},
		{
			name:   "unknown status",
			misses: []lookupMiss{miss("XYZ", false, lookupErr(finance.LookupHTTP, "yahoo", 418))},
			want: "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't check with yahoo whether " +
				"XYZ was delisted: yahoo refused the request (HTTP 418). The backtest wasn't saved. " +
				"Try another provider with --provider, then run `fbull backtest create` again.",
		},
		{
			name:   "bot check",
			misses: []lookupMiss{miss("XYZ", true, lookupErr(finance.LookupBot, "yahoo", 0))},
			want: "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't check with yahoo when XYZ " +
				"was listed: yahoo answered with a bot check instead of data. The backtest wasn't saved. " +
				"Try another provider with --provider, then run `fbull backtest create` again.",
		},
		{
			name:   "parse",
			misses: []lookupMiss{miss("XYZ", true, lookupErr(finance.LookupParse, "yahoo", 0))},
			want: "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't check with yahoo when XYZ " +
				"was listed: yahoo's answer couldn't be read. The backtest wasn't saved. " +
				"Try another provider with --provider, then run `fbull backtest create` again. If it keeps " +
				"happening, please report it at https://github.com/lhjnilsson/foreverbull/issues with this message.",
		},
		{
			name:   "partly saved",
			misses: []lookupMiss{miss("XYZ", false, lookupErr(finance.LookupHTTP, "alpaca", 503))},
			want: "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't check with alpaca whether " +
				"XYZ was delisted: alpaca had a server error (HTTP 503). The backtest was partly saved and " +
				"couldn't be removed. Run `fbull backtest delete oef-2024q1` first. Run `fbull backtest create` " +
				"again in a few minutes, or try another provider with --provider.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			removed := tc.name != "partly saved"
			err := plainSentence(lookupFailureMessage("oef-2024q1", tc.misses[0].cause.Provider, tc.misses, removed))
			require.EqualError(t, err, tc.want)
			require.NotContains(t, err.Error(), "provider.example")
			require.NotContains(t, err.Error(), "hidden")
		})
	}
}

func TestLookupFailureMessageBlockAndDifferingSteps(t *testing.T) {
	t.Parallel()

	block := []lookupMiss{
		miss("XYZ", false, lookupErr(finance.LookupHTTP, "alpaca", 503)),
		miss("NEWCO", true, lookupErr(finance.LookupHTTP, "alpaca", 503)),
	}
	err := plainSentence(lookupFailureMessage("oef-2024q1", "alpaca", block, true))
	require.EqualError(t, err, "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't check 2 symbols "+
		"with alpaca. The backtest wasn't saved.\n"+
		"  XYZ: couldn't check whether it was delisted: alpaca had a server error (HTTP 503).\n"+
		"  NEWCO: couldn't check when it was listed: alpaca had a server error (HTTP 503).\n"+
		"Run `fbull backtest create` again in a few minutes, or try another provider with --provider.")

	differ := []lookupMiss{
		miss("XYZ", false, lookupErr(finance.LookupNetwork, "yahoo", 0)),
		miss("NEWCO", true, lookupErr(finance.LookupHTTP, "yahoo", 503)),
	}
	err = plainSentence(lookupFailureMessage("oef-2024q1", "yahoo", differ, true))
	require.EqualError(t, err, "Couldn't load data for backtest 'oef-2024q1': foreverbull couldn't check 2 symbols "+
		"with yahoo. The backtest wasn't saved.\n"+
		"  XYZ: couldn't check whether it was delisted: yahoo didn't answer.\n"+
		"  NEWCO: couldn't check when it was listed: yahoo had a server error (HTTP 503).\n"+
		"Check your internet connection, then run `fbull backtest create` again. "+
		"Run `fbull backtest create` again in a few minutes, or try another provider with --provider.")
}

func TestMixedCoverageMessageJoinsTheSteps(t *testing.T) {
	t.Parallel()

	day := time.Date(2024, 2, 14, 0, 0, 0, 0, time.UTC)
	start := time.Date(2024, 2, 15, 0, 0, 0, 0, time.UTC)
	gaps := []symbolGaps{{symbol: "NEWCO", noneBefore: start}}
	misses := []lookupMiss{miss("XYZ", false, lookupErr(finance.LookupHTTP, "alpaca", 503))}
	err := plainSentence(mixedCoverageMessage("oef-2024q1", "alpaca", []time.Time{day, start}, gaps, misses, nil, true))
	require.EqualError(t, err, "Couldn't load data for backtest 'oef-2024q1': 2 symbols are missing prices or "+
		"couldn't be checked. The backtest wasn't saved.\n"+
		"  NEWCO: no prices before 2024-02-15.\n"+
		"alpaca didn't confirm a listing, so foreverbull treats it as missing data.\n"+
		"  XYZ: couldn't check whether it was delisted: alpaca had a server error (HTTP 503).\n"+
		"Run `fbull backtest create` again in a few minutes, or try another provider with --provider. "+
		"Choose a start on or after 2024-02-15, or remove NEWCO, then run `fbull backtest create` again.")

	inside := []symbolGaps{{symbol: "AAA", sessions: []int{0}}}
	err = plainSentence(mixedCoverageMessage("oef-2024q1", "yahoo", []time.Time{day}, inside,
		[]lookupMiss{miss("XYZ", true, lookupErr(finance.LookupHTTP, "yahoo", 429))}, nil, false))
	require.EqualError(t, err, "Couldn't load data for backtest 'oef-2024q1': 2 symbols are missing prices or "+
		"couldn't be checked. The backtest was partly saved and couldn't be removed.\n"+
		"  AAA: 2024-02-14\n"+
		"  XYZ: couldn't check when it was listed: yahoo limited how many requests foreverbull can make.\n"+
		"Run `fbull backtest delete oef-2024q1` first. Wait a few minutes, then run `fbull backtest create` "+
		"again, or try another provider with --provider. Or change the backtest dates, then run "+
		"`fbull backtest create` again.")
}

func TestJoinStepsOffersAnotherProviderOnce(t *testing.T) {
	t.Parallel()

	require.Equal(t, "Wait a few minutes, then run `fbull backtest create` again, or try another provider with --provider. "+
		"Check that your alpaca API key and secret work for paper trading, then run `fbull backtest create` again.",
		joinSteps([]stepKind{stepAlpacaKey, stepRate, stepRefused}, ""))
	require.Equal(t, "Run `fbull backtest create` again in a few minutes, or try another provider with --provider. "+
		"Wait a few minutes, then run `fbull backtest create` again. If it keeps happening, please report it at "+
		"https://github.com/lhjnilsson/foreverbull/issues with this message.",
		joinSteps([]stepKind{stepParse, stepRate, stepServer}, ""))
	require.Equal(t, "Try another provider with --provider, then run `fbull backtest create` again. "+
		"Check the symbol and dates, then run `fbull backtest create` again.",
		joinSteps([]stepKind{stepRefused}, noPriceDataNextStep))
}
