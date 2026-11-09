package environment

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// validEnv sets every variable Setup reads to a valid value through
// t.Setenv, so Setup's own os.Setenv calls are undone when the test ends.
func validEnv(t *testing.T) {
	t.Helper()
	for _, envVar := range envVars {
		value, err := envVar.getDefault()
		require.NoError(t, err)
		t.Setenv(envVar.name, value)
	}
	t.Setenv(PostgresURL, "postgres://foreverbull:pw@postgres:5432/foreverbull")
	t.Setenv(NatsURL, "nats://nats:4222")
	t.Setenv(MinioURL, "minio:9000")
	t.Setenv(MinioAccessKey, "access-key-value")
	t.Setenv(MinioSecretKey, "secret-key-value")
	t.Setenv(AuthTrustedProxies, "")
	t.Setenv(WorkerDatabaseURL, "")
}

func TestSetupStartsWithAValidFullConfig(t *testing.T) {
	validEnv(t)
	t.Setenv(LogLevel, "info")
	t.Setenv(AuthEnabled, "true")
	t.Setenv(AuthCookieSecure, "1")
	t.Setenv(AuthTrustProxy, "TRUE")
	t.Setenv(AuthTrustedProxies, "10.0.0.0/8, 192.168.1.1, ::1")
	t.Setenv(TradingEnabled, "false")
	t.Setenv(MinioSecure, "0")
	t.Setenv(GrafanaURL, "https://grafana.example.com/sub")

	require.NoError(t, Setup())

	require.Equal(t, "postgres://foreverbull:pw@postgres:5432/foreverbull", GetPostgresURL())
	require.Equal(t, "nats://nats:4222", GetNATSURL())
	require.Equal(t, "minio:9000", GetMinioURL())
	require.Equal(t, "access-key-value", GetMinioAccessKey())
	require.Equal(t, "secret-key-value", GetMinioSecretKey())
	require.True(t, GetAuthEnabled())
	require.True(t, GetAuthCookieSecure())
	require.True(t, GetAuthTrustProxy())
	require.False(t, GetTradingEnabled())
	require.False(t, GetMinioSecure())
	require.Len(t, GetAuthTrustedProxyNets(), 3)
}

func TestSetupFillsOptionalDefaultsOnly(t *testing.T) {
	validEnv(t)
	t.Setenv(HTTPPort, "")
	t.Setenv(MinioSecure, "")
	t.Setenv(AuthEnabled, "")

	require.NoError(t, Setup())
	require.Equal(t, HTTPPortDefault, GetHTTPPort())
	require.Equal(t, "false", os.Getenv(MinioSecure))
	require.False(t, GetMinioSecure())
	require.False(t, GetAuthEnabled())
}

func TestSetupRefusesMissingConnectionSettings(t *testing.T) {
	for _, name := range requiredVars {
		t.Run(name, func(t *testing.T) {
			validEnv(t)
			t.Setenv(name, "")

			err := Setup()
			require.EqualError(t, err, name+" is not set; it is required and has no default")
		})
	}
}

func TestSetupNamesEveryMissingConnectionSetting(t *testing.T) {
	validEnv(t)
	for _, name := range requiredVars {
		t.Setenv(name, "")
	}

	err := Setup()
	require.EqualError(t, err, "POSTGRES_URL is not set; it is required and has no default\n"+
		"NATS_URL is not set; it is required and has no default\n"+
		"MINIO_URL is not set; it is required and has no default\n"+
		"MINIO_ACCESS_KEY is not set; it is required and has no default\n"+
		"MINIO_SECRET is not set; it is required and has no default")
}

func TestSetupRefusesWhitespaceAsAConnectionSetting(t *testing.T) {
	validEnv(t)
	t.Setenv(MinioSecretKey, "   ")

	require.EqualError(t, Setup(), "MINIO_SECRET is not set; it is required and has no default")
}

func TestSetupRefusesInvalidBooleans(t *testing.T) {
	for _, name := range boolVars {
		for _, value := range []string{"yes", "no", "on", "off", "enabled", "t", "2", "  ", "true!"} {
			t.Run(name+"="+value, func(t *testing.T) {
				validEnv(t)
				t.Setenv(name, value)

				err := Setup()
				require.EqualError(t, err, name+"=\""+value+"\" is not a boolean; set it to true or false")
			})
		}
	}
}

func TestBooleansAcceptTrueFalseOneZeroInAnyCaseAndTrimmed(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"true", true}, {"TRUE", true}, {"True", true}, {"1", true}, {"true ", true}, {" 1\t", true},
		{"false", false}, {"FALSE", false}, {"0", false}, {" false", false}, {"", false},
	}
	for _, name := range boolVars {
		for _, tc := range cases {
			t.Run(name+"="+tc.value, func(t *testing.T) {
				t.Setenv(name, tc.value)
				got, err := parseBool(name)
				require.NoError(t, err)
				require.Equal(t, tc.want, got)
			})
		}
	}
}

func TestAuthEnabledYesDoesNotTurnAuthOff(t *testing.T) {
	validEnv(t)
	t.Setenv(AuthEnabled, "yes")

	require.EqualError(t, Setup(), `AUTH_ENABLED="yes" is not a boolean; set it to true or false`)
	require.Panics(t, func() { GetAuthEnabled() })
}

func TestBooleanGettersPanicOnAValueSetupWouldRefuse(t *testing.T) {
	getters := map[string]func() bool{
		TradingEnabled:   GetTradingEnabled,
		AuthEnabled:      GetAuthEnabled,
		AuthCookieSecure: GetAuthCookieSecure,
		AuthTrustProxy:   GetAuthTrustProxy,
		MinioSecure:      GetMinioSecure,
	}
	require.Len(t, getters, len(boolVars))
	for name, get := range getters {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "not-a-bool")
			require.PanicsWithError(t, name+`="not-a-bool" is not a boolean; set it to true or false`, func() { get() })
		})
	}
}

func TestSetupRefusesAnUnknownLogLevel(t *testing.T) {
	for _, value := range []string{"verbose", "WARNING", "warn", "trace", " "} {
		t.Run(value, func(t *testing.T) {
			validEnv(t)
			t.Setenv(LogLevel, value)

			require.EqualError(t, Setup(), "LOG_LEVEL=\""+value+"\" is not a log level; set it to debug, info, warning, or error")
		})
	}
}

func TestPublicHTTPOrigin(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{raw: "http://localhost:3000", want: "http://localhost:3000", ok: true},
		{raw: "https://example.com/grafana/", want: "https://example.com/grafana", ok: true},
		{raw: "https://example.com/grafana/./", want: "https://example.com/grafana", ok: true},
		{raw: "http://user:hunter2@localhost:3000/grafana?token=secret#x", want: "http://localhost:3000/grafana", ok: true},
		{raw: "", ok: false},
		{raw: "   ", ok: false},
		{raw: "not a url", ok: false},
		{raw: "ftp://localhost:3000", ok: false},
		{raw: "http://", ok: false},
		{raw: "javascript:alert(1)", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, ok := PublicHTTPOrigin(tc.raw)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
			require.NotContains(t, got, "hunter2")
			require.NotContains(t, got, "secret")
		})
	}
}

func TestSetupRefusesAnInvalidGrafanaURL(t *testing.T) {
	cases := []struct {
		value string
		want  string
	}{
		{"not a url", "GRAFANA_URL must start with http:// or https://; set it to the Grafana origin, such as http://localhost:3000"},
		{"ftp://grafana.example.com", "GRAFANA_URL must start with http:// or https://; set it to the Grafana origin, such as http://localhost:3000"},
		{"http://", "GRAFANA_URL has no host; set it to the Grafana origin, such as http://localhost:3000"},
		{"http://user:hunter2@[::1", "GRAFANA_URL is not a URL; set it to the Grafana origin, such as http://localhost:3000"},
		{"   ", "GRAFANA_URL is empty; set it to the Grafana origin, such as http://localhost:3000"},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			validEnv(t)
			t.Setenv(GrafanaURL, tc.value)

			err := Setup()
			require.EqualError(t, err, tc.want)
			require.NotContains(t, err.Error(), "hunter2")
		})
	}
}

func TestSetupRefusesAnInvalidTrustedProxyEntry(t *testing.T) {
	validEnv(t)
	t.Setenv(AuthTrustedProxies, "10.0.0.0/8, 10.0.0.300, proxy.internal,192.168.0.0/33")

	require.EqualError(t, Setup(), "AUTH_TRUSTED_PROXIES must be a comma-separated list of IP addresses or "+
		"CIDR ranges, but '10.0.0.300', 'proxy.internal' and '192.168.0.0/33' aren't.")
}

func TestSetupNamesTheOneBadTrustedProxyEntry(t *testing.T) {
	validEnv(t)
	t.Setenv(AuthTrustedProxies, "10.0.0.0/8, x")

	require.EqualError(t, Setup(), "AUTH_TRUSTED_PROXIES must be a comma-separated list of IP addresses or "+
		"CIDR ranges, but 'x' isn't one.")
}

func TestSetupNamesTwoBadTrustedProxyEntries(t *testing.T) {
	validEnv(t)
	t.Setenv(AuthTrustedProxies, "x, 10.0.0.0/8, y")

	require.EqualError(t, Setup(), "AUTH_TRUSTED_PROXIES must be a comma-separated list of IP addresses or "+
		"CIDR ranges, but 'x' and 'y' aren't.")
}

func TestTrustedProxyNetsPanicsOnABadListAfterStartup(t *testing.T) {
	t.Setenv(AuthTrustedProxies, "10.0.0.0/8, x")

	require.PanicsWithError(t, "AUTH_TRUSTED_PROXIES must be a comma-separated list of IP addresses or "+
		"CIDR ranges, but 'x' isn't one.", func() { GetAuthTrustedProxyNets() })
}

func TestSetupNamesEveryBadVariableWithoutSecretValues(t *testing.T) {
	validEnv(t)
	t.Setenv(PostgresURL, "")
	t.Setenv(AuthEnabled, "yes")
	t.Setenv(MinioSecure, "on")
	t.Setenv(LogLevel, "loud")

	err := Setup()
	require.EqualError(t, err, "POSTGRES_URL is not set; it is required and has no default\n"+
		`AUTH_ENABLED="yes" is not a boolean; set it to true or false`+"\n"+
		`MINIO_SECURE="on" is not a boolean; set it to true or false`+"\n"+
		`LOG_LEVEL="loud" is not a log level; set it to debug, info, warning, or error`)
	require.NotContains(t, err.Error(), "secret-key-value")
	require.NotContains(t, err.Error(), "access-key-value")
}

func TestSetupForTestFillsTestConnectionsWithoutDocker(t *testing.T) {
	t.Setenv(HTTPPort, "")
	t.Setenv(GRPCPort, "")
	t.Setenv(GrafanaURL, "")
	t.Setenv(PostgresURL, "")
	t.Setenv(NatsURL, "")
	t.Setenv(MinioURL, "")
	t.Setenv(MinioAccessKey, "")
	t.Setenv(MinioSecretKey, "")
	t.Setenv(ServerAddress, "")
	t.Setenv(BacktestPortRangeStart, "")
	t.Setenv(BacktestPortRangeEnd, "")

	SetupForTest(t)

	require.Equal(t, HTTPPortDefault, GetHTTPPort())
	require.Equal(t, GRPCPortDefault, GetGRPCPort())
	require.Equal(t, GrafanaURLDefault, GetGrafanaURL())
	require.Equal(t, PostgresURLTestDefault, GetPostgresURL())
	require.Equal(t, NatsURLTestDefault, GetNATSURL())
	require.Equal(t, MinioURLTestDefault, GetMinioURL())
	require.Equal(t, MinioAccessKeyTestDefault, GetMinioAccessKey())
	require.Equal(t, MinioSecretKeyTestDefault, GetMinioSecretKey())
	require.Equal(t, ServerAddressDefault, GetServerAddress())
	portRange, err := GetBacktestPortRange()
	require.NoError(t, err)
	require.Equal(t, PortRange{Start: 27000, End: 27015}, portRange)
}

func TestWorkerDatabaseURLNeverFallsBackToTheOwner(t *testing.T) {
	for _, auth := range []string{"true", "false"} {
		t.Run("AUTH_ENABLED="+auth, func(t *testing.T) {
			t.Setenv(AuthEnabled, auth)
			t.Setenv(WorkerDatabaseURL, "")
			t.Setenv(PostgresURL, "postgres://postgres:foreverbull@localhost:5432/postgres")

			url, err := GetWorkerDatabaseURL()
			require.EqualError(t, err, "WORKER_DATABASE_URL is not set; the owner POSTGRES_URL is not handed to a service pod")
			require.Empty(t, url)

			reader := "postgres://foreverbull_worker:reader@postgres:5432/foreverbull"
			t.Setenv(WorkerDatabaseURL, reader)
			url, err = GetWorkerDatabaseURL()
			require.NoError(t, err)
			require.Equal(t, reader, url)
		})
	}
}

func TestGetBacktestPortRangeRejectsBadValues(t *testing.T) {
	cases := []struct {
		name  string
		start string
		end   string
		want  string
	}{
		{name: "start not a number", start: "abc", end: "27015", want: "BACKTEST_PORT_RANGE_START=\"abc\" is not a port number"},
		{name: "end empty", start: "27000", end: "", want: "BACKTEST_PORT_RANGE_END=\"\" is not a port number"},
		{name: "start zero", start: "0", end: "27015", want: "BACKTEST_PORT_RANGE_START=0 is outside 1-65535"},
		{name: "end above 65535", start: "27000", end: "70000", want: "BACKTEST_PORT_RANGE_END=70000 is outside 1-65535"},
		{name: "start above end", start: "27016", end: "27015", want: "BACKTEST_PORT_RANGE_START (27016) is above BACKTEST_PORT_RANGE_END (27015)"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(BacktestPortRangeStart, tc.start)
			t.Setenv(BacktestPortRangeEnd, tc.end)

			_, err := GetBacktestPortRange()
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestGetBacktestPortRangeSinglePortIsInclusive(t *testing.T) {
	t.Setenv(BacktestPortRangeStart, "27000")
	t.Setenv(BacktestPortRangeEnd, "27000")

	portRange, err := GetBacktestPortRange()
	require.NoError(t, err)
	require.True(t, portRange.Contains(27000))
	require.False(t, portRange.Contains(27001))
}

func TestSetupFailsOnBadPortRange(t *testing.T) {
	validEnv(t)
	t.Setenv(BacktestPortRangeStart, "not-a-port")

	err := Setup()
	require.ErrorContains(t, err, "BACKTEST_PORT_RANGE_START")
}

func TestSetupForTestDoesNotOverrideSetValues(t *testing.T) {
	t.Setenv(HTTPPort, "12345")
	t.Setenv(PostgresURL, "postgres://set:by@test:5432/db")
	SetupForTest(t)
	require.Equal(t, "12345", GetHTTPPort())
	require.Equal(t, "postgres://set:by@test:5432/db", GetPostgresURL())
}

func TestTradingEnabledDefaultsFalse(t *testing.T) {
	t.Setenv(TradingEnabled, "")
	t.Setenv(TradingProfilesFile, "")
	t.Setenv(TradingListingsFile, "")

	SetupForTest(t)

	require.False(t, GetTradingEnabled())
	require.Empty(t, GetTradingProfilesFile())
	require.Empty(t, GetTradingListingsFile())

	t.Setenv(TradingProfilesFile, "/tmp/profiles.yaml")
	require.Equal(t, "/tmp/profiles.yaml", GetTradingProfilesFile())
}

func TestGetTradingEnabledAndServerGRPCURL(t *testing.T) {
	t.Setenv(GRPCPort, "1234")
	require.Equal(t, "localhost:1234", GetServerGRPCURL())

	t.Setenv(TradingEnabled, "true")
	require.True(t, GetTradingEnabled())

	t.Setenv(TradingEnabled, "false")
	require.False(t, GetTradingEnabled())
}

func TestSetupForAccountsNeedsOnlyPostgres(t *testing.T) {
	validEnv(t)
	t.Setenv(NatsURL, "")
	t.Setenv(MinioURL, "")
	t.Setenv(MinioAccessKey, "")
	t.Setenv(MinioSecretKey, "")
	require.NoError(t, SetupForAccounts())

	t.Setenv(PostgresURL, "")
	t.Setenv(AuthEnabled, "yes")
	require.EqualError(t, SetupForAccounts(), "POSTGRES_URL is not set; it is required and has no default\n"+
		`AUTH_ENABLED="yes" is not a boolean; set it to true or false`)
}

func TestFinanceTaskTimeLimitDefaultsToTwoHours(t *testing.T) {
	validEnv(t)
	t.Setenv(FinanceTaskTimeLimit, "")
	require.NoError(t, Setup())
	require.Equal(t, "2h", os.Getenv(FinanceTaskTimeLimit))
	limit, err := GetFinanceTaskTimeLimit()
	require.NoError(t, err)
	require.Equal(t, 2*time.Hour, limit)

	t.Setenv(FinanceTaskTimeLimit, "90m")
	limit, err = GetFinanceTaskTimeLimit()
	require.NoError(t, err)
	require.Equal(t, 90*time.Minute, limit)

	t.Setenv(FinanceTaskTimeLimit, "6h")
	limit, err = GetFinanceTaskTimeLimit()
	require.NoError(t, err)
	require.Equal(t, FinanceTaskTimeLimitMax, limit)
}

// A bad limit fails at startup and names the variable.
func TestSetupRejectsAnInvalidFinanceTaskTimeLimit(t *testing.T) {
	validEnv(t)
	for value, want := range map[string]string{
		"two hours": `FINANCE_TASK_TIME_LIMIT="two hours" is not a duration; set it to a Go duration such as 2h or 90m`,
		"0s":        `FINANCE_TASK_TIME_LIMIT="0s" must be above zero; set it to a Go duration such as 2h or 90m`,
		"-1h":       `FINANCE_TASK_TIME_LIMIT="-1h" must be above zero; set it to a Go duration such as 2h or 90m`,
		"6h1s":      `FINANCE_TASK_TIME_LIMIT="6h1s" is above the 6h maximum; foreverbull keeps a task's progress for 24h, so set it to 6h or less`,
		"24h":       `FINANCE_TASK_TIME_LIMIT="24h" is above the 6h maximum; foreverbull keeps a task's progress for 24h, so set it to 6h or less`,
	} {
		t.Setenv(FinanceTaskTimeLimit, value)
		require.EqualError(t, Setup(), want, value)
	}
}

func TestDurationText(t *testing.T) {
	require.Equal(t, "24h", durationText(24*time.Hour))
	require.Equal(t, "6h", durationText(FinanceTaskTimeLimitMax))
	require.Equal(t, "90m", durationText(90*time.Minute))
	require.Equal(t, "1m30s", durationText(90*time.Second))
}
