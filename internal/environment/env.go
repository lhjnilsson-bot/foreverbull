package environment

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	ServerAddress        = "SERVER_ADDRESS"
	ServerAddressDefault = "foreverbull"

	HTTPPort        = "HTTP_PORT"
	HTTPPortDefault = "8080"

	GRPCPort        = "GRPC_PORT"
	GRPCPortDefault = "50055"

	// GrafanaURL is the Grafana origin used in broker-emitted deep links.
	// Kind host-port default is localhost:3000; remote installs override it.
	GrafanaURL        = "GRAFANA_URL"
	GrafanaURLDefault = "http://localhost:3000"

	BacktestIngestionDefaultName        = "BACKTEST_INGESTION_DEFAULT_NAME"
	BacktestIngestionDefaultNameDefault = "default_ingestion"

	ZiplineContainerName          = "ZIPLINE_CONTAINER_NAME"
	ZiplineContainerNameDefault   = "foreverbull_zipline"
	BacktestPortRangeStart        = "BACKTEST_PORT_RANGE_START"
	BacktestPortRangeStartDefault = "27000"
	BacktestPortRangeEnd          = "BACKTEST_PORT_RANGE_END"
	BacktestPortRangeEndDefault   = "27015"

	LogLevel        = "LOG_LEVEL"
	LogLevelDefault = "warning"

	// PostgresURL is required. Setup refuses to start without it.
	PostgresURL = "POSTGRES_URL"

	// WorkerDatabaseURL is the reader role URL for a service pod. Empty is an
	// error from GetWorkerDatabaseURL; the owner URL is never used in its place.
	WorkerDatabaseURL = "WORKER_DATABASE_URL"
	// WorkerDBPassword is the password for role foreverbull_worker. It is not
	// the database owner password.
	WorkerDBPassword = "WORKER_DB_PASSWORD"

	// NatsURL is required. Setup refuses to start without it.
	NatsURL                   = "NATS_URL"
	NatsUser                  = "NATS_USER"
	NatsPassword              = "NATS_PASSWORD"
	NatsDurable               = "NATS_DURABLE"
	NatsDurableDefault        = "foreverbull"
	NatsDeliveryPolicy        = "NATS_DELIVERY_POLICY"
	NatsDeliveryPolicyDefault = "all"

	// MinioURL, MinioAccessKey, and MinioSecretKey are required. Setup
	// refuses to start without them.
	MinioURL       = "MINIO_URL"
	MinioAccessKey = "MINIO_ACCESS_KEY"
	MinioSecretKey = "MINIO_SECRET"

	// MinioSecure selects TLS to MinIO. The chart's in-cluster MinIO serves
	// plain HTTP, so the default is false.
	MinioSecure        = "MINIO_SECURE"
	MinioSecureDefault = "false"

	// FinanceTaskTimeLimit bounds one price download (DownloadHistoricalData)
	// and one finance ingest, as a Go duration such as 2h or 90m.
	FinanceTaskTimeLimit        = "FINANCE_TASK_TIME_LIMIT"
	FinanceTaskTimeLimitDefault = "2h"

	TradingEnabled        = "TRADING_ENABLED"
	TradingEnabledDefault = "false"

	// TradingProfilesFile is the path of the trading profiles YAML.
	// Credentials live in that file. Empty means the file is not configured.
	TradingProfilesFile = "TRADING_PROFILES_FILE"

	// TradingListingsFile replaces the embedded listing sample when a profile
	// does not set its own listings path.
	TradingListingsFile = "TRADING_LISTINGS_FILE"

	// AuthEnabled defaults false. Empty is false. Helm and fbenv leave it
	// unset. Any value other than true, false, 1, or 0 stops startup.
	AuthEnabled        = "AUTH_ENABLED"
	AuthEnabledDefault = "false"

	// AuthAdminUsername is the platform admin login. Required only when
	// AUTH_ENABLED is true. Empty means it is not configured.
	AuthAdminUsername = "AUTH_ADMIN_USERNAME"

	// AuthAdminPasswordFile is the path of the admin password file.
	// The password is not an environment variable.
	AuthAdminPasswordFile = "AUTH_ADMIN_PASSWORD_FILE"

	AuthCookieSecure        = "AUTH_COOKIE_SECURE"
	AuthCookieSecureDefault = "false"

	AuthTrustProxy        = "AUTH_TRUST_PROXY"
	AuthTrustProxyDefault = "false"

	// AuthTrustedProxies is a comma-separated CIDR list. It is used only when
	// AUTH_TRUST_PROXY is true. The client address is the rightmost forwarded
	// hop that is not in this list.
	AuthTrustedProxies = "AUTH_TRUSTED_PROXIES"

	// AuthPublicURL is the broker origin browsers use, such as https://fb.example.com.
	// Cookie writes may send this Origin when Sec-Fetch-Site is absent.
	AuthPublicURL = "AUTH_PUBLIC_URL"

	// GitHub OAuth is read only when AUTH_ENABLED is true. All three stay
	// empty by default so a partial set is visible at startup.
	AuthGitHubClientID     = "AUTH_GITHUB_CLIENT_ID"
	AuthGitHubClientSecret = "AUTH_GITHUB_CLIENT_SECRET"
	AuthGitHubRedirectURL  = "AUTH_GITHUB_REDIRECT_URL"

	// AuthGitHubAllowedOrgs is an optional comma-separated org allowlist.
	// Empty means GitHub login does not check org membership.
	AuthGitHubAllowedOrgs = "AUTH_GITHUB_ALLOWED_ORGS"
)

// Test-only connection values. SetupForTest fills them when the variable is
// empty. Setup never does: a broker without its own Postgres, NATS, or MinIO
// settings stops at startup instead of dialling localhost with public
// credentials.
const (
	PostgresURLTestDefault    = "postgres://postgres:foreverbull@localhost:5432/postgres?sslmode=disable"
	NatsURLTestDefault        = "nats://localhost:4222"
	MinioURLTestDefault       = "localhost:9000"
	MinioAccessKeyTestDefault = "minioadmin"
	MinioSecretKeyTestDefault = "minioadmin"
)

type envVar struct {
	name       string
	getDefault func() (string, error)
}

var envVars = []envVar{ //nolint: gochecknoglobals
	{ServerAddress, func() (string, error) { return ServerAddressDefault, nil }},
	{HTTPPort, func() (string, error) { return HTTPPortDefault, nil }},
	{GRPCPort, func() (string, error) { return GRPCPortDefault, nil }},
	{GrafanaURL, func() (string, error) { return GrafanaURLDefault, nil }},
	{BacktestIngestionDefaultName, func() (string, error) { return BacktestIngestionDefaultNameDefault, nil }},
	{ZiplineContainerName, func() (string, error) { return ZiplineContainerNameDefault, nil }},
	{BacktestPortRangeStart, func() (string, error) { return BacktestPortRangeStartDefault, nil }},
	{BacktestPortRangeEnd, func() (string, error) { return BacktestPortRangeEndDefault, nil }},
	{LogLevel, func() (string, error) { return LogLevelDefault, nil }},
	{NatsDurable, func() (string, error) { return NatsDurableDefault, nil }},
	{NatsDeliveryPolicy, func() (string, error) { return NatsDeliveryPolicyDefault, nil }},
	{MinioSecure, func() (string, error) { return MinioSecureDefault, nil }},
	{FinanceTaskTimeLimit, func() (string, error) { return FinanceTaskTimeLimitDefault, nil }},
	{TradingEnabled, func() (string, error) { return TradingEnabledDefault, nil }},
	{TradingProfilesFile, func() (string, error) { return "", nil }},
	{TradingListingsFile, func() (string, error) { return "", nil }},
	{AuthEnabled, func() (string, error) { return AuthEnabledDefault, nil }},
	{AuthAdminUsername, func() (string, error) { return "", nil }},
	{AuthAdminPasswordFile, func() (string, error) { return "", nil }},
	{AuthCookieSecure, func() (string, error) { return AuthCookieSecureDefault, nil }},
	{AuthTrustProxy, func() (string, error) { return AuthTrustProxyDefault, nil }},
	{AuthGitHubClientID, func() (string, error) { return "", nil }},
	{AuthGitHubClientSecret, func() (string, error) { return "", nil }},
	{AuthGitHubRedirectURL, func() (string, error) { return "", nil }},
}

// testEnvVars are filled only by SetupForTest.
var testEnvVars = []envVar{ //nolint: gochecknoglobals
	{PostgresURL, func() (string, error) { return PostgresURLTestDefault, nil }},
	{NatsURL, func() (string, error) { return NatsURLTestDefault, nil }},
	{MinioURL, func() (string, error) { return MinioURLTestDefault, nil }},
	{MinioAccessKey, func() (string, error) { return MinioAccessKeyTestDefault, nil }},
	{MinioSecretKey, func() (string, error) { return MinioSecretKeyTestDefault, nil }},
}

// requiredVars have no default in Setup. Their values are never printed.
var requiredVars = []string{PostgresURL, NatsURL, MinioURL, MinioAccessKey, MinioSecretKey} //nolint: gochecknoglobals

// boolVars accept true, false, 1, or 0, in any case, around which spaces are
// ignored. Anything else stops startup.
var boolVars = []string{TradingEnabled, AuthEnabled, AuthCookieSecure, AuthTrustProxy, MinioSecure} //nolint: gochecknoglobals

type envSetter interface {
	Helper()
	Setenv(key, value string)
	Fatalf(format string, args ...any)
}

func fillDefaults(vars []envVar, setenv func(name, value string) error) error {
	for _, envVar := range vars {
		if os.Getenv(envVar.name) != "" {
			continue
		}
		defaultEnv, err := envVar.getDefault()
		if err != nil {
			return fmt.Errorf("failed to set default value for %s: %w", envVar.name, err)
		}
		if err := setenv(envVar.name, defaultEnv); err != nil {
			return fmt.Errorf("failed to set default value for %s: %w", envVar.name, err)
		}
	}
	return nil
}

func applyEnvDefaults(setenv func(name, value string) error, testConnections bool, required []string) error {
	if err := fillDefaults(envVars, setenv); err != nil {
		return err
	}
	if testConnections {
		if err := fillDefaults(testEnvVars, setenv); err != nil {
			return err
		}
	}

	if err := validate(required); err != nil {
		return err
	}

	level, err := parseLogLevel(os.Getenv(LogLevel))
	if err != nil {
		return err
	}
	configureLogging(level)

	return nil
}

// Validate checks every setting the broker reads at startup and returns one
// error per bad variable, joined. A missing required variable is named
// without a value. A malformed non-secret value is quoted so the operator
// can see what was read.
func Validate() error {
	return validate(requiredVars)
}

func validate(required []string) error {
	var errs []error

	for _, name := range required {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			errs = append(errs, fmt.Errorf("%s is not set; it is required and has no default", name))
		}
	}

	for _, name := range boolVars {
		if _, err := parseBool(name); err != nil {
			errs = append(errs, err)
		}
	}

	if _, err := parseLogLevel(os.Getenv(LogLevel)); err != nil {
		errs = append(errs, err)
	}

	if _, err := ParseGrafanaURL(os.Getenv(GrafanaURL)); err != nil {
		errs = append(errs, err)
	}

	if _, err := ParseTrustedProxies(os.Getenv(AuthTrustedProxies)); err != nil {
		errs = append(errs, err)
	}

	if _, err := GetBacktestPortRange(); err != nil {
		errs = append(errs, err)
	}

	if _, err := GetFinanceTaskTimeLimit(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func parseLogLevel(value string) (zerolog.Level, error) {
	switch value {
	case "debug":
		return zerolog.DebugLevel, nil
	case "info":
		return zerolog.InfoLevel, nil
	case "warning":
		return zerolog.WarnLevel, nil
	case "error":
		return zerolog.ErrorLevel, nil
	default:
		return zerolog.NoLevel, fmt.Errorf("%s=%q is not a log level; set it to debug, info, warning, or error", LogLevel, value)
	}
}

func configureLogging(level zerolog.Level) {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr}).With().Caller().Logger()
	zerolog.SetGlobalLevel(level)
}

// parseBool reads a boolean variable. Empty is false, which is the default of
// every boolean variable here. The value is not a secret, so it is quoted in
// the error.
func parseBool(name string) (bool, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	default:
		return false, fmt.Errorf("%s=%q is not a boolean; set it to true or false", name, raw)
	}
}

// mustBool is for getters called after Setup, which already refused a bad
// value. A bad value here means Setup was skipped or the variable changed
// after startup, so the process stops instead of guessing.
func mustBool(name string) bool {
	verdict, err := parseBool(name)
	if err != nil {
		panic(err)
	}
	return verdict
}

// ParseGrafanaURL accepts an http or https origin with a host, optionally
// with a path for a subpath install. The value is not quoted in the error
// because a URL may carry user info.
func ParseGrafanaURL(value string) (*url.URL, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, fmt.Errorf("%s is empty; set it to the Grafana origin, such as %s", GrafanaURL, GrafanaURLDefault)
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%s is not a URL; set it to the Grafana origin, such as %s", GrafanaURL, GrafanaURLDefault)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%s must start with http:// or https://; set it to the Grafana origin, such as %s", GrafanaURL, GrafanaURLDefault)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%s has no host; set it to the Grafana origin, such as %s", GrafanaURL, GrafanaURLDefault)
	}
	return u, nil
}

// PublicHTTPOrigin is the scheme, host, and path of an http(s) URL.
// Userinfo, query, and fragment are dropped so they cannot be copied into a link.
// An empty or unusable value returns false.
func PublicHTTPOrigin(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.ContainsAny(trimmed, " \t\r\n") {
		return "", false
	}
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", false
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.RawPath = ""
	cleaned := path.Clean("/" + strings.Trim(u.Path, "/"))
	if cleaned == "/" || cleaned == "." {
		u.Path = ""
	} else {
		u.Path = cleaned
	}
	origin := u.String()
	if strings.Contains(origin, "@") || strings.ContainsAny(origin, " \t\r\n\"\\") {
		return "", false
	}
	return origin, true
}

// ParseTrustedProxies parses a comma-separated list of IP addresses and CIDRs.
// A bare address is one host. Every entry that is neither is named in one
// error; none is skipped.
func ParseTrustedProxies(raw string) ([]*net.IPNet, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	fields := strings.Split(raw, ",")
	nets := make([]*net.IPNet, 0, len(fields))
	var bad []string
	for _, field := range fields {
		entry := strings.TrimSpace(field)
		if entry == "" {
			continue
		}
		cidr := entry
		if !strings.Contains(cidr, "/") {
			if strings.Contains(cidr, ":") {
				cidr += "/128"
			} else {
				cidr += "/32"
			}
		}
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			bad = append(bad, "'"+entry+"'")
			continue
		}
		nets = append(nets, network)
	}
	if len(bad) > 0 {
		return nil, trustedProxiesError(bad)
	}
	return nets, nil
}

// trustedProxiesError names every quoted bad entry: 'x' isn't one, 'x' and
// 'y' aren't, 'x', 'y' and 'z' aren't.
func trustedProxiesError(quoted []string) error {
	const prefix = " must be a comma-separated list of IP addresses or CIDR ranges, but "
	if len(quoted) == 1 {
		return fmt.Errorf("%s%s%s isn't one.", AuthTrustedProxies, prefix, quoted[0]) //nolint:staticcheck // ST1005: agreed user-facing copy ends with a period
	}
	names := strings.Join(quoted[:len(quoted)-1], ", ") + " and " + quoted[len(quoted)-1]
	return fmt.Errorf("%s%s%s aren't.", AuthTrustedProxies, prefix, names) //nolint:staticcheck // ST1005: agreed user-facing copy ends with a period
}

// Setup fills empty optional variables with package defaults, validates every
// setting, and configures logging. Postgres, NATS, and MinIO settings have no
// default here. It does not start Docker or any other infrastructure.
func Setup() error {
	return applyEnvDefaults(os.Setenv, false, requiredVars)
}

// SetupForAccounts is Setup for the account subcommand, which opens only
// Postgres. POSTGRES_URL is required; NATS and MinIO are not read. Every
// other check is the same as Setup.
func SetupForAccounts() error {
	return applyEnvDefaults(os.Setenv, false, []string{PostgresURL})
}

// SetupForTest applies the Setup defaults plus the local test connection
// values, using t.Setenv so values are restored when the test ends. It
// validates the same way Setup does. It does not start Docker or other
// infrastructure. t is typically *testing.T.
func SetupForTest(t envSetter) {
	t.Helper()

	if err := applyEnvDefaults(func(name, value string) error {
		t.Setenv(name, value)
		return nil
	}, true, requiredVars); err != nil {
		t.Fatalf("environment.SetupForTest: %v", err)
	}
}

func GetServerAddress() string {
	return os.Getenv(ServerAddress)
}

func GetHTTPPort() string {
	return os.Getenv(HTTPPort)
}

func GetGRPCPort() string {
	return os.Getenv(GRPCPort)
}

func GetGrafanaURL() string {
	return os.Getenv(GrafanaURL)
}

func GetServerGRPCURL() string {
	return fmt.Sprintf("localhost:%s", GetGRPCPort())
}

func GetBacktestIngestionDefaultName() string {
	return os.Getenv(BacktestIngestionDefaultName)
}

func GetZiplineContainerName() string {
	return os.Getenv(ZiplineContainerName)
}

// PortRange is an inclusive range of TCP ports: Start and End can both be used.
type PortRange struct {
	Start int
	End   int
}

// Contains reports whether port is inside the inclusive range.
func (r PortRange) Contains(port int) bool {
	return port >= r.Start && port <= r.End
}

// GetBacktestPortRange reads BACKTEST_PORT_RANGE_START and BACKTEST_PORT_RANGE_END.
// Both ends are inclusive. A value that is not a port, or a start above the end,
// is an error that names the variable, so a bad setting fails at startup
// instead of on the first session.
func GetBacktestPortRange() (PortRange, error) {
	start, err := parsePort(BacktestPortRangeStart)
	if err != nil {
		return PortRange{}, err
	}

	end, err := parsePort(BacktestPortRangeEnd)
	if err != nil {
		return PortRange{}, err
	}

	if start > end {
		return PortRange{}, fmt.Errorf(
			"%s (%d) is above %s (%d); set the start to the lower port",
			BacktestPortRangeStart, start, BacktestPortRangeEnd, end,
		)
	}

	return PortRange{Start: start, End: end}, nil
}

func parsePort(name string) (int, error) {
	value := os.Getenv(name)

	port, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a port number; set it to a value from 1 to 65535", name, value)
	}

	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s=%d is outside 1-65535; set it to a valid TCP port", name, port)
	}

	return port, nil
}

// TaskProgressRetention is how long the foreverbull JetStream stream keeps a
// message, and so a task's states (stream.MaxAge). It lives here so
// FINANCE_TASK_TIME_LIMIT's error can name it without a dependency on the
// stream package.
const TaskProgressRetention = 24 * time.Hour

// FinanceTaskTimeLimitMax is the longest FINANCE_TASK_TIME_LIMIT the broker
// accepts. A task's states are read back from the foreverbull JetStream
// stream, which keeps a message for TaskProgressRetention (24 hours), so a
// task must end well inside that. 6 hours keeps a margin of four times the
// longest task, and is three times the 2h default.
const FinanceTaskTimeLimitMax = 6 * time.Hour

// durationText writes d as a Go duration without zero units: 24h, 90m.
func durationText(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}

// GetFinanceTaskTimeLimit reads FINANCE_TASK_TIME_LIMIT. Unset is the 2h
// default. A value that is not a Go duration, is 0 or negative, or is above
// FinanceTaskTimeLimitMax is an error that names the variable, so it fails
// at startup.
func GetFinanceTaskTimeLimit() (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(FinanceTaskTimeLimit))
	if value == "" {
		value = FinanceTaskTimeLimitDefault
	}
	limit, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a duration; set it to a Go duration such as 2h or 90m", FinanceTaskTimeLimit, value)
	}
	if limit <= 0 {
		return 0, fmt.Errorf("%s=%q must be above zero; set it to a Go duration such as 2h or 90m", FinanceTaskTimeLimit, value)
	}
	if limit > FinanceTaskTimeLimitMax {
		limitMax := durationText(FinanceTaskTimeLimitMax)
		return 0, fmt.Errorf("%s=%q is above the %s maximum; foreverbull keeps a task's progress for %s, so set it to %s or less",
			FinanceTaskTimeLimit, value, limitMax, durationText(TaskProgressRetention), limitMax)
	}
	return limit, nil
}

func GetLogLevel() string {
	return os.Getenv(LogLevel)
}

func GetPostgresURL() string {
	return os.Getenv(PostgresURL)
}

// GetWorkerDatabaseURL is the reader role URL a service pod may use. An empty
// value is an error that names WORKER_DATABASE_URL, whether or not
// authentication is on. The owner URL is never returned in its place.
func GetWorkerDatabaseURL() (string, error) {
	if url := strings.TrimSpace(os.Getenv(WorkerDatabaseURL)); url != "" {
		return url, nil
	}
	return "", fmt.Errorf("%s is not set; the owner %s is not handed to a service pod", WorkerDatabaseURL, PostgresURL)
}

func GetNATSURL() string {
	return os.Getenv(NatsURL)
}

func GetNATSUser() string {
	return os.Getenv(NatsUser)
}

func GetNATSPassword() string {
	return os.Getenv(NatsPassword)
}

func GetNATSDurable() string {
	return os.Getenv(NatsDurable)
}

func GetNATSDeliveryPolicy() string {
	return os.Getenv(NatsDeliveryPolicy)
}

func GetMinioURL() string {
	return os.Getenv(MinioURL)
}

func GetMinioAccessKey() string {
	return os.Getenv(MinioAccessKey)
}

func GetMinioSecretKey() string {
	return os.Getenv(MinioSecretKey)
}

// GetMinioSecure reports whether the broker dials MinIO over TLS.
func GetMinioSecure() bool {
	return mustBool(MinioSecure)
}

func GetTradingEnabled() bool {
	return mustBool(TradingEnabled)
}

func GetTradingProfilesFile() string {
	return os.Getenv(TradingProfilesFile)
}

func GetTradingListingsFile() string {
	return os.Getenv(TradingListingsFile)
}

// GetAuthEnabled reports whether authentication and authorization are on.
// An empty value is false. Setup refuses any value that is not a boolean.
func GetAuthEnabled() bool {
	return mustBool(AuthEnabled)
}

func GetAuthAdminUsername() string {
	return os.Getenv(AuthAdminUsername)
}

func GetAuthAdminPasswordFile() string {
	return os.Getenv(AuthAdminPasswordFile)
}

func GetAuthCookieSecure() bool {
	return mustBool(AuthCookieSecure)
}

func GetAuthTrustProxy() bool {
	return mustBool(AuthTrustProxy)
}

func GetAuthTrustedProxies() string {
	return os.Getenv(AuthTrustedProxies)
}

// GetAuthTrustedProxyNets parses AUTH_TRUSTED_PROXIES. Setup already refused
// a bad entry, so a bad list here means Setup was skipped or the variable
// changed after startup, and the process stops like mustBool.
func GetAuthTrustedProxyNets() []*net.IPNet {
	nets, err := ParseTrustedProxies(os.Getenv(AuthTrustedProxies))
	if err != nil {
		panic(err)
	}
	return nets
}

func GetAuthPublicURL() string {
	return os.Getenv(AuthPublicURL)
}

func GetAuthGitHubClientID() string {
	return os.Getenv(AuthGitHubClientID)
}

func GetAuthGitHubClientSecret() string {
	return os.Getenv(AuthGitHubClientSecret)
}

func GetAuthGitHubRedirectURL() string {
	return os.Getenv(AuthGitHubRedirectURL)
}

func GetAuthGitHubAllowedOrgs() string {
	return os.Getenv(AuthGitHubAllowedOrgs)
}
