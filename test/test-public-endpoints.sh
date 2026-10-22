#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
checker="$repo_root/scripts/check-public-endpoints.sh"
fixtures=$(mktemp -d)
trap 'rm -rf "$fixtures"' EXIT
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR

run_case() {
  local name="$1" expected="$2" path="$3" content="$4"
  local fixture="$fixtures/$name" output status report_path="${5:-$path}"
  mkdir -p "$fixture"
  git -C "$fixture" init -q --initial-branch=main --template=""
  mkdir -p "$fixture/$(dirname "$path")"
  printf '%s\n' "$content" > "$fixture/$path"
  git -C "$fixture" add --force -- "$path"
  status=0
  output=$(cd "$fixture" && bash "$checker" 2>&1) || status=$?
  if [ "$status" -ne "$expected" ]; then
    printf '%s: expected status %s, got %s\n%s\n' "$name" "$expected" "$status" "$output" >&2
    exit 1
  fi
  if [ "$expected" -eq 1 ] && [[ "$output" != *"$report_path"* ]]; then
    printf '%s: violation report must identify the file\n' "$name" >&2
    exit 1
  fi
  printf 'PASS %s\n' "$name"
}

# Bug caught: missing an internal endpoint family. Dropping either suffix from the checker makes that case pass incorrectly.
app_suffix=delphoslabs".app"
internal_suffix=delphos".internal"
run_case application-host 1 server.go "var endpoint = \"https://registry.$app_suffix/token\""
run_case internal-host 1 config.yaml "url: https://archive.$internal_suffix:8080"
run_case apex-internal-host 1 config.yaml "url: https://$internal_suffix:8080/index"
run_case apex-application-host 1 server.go "var endpoint = \"https://$app_suffix/token\""

# Bug caught: tracking private working artifacts. Removing either path check admits these files even without endpoint text.
run_case private-evidence 1 .local/evidence.json '{}'
run_case private-plan 1 .do-work/plan.md 'private plan'

# Bug caught: rejecting legitimate public attribution and ignore-rule descriptions. An overbroad company or dot-path ban breaks this case.
run_case public-references 0 README.md 'https://github.com/Delphos-Labs/roxxy
security@delphoslabs.com
Ignore .local/ and .do-work/ working artifacts.
Example registry: https://registry.example.com'

# Bug caught: disclosing the rejected endpoint through CI logs. Replacing filename-only reporting with matched-line output fails this assertion.
fixture="$fixtures/application-host"
output=$(cd "$fixture" && bash "$checker" 2>&1) || status=$?
if [ "${status:-0}" -ne 1 ] || [[ "$output" == *"registry.$app_suffix"* ]]; then
  printf 'violation report must fail without printing the internal hostname\n' >&2
  exit 1
fi
printf 'PASS redacted-report\n'

# Bug caught: disclosing an endpoint embedded in a filename. Reporting the original filename without redaction leaks it.
run_case endpoint-in-path 1 "docs/registry.$app_suffix.md" "https://registry.$app_suffix" 'docs/'
output=$(cd "$fixtures/endpoint-in-path" && bash "$checker" 2>&1) || status=$?
if [[ "$output" == *"registry.$app_suffix"* ]]; then
  printf 'violation report must redact the endpoint in the filename\n' >&2
  exit 1
fi
printf 'PASS redacted-filename\n'

run_case apex-in-path 1 "docs/$internal_suffix.md" "https://$internal_suffix" 'docs/'
output=$(cd "$fixtures/apex-in-path" && bash "$checker" 2>&1) || status=$?
if [[ "$output" == *"$internal_suffix"* ]]; then
  printf 'violation report must redact the apex endpoint in the filename\n' >&2
  exit 1
fi
printf 'PASS redacted-apex-filename\n'

# Bug caught: silently treating inspection errors as a clean tree. Swallowing Git errors makes this return success.
mkdir -p "$fixtures/no-repo"
status=0
output=$(cd "$fixtures/no-repo" && GIT_CEILING_DIRECTORIES="$fixtures" bash "$checker" 2>&1) || status=$?
if [ "$status" -eq 0 ]; then
  printf 'inspection outside a Git repository must fail\n' >&2
  exit 1
fi
printf 'PASS inspection-error\n'
