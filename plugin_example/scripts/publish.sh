#!/usr/bin/env bash
# Sign the built package and pack it into a distributable tarball.
#
# Usage: plugin_example/scripts/publish.sh <private-key file> [agent binary]
#
# The private key comes from `agent plugins keygen --key-id <id> --private-key
# <file>`; its public half goes into the deployment's keyring.json. Signing is
# separate from building because the two have different owners: anyone can
# build, only a key holder can vouch.
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <private-key file> [agent binary]" >&2
  exit 2
fi

key=$1
agent=${2:-agent}

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/.." && pwd)

if [[ ! -f "$root/package/plugin.wasm" ]]; then
  echo "package/plugin.wasm is missing; run plugin_example/scripts/build.sh first" >&2
  exit 1
fi

echo "==> signing package/plugin.json"
# Signs plugin.json's RAW BYTES. Editing plugin.json after this — even one
# space — invalidates the signature.
"$agent" plugins sign "$root/package" --private-key "$key"

mkdir -p "$root/dist"
echo "==> packing dist/legion-hello.tar.gz"
# Exactly three files, flat, no directories: Unpack refuses a fourth entry and
# refuses any path component.
(cd "$root/package" && tar -czf ../dist/legion-hello.tar.gz plugin.json plugin.wasm plugin.sig)

digest=$(sha256sum "$root/dist/legion-hello.tar.gz" | cut -d' ' -f1)
echo
echo "tarball sha256 = $digest"
echo "(this is the TARBALL digest for --digest; plugin.json carries the WASM digest — different values)"
echo
echo "publish dist/legion-hello.tar.gz, then on the deployment:"
echo "  $agent plugins install <url> --digest sha256:$digest --config agent.json"
echo "  $agent plugins grant legion-hello --capabilities log --config agent.json"
