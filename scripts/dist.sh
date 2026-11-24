#!/bin/sh
# dist.sh VERSION — the release archives in dist/: taskmoor for macOS and
# Linux (arm64, amd64) with the version set, LICENSE and README.md beside
# it, and checksums.txt. The release workflow runs it on a tag.
set -eu
version=${1:?usage: scripts/dist.sh VERSION}
rm -rf dist
mkdir -p dist
for target in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
	os=${target%/*}
	arch=${target#*/}
	name="taskmoor_${version}_${os}_${arch}"
	mkdir -p "dist/$name"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$version" \
		-o "dist/$name/taskmoor" ./cmd/taskmoor
	cp LICENSE README.md "dist/$name/"
	tar -C dist -czf "dist/$name.tar.gz" "$name"
	rm -rf "dist/$name"
done
cd dist
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum ./*.tar.gz >checksums.txt
else
	shasum -a 256 ./*.tar.gz >checksums.txt
fi
cat checksums.txt
