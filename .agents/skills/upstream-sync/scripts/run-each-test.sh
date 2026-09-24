#!/bin/sh
# Runs every top-level test in one Go package as its own process, so a hang
# or a load-sensitive timeout is pinned to a single test name.
#
# usage: run-each-test.sh <package dir> [parallelism]
# Prints "FAIL <TestName>" per failure; full output lands in $OUT/<TestName>.
set -eu
dir=$(cd "$1" && pwd)
par=${2:-8}
tag=$(basename "$dir")
bin=${TMPDIR:-/tmp}/run-each-$tag.test
OUT=${TMPDIR:-/tmp}/run-each-$tag
rm -rf "$OUT" && mkdir -p "$OUT"
(cd "$dir" && go test -race -c -o "$bin" .)
export dir bin OUT
"$bin" -test.list '.*' | grep '^Test' |
	xargs -P"$par" -n1 sh -c 'cd "$dir" && "$bin" -test.run "^$1\$" -test.v -test.timeout 90s >"$OUT/$1" 2>&1 || echo "FAIL $1"' sh |
	sort
echo "outputs: $OUT"
