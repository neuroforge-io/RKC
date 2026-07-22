#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
OUT=dist/validation
if [ -L dist ] || { [ -e dist ] && [ ! -d dist ]; }; then
  echo "release verification: dist must be a real directory, not a symlink or non-directory" >&2
  exit 1
fi
if [ -L "$OUT" ] || { [ -e "$OUT" ] && [ ! -d "$OUT" ]; }; then
  echo "release verification: $OUT must be a real directory, not a symlink or non-directory" >&2
  exit 1
fi
if [ -L "$OUT/logs" ] || { [ -e "$OUT/logs" ] && [ ! -d "$OUT/logs" ]; }; then
  echo "release verification: $OUT/logs must be a real directory, not a symlink or non-directory" >&2
  exit 1
fi
mkdir -p "$OUT/logs"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/rkc-release-verification.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM
STEPS="$WORK/steps.tsv"
: >"$STEPS"
START=$(date +%s)
SUMMARY="$WORK/summary.json"
python3 - "$SUMMARY" "$START" <<'PY'
import json,sys
path,start=sys.argv[1],int(sys.argv[2])
with open(path,'w',encoding='utf-8') as output:
    json.dump({'schema_version':'1.0','status':'running','ok':False,'started_at_unix':start},output,indent=2,sort_keys=True)
    output.write('\n')
PY
python3 scripts/publish_file.py --source "$STEPS" --destination "$OUT/steps.tsv" --mode 0644
python3 scripts/publish_file.py --source "$SUMMARY" --destination "$OUT/summary.json" --mode 0644
run_step() {
  name=$1
  shift
  echo "==> $name"
  step_start=$(date +%s)
  step_log="$WORK/$name.log"
  if "$@" >"$step_log" 2>&1; then
    status=passed
  else
    status=failed
    python3 scripts/publish_file.py --source "$step_log" --destination "$OUT/logs/$name.log" --mode 0644
    cat "$OUT/logs/$name.log" >&2
    python3 - "$SUMMARY" "$START" "$name" <<'PY'
import json,sys,time
path,start,name=sys.argv[1],int(sys.argv[2]),sys.argv[3]
json.dump({'schema_version':'1.0','ok':False,'failed_step':name,'elapsed_seconds':time.time()-start},open(path,'w'),indent=2,sort_keys=True);open(path,'a').write('\n')
PY
    python3 scripts/publish_file.py --source "$STEPS" --destination "$OUT/steps.tsv" --mode 0644
    python3 scripts/publish_file.py --source "$SUMMARY" --destination "$OUT/summary.json" --mode 0644
    exit 1
  fi
  python3 scripts/publish_file.py --source "$step_log" --destination "$OUT/logs/$name.log" --mode 0644
  step_end=$(date +%s)
  printf '%s\t%s\t%s\n' "$name" "$status" "$((step_end-step_start))" >>"$STEPS"
}
run_step format make format-check
run_step vet make vet
run_step coverage make coverage
run_step contracts make contracts
run_step docs make docs-check
run_step licenses python3 scripts/validate-licenses.py
run_step build make build
run_step plugins make plugins
run_step smoke make smoke
run_step reproducibility make reproducibility
run_step api-smoke make smoke-api
run_step mcp-smoke make smoke-mcp
run_step git-smoke make smoke-git
run_step race make test-race
run_step benchmark timeout 180 sh scripts/benchmark-reference.sh dist/benchmark
END=$(date +%s)
python3 - "$STEPS" "$SUMMARY" "$START" "$END" <<'PY'
import json,sys
steps=[]
for line in open(sys.argv[1]):
    name,status,duration=line.rstrip('\n').split('\t')
    steps.append({'name':name,'status':status,'duration_seconds':int(duration)})
json.dump({'schema_version':'1.0','ok':all(s['status']=='passed' for s in steps),'elapsed_seconds':int(sys.argv[4])-int(sys.argv[3]),'steps':steps},open(sys.argv[2],'w'),indent=2,sort_keys=True);open(sys.argv[2],'a').write('\n')
PY
python3 scripts/publish_file.py --source "$STEPS" --destination "$OUT/steps.tsv" --mode 0644
python3 scripts/publish_file.py --source "$SUMMARY" --destination "$OUT/summary.json" --mode 0644
printf 'release verification: passed (%ss)\n' "$((END-START))"
