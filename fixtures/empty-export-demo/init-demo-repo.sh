#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEST=""
IMAGE_DIGEST="docker.io/library/alpine@sha256:0000000000000000000000000000000000000000000000000000000000000000"
TESTS_OUT=""
FORCE=0

usage() {
  cat <<'EOF'
Usage: init-demo-repo.sh --dest DIR [--image-digest IMAGE@sha256:...] [--tests-out FILE] [--force]
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dest) DEST="$2"; shift 2 ;;
    --image-digest) IMAGE_DIGEST="$2"; shift 2 ;;
    --tests-out) TESTS_OUT="$2"; shift 2 ;;
    --force) FORCE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage; exit 2 ;;
  esac
done

if [[ -z "$DEST" ]]; then
  echo "--dest is required" >&2
  usage
  exit 2
fi

if [[ -e "$DEST" ]]; then
  if [[ "$FORCE" -ne 1 ]]; then
    echo "target already exists: $DEST (pass --force to replace)" >&2
    exit 1
  fi
  rm -rf "$DEST"
fi

mkdir -p "$DEST/src" "$DEST/samples" "$DEST/tests"
cp "$SCRIPT_DIR/src/export_report.sh" "$DEST/src/export_report.sh"
cp "$SCRIPT_DIR/samples/"*.csv "$DEST/samples/"
cp "$SCRIPT_DIR/tests/export_check.sh" "$DEST/tests/export_check.sh"
chmod +x "$DEST/src/export_report.sh" "$DEST/tests/export_check.sh"

cat >"$DEST/README.md" <<'EOF'
# Report Export Service (Demo Fixture)

Exports normalized CSV reports (`id,region,amount`) from input CSV files.
Known defect at baseline: empty datasets (`samples/empty-input.csv`, `samples/blank-input.csv`) fail with exit code `1`.
EOF

git -C "$DEST" init -q -b main
git -C "$DEST" config user.name "NextBot Fixture"
git -C "$DEST" config user.email "fixture@nextbot.local"
git -C "$DEST" add src samples tests README.md
GIT_AUTHOR_DATE="2026-11-27T00:00:00Z" \
GIT_COMMITTER_DATE="2026-11-27T00:00:00Z" \
  git -C "$DEST" commit -q -m "feat: add CSV report exporter baseline with empty-data defect"

BASE_COMMIT="$(git -C "$DEST" rev-parse HEAD)"

# Verify baseline behavior: normal input passes, empty input reproduces defect.
TMP_OUT="$(mktemp -d)"
trap 'rm -rf "$TMP_OUT"' EXIT
/bin/sh "$DEST/src/export_report.sh" "$DEST/samples/normal-input.csv" "$TMP_OUT/normal.csv"
diff -u "$DEST/samples/expected-normal.csv" "$TMP_OUT/normal.csv"
if /bin/sh "$DEST/src/export_report.sh" "$DEST/samples/empty-input.csv" "$TMP_OUT/empty.csv" >/dev/null 2>&1; then
  echo "expected baseline defect on empty-input.csv, but exporter exited 0" >&2
  exit 1
fi

if [[ -n "$TESTS_OUT" ]]; then
  mkdir -p "$(dirname "$TESTS_OUT")"
  python3 - "$IMAGE_DIGEST" "$TESTS_OUT" <<'PY'
import json, sys
digest, out = sys.argv[1], sys.argv[2]
data = [{
    "testId": "export-check",
    "label": "空数据导出回归验证",
    "image": digest,
    "args": ["/bin/sh", "/workspace/tests/export_check.sh"],
    "timeoutSeconds": 60,
}]
with open(out, "w", encoding="utf-8") as f:
    json.dump(data, f, ensure_ascii=False, indent=2)
    f.write("\n")
PY
fi

echo "DEMO_REPO=$DEST"
echo "BASE_COMMIT=$BASE_COMMIT"
if [[ -n "$TESTS_OUT" ]]; then
  echo "TESTS_FILE=$TESTS_OUT"
fi
