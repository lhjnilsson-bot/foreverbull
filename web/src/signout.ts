// The account page's Sign out everywhere control. Backticks mark code.
export const signOutEverywhereCopy = {
  button: "Sign out everywhere",
  question: "Sign out everywhere?",
  detail:
    "This revokes every browser session and every saved token for your account, including tokens saved with " +
    "`fbull token save` and any Grafana token made from this account. You'll need to sign in again in each " +
    "browser and run `fbull login` again on each computer.",
  confirm: "Sign out everywhere",
  cancel: "Cancel",
} as const;

// codeSpans splits copy on backticks: odd pieces are code.
export function codeSpans(text: string): { text: string; code: boolean }[] {
  return text
    .split("`")
    .map((piece, i) => ({ text: piece, code: i % 2 === 1 }))
    .filter((piece) => piece.text !== "");
}
