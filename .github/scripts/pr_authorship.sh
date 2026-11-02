#!/usr/bin/env bash
# Compares a pull request's author with the authors of its commits.
#
# Usage: pr_authorship.sh COMMITS_JSON
#   COMMITS_JSON is the output of `gh api --paginate
#   repos/OWNER/REPO/pulls/N/commits`: one JSON array per page.
#   PR_AUTHOR is the pull request author's login.
#   BOT_LOGINS is a space-separated list of bot logins.
#
# A commit whose author or committer is a coding-agent identity (see
# automation_* below) always fails, whoever opened the pull request: agent
# work must be pushed from the bot's fork with bot authorship. Otherwise,
# mixed authorship is a warning; it fails only when a bot account is on one
# side of the mismatch.
set -euo pipefail

commits_json="${1:?usage: pr_authorship.sh COMMITS_JSON}"
: "${PR_AUTHOR:?PR_AUTHOR is required}"
: "${BOT_LOGINS:?BOT_LOGINS is required}"

# Values written into a workflow command are escaped as GitHub requires.
# shellcheck disable=SC2016 # jq source, not shell.
jq_escape='def esc: gsub("%"; "%25") | gsub("\r"; "%0D") | gsub("\n"; "%0A");'
escape() {
  local s="$1"
  s="${s//%/%25}"
  s="${s//$'\r'/%0D}"
  s="${s//$'\n'/%0A}"
  printf '%s' "${s}"
}

# Coding-agent identities. Cursor Agent commits carry the git identity
# "Cursor Agent <cursoragent@cursor.com>", which GitHub links to the
# cursoragent account; cursor[bot] is the Cursor GitHub App. Emails and
# logins are compared lowercased. Names are lowercased, trimmed and have
# internal whitespace collapsed first. GitHub noreply addresses of both
# accounts, with or without the numeric id prefix, also match.
automation_emails='["cursoragent@cursor.com"]'
automation_email_regex='^([0-9]+\+)?(cursoragent|cursor\[bot\])@users\.noreply\.github\.com$'
automation_names='["cursor agent"]'
automation_logins='["cursoragent", "cursor[bot]"]'

# Every commit is checked here, including "Update branch" merges.
# shellcheck disable=SC2016 # $c, $emails, $names and $logins are jq variables.
automation_filter="${jq_escape}"'
  def norm_name: ascii_downcase | gsub("\\s+"; " ") | ltrimstr(" ") | rtrimstr(" ");
  .[]
  | . as $c
  | [
      ($c.commit.author.email // "" | ascii_downcase),
      ($c.commit.committer.email // "" | ascii_downcase)
    ] as $e
  | [
      ($c.commit.author.name // "" | norm_name),
      ($c.commit.committer.name // "" | norm_name)
    ] as $n
  | [
      ($c.author.login // "" | ascii_downcase),
      ($c.committer.login // "" | ascii_downcase)
    ] as $l
  | select(
      any($e[]; IN($emails[]) or test($email_regex))
      or any($n[]; IN($names[]))
      or any($l[]; IN($logins[]))
    )
  | "\($c.sha // "?" | .[0:12]) (author \($c.commit.author.name // "?" | esc) <\($c.commit.author.email // "?" | esc)>, committer \($c.commit.committer.name // "?" | esc) <\($c.commit.committer.email // "?" | esc)>)"
'
agent_commits="$(
  jq -r \
    --argjson emails "${automation_emails}" \
    --arg email_regex "${automation_email_regex}" \
    --argjson names "${automation_names}" \
    --argjson logins "${automation_logins}" \
    "${automation_filter}" "${commits_json}"
)"
if [[ -n "${agent_commits}" ]]; then
  agent_list="$(paste -sd ';' <<<"${agent_commits}" | sed 's/;/; /g')"
  echo "::error title=Pull request authorship::Commits by a coding agent: ${agent_list}. Agent work must be pushed from the lhjnilsson-bot fork with bot authorship, whoever opens the pull request."
  exit 1
fi

# A merge commit GitHub made for "Update branch" is authored by whoever
# clicked, so it is skipped. Only a verified merge committed by web-flow
# qualifies: a hand-made merge can carry arbitrary changes (an evil merge)
# and is checked like any other commit. A missing committer login does not
# count as web-flow.
# Commits with no linked GitHub account show their git author name.
# shellcheck disable=SC2016 # $c is a jq variable.
filter="${jq_escape}"'
  .[]
  | . as $c
  | if (($c.parents | length) > 1)
       and ($c.commit.committer.email == "noreply@github.com")
       and ($c.committer.login == "web-flow")
       and ($c.commit.verification.verified == true)
    then empty
    else ($c.author.login // ("git:" + ($c.commit.author.name // "?" | esc)))
    end
'
authors="$(jq -r "${filter}" "${commits_json}" | sort -u)"

others="$(grep -vxF "${PR_AUTHOR}" <<<"${authors}" || true)"
if [[ -z "${others}" ]]; then
  echo "All commits are authored by ${PR_AUTHOR}."
  exit 0
fi
others_list="$(paste -sd ',' <<<"${others}" | sed 's/,/, /g')"
message="Pull request author $(escape "${PR_AUTHOR}") differs from commit author(s): ${others_list}."
bot_involved=false
for bot in ${BOT_LOGINS}; do
  if [[ "${PR_AUTHOR}" == "${bot}" ]] || grep -qxF "${bot}" <<<"${others}"; then
    bot_involved=true
  fi
done
if [[ "${bot_involved}" == true ]]; then
  echo "::error title=Pull request authorship::${message} A bot must open the pull requests for its own commits, from its own account."
  exit 1
fi
echo "::warning title=Pull request authorship::${message}"
