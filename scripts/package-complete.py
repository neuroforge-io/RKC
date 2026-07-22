#!/usr/bin/env python3
"""Build a deterministic, self-contained RKC release ZIP."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import stat
import tempfile
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
TOP = "repository-knowledge-compiler-complete"
EXCLUDED_PARTS = {".git", "bin", "dist", ".rkc", ".rkc-smoke", ".rkc-state", ".rkc-state-smoke", "__pycache__"}
EXCLUDED_SUFFIXES = {".pyc", ".pyo"}


def sha256(path: Path) -> str:
    h=hashlib.sha256()
    with path.open('rb') as f:
        for chunk in iter(lambda:f.read(1024*1024), b''):
            h.update(chunk)
    return h.hexdigest()


def copy_tree(source: Path, target: Path) -> None:
    for path in sorted(source.rglob('*')):
        relative=path.relative_to(source)
        if any(part in EXCLUDED_PARTS or part.startswith('.rkc-') for part in relative.parts):
            continue
        if path.suffix in EXCLUDED_SUFFIXES or path.is_symlink():
            continue
        destination=target/relative
        if path.is_dir():
            destination.mkdir(parents=True,exist_ok=True)
        elif path.is_file():
            destination.parent.mkdir(parents=True,exist_ok=True)
            shutil.copyfile(path,destination)
            os.chmod(destination, path.stat().st_mode & 0o777)


def write_zip(staging: Path, output: Path) -> None:
    output.parent.mkdir(parents=True,exist_ok=True)
    tmp=output.with_suffix(output.suffix+'.tmp')
    if tmp.exists(): tmp.unlink()
    with zipfile.ZipFile(tmp,'w',compression=zipfile.ZIP_DEFLATED,compresslevel=9) as zf:
        for path in sorted(staging.rglob('*')):
            if not path.is_file(): continue
            relative=Path(TOP)/path.relative_to(staging)
            info=zipfile.ZipInfo(str(relative).replace(os.sep,'/'), date_time=(1980,1,1,0,0,0))
            mode=path.stat().st_mode
            permissions=0o755 if mode & stat.S_IXUSR else 0o644
            info.external_attr=(permissions & 0xFFFF)<<16
            info.compress_type=zipfile.ZIP_DEFLATED
            info.create_system=3
            zf.writestr(info,path.read_bytes())
    tmp.replace(output)


def main() -> None:
    parser=argparse.ArgumentParser()
    parser.add_argument('--output',required=True)
    args=parser.parse_args()
    output=(ROOT/args.output).resolve() if not Path(args.output).is_absolute() else Path(args.output)
    required=[ROOT/'dist/binaries',ROOT/'dist/demo',ROOT/'dist/validation',ROOT/'dist/benchmark']
    missing=[str(p) for p in required if not p.exists()]
    if missing: raise SystemExit('missing release inputs: '+', '.join(missing))
    with tempfile.TemporaryDirectory(prefix='rkc-package-') as temp:
        stage=Path(temp)
        source=stage/'source'
        copy_tree(ROOT,source)
        shutil.copytree(ROOT/'dist/binaries',stage/'artifacts/binaries')
        shutil.copytree(ROOT/'dist/demo',stage/'artifacts/demo')
        shutil.copytree(ROOT/'dist/validation',stage/'artifacts/validation')
        benchmark_target=stage/'artifacts/benchmark'
        benchmark_target.mkdir(parents=True,exist_ok=True)
        for name in ('result.json','time.txt','scan.stdout'):
            source_file=ROOT/'dist/benchmark'/name
            if source_file.exists(): shutil.copy2(source_file,benchmark_target/name)
        for name in ('demo-scan.txt','demo-check.txt','demo-synthesis.txt'):
            p=ROOT/'dist'/name
            if p.exists():
                (stage/'artifacts/demo-logs').mkdir(parents=True,exist_ok=True)
                shutil.copy2(p,stage/'artifacts/demo-logs'/name)
        readme=f'''# Repository Knowledge Compiler complete package\n\nVersion: {(ROOT/'VERSION').read_text().strip()}\n\nThis archive contains the complete source tree, Linux amd64/arm64 binaries, a generated mixed-language demonstration atlas, release-verification logs, contracts, and the detailed remainder implementation plan.\n\n## Fast path\n\n```sh\ncd source\nmake verify\nmake build\n./bin/rkc scan --out /tmp/rkc-output --force examples\n./bin/rkc serve --dir /tmp/rkc-output --addr 127.0.0.1:8787\n```\n\nThe reference implementation is functional and tested. Production gaps are stated explicitly in `source/docs/REMAINDER_IMPLEMENTATION_PLAN.md`; in particular compiler-grade semantic adapters, enforced WASI/native-worker isolation, a canonical SQLite runtime writer, multi-tenant PostgreSQL mode, and a measured real-GGUF under-4-GiB benchmark remain planned work.\n'''
        (stage/'README-FIRST.md').write_text(readme,encoding='utf-8')
        payload=[]
        for path in sorted(stage.rglob('*')):
            if path.is_file() and path.name not in {'SHA256SUMS.txt','MANIFEST.json'}:
                payload.append({'path':str(path.relative_to(stage)).replace(os.sep,'/'),'size_bytes':path.stat().st_size,'sha256':sha256(path)})
        manifest={'schema_version':'1.0','name':'repository-knowledge-compiler-complete','version':(ROOT/'VERSION').read_text().strip(),'payload_files':len(payload),'payload_bytes':sum(x['size_bytes'] for x in payload),'files':payload}
        (stage/'MANIFEST.json').write_text(json.dumps(manifest,indent=2,sort_keys=True)+'\n',encoding='utf-8')
        checks=[]
        for path in sorted(stage.rglob('*')):
            if path.is_file() and path.name!='SHA256SUMS.txt':
                checks.append(f"{sha256(path)}  {str(path.relative_to(stage)).replace(os.sep,'/')}")
        (stage/'SHA256SUMS.txt').write_text('\n'.join(checks)+'\n',encoding='utf-8')
        write_zip(stage,output)
    print(json.dumps({'output':str(output),'size_bytes':output.stat().st_size,'sha256':sha256(output)},indent=2))

if __name__=='__main__': main()
