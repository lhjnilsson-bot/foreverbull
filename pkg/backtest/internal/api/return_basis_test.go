package api

import (
	"testing"

	"github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	"github.com/stretchr/testify/require"
)

func TestReturnBasisFollowsTheProvider(t *testing.T) {
	for provider, want := range map[string]backtest.ReturnBasis{
		"yahoo":  backtest.ReturnBasis_RETURN_BASIS_TOTAL,
		"alpaca": backtest.ReturnBasis_RETURN_BASIS_TOTAL,
		"stooq":  backtest.ReturnBasis_RETURN_BASIS_ADJUSTED_CLOSE,
	} {
		got, err := returnBasis(provider)
		require.NoError(t, err, provider)
		require.Equal(t, want, got, provider)
	}
	_, err := returnBasis("")
	require.ErrorContains(t, err, `no known price provider ("")`)
}
