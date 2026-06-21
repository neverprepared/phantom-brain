# Changelog

## [1.2.1](https://github.com/neverprepared/mcp-phantom-brain/compare/v1.2.0...v1.2.1) (2026-06-21)


### Bug Fixes

* quiet stale ship-queue + checkpoint-ticker log noise ([#13](https://github.com/neverprepared/mcp-phantom-brain/issues/13)) ([2b2753c](https://github.com/neverprepared/mcp-phantom-brain/commit/2b2753cc308e20867eb55fef0c902f0fe329fe43))

## [1.2.0](https://github.com/neverprepared/mcp-phantom-brain/compare/v1.1.0...v1.2.0) (2026-06-21)


### Features

* add brain_trace tool — synthesis audit trail query ([4d9a232](https://github.com/neverprepared/mcp-phantom-brain/commit/4d9a232b8df334b178700466b7e11892db537e31))
* add topic backfill script for existing summaries ([1c9c264](https://github.com/neverprepared/mcp-phantom-brain/commit/1c9c26433ae49d720051377e3a97f998d692944e))
* add topic classification to gate verdict ([5370565](https://github.com/neverprepared/mcp-phantom-brain/commit/53705656cce371fd0e1e148affc77e5c329334fb))
* batch ingest support for brain_learn and brain_perceive ([f3df8fc](https://github.com/neverprepared/mcp-phantom-brain/commit/f3df8fcc4790baa22accf9c273f36c4c1c5f39ac))
* brain_attach tool and Obsidian drilldown links ([3f557b0](https://github.com/neverprepared/mcp-phantom-brain/commit/3f557b08c11b4757ff1cf5045ab001ff41e5f50e))
* **go:** real MinIO storage backend (Phase 5) ([#7](https://github.com/neverprepared/mcp-phantom-brain/issues/7)) ([edd1ca1](https://github.com/neverprepared/mcp-phantom-brain/commit/edd1ca115a6e498fc4f649f618a20040739b08a4))
* initial mcp-brain implementation ([1446146](https://github.com/neverprepared/mcp-phantom-brain/commit/1446146d2b287f6294587705f60bdde1c01e673b))
* Karpathy brain model — Raw/Gate/Wiki pipeline (Phases 0-5) ([#2](https://github.com/neverprepared/mcp-phantom-brain/issues/2)) ([0f95fd8](https://github.com/neverprepared/mcp-phantom-brain/commit/0f95fd8cb34bf762ea2afcc874fd3a37fe826cd9))
* LLM summary distillation, batch synthesize, and lifecycle cleanup ([f2e1f3e](https://github.com/neverprepared/mcp-phantom-brain/commit/f2e1f3e45a5358b72d7bc433600562ccec0f40ae))
* remove summary body truncation, expand entity snippet to 1500 chars ([250b1dd](https://github.com/neverprepared/mcp-phantom-brain/commit/250b1dd45d64badfe3e4006595d9431b21065e9a))
* topic filter for brain_recall + auto-cleanup broken provenance ([70d6d91](https://github.com/neverprepared/mcp-phantom-brain/commit/70d6d91a43cdd685ec33fd14a671835d345b0019))


### Bug Fixes

* enable WAL mode and busy_timeout on working memory DB ([00174b7](https://github.com/neverprepared/mcp-phantom-brain/commit/00174b76c8e041647f295239ce2daa105687773b))
* fall back to disk read on wikiIndex cache miss ([e15330c](https://github.com/neverprepared/mcp-phantom-brain/commit/e15330cc2c33bcc7b319dbb248b7ca35dfa6ddbc))
* make _index.md update atomic under concurrent agents ([d3964c0](https://github.com/neverprepared/mcp-phantom-brain/commit/d3964c03bad30889d114d2bf0aced6dd94a97f84))
* make brain_reflect provenance updates atomic per-entry ([d136365](https://github.com/neverprepared/mcp-phantom-brain/commit/d13636566a31fe83817fc696a5a7dd84d1587b3d))
* make entity page create-or-append atomic under concurrent agents ([503a24d](https://github.com/neverprepared/mcp-phantom-brain/commit/503a24d3a87783d5fcb1c8e628a6ec91f6b17451))
* make provenance.json update atomic under concurrent agents ([1d2fe23](https://github.com/neverprepared/mcp-phantom-brain/commit/1d2fe23af9b67907d2a42d99d935b1a3196451dc))
* mark deferred-fetch items done on permanent HTTP errors (4xx) ([0a22acc](https://github.com/neverprepared/mcp-phantom-brain/commit/0a22acc40b394d38a7dabb4ad5aa2b56e8a3eae5))
* raise gate CLI timeout from 15s to 30s ([9bf688c](https://github.com/neverprepared/mcp-phantom-brain/commit/9bf688cddf439ed10d4b617efec8c592434be72c))
* strip trailing } from vault path and store content in rejection log ([983401a](https://github.com/neverprepared/mcp-phantom-brain/commit/983401a45745aedf86474cbf0c5295ba93e149e4))
* tighten entity extraction to reject generic section headings ([24a10ad](https://github.com/neverprepared/mcp-phantom-brain/commit/24a10ad5eeda18065768bec2ef063f99acf5d5fe))


### Performance Improvements

* parallelize startup, entity writes, search resolution, and vector sync ([83deece](https://github.com/neverprepared/mcp-phantom-brain/commit/83deeceedae498aa0613f34546e6b0d360819bf5))

## 1.0.0 (2026-05-23)


### Features

* initial mcp-brain implementation ([1446146](https://github.com/mindmorass/mcp-phantom-brain/commit/1446146d2b287f6294587705f60bdde1c01e673b))


### Bug Fixes

* strip trailing } from vault path and store content in rejection log ([983401a](https://github.com/mindmorass/mcp-phantom-brain/commit/983401a45745aedf86474cbf0c5295ba93e149e4))
