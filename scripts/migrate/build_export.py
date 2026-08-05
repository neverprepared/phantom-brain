#!/usr/bin/env python3
"""Reconstruct a `pbrainctl client export` dir from mini OpenSearch records.

Usage: build_export.py <search_json> <out_dir> [limit]
Reads an OpenSearch _search JSON (hits.hits[]._source), writes out_dir/records/*.md
in the exact frontmatter+body format `pbrainctl client import` consumes.
"""
import json, sys, re, os

def slug(t):
    s = re.sub(r'[^a-zA-Z0-9]+', '-', (t or 'untitled')).strip('-').lower()
    return s[:60] or 'untitled'

def yaml_scalar(s):
    # quote to survive colons/quotes/newlines in titles
    return json.dumps(str(s), ensure_ascii=False)

def main():
    inp, outdir = sys.argv[1], sys.argv[2]
    limit = int(sys.argv[3]) if len(sys.argv) > 3 else 0
    d = json.load(open(inp))
    hits = d.get('hits', {}).get('hits', [])
    if limit:
        hits = hits[:limit]
    recdir = os.path.join(outdir, 'records')
    os.makedirs(recdir, exist_ok=True)
    seen = set()
    n = 0
    for h in hits:
        s = h['_source']
        sha = str(s.get('sha') or h.get('_id'))
        title = s.get('title') or 'untitled'
        fn = f"{slug(title)}-{sha[:12]}.md"
        # de-dupe filenames defensively
        if fn in seen:
            fn = f"{slug(title)}-{sha[:16]}-{n}.md"
        seen.add(fn)
        # Quote EVERY scalar + list item (JSON scalars are valid YAML) so values
        # with colons/quotes/brackets (e.g. tags like "vendor:CR&R Inc",
        # source like 'from_email:"X" <a@b>') can't break the frontmatter parse.
        fm = ['---', f"sha: {sha}", f"kind: {yaml_scalar(s.get('kind','note'))}",
              f"title: {yaml_scalar(title)}"]
        tags = s.get('tags')
        if tags:
            tags = [tags] if isinstance(tags, str) else tags
            fm.append('tags:')
            fm += [f"  - {yaml_scalar(t)}" for t in tags]
        if s.get('topic'):       fm.append(f"topic: {yaml_scalar(s['topic'])}")
        if s.get('reliability'): fm.append(f"reliability: {yaml_scalar(s['reliability'])}")
        if s.get('memory_type'): fm.append(f"memory_type: {yaml_scalar(s['memory_type'])}")
        if s.get('captured_at'): fm.append(f"captured_at: {yaml_scalar(s['captured_at'])}")
        src = s.get('source') or s.get('source_url')
        if src:
            src = [src] if isinstance(src, str) else src
            fm.append('source:')
            fm += [f"  - {yaml_scalar(x)}" for x in src]
        # attachment linkage: carry mime/filename so nothing is lost
        if s.get('mime_type'):         fm.append(f"mime_type: {yaml_scalar(s['mime_type'])}")
        if s.get('original_filename'): fm.append(f"original_filename: {yaml_scalar(s['original_filename'])}")
        fm.append('---')
        body = s.get('body') or s.get('extracted_text') or ''
        with open(os.path.join(recdir, fn), 'w') as f:
            f.write('\n'.join(fm) + '\n' + body + '\n')
        n += 1
    print(f"wrote {n} record files to {recdir}")

if __name__ == '__main__':
    main()
