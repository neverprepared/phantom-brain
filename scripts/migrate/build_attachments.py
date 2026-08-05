#!/usr/bin/env python3
"""Build a pbrainctl-import attachments/ dir from mirrored blobs + record metadata.

Usage: build_attachments.py <staging_blob_dir> <records_dump.json> <out_dir>
- staging_blob_dir: mc-mirrored blobs, named <sha>.<ext>
- records_dump.json: OpenSearch _search dump (source of mime/filename/tags/text per sha)
- out_dir: gets attachments/<sha> (raw blob) + attachments/<sha>.json (metadata) + empty records/
"""
import json, sys, os, glob, shutil

def main():
    staging, dump, outdir = sys.argv[1], sys.argv[2], sys.argv[3]
    recs = {}
    for h in json.load(open(dump))['hits']['hits']:
        s = h['_source']
        if s.get('kind') == 'attachment' and s.get('sha'):
            recs[s['sha']] = s
    attdir = os.path.join(outdir, 'attachments')
    os.makedirs(attdir, exist_ok=True)
    os.makedirs(os.path.join(outdir, 'records'), exist_ok=True)  # import expects records/
    n = miss = 0
    for blob in glob.glob(os.path.join(staging, '*')):
        base = os.path.basename(blob)
        if base.endswith('.json'):
            continue
        sha = os.path.splitext(base)[0]
        s = recs.get(sha)
        if not s:
            miss += 1
            continue
        shutil.copyfile(blob, os.path.join(attdir, sha))
        meta = {
            'sha': sha,
            'original_filename': s.get('original_filename') or base,
            'mime_type': s.get('mime_type') or 'application/octet-stream',
            'tags': s.get('tags') or ['attachment'],
            'extracted_text': s.get('extracted_text') or s.get('body') or '',
        }
        with open(os.path.join(attdir, sha + '.json'), 'w') as f:
            json.dump(meta, f)
        n += 1
    print(f"built {n} attachments ({miss} blobs without a matching record) in {attdir}")

if __name__ == '__main__':
    main()
