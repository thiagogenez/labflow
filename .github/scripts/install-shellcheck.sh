#!/usr/bin/env bash
# Install the same checksum-pinned ShellCheck release in local and CI environments.
set -euo pipefail

readonly SHELLCHECK_VERSION="0.11.0"

case "$(uname -s):$(uname -m)" in
  Darwin:arm64)
    shellcheck_platform="darwin.aarch64"
    shellcheck_sha256="339b930feb1ea764467013cc1f72d09cd6b869ebf1013296ba9055ab2ffbd26f"
    ;;
  Darwin:x86_64)
    shellcheck_platform="darwin.x86_64"
    shellcheck_sha256="c2c15e08df0e8fbc374c335b230a7ee958c313fa5714817a59aa59f1aa594f51"
    ;;
  Linux:aarch64|Linux:arm64)
    shellcheck_platform="linux.aarch64"
    shellcheck_sha256="68a8133197a50beb8803f8d42f9908d1af1c5540d4bb05fdfca8c1fa47decefc"
    ;;
  Linux:x86_64)
    shellcheck_platform="linux.x86_64"
    shellcheck_sha256="b7af85e41cc99489dcc21d66c6d5f3685138f06d34651e6d34b42ec6d54fe6f6"
    ;;
  *)
    printf 'Unsupported ShellCheck platform: %s:%s\n' "$(uname -s)" "$(uname -m)" >&2
    exit 1
    ;;
esac

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if [[ ! -d "$project_root/.venv/bin" ]]; then
  printf 'Create the project .venv before installing ShellCheck.\n' >&2
  exit 1
fi

shellcheck_tmp="$(mktemp -d "${TMPDIR:-/tmp}/labflow-shellcheck.XXXXXX")"
trap 'rm -rf "$shellcheck_tmp"' EXIT
archive="$shellcheck_tmp/shellcheck.tar.gz"

curl --fail --silent --show-error --location \
  --proto '=https' --tlsv1.2 --connect-timeout 10 --max-time 60 \
  --output "$archive" \
  "https://github.com/koalaman/shellcheck/releases/download/v${SHELLCHECK_VERSION}/shellcheck-v${SHELLCHECK_VERSION}.${shellcheck_platform}.tar.gz"

if command -v sha256sum >/dev/null 2>&1; then
  actual_sha256="$(sha256sum "$archive")"
else
  actual_sha256="$(shasum -a 256 "$archive")"
fi
actual_sha256="${actual_sha256%% *}"
if [[ "$actual_sha256" != "$shellcheck_sha256" ]]; then
  printf 'ShellCheck archive checksum verification failed.\n' >&2
  exit 1
fi

tar -xzf "$archive" -C "$shellcheck_tmp" "shellcheck-v${SHELLCHECK_VERSION}/shellcheck"
install -m 0755 "$shellcheck_tmp/shellcheck-v${SHELLCHECK_VERSION}/shellcheck" \
  "$project_root/.venv/bin/shellcheck"
"$project_root/.venv/bin/shellcheck" --version
