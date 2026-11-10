# foreverbull-cli

Python CLIs for a running Foreverbull broker and for installing the platform.

After `uv sync` in `client/`:

```bash
uv run fbull --help
uv run fbenv --help
```

Entry points (`client/cli/pyproject.toml`):

| Command | Module | Role |
| --- | --- | --- |
| `fbull` | `foreverbull_cli.bull.cli` | Backtests (and trading) against a running broker |
| `fbenv` | `foreverbull_cli.env.cli` | Kind/Helm environment (`local` or `remote`) |

Requires Python 3.12+. Local `fbenv` also needs Docker, Kind, Helm, and kubectl.

`fbull` does not start the cluster. `fbenv` does not run algorithms. Use both: `fbenv create`, `fbenv start`, then `fbull backtest …`.

## fbull

Talks to the broker over gRPC. Subcommands: `fbull backtest …`, `fbull trading …`.

Broker address (`foreverbull.broker.config.resolve_broker_address`):

1. `FOREVERBULL_BROKER_ADDRESS` (`host:port`)
2. `remote.broker_address` when cluster config is `remote`
3. `localhost:50055` when cluster is `local` (the default)

A remote broker is always TLS, with or without a token. `127.0.0.1`, `::1`, and `localhost` stay plaintext, which is the Kind host port. Any other host is remote: `fbull` opens a TLS channel and does not fall back to plaintext. The token only adds the authorization metadata. A private CA is the PEM file in `FOREVERBULL_BROKER_CA`. When that variable is unset, the system trust store is used, which covers a public Let's Encrypt certificate. A CA path that cannot be read fails the call.

Logging: `-v` warning, `-vv` info, `-vvv` debug (`LOGGING_LEVEL`).

### Backtests

```bash
uv run fbull backtest list
uv run fbull backtest etfs
uv run fbull backtest create --file backtest.yaml
uv run fbull backtest get demo
uv run fbull backtest get demo --output yaml
uv run fbull backtest run demo path/to/strategy.py
uv run fbull backtest run demo path/to/strategy.py --start 2021-01-01 --end 2021-03-31 --dry
uv run fbull backtest executions --backtest demo
uv run fbull backtest execution EXECUTION_ID   # full ID or a unique prefix, such as the short ID run prints
uv run fbull backtest delete demo
```

`create` stores the universe, downloads daily bars, and ingests them into Zipline (progress: Creating, Downloading, Ingesting, Storing). A symbol whose download fails is stored with `enabled: false`.

`create` reads optional `provider` and `profile` from the YAML. `--provider` overrides the file. Omitting `provider` sends `yahoo`. Create prints `Using provider: <name> (<source>)`, where the source is `default`, `from --provider`, or `from <YAML file name>`, for example `Using provider: yahoo (default)` or `Using provider: stooq (from --provider)`. The printed backtest includes the provider. An unknown name exits non-zero and lists the valid providers (`yahoo`, `stooq`, `alpaca`). `profile` is an Alpaca profile name and is ignored unless `provider` is `alpaca`. Do not put API keys in the YAML.

| Provider | How to select it | What it prices |
| --- | --- | --- |
| Yahoo (default) | omit `provider`, or `yahoo` | Asset lookup and daily bars |
| Stooq | `provider: stooq` or `--provider stooq` | Daily bars from Stooq. Asset lookup stays on Yahoo |
| Alpaca | `provider: alpaca` and optional `profile` | Daily bars with that profile's key. Asset lookup stays on Yahoo |

Bars are stored per provider, so Yahoo and Stooq can both keep a close for the same symbol and day. A run reads only its backtest's provider. The demo file is [examples/demo.yaml](../../examples/demo.yaml) (name `demo`, benchmark `SPY`, ten liquid US symbols, 2023-01-01 through 2023-12-31). From `client/`:

```bash
# Yahoo (provider omitted)
uv run fbull backtest create --file ../examples/demo.yaml
uv run fbull backtest get demo

# Stooq (delete first when demo already exists)
uv run fbull backtest delete demo
uv run fbull backtest create --file ../examples/demo.yaml --provider stooq
```

Stooq may block automated downloads with a bot-check page. Create then stops at the first blocked symbol and says to retry with `--provider yahoo`.

`--file` YAML matches the cookiecutter `backtest.yaml` shape (`name`, `benchmark`, `periods` with `start_date` / `end_date` / `assets`) plus optional `provider` and `profile`. Without `--file`, create prompts for a name, an ETF, and dates, and sends `yahoo` unless `--provider` is set. `fbull backtest etfs` prints the same funds the prompt offers (name, benchmark, inception). The known fund is `iShares S&P 100` (benchmark `OEF`, inception 2000-10-23). An unknown name stops before the broker is called. That path builds a point-in-time universe from iShares historical holdings for the backtest dates (not today's list).

`run` starts a backtest session on the broker and executes the strategy file on the host (NNG ports `27000`–`27015` on local Kind). The backtest must already exist from `create`. After a run, `fbull` prints a Grafana link at `http://localhost:3000` (local Kind host port).

### Trading

```bash
uv run fbull trading run demo path/to/strategy.py --dry
```

This talks to a running broker. Paper trading stays off unless `TRADING_ENABLED=true`. That flag is a kill switch, not a provider. `fbull strategy deploy --no-dry-run` requires `--backend <profile>` plus `--yes` or `--confirm`. Dry-run may omit `--backend`. `--mode live` is refused.

Credentials live only in the profiles file. In the cluster, `TRADING_PROFILES_FILE` is `/etc/foreverbull/profiles.yaml`, mounted from Secret `foreverbull-trading`, key `profiles.yaml`. On local Kind, `fbenv` applies `.foreverbull/trading-profiles.yaml` as that Secret when the file exists, and does not print it. The committed example is [deploy/trading-profiles.example.yaml](../../deploy/trading-profiles.example.yaml) and its credential fields are empty. Do not put keys in the Helm chart, Postgres, or `.foreverbull/config.yaml`.

### Token

`fbull` sends a bearer only when a token is configured. While `AUTH_ENABLED` is false, leave it unset. `task test:integration` does not set `FOREVERBULL_TOKEN`.

Resolution order, first match, each value trimmed:

1. `--token` (not stored; it can land in shell history)
2. `FOREVERBULL_TOKEN`
3. `~/.foreverbull/token`

The token is not a key in `.foreverbull/config.yaml`, `~/.foreverbull/config.yaml`, or `FOREVERBULL_CONFIG`. `fbenv` does not send it.

```bash
uv run fbull token save
uv run fbull token save --force
```

`token save` reads stdin and writes `~/.foreverbull/token` with mode `0600`. It does not take the token as an argument. If the file already exists, the command refuses unless `--force` is passed. A saved token has no expiry. It replaces a `fbull login` token and removes its `token.meta`.

`~/.foreverbull` is `0700` and `token` and `token.meta` are `0600`, owned by you. Both are written to a temporary `0600` file and renamed into place. A token file, metadata file, or directory with group or other bits, another owner, or a symlink is refused. The error names the path and the `chmod` that fixes it, not the contents. There is no keyring.

Remote brokers are always reached over TLS via `ingress.host`, which requires `ingress.tls=true`. `127.0.0.1`, `::1`, and `localhost` stay plaintext.

### Sign in with the browser

```bash
uv run fbull login
uv run fbull logout
uv run fbull logout --local
uv run fbull logout --all
```

`fbull login` needs `AUTH_ENABLED=true` on the broker. It listens on `127.0.0.1` on a random port, opens `<broker>/cli/authorize` in your browser, and waits up to five minutes for one request on `http://127.0.0.1:<port>/callback`. The page uses your web sign-in (password or GitHub), asks "Let fbull sign in?", shows the computer's host name, the scope, and your login, and issues nothing until you press Approve. Deny ends the sign-in. The broker accepts no other redirect host, path, or scheme.

The flow is PKCE (S256) with a `state` that `fbull` checks. The code from Approve is single use and valid for 60 seconds. `fbull` exchanges it with a POST, and the token comes back in the response body, never in a URL. A second use of a code revokes the token the first one issued. The exchange is rate limited per address and per account. The broker URL is `http://localhost:8080` on local Kind, `remote.broker_url` in the cluster config on a remote cluster, or `FOREVERBULL_BROKER_URL`. It must be `https://` unless the host is `127.0.0.1` or `localhost`. When no browser can be opened, `fbull login` fails and points to `fbull token save`.

The token has scope `write`, or `read` for a viewer. A pending account cannot approve. It is saved to `~/.foreverbull/token`, with its expiry, client, and broker (not the secret) in `~/.foreverbull/token.meta`. After sign-in, `fbull` prints that it's valid until a time 30 days out, or until it goes unused for 14 days, whichever comes first. Then every command fails with ``Your sign-in has expired. Run `fbull login` to sign in again.``

`fbull logout` revokes the token on the broker, then deletes both files. When the broker can't be reached or refuses the revoke, it exits non-zero, keeps the files, and says the token is still valid until its expiry, or until it goes unused for 14 days, whichever comes first. When the broker named in `token.meta` answers in its own JSON (`sign_in_expired` or `unauthorized`) that the token is already expired or revoked, the files are deleted. Any other 401, or a 401 for a token saved with `fbull token save`, keeps them. `fbull` doesn't follow HTTP redirects from the broker, so the bearer never goes to another address. `--local` deletes the files only, and the token stays valid on the broker. `--all` revokes every token and browser session of your account, saved tokens included. The web UI's account page has the same Sign out everywhere button, which asks for confirmation first.

When the broker has authentication on, set `GRAFANA_BROKER_TOKEN` on the Grafana deployment from a Secret you create. The chart does not generate that token and does not ship a default. Mint it from a dedicated editor account that no person signs in with, with scope `read` (`{"name":"grafana","scope":"read"}` on `POST /api/auth/tokens`). A token minted from a person's account is revoked when that person rotates their password, loses the editor role, or uses Sign out everywhere. A token from a dedicated editor account sees only that account's rows, not every row the way an admin-minted token does, so Grafana dashboards may show less than before. A `read` token may call only the read RPCs. An omitted scope is `write`. A token minted by the admin shows every row. A viewer cannot mint a token. Leave `GRAFANA_BROKER_TOKEN` unset while `AUTH_ENABLED` is false: the plugin then dials with no metadata. Grafana reaches the broker at `foreverbull:50055` without TLS. Its NetworkPolicy allows only DNS and that port. Do not publish `50055` beyond the cluster. Do not put the user bearer or the session secret on a worker pod. `fbenv` does not set `AUTH_ENABLED`.

## Accounts

The server binary creates further password accounts. It is not an `fbull` command, and it does not set `AUTH_ENABLED`. The platform admin is not created here. The first `AUTH_ENABLED=true` boot creates that account from `AUTH_ADMIN_USERNAME` and `AUTH_ADMIN_PASSWORD_FILE`. The password is the file from Secret `foreverbull-admin`, key `password`, not an environment variable. See [deploy/README.md](../../deploy/README.md#authentication).

```bash
foreverbull account create --username reader
foreverbull account rotate --username reader
```

🔒️ `account create` inserts role `editor`. There is no `--role` flag. Passing `--role`, including `--role viewer` or `--role admin`, is an error that says role changes are SQL. `account rotate` replaces `password_hash`, deletes that account's browser sessions, and revokes all of its API tokens, saved tokens and a Grafana `read` token included. It does not change `role`. Moving `account.role` down (for example `editor` to `viewer`) also revokes every token and session of that account. No command and no HTTP route changes an existing `account.role`. GitHub first sign-in inserts `pending`. The enable boot inserts `admin` from the password file.

The password is read from stdin with echo off, length 12 to 128. It is not a flag and not an environment variable, and it is not written to a log. Stdout is the account id. A duplicate username is an error and does not change the hash. The command uses `POSTGRES_URL`.

Role changes are SQL. For a password account:

```sql
UPDATE account AS a
SET role = 'viewer'
FROM account_password AS p
WHERE p.account_id = a.id
  AND p.username = 'reader';
```

Use `'none'`, `'editor'`, or `'admin'` in that statement for the other changes. For a GitHub account, match `account_github.github_subject` instead of the password username:

```sql
UPDATE account AS a
SET role = 'viewer'
FROM account_github AS g
WHERE g.account_id = a.id
  AND g.github_subject = '123456';
```

Use `'editor'` in that statement when the person should create rows and mint a token. The broker does not run these statements.

## fbenv

Installs the six workloads (Postgres, NATS JetStream, Minio, Zipline, broker, Grafana) with **one Helm chart**. It does **not** create a Docker network or mount `/var/run/docker.sock`.

- `local` (default): Kind cluster named `foreverbull`, chart in namespace `foreverbull`, `expose.hostPort=true`. Host ports match Kind `extraPortMappings` (`5432`, `4222`, `9000`, `50055`, `8080`, `27000`–`27015`, `3000`). After `fbenv start`, the broker page is `http://localhost:8080`.
- `remote`: `helm upgrade --install` against `--kubeconfig` / `--context` / `--namespace`. Requires `--broker-address` (or `remote.broker_address` / `FOREVERBULL_BROKER_ADDRESS`).

```bash
uv run fbenv status                      # host checks, then workloads
uv run fbenv use                         # print selected cluster
uv run fbenv use local                   # writes .foreverbull/config.yaml
uv run fbenv use remote --kubeconfig PATH --context NAME --namespace NS --broker-address grpc.example.com:443
VERSION=local uv run fbenv create        # Kind + helm upgrade --install, workloads stopped
uv run fbenv create --start              # same install, then start
uv run fbenv create --verbose            # on a failed image pull, also print Docker's error
uv run fbenv start                       # scale deployments to 1 and wait until ready
uv run fbenv stop                        # scale to 0
uv run fbenv stop --remove               # helm uninstall
uv run fbenv stop --remove --purge       # uninstall; local also deletes the Kind cluster
uv run fbenv update                      # reload images (local) and helm upgrade --reuse-values
                                         # remote: fails if a node can't pull an image
```

`create` and `start` run the host checks first and print nothing when they pass: tools installed, Docker running, at least 4 GB of memory for Docker, and the Kind host ports free (or already held by the `foreverbull` cluster). The first failed check stops the command before it changes anything, with a fix hint. A remote cluster checks only `helm` and `kubectl`.

`--cluster`, `--kubeconfig`, `--context`, `--namespace`, `--broker-address` override the selected cluster for one command.

Without `VERSION`, `fbenv` pulls `ghcr.io/lhjnilsson/{foreverbull,zipline,fb-grafana}:latest` (the published nightly channel). The `ghcr.io/lhjnilsson` prefix is the CLI's `IMAGE_REPOSITORY`, passed to the chart as `global.imageRepository`, so the CLI and the chart cannot drift apart. On a local Kind cluster, `fbenv create` stops before installing anything when one of them can't be pulled; on a remote cluster it cannot promise that, because the remote nodes pull the images after Helm runs, so the release can be created and the pods can then sit in `ImagePullBackOff`. `VERSION=local` uses images built via `task build` / `task build:docker`; any other `VERSION` is refused. `task build` does not create the cluster.

Cluster config files and Helm recipes: [deploy/README.md](../../deploy/README.md).

## Tests

From `client/`:

```bash
uv run pytest cli/
```
