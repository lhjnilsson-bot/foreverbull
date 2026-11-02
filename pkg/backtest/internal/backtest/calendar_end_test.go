package backtest

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/lhjnilsson/foreverbull/pkg/backtest/engine"
	financeServicePb "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// pastCalendar answers every read the way Zipline's calendar service
// answers one past its last session.
type pastCalendar struct {
	financeServicePb.UnimplementedCalendarServer
}

func (pastCalendar) ListCalendarDates(context.Context, *financeServicePb.ListCalendarDatesRequest) (*financeServicePb.ListCalendarDatesResponse, error) {
	return nil, status.Error(codes.OutOfRange, "the XNYS calendar ends on 2027-11-02")
}

// A read past the calendar's last session is a CalendarEndError naming
// that session, not an error listing sessions.
func TestZiplineSessionsPastTheCalendarIsACalendarEndError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	financeServicePb.RegisterCalendarServer(server, pastCalendar{})
	go server.Serve(listener) //nolint: errcheck
	t.Cleanup(server.Stop)

	zipline, err := NewZiplineEngine(context.Background(), ZiplineTarget{ControlAddress: listener.Addr().String()}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { zipline.Close() }) //nolint: errcheck

	_, err = zipline.Sessions(context.Background(),
		time.Date(2027, 12, 3, 0, 0, 0, 0, time.UTC), time.Date(2027, 12, 17, 0, 0, 0, 0, time.UTC))
	var calendarEnd *engine.CalendarEndError
	require.ErrorAs(t, err, &calendarEnd)
	require.Equal(t, "XNYS", calendarEnd.Calendar)
	require.Equal(t, time.Date(2027, 11, 2, 0, 0, 0, 0, time.UTC), calendarEnd.LastSession)
}

// Only OUT_OF_RANGE with the service's exact text is read as the calendar's
// end. Anything else stays the error it was.
func TestCalendarEndErrorReadsOnlyTheServicesStatus(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"other code":          status.Error(codes.Unknown, "the XNYS calendar ends on 2027-11-02"),
		"other text":          status.Error(codes.OutOfRange, "end is past the calendar"),
		"trailing text":       status.Error(codes.OutOfRange, "the XNYS calendar ends on 2027-11-02: more"),
		"not a date":          status.Error(codes.OutOfRange, "the XNYS calendar ends on 2027-13-40"),
		"not a status":        errors.New("the XNYS calendar ends on 2027-11-02"),
		"library's own error": status.Error(codes.Unknown, "Exception calling application: Parameter `end` receieved as '2027-12-31 00:00:00' although cannot be later than the last session of calendar 'XNYS' ('2027-11-02 00:00:00')."),
	} {
		_, ok := calendarEndError(err)
		require.False(t, ok, name)
	}
	calendarEnd, ok := calendarEndError(status.Error(codes.OutOfRange, "the XNYS calendar ends on 2027-11-02"))
	require.True(t, ok)
	require.Equal(t, &engine.CalendarEndError{Calendar: "XNYS", LastSession: time.Date(2027, 11, 2, 0, 0, 0, 0, time.UTC)}, calendarEnd)
}
