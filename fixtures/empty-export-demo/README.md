# Empty Export Demo Fixture (`修复空数据导出失败并补回归测试`)

This fixture provides the shared P1 demonstration repository for NextBot:

- `src/export_report.sh`: CSV report exporter that works for normal inputs but fails with exit code `1` when the dataset has zero data rows (header-only or blank file).
- `samples/`: Fixed baseline inputs and expected CSV outputs (`normal-input.csv`, `empty-input.csv`, `blank-input.csv`, `expected-normal.csv`, `expected-empty.csv`).
- `tests/export_check.sh`: Pre-registered managed test script for `testId = "export-check"`. It verifies normal export, header-only empty export, blank 0-byte export, `/workspace` read-only enforcement inside the container, and runs any `tests/test_*.sh` or `tests/*_regression.sh` scripts added by the agent.
- `tests.template.json`: Template for `nextbot project-add --tests ...` (requires replacing the image digest with a locally pulled digest-pinned image).
- `init-demo-repo.sh`: Initializes a standalone, clean Git repository at a target directory and verifies the baseline defect reproduces before running NextBot tasks.

## Quick Usage

```sh
./fixtures/empty-export-demo/init-demo-repo.sh \
  --dest /tmp/nextbot-demo-repo \
  --image-digest "docker.io/library/alpine@sha256:..." \
  --tests-out /tmp/nextbot-tests.json
```
