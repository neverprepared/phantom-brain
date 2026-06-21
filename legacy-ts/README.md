# legacy-ts/

The original TypeScript implementation of `mcp-phantom-brain`. Lives here per the v5.0 spec's one-release-cycle deprecation policy after the Go rewrite landed (Phases 0–2.5, PR #4).

**Status:** frozen. No new features. Bug fixes only if Go can't be cut over for a specific operator. Scheduled for deletion in Phase 5+.

## Why it's still here

- A safety net in case the Go binary turns up a regression in the field and someone needs to roll back fast.
- Operators who haven't yet flipped their `.claude.json` to point at `pbrainctl` can still build + run this — it works unchanged.
- The README at the repo root is now Go-first; this directory keeps the TS instructions intact.

## Running it

```bash
cd legacy-ts
npm install
cp .env.example .env  # edit BRAIN_VAULT_PATH
npm run build
node ./dist/index.js  # via your MCP client; see ../README for env contract
```

`npm run dev` (tsx) and `npm run typecheck` work as before. There are no tests; typecheck is the only verification step.

## What moved here from the repo root

- `src/`, `package.json`, `package-lock.json`, `tsconfig.json`, `.env.example`
- `scripts/backfill-topics.mjs` (one-off topic-backfill utility)
- `.github/workflows/{ci,publish}.yml` — kept for reference; **they do NOT fire** from this path (GitHub Actions only reads `.github/workflows/` at the repo root)

## What did NOT move

- `release-please.yml` — language-neutral, useful for tagging Go releases too
- `CHANGELOG.md` — shared history across both implementations
- `CLAUDE.md` — agent instructions; covers both runtimes

## Cutover path

If you're still running this:

1. Build the Go binary from the repo root: `cd .. && make build`
2. Update `.claude.json` to point at `pbrainctl` (legacy mode reads the same `BRAIN_VAULT_PATH`)
3. Restart your MCP client
4. Verify with the Go binary's `pbrainctl mcp` for a few sessions
5. (Optional) Flip env to `CL_BRAIN_*` for v5.0 agent-contract mode + daemon ingest

Until you reach step 5, the agent operates on the same on-disk vault — the migration is reversible.
