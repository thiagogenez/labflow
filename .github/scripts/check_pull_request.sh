#!/usr/bin/env bash
# Check a pull request's title, description and the issues that it and its commits reference.
# Environment: PR_TITLE, PR_NUMBER, PR_BODY, BASE_SHA, HEAD_SHA, and GH_TOKEN for the GitHub API.
set -euo pipefail

repo='thiagogenez/labflow-platform'
ref_pattern="$repo#[1-9][0-9]*"
max_refs=25
body=${PR_BODY//$'\r'/}
failed=0
lookup_error=$(mktemp)
trap 'rm -f "$lookup_error"' EXIT

fail() {
  printf '::error::%s\n' "$1"
  failed=1
}

# Prints OPEN, CLOSED or A_PULL_REQUEST; exits non-zero with gh's message when the lookup fails.
# REST is used because GitHub documents reading public issues there without extra permissions.
issue_state() {
  gh api "repos/$repo/issues/${1##*#}" \
    --jq 'if .pull_request then "A_PULL_REQUEST" else (.state | ascii_upcase) end'
}

# The title becomes the merge commit subject "<title> (#<number>)". Types and scope characters
# follow conventional-pre-commit v4.5.0 (conventional_pre_commit/format.py).
max_title=$((72 - 4 - ${#PR_NUMBER}))
if ! grep -qiE '^(build|chore|ci|docs|feat|fix|perf|refactor|revert|style|test)(\([[:alnum:]_ :,./#-]+\))?!?: .' <<<"$PR_TITLE"; then
  fail 'Title is not a Conventional Commits subject.'
elif ((${#PR_TITLE} > max_title)); then
  fail "Title is longer than $max_title characters, the room left before ' (#$PR_NUMBER)'."
fi

for section in What Verification Limitations Review Checklist; do
  grep -qx "## $section" <<<"$body" || fail "Description is missing the '## $section' section."
done

# Mirrors the no-attribution-lines hook in .pre-commit-config.yaml.
if grep -qiE '^(co-authored-by:|[^[:alnum:]]*generated (with|by) .{0,40}\b(claude|copilot|chatgpt|codex|cursor|gemini)\b)' <<<"$body"; then
  fail 'Description contains an attribution line.'
fi

# References are matched case-insensitively, as GitHub does, in the title (the merge commit
# subject), the description and the commits.
messages=$(git log --format=%B "$BASE_SHA..$HEAD_SHA")
refs_text=$(tr '[:upper:]' '[:lower:]' <<<"$PR_TITLE"$'\n'"$body"$'\n'"$messages")

# GitHub closes the issue named after any of these keywords when the pull request merges.
keyword='\b(close[sd]?|fix(e[sd])?|resolve[sd]?):? +'
closes_refs=$(grep -oE "$keyword$ref_pattern" <<<"$refs_text" | grep -oE "$ref_pattern" || true)
if grep -qE "$keyword#[0-9]" <<<"$refs_text"; then
  fail "Write '<keyword> $repo#<number>'; a short #<number> is ambiguous across the two repositories."
fi
grep -qiE "$ref_pattern" <<<"$body" || fail "Description names no $repo issue."

refs=$(grep -oE "$ref_pattern" <<<"$refs_text" | sort -u || true)
ref_count=$(grep -c . <<<"$refs" || true)
if ((ref_count > max_refs)); then
  fail "Found $ref_count distinct issue references; split the pull request to stay within $max_refs."
  refs=''
fi

# Every reference must be an existing issue; a closing keyword also needs it open.
while read -r ref; do
  [[ -n $ref ]] || continue
  if ! state=$(issue_state "$ref" 2>"$lookup_error"); then fail "Could not look up $ref: $(tail -n 1 "$lookup_error")"; continue; fi
  if [[ $state == A_PULL_REQUEST ]]; then
    fail "$ref is a pull request; reference an issue."
  elif [[ $state != OPEN ]] && grep -qxF "$ref" <<<"$closes_refs"; then
    fail "$ref is $state; a closing keyword needs an open issue."
  fi
done <<<"$refs"

exit "$failed"
