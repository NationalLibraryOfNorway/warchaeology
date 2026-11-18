#!/usr/bin/env python3
"""Run each engine in a fresh process; retain only measurements, not databases."""
import argparse, json, os, pathlib, platform, subprocess, time
p=argparse.ArgumentParser();p.add_argument('--n',type=int,default=1000000);p.add_argument('--output',default='results.jsonl');p.add_argument('--seeds',default='11,29,47');p.add_argument('--workloads',default='capture,dedup,inventory');p.add_argument('--order',choices=['ingestion','sorted'],default='ingestion');p.add_argument('--wal-off',action='store_true');a=p.parse_args()
if a.wal_off and a.workloads!='inventory':raise ValueError('WAL-off requires inventory')
binary=os.environ.get('CDXSTORAGE_BINARY','/tmp/cdxstorage')
def disk(root):
    size=allocated=0
    for path in pathlib.Path(root).rglob('*'):
        if path.is_file():
            s=path.stat();size+=s.st_size;allocated+=s.st_blocks*512
    return size,allocated
root='/tmp/cdxstorage-runs-'+os.urandom(8).hex();os.mkdir(root)
with open(a.output,'w') as out:
 for workload in a.workloads.split(','):
  for seed in map(int,a.seeds.split(',')):
   for encoding in ('current','compact'):
    for engine in ('badger','pebble'):
     directory=f'{root}/db';os.mkdir(directory);previous=None;captures=a.n
     phases=['ingest','verify','compact','verify']
     if workload!='dedup':phases+=['churn','verify','compact','verify']
     for phase in phases:
      cmd=[binary,'-engine',engine,'-encoding',encoding,'-workload',workload,'-seed',str(seed),'-n',str(a.n),'-dir',directory,'-phase',phase,'-order',a.order]+(['-wal-off'] if a.wal_off else [])
      with open(f'{root}/stdout','w') as stdout,open(f'{root}/stderr','w') as stderr:
       proc=subprocess.Popen(cmd,stdout=stdout,stderr=stderr);peak_disk=peak_rss=0
       while proc.poll() is None:
        try:
         for line in pathlib.Path(f'/proc/{proc.pid}/status').read_text().splitlines():
          if line.startswith('VmHWM:'):peak_rss=max(peak_rss,int(line.split()[1])*1024)
         peak_disk=max(peak_disk,disk(directory)[1])
        except FileNotFoundError:pass
        time.sleep(.1)
       if proc.returncode:raise RuntimeError(pathlib.Path(f'{root}/stderr').read_text())
      result=json.loads(pathlib.Path(f'{root}/stdout').read_text());
      if phase=='churn':captures=a.n*9//10
      result.update(live_captures=captures,logical_disk_bytes=disk(directory)[0],allocated_disk_bytes=disk(directory)[1],peak_allocated_disk_bytes=peak_disk,peak_rss_bytes=peak_rss,machine=platform.platform())
      if phase in ('verify','compact') and previous:
       assert (result['entries'],result['checksum'])==previous,'reopen or compaction changed data'
      previous=(result['entries'],result['checksum']);out.write(json.dumps(result)+'\n');out.flush()
     subprocess.run(['rm','-rf',directory],check=True)

for name in ("stdout","stderr"):
    os.unlink(root+"/"+name)
os.rmdir(root)
