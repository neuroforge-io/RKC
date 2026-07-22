#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
OUT=dist/validation
rm -rf "$OUT"
mkdir -p "$OUT/logs"
START=$(date +%s)
STEPS=""
run_step() {
  name=$1
  shift
  echo "==> $name"
  step_start=$(date +%s)
  if "$@" >"$OUT/logs/$name.log" 2>&1; then
    status=passed
  else
    status=failed
    cat "$OUT/logs/$name.log" >&2
    python3 - "$OUT/summary.json" "$START" "$name" <<'PY'
import json,sys,time
path,start,name=sys.argv[1],int(sys.argv[2]),sys.argv[3]
json.dump({'schema_version':'1.0','ok':False,'failed_step':name,'elapsed_seconds':time.time()-start},open(path,'w'),indent=2,sort_keys=True);open(path,'a').write('\n')
PY
    exit 1
  fi
  step_end=$(date +%s)
  printf '%s\t%s\t%s\n' "$name" "$status" "$((step_end-step_start))" >>"$OUT/steps.tsv"
}
run_step format make format-check
run_step vet make vet
run_step go-tests make test
run_step python-tests make python-test
run_step contracts make contracts
run_step docs make docs-check
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
python3 - "$OUT/steps.tsv" "$OUT/summary.json" "$START" "$END" <<'PY'
import json,sys
steps=[]
for line in open(sys.argv[1]):
    name,status,duration=line.rstrip('\n').split('\t')
    steps.append({'name':name,'status':status,'duration_seconds':int(duration)})
json.dump({'schema_version':'1.0','ok':all(s['status']=='passed' for s in steps),'elapsed_seconds':int(sys.argv[4])-int(sys.argv[3]),'steps':steps},open(sys.argv[2],'w'),indent=2,sort_keys=True);open(sys.argv[2],'a').write('\n')
PY
printf 'release verification: passed (%ss)\n' "$((END-START))"
