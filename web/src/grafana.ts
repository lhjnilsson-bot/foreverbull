// Grafana deep links from the Web UI.
//
// Backtest execution is dashboard UID ee9ep4jxtwkcgf. The query key is
// var-execution. Backtest overview is UID fb-backtest-overview. Its filter
// variable is backtest, so the query key is var-backtest. Neither URL uses a
// slug. GRAFANA_URL is the origin, including a subpath when Grafana is not at /.

export const executionDashboardUID = "ee9ep4jxtwkcgf";
export const overviewDashboardUID = "fb-backtest-overview";
export const overviewBacktestVar = "backtest";

export const openRunInGrafanaLabel = "Open this run in Grafana.";
export const rankRunsInGrafanaLabel = "Rank these runs in Grafana.";

export function openRunInGrafana(
  grafanaURL: string,
  executionID: string,
): { href: string; label: string } | null {
  const href = grafanaExecutionURL(grafanaURL, executionID);
  if (href === null) return null;
  return { href, label: openRunInGrafanaLabel };
}

export function rankRunsInGrafana(
  grafanaURL: string,
  backtestName: string,
): { href: string; label: string } | null {
  const href = grafanaOverviewURL(grafanaURL, backtestName);
  if (href === null) return null;
  return { href, label: rankRunsInGrafanaLabel };
}

// grafanaExecutionURL is the Backtest execution dashboard for one run.
// An unusable origin or an empty id returns null so the page shows no link.
export function grafanaExecutionURL(grafanaURL: string, executionID: string): string | null {
  return dashboardURL(grafanaURL, executionDashboardUID, "var-execution", executionID);
}

// grafanaOverviewURL is the Backtest overview dashboard filtered to one backtest.
export function grafanaOverviewURL(grafanaURL: string, backtestName: string): string | null {
  return dashboardURL(grafanaURL, overviewDashboardUID, `var-${overviewBacktestVar}`, backtestName);
}

function dashboardURL(grafanaURL: string, uid: string, queryKey: string, queryValue: string): string | null {
  const origin = publicOrigin(grafanaURL);
  const value = queryValue.trim();
  if (origin === null || value === "") return null;
  const query = new URLSearchParams();
  query.set(queryKey, value);
  return `${origin}/d/${uid}?${query.toString()}`;
}

// publicOrigin is scheme, host, and path. Userinfo, query, and fragment are
// dropped so a credential in the setting cannot ride along on the link.
function publicOrigin(raw: string): string | null {
  const trimmed = raw.trim();
  if (trimmed === "" || /[\u0000-\u0020]/.test(trimmed)) return null;
  let parsed: URL;
  try {
    parsed = new URL(trimmed);
  } catch {
    return null;
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") return null;
  if (parsed.hostname === "") return null;
  parsed.username = "";
  parsed.password = "";
  const path = cleanPath(parsed.pathname);
  return `${parsed.protocol}//${parsed.host}${path}`;
}

function cleanPath(pathname: string): string {
  const parts: string[] = [];
  for (const part of pathname.split("/")) {
    if (part === "" || part === ".") continue;
    if (part === "..") {
      parts.pop();
      continue;
    }
    parts.push(part);
  }
  if (parts.length === 0) return "";
  return `/${parts.join("/")}`;
}
