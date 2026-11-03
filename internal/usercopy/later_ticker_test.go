package usercopy_test

import (
	"testing"

	"github.com/lhjnilsson/foreverbull/internal/usercopy"
	"github.com/stretchr/testify/require"
)

func TestLaterTickerNotRecorded(t *testing.T) {
	require.Equal(t, "foreverbull couldn't save which later tickers were used for some symbols.", usercopy.LaterTickerNotRecorded)
}
