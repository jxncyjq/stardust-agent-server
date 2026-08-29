#!/usr/bin/env bash
# Build the example guest and render package/plugin.json against it.
#
# The rendering is the point: plugin.json carries plugin.wasm's sha256, and the
# host refuses to load a package whose declared digest does not match the bytes
# beside it. Editing the wasm without re-running this script produces a package
# that fails at load time with "sha256 mismatch".
#
# Usage: plugin_example/scripts/build.sh
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/.." && pwd)

echo "==> building guest (wasm32-wasip1)"
(cd "$root/guest" && cargo build --release --target wasm32-wasip1 --offline)

cp "$root/guest/target/wasm32-wasip1/release/legion_hello.wasm" "$root/package/plugin.wasm"

digest=$(sha256sum "$root/package/plugin.wasm" | cut -d' ' -f1)
echo "==> plugin.wasm sha256 = $digest"

cat > "$root/package/plugin.json" <<EOF
{
  "name": "legion-hello",
  "version": "0.1.0",
  "abi": 1,
  "sha256": "$digest",
  "capabilities": ["log"],
  "extensions": ["observe"],
  "limits": {
    "timeout_ms": 5000,
    "max_memory_pages": 64,
    "max_instances": 2
  },
  "tools": [
    {
      "name": "hello_echo",
      "description": "Greet someone by name, from inside a WASM plugin",
      "group": "example",
      "risk_level": "low",
      "input_schema": {
        "type": "object",
        "properties": {
          "name": { "type": "string", "description": "who to greet" }
        },
        "required": ["name"]
      },
      "timeout_ms": 3000,
      "sensitive": false
    }
  ]
}
EOF

echo "==> wrote $root/package/plugin.json"
echo "next: plugin_example/scripts/publish.sh <private-key> (sign + package)"
