#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Scans files (tracked files in a git checkout, otherwise the working tree)
# and commit messages for credentials, private keys, kubeconfig material,
# personal e-mail addresses, absolute home-directory paths and references to
# AI coding assistants. Usage: hack/check-secrets.sh [dir]   (default: repo root)
set -euo pipefail
dir="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
cd "${dir}"
self="hack/check-secrets.sh"

if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  files="$(git ls-files -co --exclude-standard | grep -v -E '^(\.work/|bin/|dist/)' || true)"
else
  files="$(find . -type f -not -path './.git/*' -not -path './.work/*' -not -path './bin/*' | sed 's|^\./||')"
fi

fail=0
check() {
  local label="$1" pattern="$2" hits
  hits="$(printf '%s\n' "${files}" | grep -v -x -e "${self}" -e 'scripts/check-secrets.sh' \
    | while read -r f; do [[ -f "$f" ]] && ! file -b --mime "$f" | grep -q 'charset=binary' && printf '%s\0' "$f"; done \
    | xargs -0 grep -n -I -E -i "${pattern}" 2>/dev/null || true)"
  if [[ -n "${hits}" ]]; then
    printf 'FAIL %s\n%s\n' "${label}" "$(head -20 <<<"${hits}")"
    fail=1
  fi
}

check "private key"             '-----BEGIN ([A-Z]+ )?PRIVATE KEY-----'
check "kubeconfig credential"   '(client-key-data|client-certificate-data|certificate-authority-data):[[:space:]]*[A-Za-z0-9+/=]{20,}'
check "GitHub token"            '(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}'
check "cloud access key"        'AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35}'
check "API key"                 'sk-[A-Za-z0-9]{32,}|xox[baprs]-[A-Za-z0-9-]{10,}'
check "authorization header"    'authorization:[[:space:]]*(bearer|basic)[[:space:]]+[A-Za-z0-9._~+/=-]{16,}'
check "password assignment"     '(password|passwd|secret_key)[[:space:]]*[:=][[:space:]]*["'"'"'][^"'"'"'$[:space:]]{6,}'
check "absolute home path"      '/Users/[a-z0-9._-]+/|/home/[a-z0-9._-]+/'
check "e-mail address"          '[A-Za-z0-9._%+-]+@([A-Za-z0-9-]+\.)+(com|net|org|io|in|dev|edu|co|ai)\b'
check "AI assistant reference"  '\b(claude|anthropic|openai|codex|copilot|antigravity|chatgpt|gemini)\b|generated (by|with) (an? )?ai|ai-assisted'

if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  msgs="$(git log --format='%H %an <%ae>%n%B' 2>/dev/null || true)"
  if grep -n -i -E 'co-authored-by|\b(claude|anthropic|openai|codex|copilot|antigravity)\b' <<<"${msgs}" >/dev/null; then
    echo "FAIL commit messages mention a co-author or an AI assistant"; fail=1
  fi
  if git log --format='%ae' | grep -v -x -E '[0-9]+\+[A-Za-z0-9-]+@users\.noreply\.github\.com|noreply@github\.com' | grep -q .; then
    echo "FAIL a commit uses a non-noreply author e-mail"; fail=1
  fi
fi

if [[ "${fail}" == 0 ]]; then
  echo "secret and personal-data scan: clean ($(wc -l <<<"${files}" | tr -d ' ') files)"
fi
exit "${fail}"
