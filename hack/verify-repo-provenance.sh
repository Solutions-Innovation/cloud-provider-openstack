#!/usr/bin/env bash
# Copyright (c) 2024-2026 Wind River Systems, Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Guards the repository's identity files against accidental replacement.
#
# This exists because of a real incident: a commit that intended to add example
# runbooks also copied another project's LICENSE and README.md over this
# repository's, and it survived for five months. Nothing caught it — the code
# still built, every test passed, and the linter was silent, because none of
# those look at provenance files. The Apache 2.0 license was replaced by a
# third party's MIT notice and shipped by the release workflows.
#
# The checks below are deliberately cheap and content-based so they need no
# network access and no remote refs.

set -o errexit
set -o nounset
set -o pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "  ok: $*"; }

expected_title="# Cloud Provider OpenStack"

echo "==> 1. LICENSE is the project's own Apache 2.0"

[[ -f LICENSE ]] || fail "LICENSE is missing"

grep -q "Apache License" LICENSE || \
  fail "LICENSE does not contain 'Apache License'. This repository is Apache 2.0; \
a different license here means the file was replaced."
grep -q "Version 2.0, January 2004" LICENSE || \
  fail "LICENSE is not the Apache 2.0 text (missing the version line)"
pass "LICENSE is Apache 2.0"

# The Apache 2.0 boilerplate carries no personal copyright line of its own; the
# holder is named in each source file instead. A "Copyright (c) <year> <person>"
# line near the top is the signature of a substituted MIT/BSD license.
if head -5 LICENSE | grep -qiE '^copyright \(c\) [0-9]{4}'; then
  fail "LICENSE begins with a personal copyright line ($(head -1 LICENSE)). \
The Apache 2.0 text does not; this looks like another project's license."
fi
pass "LICENSE carries no foreign copyright holder"

# Apache 2.0 is ~200 lines. A drastically shorter file is a permissive license
# swapped in, which the marker checks above could miss if text were appended.
lines=$(wc -l < LICENSE)
(( lines >= 175 )) || fail "LICENSE is only ${lines} lines; the Apache 2.0 text is ~202"
pass "LICENSE length plausible (${lines} lines)"

echo "==> 2. no source file claims a different license"

# Every Go file in this tree declares Apache-2.0. A different SPDX identifier
# means either a file was imported from elsewhere or the project's licensing
# changed without the LICENSE file following.
foreign=$(grep -rl 'SPDX-License-Identifier:' --include='*.go' . 2>/dev/null \
  | xargs -r grep -l 'SPDX-License-Identifier:' 2>/dev/null \
  | xargs -r grep -hoE 'SPDX-License-Identifier: *[A-Za-z0-9.+-]+' 2>/dev/null \
  | sed 's/.*: *//' | sort -u | grep -v '^Apache-2.0$' || true)
if [[ -n "${foreign}" ]]; then
  echo "${foreign}" | sed 's/^/    unexpected SPDX identifier: /' >&2
  fail "Go sources declare a license other than Apache-2.0"
fi
pass "all Go SPDX identifiers are Apache-2.0"

echo "==> 3. README belongs to this project"

[[ -f README.md ]] || fail "README.md is missing"

actual_title=$(head -1 README.md)
[[ "${actual_title}" == "${expected_title}" ]] || \
  fail "README.md starts with '${actual_title}', expected '${expected_title}'. \
A different title means the file was replaced with another project's."
pass "README title is '${expected_title}'"

# A README copied from another repository brings its relative links along, and
# those targets do not exist here. This catches the substitution generically,
# without needing to know which project it came from.
missing=0
while read -r target; do
  # Drop any "#anchor" and any link title after whitespace.
  target=${target%%#*}
  target=${target%% *}
  case "${target}" in
    http://*|https://*|mailto:*|'') continue ;;
  esac
  # This repository's README writes links repo-root-relative with a leading
  # slash, and often with a trailing slash, e.g. "/docs/developers-guide.md/".
  # Normalise both so the target can be resolved on disk.
  target=${target#/}
  target=${target%/}
  [[ -z "${target}" ]] && continue
  if [[ ! -e "${target}" ]]; then
    echo "    broken relative link: ${target}" >&2
    missing=$((missing + 1))
  fi
done < <(grep -oE '\]\([^)]+\)' README.md | sed 's/^](//; s/)$//')

(( missing == 0 )) || fail "README.md has ${missing} relative link(s) pointing at \
files that do not exist. Either the links are wrong or the README came from \
another repository."
pass "all README relative links resolve"

echo
echo "All repository provenance checks passed."
