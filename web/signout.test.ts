import assert from "node:assert/strict";
import test from "node:test";

import { codeSpans, signOutEverywhereCopy } from "./src/signout.ts";

test("sign out everywhere asks before it revokes", () => {
  assert.deepEqual(signOutEverywhereCopy, {
    button: "Sign out everywhere",
    question: "Sign out everywhere?",
    detail:
      "This revokes every browser session and every saved token for your account, including tokens saved with " +
      "`fbull token save` and any Grafana token made from this account. You'll need to sign in again in each " +
    "browser and run `fbull login` again on each computer.",
    confirm: "Sign out everywhere",
    cancel: "Cancel",
  });
});

test("code spans are the backticked pieces", () => {
  assert.deepEqual(codeSpans(signOutEverywhereCopy.detail), [
    {
      text: "This revokes every browser session and every saved token for your account, including tokens saved with ",
      code: false,
    },
    { text: "fbull token save", code: true },
    {
      text:
        " and any Grafana token made from this account. You'll need to sign in again in each browser and run ",
      code: false,
    },
    { text: "fbull login", code: true },
    { text: " again on each computer.", code: false },
  ]);
});
