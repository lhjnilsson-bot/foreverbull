package backtest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"time"

	"github.com/lhjnilsson/foreverbull/internal/container"
	"github.com/lhjnilsson/foreverbull/internal/storage"
	"github.com/lhjnilsson/foreverbull/pkg/backtest/engine"
	commonPb "github.com/lhjnilsson/foreverbull/pkg/pb"
	pb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest"
	servicePb "github.com/lhjnilsson/foreverbull/pkg/pb/backtest/service"
	financeServicePb "github.com/lhjnilsson/foreverbull/pkg/pb/finance/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// ziplineControlPort is the Zipline engine gRPC port, on the pod and on
// the Service that fronts it.
const ziplineControlPort = 50055

// maxPort is the largest TCP port a session can listen on.
const maxPort = 65535

// ZiplineTarget says where the broker reaches Zipline.
type ZiplineTarget struct {
	// ControlAddress is host:port for Ingest and DownloadIngestion. In the
	// cluster it is the Service DNS name, so a restarted Zipline pod keeps
	// the same address.
	ControlAddress string
	// Pods resolves Workload to a pod for each new session. A session
	// listens on a port the pod picks, which a ClusterIP Service does not
	// route, so the session needs the address of the pod that opened it.
	Pods     container.Engine
	Workload string
	// SessionControlPort is the engine gRPC port on that pod.
	SessionControlPort int
}

func NewZiplineEngine(ctx context.Context, target ZiplineTarget, ingestionURL *string) (*Zipline, error) {
	if target.ControlAddress == "" {
		return nil, errors.New("zipline control address is empty")
	}

	conn, err := grpc.NewClient(
		target.ControlAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("error getting grpc client: %w", err)
	}

	client := servicePb.NewEngineClient(conn)
	if ingestionURL != nil {
		_, err = client.DownloadIngestion(ctx, &servicePb.DownloadIngestionRequest{})
		if err != nil {
			conn.Close() //nolint: errcheck
			return nil, fmt.Errorf("error downloading ingestion: %w", err)
		}
	}

	return &Zipline{conn: conn, client: client, target: target}, nil
}

type Zipline struct {
	conn   *grpc.ClientConn
	client servicePb.EngineClient
	target ZiplineTarget
}

// Close releases the control connection.
func (z *Zipline) Close() error {
	return z.conn.Close()
}

func (z *Zipline) Ingest(ctx context.Context, backtest *pb.Backtest, object *storage.Object) ([]string, error) {
	bucket := string(object.Bucket)
	req := servicePb.IngestRequest{
		Backtest: backtest,
		Bucket:   &bucket,
		Object:   &object.Name,
	}

	rsp, err := z.client.Ingest(ctx, &req)
	if err != nil {
		return nil, fmt.Errorf("error ingesting: %w", err)
	}

	return rsp.GetNotices(), nil
}

// sessionCalendar is the exchange the bundle is written for.
const sessionCalendar = "XNYS"

// calendarEndStatus is the Zipline calendar service's OUT_OF_RANGE text for
// a read past its last session: "the XNYS calendar ends on 2027-11-02"
// (calendar_end_reason in service.py).
var calendarEndStatus = regexp.MustCompile(`^the ([A-Z0-9]+) calendar ends on (\d{4}-\d{2}-\d{2})$`)

// calendarEndError reads that status into an engine.CalendarEndError. It is
// false for any other error, and for OUT_OF_RANGE with other text.
func calendarEndError(err error) (*engine.CalendarEndError, bool) {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.OutOfRange {
		return nil, false
	}
	match := calendarEndStatus.FindStringSubmatch(st.Message())
	if match == nil {
		return nil, false
	}
	last, parseErr := time.Parse(time.DateOnly, match[2])
	if parseErr != nil {
		return nil, false
	}
	return &engine.CalendarEndError{Calendar: match[1], LastSession: last}, true
}

// Sessions asks Zipline's calendar service, on the control connection, for
// the XNYS sessions. It is the exchange_calendars calendar the ingester
// writes the bundle with, so both sides agree on which days are sessions.
func (z *Zipline) Sessions(ctx context.Context, start, end time.Time) ([]time.Time, error) {
	rsp, err := financeServicePb.NewCalendarClient(z.conn).ListCalendarDates(ctx, &financeServicePb.ListCalendarDatesRequest{
		Code:  sessionCalendar,
		Start: commonPb.GoTimeToDate(start),
		End:   commonPb.GoTimeToDate(end),
	})
	if err != nil {
		if calendarEnd, ok := calendarEndError(err); ok {
			return nil, calendarEnd
		}
		return nil, fmt.Errorf("error listing %s sessions: %w", sessionCalendar, err)
	}

	sessions := make([]time.Time, 0, len(rsp.GetDates()))
	for _, day := range rsp.GetDates() {
		sessions = append(sessions, commonPb.DateToTime(day.GetDate()))
	}
	return sessions, nil
}

func (z *Zipline) DownloadIngestion(ctx context.Context, object *storage.Object) error {
	bucket := string(object.Bucket)
	req := servicePb.DownloadIngestionRequest{
		Bucket: bucket,
		Object: object.Name,
	}

	_, err := z.client.DownloadIngestion(ctx, &req)
	if err != nil {
		return fmt.Errorf("error downloading ingestion: %w", err)
	}

	return nil
}

// NewSession looks the Zipline pod up again and opens the session on that
// pod, so the session port and the host it is dialled on belong to the
// same, current pod.
func (z *Zipline) NewSession(ctx context.Context, session *pb.Session) (engine.EngineSession, error) {
	host, err := z.sessionHost(ctx)
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(
		net.JoinHostPort(host, strconv.Itoa(z.target.SessionControlPort)),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("error getting grpc client: %w", err)
	}
	defer conn.Close() //nolint: errcheck

	rsp, err := servicePb.NewEngineClient(conn).NewSession(ctx, &servicePb.NewSessionRequest{Id: session.Id})
	if err != nil {
		return nil, fmt.Errorf("error creating zipline session: %w", err)
	}

	port := rsp.GetPort()
	if port < 1 || port > maxPort {
		return nil, fmt.Errorf("zipline returned session port %d, outside 1-%d", port, maxPort)
	}

	return NewZiplineEngineSession(ctx, net.JoinHostPort(host, strconv.FormatInt(port, 10)))
}

func (z *Zipline) sessionHost(ctx context.Context) (string, error) {
	c, err := z.target.Pods.Get(ctx, z.target.Workload)
	if err != nil {
		return "", fmt.Errorf("failed to get container for zipline: %w", err)
	}

	health, err := c.GetHealth()
	if err != nil {
		return "", fmt.Errorf("failed to get health for zipline container: %w", err)
	}
	if health != container.HealthHealthy {
		return "", errors.New("zipline container is unhealthy")
	}

	host, err := c.GetIpAddress()
	if err != nil {
		return "", fmt.Errorf("failed to get zipline pod address: %w", err)
	}

	return host, nil
}
