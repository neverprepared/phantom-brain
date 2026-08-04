# Vault round-trip: `export` / `import`

`pbrainctl client export` and `import` move a vault's content — text records,
attachment blobs, and (optionally) exact embedding vectors — to and from a
portable, human-readable markdown vault. Import is **idempotent and SHA-deduped**,
so it doubles as the **merge**, **portability**, and **archival** path.

Identity is content-addressed and derived daemon-side (`canonicalize.Sum` for
verbatim kinds, `SumBody` otherwise), so a round-trip recomputes the same SHA and
dedups — even for records whose body carries its own frontmatter.

## Layout

```
<vault>/
  records/<slug>-<sha>.md    # metadata frontmatter + raw body, verbatim
  attachments/<sha>          # blob bytes  (+ <sha>.json sidecar: filename, mime, tags)
  embeddings/<sha>.json      # only with --with-embeddings: the 768-dim vector
```

## Usage

```bash
export CL_BRAIN_API=http://localhost:9998
export CL_BRAIN_API_TOKEN=<vault bearer token>   # or pass --api/--token

pbrainctl client export ./my-vault                    # text + attachments
pbrainctl client export ./my-vault --with-embeddings  # + exact vectors (no re-embed on import)
pbrainctl client import ./my-vault                    # idempotent, SHA-deduped union
pbrainctl client import ./my-vault --dry-run          # list what would import
```

Per-vault tokens come from the app's Memory panel or
`GET /api/brain/profiles/<profile>/tokens`.

## What round-trips

- **Faithful:** identity (SHA), body, kind, memory_type, captured_at, source, tags;
  attachment blobs + their filename/mime; embeddings (with `--with-embeddings`).
- **Re-derived by design:** topic and reliability are system/synth-owned, so they
  re-derive on import. Synthesis + the entity graph also regenerate.

## Choosing the right tool

| Job | Tool |
|-----|------|
| Merge / portability / cross-version / archival / hand-curation | **`export` ↔ `import`** |
| Bit-exact clone / DR (same version) | `pg_dump -Fc` + restore `--clean` |
| Same-lineage migrate keeping vectors, no markdown | `pbrainctl server backfill-to-pg` |

Unlike `mart build` (a lossy human projection — distilled body, cosmetic framing,
an `index.md`), `export` preserves the raw body and full metadata, so it is the
one that round-trips.
