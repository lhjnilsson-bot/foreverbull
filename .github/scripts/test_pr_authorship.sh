#!/usr/bin/env bash
# Runs pr_authorship.sh against fixture commit lists and checks each exit
# status. The fixtures follow the shape of the pulls commits API.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
testdata="${here}/testdata/pr_authorship"
bot=lhjnilsson-bot
failures=0

agent='Commits by a coding agent'

# expect STATUS PR_AUTHOR FIXTURE [+TEXT|-TEXT]...
#   +TEXT: the output must contain TEXT. -TEXT: it must not.
expect() {
  local want="$1" author="$2" fixture="$3" got=0 out check
  shift 3
  out="$(
    PR_AUTHOR="${author}" BOT_LOGINS="${bot}" \
      "${here}/pr_authorship.sh" "${testdata}/${fixture}"
  )" || got=$?
  if [[ "${got}" -ne "${want}" ]]; then
    echo "FAIL ${fixture} (PR by ${author}): exit ${got}, want ${want}"
    failures=$((failures + 1))
    return
  fi
  for check in "$@"; do
    case "${check}" in
      +*) grep -qF -- "${check#+}" <<<"${out}" && continue ;;
      -*) grep -qF -- "${check#-}" <<<"${out}" || continue ;;
    esac
    echo "FAIL ${fixture} (PR by ${author}): output check ${check} failed: ${out}"
    failures=$((failures + 1))
    return
  done
  echo "ok   ${fixture} (PR by ${author}): exit ${got}"
}

expect 0 "${bot}" pass_all_by_author.json
expect 0 "${bot}" pass_update_branch_merge.json
expect 1 "${bot}" fail_hand_merge_not_web_flow.json
expect 1 "${bot}" fail_hand_merge_unverified.json
expect 1 "${bot}" fail_hand_merge_github_email_other_login.json
expect 1 "${bot}" fail_hand_merge_unlinked_committer.json
expect 1 "${bot}" fail_hand_merge_null_login_github_email.json
expect 1 "${bot}" fail_hand_merge_web_flow_other_email.json
expect 1 "${bot}" fail_non_merge_other_author.json
expect 0 maintainer warn_human_mismatch.json
expect 0 "${bot}" pass_bot_pr_bot_commits.json "-${agent}"

# Coding-agent commits fail whoever opens the pull request.
expect 1 maintainer fail_owner_pr_cursor_agent_commits.json "+${agent}"
expect 1 "${bot}" fail_owner_pr_cursor_agent_commits.json "+${agent}"
expect 1 maintainer fail_cursor_agent_committer_only.json "+${agent}"
expect 1 "${bot}" fail_cursor_bot_login.json "+${agent}"
expect 1 maintainer fail_cursor_agent_author_email_only.json "+${agent}"
expect 1 maintainer fail_cursor_agent_noreply_email.json "+${agent}"
expect 1 maintainer fail_cursor_agent_noreply_email_no_id.json "+${agent}"
expect 1 maintainer fail_cursor_bot_noreply_email.json "+${agent}"
expect 1 maintainer fail_cursor_agent_name_null_login.json "+${agent}"
expect 1 maintainer fail_cursor_agent_name_lowercase.json "+${agent}"
expect 1 maintainer fail_cursor_agent_name_whitespace.json "+${agent}"
expect 1 maintainer fail_mixed_clean_and_cursor.json "+${agent}"
expect 1 maintainer fail_cursor_agent_escaped_fields.json "+${agent}" \
  "+<50%25%0D%0Asplit@example.com>"

# Identities that are not coding agents.
expect 0 lhjnilsson pass_owner_commits_not_agent.json "-${agent}"
expect 0 lhjnilsson warn_dependabot_not_agent.json "-${agent}" "+::warning"
expect 0 "${bot}" pass_bot_noreply_committer_not_agent.json "-${agent}"
if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} case(s) failed"
  exit 1
fi
