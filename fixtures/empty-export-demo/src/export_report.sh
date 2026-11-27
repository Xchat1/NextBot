#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: export_report.sh <input.csv> <output.csv>" >&2
  exit 2
fi

input="$1"
output="$2"

if [ ! -f "$input" ]; then
  echo "input file not found: $input" >&2
  exit 2
fi

# Known defect: fails when input contains no data rows (header-only or blank file).
row_count=$(awk 'NR > 1 && NF > 0 { count++ } END { print count + 0 }' "$input")
if [ "$row_count" -eq 0 ]; then
  echo "export failed: empty dataset" >&2
  exit 1
fi

awk '
  NR == 1 { print "id,region,amount"; next }
  NF > 0 { print $0 }
' "$input" > "$output"
