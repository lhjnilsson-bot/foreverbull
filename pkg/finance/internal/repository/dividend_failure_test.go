package repository_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"testing"

	financeapi "github.com/lhjnilsson/foreverbull/pkg/finance/api"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/repository"
	"github.com/lhjnilsson/foreverbull/pkg/finance/internal/suppliers/marketdata"
	"github.com/stretchr/testify/require"
)

// A failed dividend fetch is shown in foreverbull's words. The provider's
// own text, such as a URL with a key or its error body, never is.
func TestDividendFetchFailureNamesTheCause(t *testing.T) {
	t.Parallel()
	status := &marketdata.DividendStatusError{
		Provider: "yahoo", Request: "yahoo dividends", Symbol: "KO", HTTPCode: http.StatusServiceUnavailable,
		HTTPStatus: "503 Service Unavailable",
	}
	refused := &url.Error{Op: "Get", URL: "https://example.test/v8?apikey=secret", Err: &net.OpError{Op: "dial", Err: errors.New("refused")}}
	cases := []struct {
		err  error
		want financeapi.DividendFetchError
	}{
		{fmt.Errorf("wrapped: %w", status), financeapi.DividendFetchError{Kind: financeapi.DividendHTTPStatus, Status: 503}},
		{fmt.Errorf("error making request: %w", refused), financeapi.DividendFetchError{Kind: financeapi.DividendNoAnswer}},
		{context.DeadlineExceeded, financeapi.DividendFetchError{Kind: financeapi.DividendNoAnswer}},
		{errors.New("yahoo dividends for KO failed: Invalid range"), financeapi.DividendFetchError{Kind: financeapi.DividendUnreadable}},
	}
	for _, tc := range cases {
		err := repository.DividendFetchFailure("Yahoo", "KO", tc.err)
		var failure *financeapi.DividendFetchError
		require.ErrorAs(t, err, &failure, tc.err.Error())
		want := tc.want
		want.Provider, want.Symbol = "yahoo", "KO"
		require.Equal(t, &want, failure, tc.err.Error())
		require.NotContains(t, err.Error(), "secret")
		require.NotContains(t, err.Error(), "Invalid range")
	}
}

// A blocked provider keeps its own sentence.
func TestDividendFetchFailureKeepsABlock(t *testing.T) {
	t.Parallel()
	blocked := &marketdata.BlockedError{Provider: "yahoo", Symbol: "KO"}
	require.Same(t, blocked, repository.DividendFetchFailure("yahoo", "KO", blocked))
}
