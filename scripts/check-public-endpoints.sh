#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
endpoint_pattern='([[:alnum:]][[:alnum:]._-]*\.)?(delphos\.internal|delphoslabs\.app)'
private_files=$(git -C "$repo_root" ls-files -- .local/ .do-work/)
if [ -n "$private_files" ]; then
  printf 'Private working artifacts must not be tracked:\n' >&2
  printf '%s\n' "$private_files" | sed -E "s/$endpoint_pattern/<internal-endpoint>/gI" >&2
  exit 1
fi

if matches=$(git -C "$repo_root" grep --no-color -I -l -i -E "$endpoint_pattern" --); then
  printf 'Internal endpoint references found in tracked text:\n' >&2
  printf '%s\n' "$matches" | sed -E "s/$endpoint_pattern/<internal-endpoint>/gI" >&2
  exit 1
else
  status=$?
  if [ "$status" -ne 1 ]; then
    exit "$status"
  fi
fi
