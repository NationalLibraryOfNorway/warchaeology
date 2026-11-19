#!/usr/bin/env python3
# Copyright 2026 National Library of Norway.
# SPDX-License-Identifier: Apache-2.0

import json,sys
rows=[]
for path in sys.argv[1:]:
    positions={}
    for line in open(path):
        r=json.loads(line);key=(r['workload'],r['seed'],r['encoding'],r['engine']);step=positions.get(key,0);positions[key]=step+1
        if r['phase']!='compact':continue
        live=r['captures']*9//10 if step>=4 else r['captures']
        if r['workload']=='dedup':live=r['unique_payloads']
        rows.append(dict(workload=r['workload'],seed=r['seed'],engine=r['engine'],encoding=r['encoding'],order=r.get('order','ingestion'),disposable=r.get('disposable',r['wal_off']),churned=step>=4,allocated_bytes=r['allocated_disk_bytes'],logical_bytes=r['logical_disk_bytes'],live_records=live,allocated_bytes_per_live_record=r['allocated_disk_bytes']/live,reclamation_seconds=r['seconds']))
print(json.dumps(rows,indent=2))
