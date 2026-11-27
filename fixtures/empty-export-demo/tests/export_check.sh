#!/bin/sh
set -eu

ROOT="${NEXTBOT_WORKSPACE:-/workspace}"
OUT_DIR="/output"
if [ ! -d "$OUT_DIR" ] || [ ! -w "$OUT_DIR" ]; then
  OUT_DIR=$(mktemp -d)
  trap 'rm -rf "$OUT_DIR"' EXIT
fi

echo "[export-check] observed_run=true test_id=export-check pid=$$ cwd=$(pwd)"
if [ "$ROOT" = "/workspace" ]; then
  if touch /workspace/.readonly_probe 2>/dev/null; then
    rm -f /workspace/.readonly_probe
    echo "[export-check] ERROR: /workspace is unexpectedly writable" >&2
    exit 98
  fi
  echo "[export-check] workspace_readonly=true"
fi

/bin/sh "$ROOT/src/export_report.sh" "$ROOT/samples/normal-input.csv" "$OUT_DIR/normal.csv"
diff -u "$ROOT/samples/expected-normal.csv" "$OUT_DIR/normal.csv"
echo "[export-check] normal_export=pass"

/bin/sh "$ROOT/src/export_report.sh" "$ROOT/samples/empty-input.csv" "$OUT_DIR/empty.csv"
diff -u "$ROOT/samples/expected-empty.csv" "$OUT_DIR/empty.csv"
echo "[export-check] empty_header_export=pass"

/bin/sh "$ROOT/src/export_report.sh" "$ROOT/samples/blank-input.csv" "$OUT_DIR/blank.csv"
diff -u "$ROOT/samples/expected-empty.csv" "$OUT_DIR/blank.csv"
echo "[export-check] blank_input_export=pass"

for extra in "$ROOT"/tests/test_*.sh "$ROOT"/tests/*_regression.sh; do
  if [ -f "$extra" ]; then
    echo "[export-check] extra_regression=$(basename "$extra")"
    /bin/sh "$extra" "$ROOT" "$OUT_DIR"
  fi
done

echo "[export-check] all_checks=pass"
