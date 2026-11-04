package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/lhjnilsson/foreverbull/pkg/finance/supplier"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const crumbURL = "https://query2.finance.yahoo.com/v1/finance/quoteType/AAPL?crumb=s3cr3tCrumb#frag"

func yahooURLError() error {
	return fmt.Errorf("yahoo get asset: %w", &url.Error{
		Op:  "Get",
		URL: crumbURL,
		Err: errors.New("dial tcp: connection refused"),
	})
}

func TestInternalDownloadErrorDoesNotLogCrumb(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = prev })

	err := internalDownloadError("AAPL", "get asset", yahooURLError())

	require.EqualError(t, err, "couldn't download prices for AAPL. Try again in a few minutes, or use another provider with --provider.")
	assert.Contains(t, buf.String(), "query2.finance.yahoo.com/v1/finance/quoteType/AAPL")
	assert.NotContains(t, buf.String(), "crumb")
	assert.NotContains(t, buf.String(), "s3cr3tCrumb")
}

func TestGetAssetErrorDoesNotReturnCrumb(t *testing.T) {
	md := supplier.NewMockMarketdata(t)
	md.On("GetAsset", mock.Anything, "AAPL").Return(nil, yahooURLError())
	server := NewMarketdataServer(nil, dividendFree{md})

	_, err := server.GetAsset(context.Background(), &servicePb.GetAssetRequest{
		Identifier: &servicePb.GetAssetRequest_Symbol{Symbol: "AAPL"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "query2.finance.yahoo.com")
	assert.NotContains(t, err.Error(), "s3cr3tCrumb")
}
