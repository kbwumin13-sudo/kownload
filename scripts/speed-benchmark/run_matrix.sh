#!/bin/bash
# Run the comparison matrix of the 2026-10-26 speed plan, one comparison at a
# time so that trials never compete for the machine or the fixture.
#   run_matrix.sh BASE_LIB CAND_LIB FIXTURE_BIN OUT_DIR [quick|long|all]
set -uo pipefail
base="$1" cand="$2" fixture="$3" out="$4" set="${5:-quick}"
here="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "$out"
work="$out/.work"
mkdir -p "$work"

run() {
  local name="$1" scenario="$2"
  shift 2
  echo "== $name ($scenario) $(date +%H:%M:%S)"
  python3 -I "$here/bench.py" --scenario "$scenario" --fixture-bin "$fixture" --work-dir "$work" \
    --output "$out/$name.json" "$@" > "$out/$name.log" 2>&1
  echo "   exit=$? $(tail -1 "$out/$name.log" | cut -c1-400)"
}

if [[ "$set" == quick || "$set" == all ]]; then
  # W00: statistics collection on/off.
  run a00-trace-r1 r1 --variant "trace-off=$cand:{\"trace\":false}" --variant "trace-on=$cand"
  run a00-trace-b1 b1-h1 --variant "trace-off=$cand:{\"trace\":false}" --variant "trace-on=$cand"
  # W01: segmentation on a per-connection limit, none on a total cap.
  run a01-r1-streams r1 --variant "one=$cand@1" --variant "four=$cand@4"
  run a01-r3-streams r3 --variant "one=$cand@1" --variant "four=$cand@4"
  run a01-r1-regress r1 --variant "base=$base" --variant "cand=$cand"
  # W02: connection reuse, then HTTP/2 separately.
  run a02-b1-h1 b1-h1 --variant "base=$base" --variant "cand=$cand"
  run a02-b1-h1-pool b1-h1 --variant "private=$cand:{\"sharedTransport\":false}" --variant "pooled=$cand"
  run a02-b1-h2 b1-h2 --variant "h1=$cand" --variant "h2=$cand:{\"http2\":true}"
  run a02-r1-tls1 r1-tls1 --variant "base=$base" --variant "cand=$cand"
  run a02-r1-tls-h2 r1-tls --variant "h1=$cand" --variant "h2=$cand:{\"http2\":true}"
  run a02-b1-1000 b1-1000 --variant "base=$base" --variant "cand=$cand"
  # W03: rate limits with Retry-After seconds, an HTTP date, and no header.
  run a03-bucket b2-bucket --variant "base=$base" --variant "cand=$cand"
  run a03-bucket-date b2-bucket-date --variant "base=$base" --variant "cand=$cand"
  run a03-bucket-none b2-bucket-none --variant "base=$base" --variant "cand=$cand"
  run a03-b2 b2 --variant "base=$base" --variant "cand=$cand"
  run a03-b2-date b2-date --variant "base=$base" --variant "cand=$cand"
  run a03-b2-none b2-none --variant "base=$base" --variant "cand=$cand"
  # W07: unpaced local source; read buffer sizes.
  run a07-d1 d1 --variant "base=$base" --variant "cand=$cand"
  run a07-d1-buffer d1 --variant "buf8k=$cand:{\"readBuffer\":8192}" --variant "buf64k=$cand"
  run a07-r1-buffer r1 --variant "buf8k=$cand:{\"readBuffer\":8192}" --variant "buf64k=$cand"
fi

if [[ "$set" == long || "$set" == all ]]; then
  # W04: fixed stream counts against throughput-validated adaptive targets.
  run a04-big-capped big-capped --variant "fixed8=$cand@8" --variant "adaptive8=$cand:{\"adaptive\":true}@8" \
    --variant "fixed1=$cand@1" --variant "fixed2=$cand@2" --variant "fixed4=$cand@4"
  run a04-big-perconn big-perconn --variant "fixed8=$cand@8" --variant "adaptive8=$cand:{\"adaptive\":true}@8" \
    --variant "fixed4=$cand@4"
  run a04-big-both big-both --variant "fixed8=$cand@8" --variant "adaptive8=$cand:{\"adaptive\":true}@8" \
    --variant "fixed4=$cand@4"
  run a04-slow-tail slow-tail --variant "base=$base" --variant "cand=$cand" \
    --variant "adaptive=$cand:{\"adaptive\":true}"
  run a04-link-drop link-drop --variant "fixed4=$cand" --variant "adaptive4=$cand:{\"adaptive\":true}"
fi
rm -rf "$work"
echo "done $(date +%H:%M:%S)"
