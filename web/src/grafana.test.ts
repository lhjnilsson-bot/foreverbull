import assert from "node:assert/strict";
import { test } from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import {
  executionDashboardUID,
  grafanaExecutionURL,
  grafanaOverviewURL,
  openRunInGrafana,
  openRunInGrafanaLabel,
  overviewBacktestVar,
  overviewDashboardUID,
  rankRunsInGrafana,
  rankRunsInGrafanaLabel,
} from "./grafana.ts";

const executionID = "550e8400-e29b-41d4-a716-446655440000";

test("a run links to the Backtest execution dashboard", () => {
  assert.equal(executionDashboardUID, "ee9ep4jxtwkcgf");
  assert.equal(
    grafanaExecutionURL("http://localhost:3000", executionID),
    `http://localhost:3000/d/ee9ep4jxtwkcgf?var-execution=${executionID}`,
  );
});

test("the execution link keeps a subpath and drops a trailing slash", () => {
  const want = `https://example.com/grafana/d/ee9ep4jxtwkcgf?var-execution=${executionID}`;
  assert.equal(grafanaExecutionURL("https://example.com/grafana", executionID), want);
  assert.equal(grafanaExecutionURL("https://example.com/grafana/", executionID), want);
  assert.equal(grafanaExecutionURL("https://example.com/grafana/./", executionID), want);
});

test("the execution link encodes the id and drops credentials", () => {
  assert.equal(
    grafanaExecutionURL("http://localhost:3000", "id with space"),
    "http://localhost:3000/d/ee9ep4jxtwkcgf?var-execution=id+with+space",
  );
  assert.equal(
    grafanaExecutionURL("http://localhost:3000", "a&b=c"),
    "http://localhost:3000/d/ee9ep4jxtwkcgf?var-execution=a%26b%3Dc",
  );
  const href = grafanaExecutionURL("http://user:hunter2@localhost:3000/grafana?token=secret#frag", executionID);
  assert.equal(href, `http://localhost:3000/grafana/d/ee9ep4jxtwkcgf?var-execution=${executionID}`);
  assert.equal(href?.includes("hunter2"), false);
  assert.equal(href?.includes("secret"), false);
});

test("a missing or unusable Grafana origin hides the run link", () => {
  for (const origin of ["", "   ", "not a url", "ftp://localhost:3000", "http://", "javascript:alert(1)", "/grafana"]) {
    assert.equal(grafanaExecutionURL(origin, executionID), null, origin);
  }
  assert.equal(grafanaExecutionURL("http://localhost:3000", ""), null);
  assert.equal(grafanaExecutionURL("http://localhost:3000", "   "), null);
});

test("a backtest links to the overview filtered by var-backtest", () => {
  assert.equal(overviewDashboardUID, "fb-backtest-overview");
  assert.equal(overviewBacktestVar, "backtest");
  assert.equal(
    grafanaOverviewURL("http://localhost:3000", "nasdaq100"),
    "http://localhost:3000/d/fb-backtest-overview?var-backtest=nasdaq100",
  );
});

test("the overview link encodes the name and keeps a base path", () => {
  assert.equal(
    grafanaOverviewURL("https://example.com/grafana/", "eu small"),
    "https://example.com/grafana/d/fb-backtest-overview?var-backtest=eu+small",
  );
  assert.equal(
    grafanaOverviewURL("http://localhost:3000", "a/b"),
    "http://localhost:3000/d/fb-backtest-overview?var-backtest=a%2Fb",
  );
  assert.equal(
    grafanaOverviewURL("http://localhost:3000", "å"),
    "http://localhost:3000/d/fb-backtest-overview?var-backtest=%C3%A5",
  );
  const href = grafanaOverviewURL("https://ada:s3cret@example.com/fb/?x=1", "spy");
  assert.equal(href, "https://example.com/fb/d/fb-backtest-overview?var-backtest=spy");
  assert.equal(href?.includes("s3cret"), false);
});

function renderedLinks(grafanaURL: string): string {
  const run = openRunInGrafana(grafanaURL, executionID);
  const rank = rankRunsInGrafana(grafanaURL, "nasdaq100");
  return renderToStaticMarkup(
    createElement(
      "div",
      null,
      run ? createElement("a", { href: run.href }, run.label) : null,
      rank ? createElement("a", { href: rank.href }, rank.label) : null,
    ),
  );
}

test("the run and backtest links use the Grafana labels", () => {
  assert.equal(openRunInGrafanaLabel, "Open this run in Grafana.");
  assert.equal(rankRunsInGrafanaLabel, "Rank these runs in Grafana.");
  const html = renderedLinks("http://localhost:3000");
  assert.match(html, /Open this run in Grafana\./);
  assert.match(html, /Rank these runs in Grafana\./);
  assert.match(html, new RegExp(`href="http://localhost:3000/d/ee9ep4jxtwkcgf\\?var-execution=${executionID}"`));
  assert.match(html, /href="http:\/\/localhost:3000\/d\/fb-backtest-overview\?var-backtest=nasdaq100"/);
});

test("neither Grafana link renders when grafanaUrl is empty", () => {
  assert.equal(openRunInGrafana("", executionID), null);
  assert.equal(rankRunsInGrafana("", "nasdaq100"), null);
  assert.equal(renderedLinks(""), "<div></div>");
  assert.equal(renderedLinks("not a url"), "<div></div>");
});

test("a missing backtest name or Grafana origin hides the overview link", () => {
  assert.equal(grafanaOverviewURL("http://localhost:3000", ""), null);
  assert.equal(grafanaOverviewURL("http://localhost:3000", "  "), null);
  assert.equal(grafanaOverviewURL("", "nasdaq100"), null);
  assert.equal(grafanaOverviewURL("http://", "nasdaq100"), null);
  assert.equal(grafanaOverviewURL("not a url", "nasdaq100"), null);
});
