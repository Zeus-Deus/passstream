#!/usr/bin/env python3
"""Build matching CPU-profile binaries and run a serialized A/B/B/A in a netns."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]
RESULTS = ROOT / 'test/torture/results/capacity'
BASELINE = '8eef8c1'
PREFIX = os.environ.get('CPU_AB_PREFIX', 'r3-final')
PROCESSORS = int(os.environ.get('CPU_AB_GOMAXPROCS', str(os.cpu_count())))


def run(args, **kw):
    return subprocess.run(args, check=True, **kw)


def quiet():
    # Bound selection time: the load of other users is outside our control.
    for attempt in range(19):
        load = Path('/proc/loadavg').read_text().strip()
        with (RESULTS / 'r3-cpu-load-selection.txt').open('a') as f:
            f.write(f'{time.time():.3f} {load}\n')
        if (float(load.split()[0]) <= 2 and float(load.split()[1]) <= 3) or attempt == 18:
            return
        time.sleep(10)


def inside(before, after, baseline, current):
    run(['ip', 'link', 'set', 'lo', 'up', 'mtu', '1500'])
    passed = True
    for label, binary, revision in [('before',before,baseline),('after',after,current),('after-repeat',after,current),('before-repeat',before,baseline)]:
        quiet()
        output = RESULTS / f'{PREFIX}-{label}.json'
        if output.exists():
            raise RuntimeError(f'refusing to overwrite evidence: {output}')
        logdir = ROOT / 'test/torture/out/cpu-ab'
        logdir.mkdir(parents=True, exist_ok=True)
        with (logdir / f'{label}.log').open('w') as log:
            observed = subprocess.run([str(ROOT/'build/capacity'), '-server',binary,'-viewers','256','-streams','1','-seconds','20','-warmup','10s','-out',str(output)],stdout=log,stderr=subprocess.STDOUT)
        result = json.loads(output.read_text())
        fps=sum(m['frames'] for m in result['metrics'])/len(result['metrics'])/result['observation_s']
        loads=[float(s.split()[0]) for s in result['load_samples']+[result['load_start'],result['load_end']]]
        result['quiet_target_met'] = max(loads) <= 3
        result['quality_comparable'] = result['pass'] and fps >= 14.7
        result['comparison_eligible'] = result['quality_comparable'] and result['quiet_target_met']
        passed = passed and result['comparison_eligible'] and observed.returncode == 0
        result.update(sfu_source_tree=subprocess.check_output(['git','rev-parse',revision+':internal/sfu'],text=True).strip(), source_commit=revision, gomaxprocs=PROCESSORS, build_flags=['-trimpath'], go_version=subprocess.check_output(['mise','exec','--','go','version'],text=True).strip(), profiling='test-only localhost overlay')
        output.write_text(json.dumps(result,indent=2)+'\n')
        with Path(str(output)+'.top.txt').open('w') as top:
            run(['mise','exec','--','go','tool','pprof','-top','-cum','-nodecount=50',binary,str(output)+'.pprof'],stdout=top)
        print(label, result['pass'], result['server_cpu_pct'], result['load_start'],result['load_end'],flush=True)
    return passed


if __name__ == '__main__':
    os.chdir(ROOT)
    if len(sys.argv)>1 and sys.argv[1]=='--inside':
        sys.exit(0 if inside(*sys.argv[2:]) else 1)
    else:
        RESULTS.mkdir(parents=True,exist_ok=True)
        env = dict(os.environ, CGO_ENABLED='0', GOMAXPROCS=str(PROCESSORS))
        go = subprocess.check_output(['mise','which','go'],text=True).strip()
        current = subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip()
        baseline = subprocess.check_output(['git','rev-parse',BASELINE],text=True).strip()
        with tempfile.TemporaryDirectory(prefix='passstream-cpu-') as tmp:
            tmp = Path(tmp)
            archive = tmp/'baseline.tar'
            run(['git','archive','--format=tar','--output',str(archive),BASELINE])
            source = tmp/'baseline';source.mkdir()
            with tarfile.open(archive) as tar:
                tar.extractall(source,filter='data')
            binaries=[]
            for label, checkout in [('before',source),('after',ROOT)]:
                main = checkout/'cmd/passstream/main.go'
                profile = tmp/f'{label}-main.go'
                profile.write_text(main.read_text().replace('"net/http"','"net/http"\n _ "net/http/pprof"')+'\nfunc init() { go http.ListenAndServe("127.0.0.1:18082", nil) }\n')
                overlay = tmp/f'{label}-overlay.json'
                overlay.write_text(json.dumps({'Replace':{str(main):str(profile)}}))
                binary = tmp/f'{label}-profile'
                run([go,'build','-trimpath',f'-overlay={overlay}','-o',str(binary),'./cmd/passstream'],cwd=checkout,env=env)
                binaries.append(str(binary))
            run([go,'build','-trimpath','-o',str(ROOT/'build/capacity'),'./test/torture/capacity'],env=env)
            run(['unshare','-rn','python3',str(Path(__file__).resolve()),'--inside',*binaries,baseline,current],env=dict(env,PASSSTREAM_PROFILE='1'))
