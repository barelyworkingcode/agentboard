#!/bin/sh
# Builds release binaries into dist/ and fails if any is 20 MiB or larger.
set -eu

cd "$(dirname "$0")"

limit=$((20 * 1024 * 1024))
targets="darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64"

mkdir -p dist

status=0
for target in $targets; do
	os=${target%/*}
	arch=${target#*/}
	out="dist/agentboard-$os-$arch"
	[ "$os" = windows ] && out="$out.exe"

	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "$out" .

	size=$(wc -c <"$out" | tr -d ' ')
	mib=$(awk "BEGIN { printf \"%.1f\", $size / 1048576 }")
	if [ "$size" -ge "$limit" ]; then
		echo "$out  $mib MiB  (limit is 20 MiB)" >&2
		status=1
	else
		echo "$out  $mib MiB"
	fi
done

exit $status
