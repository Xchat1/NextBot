#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

STATE_DIR=""
BINARY="$REPO_ROOT/bin/nextbot"
ENGINE="podman"
IMAGE_REF="docker.io/library/alpine:3.21"
IMAGE_DIGEST=""
ORIGIN="http://127.0.0.1:8787"
DEV_HTTP=1
FORCE=0
SKIP_BUILD=0
NO_PULL=0

usage() {
  cat <<'EOF'
Usage: setup-clean-instance.sh --state-dir DIR [options]

Options:
  --state-dir DIR       Target directory for clean NextBot state, demo repo, and tests.json
  --binary PATH         Path to built nextbot binary (default: <repo>/bin/nextbot)
  --engine ENGINE       Container runtime: podman or docker (default: podman)
  --image REF           Container image to resolve/pull (default: docker.io/library/alpine:3.21)
  --image-digest REF    Explicit digest-pinned image (<image>@sha256:...) to use directly
  --origin URL          Control public origin (default: http://127.0.0.1:8787)
  --https               Disable --dev-http hint in generated instance.env
  --skip-build          Skip building bin/nextbot even if missing
  --no-pull             Do not pull container image; require it already in local storage
  --force               Remove existing --state-dir before initializing
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --state-dir) STATE_DIR="$2"; shift 2 ;;
    --binary) BINARY="$2"; shift 2 ;;
    --engine) ENGINE="$2"; shift 2 ;;
    --image) IMAGE_REF="$2"; shift 2 ;;
    --image-digest) IMAGE_DIGEST="$2"; shift 2 ;;
    --origin) ORIGIN="$2"; shift 2 ;;
    --https) DEV_HTTP=0; shift ;;
    --skip-build) SKIP_BUILD=1; shift ;;
    --no-pull) NO_PULL=1; shift ;;
    --force) FORCE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage; exit 2 ;;
  esac
done

if [[ -z "$STATE_DIR" ]]; then
  echo "--state-dir is required" >&2
  usage
  exit 2
fi

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "Warning: NextBot Control/Runner binaries require Linux (current OS: $(uname -s))." >&2
fi

if [[ -e "$STATE_DIR" ]]; then
  if [[ "$FORCE" -ne 1 ]]; then
    echo "state dir already exists: $STATE_DIR (pass --force to recreate)" >&2
    exit 1
  fi
  rm -rf "$STATE_DIR"
fi

if [[ "$SKIP_BUILD" -ne 1 ]]; then
  if [[ ! -f "$REPO_ROOT/internal/web/dist/index.html" ]]; then
    make -C "$REPO_ROOT" web
  fi
  mkdir -p "$(dirname "$BINARY")"
  (cd "$REPO_ROOT" && go build -trimpath -o "$BINARY" ./cmd/nextbot)
fi

if [[ ! -x "$BINARY" ]]; then
  echo "nextbot binary not found or not executable: $BINARY" >&2
  exit 1
fi

if ! command -v "$ENGINE" >/dev/null 2>&1; then
  echo "container engine not found in PATH: $ENGINE" >&2
  exit 1
fi

if [[ "$ENGINE" == "podman" ]]; then
  ROOTLESS="$("$ENGINE" info --format '{{.Host.Security.Rootless}}' 2>/dev/null || true)"
  if [[ "$ROOTLESS" != "true" ]]; then
    echo "podman must be rootless (got Rootless=$ROOTLESS)" >&2
    exit 1
  fi
elif [[ "$ENGINE" == "docker" ]]; then
  SEC="$("$ENGINE" info --format '{{json .SecurityOptions}}' 2>/dev/null || true)"
  if [[ "$SEC" != *"rootless"* ]]; then
    echo "docker must be rootless (SecurityOptions=$SEC)" >&2
    exit 1
  fi
else
  echo "unsupported container engine: $ENGINE" >&2
  exit 1
fi

if [[ -z "$IMAGE_DIGEST" ]]; then
  if [[ "$NO_PULL" -ne 1 ]]; then
    "$ENGINE" pull "$IMAGE_REF" >/dev/null
  fi
  IMAGE_DIGEST="$("$ENGINE" image inspect "$IMAGE_REF" --format '{{index .RepoDigests 0}}' 2>/dev/null || true)"
  if [[ -z "$IMAGE_DIGEST" || "$IMAGE_DIGEST" != *@sha256:* ]]; then
    echo "failed to resolve digest-pinned RepoDigest for $IMAGE_REF (got: $IMAGE_DIGEST)" >&2
    exit 1
  fi
fi

mkdir -p "$STATE_DIR/control" "$STATE_DIR/runner" "$STATE_DIR/codex-home" "$STATE_DIR/evidence"

DEMO_INFO="$("$REPO_ROOT/fixtures/empty-export-demo/init-demo-repo.sh" \
  --dest "$STATE_DIR/demo-repo" \
  --image-digest "$IMAGE_DIGEST" \
  --tests-out "$STATE_DIR/tests.json" \
  --force)"

BASE_COMMIT="$(printf '%s\n' "$DEMO_INFO" | awk -F= '$1=="BASE_COMMIT"{print $2}')"

PROJECT_JSON="$("$BINARY" project-add \
  --data "$STATE_DIR/control" \
  --name "报告服务" \
  --repo "$STATE_DIR/demo-repo" \
  --tests "$STATE_DIR/tests.json")"

PROJECT_ID="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["projectId"])' <<<"$PROJECT_JSON")"

cat >"$STATE_DIR/instance.env" <<EOF
STATE_DIR=$STATE_DIR
NEXTBOT_BIN=$BINARY
CONTROL_DATA=$STATE_DIR/control
RUNNER_DATA=$STATE_DIR/runner
RUNNER_SOCKET=$STATE_DIR/runner.sock
CODEX_HOME=$STATE_DIR/codex-home
DEMO_REPO=$STATE_DIR/demo-repo
TESTS_FILE=$STATE_DIR/tests.json
CONTAINER_ENGINE=$ENGINE
IMAGE_DIGEST=$IMAGE_DIGEST
PROJECT_ID=$PROJECT_ID
BASE_COMMIT=$BASE_COMMIT
ORIGIN=$ORIGIN
DEV_HTTP=$DEV_HTTP
EOF

echo "$DEMO_INFO"
echo "PROJECT_ID=$PROJECT_ID"
echo "IMAGE_DIGEST=$IMAGE_DIGEST"
echo "INSTANCE_ENV=$STATE_DIR/instance.env"
