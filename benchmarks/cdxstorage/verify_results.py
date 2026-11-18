#!/usr/bin/env python3
"""Check independent engine checksums, expected live counts and order equivalence."""
import json,sys
seen={}
for name in sys.argv[1:]:
    stages={}
    for line in open(name):
        r=json.loads(line)
        run=(r['workload'],r['seed'],r['encoding'],r['engine'])
        step=stages.get(run,0);stages[run]=step+1
        n=r['captures'];churned=step>=4
        live=n*9//10 if churned else n
        dictionary=(n+999)//1000 if r['encoding']=='compact' else 0
        expected=n//3 if r['workload']=='dedup' else live*(4 if r['workload']=='inventory' else 1)+dictionary
        assert r['entries']==expected,(name,run,step,r['entries'],expected)
        key=(r['workload'],r['seed'],r['encoding'],churned)
        state=(r['entries'],r['checksum'])
        if key in seen:assert state==seen[key],(name,run,step,'engine/order/checksum mismatch')
        seen[key]=state
    for run,count in stages.items():assert count==(4 if run[0]=='dedup' else 8),(name,run,count)
print('Verified engine/order equivalence, reopened checksums, compaction and 20% delete / 10% changed reinsert counts.')
