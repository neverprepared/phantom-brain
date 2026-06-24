# Changelog

## [3.5.0](https://github.com/neverprepared/phantom-brain/compare/v3.4.0...v3.5.0) (2026-06-24)


### Features

* brain_fetch — retrieve a doc's full body by SHA ([#80](https://github.com/neverprepared/phantom-brain/issues/80)) ([8761a02](https://github.com/neverprepared/phantom-brain/commit/8761a024a96e0dcd255d6eaa57036ed7cece62f7))

## [3.4.0](https://github.com/neverprepared/phantom-brain/compare/v3.3.0...v3.4.0) (2026-06-24)


### Features

* brain_reflect maintenance cycle — Phase 1 (report + forget) ([#79](https://github.com/neverprepared/phantom-brain/issues/79)) ([820e160](https://github.com/neverprepared/phantom-brain/commit/820e1604c1554ca5299259e2e164e6f2026ea0c2)), closes [#72](https://github.com/neverprepared/phantom-brain/issues/72)
* pbrainctl server config validate — dry-run startup config load ([#77](https://github.com/neverprepared/phantom-brain/issues/77)) ([d32c14b](https://github.com/neverprepared/phantom-brain/commit/d32c14bca5e281b892bbc6597bb54d3f54338e12)), closes [#70](https://github.com/neverprepared/phantom-brain/issues/70)


### Bug Fixes

* actionable error when binding create hits read-only config dir ([#75](https://github.com/neverprepared/phantom-brain/issues/75)) ([ec21c64](https://github.com/neverprepared/phantom-brain/commit/ec21c64dc26f0c39e95c294d2d4f22d78e426aa3)), closes [#69](https://github.com/neverprepared/phantom-brain/issues/69)

## [3.3.0](https://github.com/neverprepared/phantom-brain/compare/v3.2.0...v3.3.0) (2026-06-23)


### Features

* v3.3 operator CLI — bucket + binding subcommands ([#67](https://github.com/neverprepared/phantom-brain/issues/67)) ([45631fd](https://github.com/neverprepared/phantom-brain/commit/45631fd0fb2836543ddba2c8bf4de5deeba3390a))

## [3.2.0](https://github.com/neverprepared/phantom-brain/compare/v3.1.0...v3.2.0) (2026-06-23)


### Features

* v3.2 per-binding storage isolation — index prefix + bucket overrides ([#65](https://github.com/neverprepared/phantom-brain/issues/65)) ([950a4f6](https://github.com/neverprepared/phantom-brain/commit/950a4f6e2377722f70ff7da6b23ac6f33c47cd02))

## [3.1.0](https://github.com/neverprepared/phantom-brain/compare/v3.0.0...v3.1.0) (2026-06-23)


### Features

* v3.1 offline resilience — write-ahead queue + gc-brains UX fix ([#62](https://github.com/neverprepared/phantom-brain/issues/62)) ([aa21c7b](https://github.com/neverprepared/phantom-brain/commit/aa21c7bf7e1efa207df67d939e5747882f31dfa4))

## [3.0.0](https://github.com/neverprepared/phantom-brain/compare/v2.6.1...v3.0.0) (2026-06-23)


### ⚠ BREAKING CHANGES

* v3.0 — client/server split + GC + module rename to phantom-brain ([#58](https://github.com/neverprepared/phantom-brain/issues/58))

### Features

* v3.0 — client/server split + GC + module rename to phantom-brain ([#58](https://github.com/neverprepared/phantom-brain/issues/58)) ([9884c22](https://github.com/neverprepared/phantom-brain/commit/9884c22e0c4080ba9ec7e36ce38069beb2900691))

## [2.6.1](https://github.com/neverprepared/mcp-phantom-brain/compare/v2.6.0...v2.6.1) (2026-06-23)


### Bug Fixes

* **snapshot:** use OS-based rebuild path, not legacy local-fs walk ([#56](https://github.com/neverprepared/mcp-phantom-brain/issues/56)) ([0c9469c](https://github.com/neverprepared/mcp-phantom-brain/commit/0c9469c4c9bfbf978f1300b20d0c74c4c4f871ba))

## [2.6.0](https://github.com/neverprepared/mcp-phantom-brain/compare/v2.5.1...v2.6.0) (2026-06-23)


### Features

* **recall:** show title, kind, and snippet on each hit ([#49](https://github.com/neverprepared/mcp-phantom-brain/issues/49)) ([#54](https://github.com/neverprepared/mcp-phantom-brain/issues/54)) ([1f109d3](https://github.com/neverprepared/mcp-phantom-brain/commit/1f109d38bcee2924b8f8c3ef1bf4d5c107ba068d))

## [2.4.1](https://github.com/neverprepared/mcp-phantom-brain/compare/v2.4.0...v2.4.1) (2026-06-23)


### Bug Fixes

* **attachments:** recall sees attachments + MinIO tag mirror ([#50](https://github.com/neverprepared/mcp-phantom-brain/issues/50)) ([ad6012d](https://github.com/neverprepared/mcp-phantom-brain/commit/ad6012dd32ddc68b11b7cda36f2d692f6dbefa54))

## [2.4.0](https://github.com/neverprepared/mcp-phantom-brain/compare/v2.3.2...v2.4.0) (2026-06-22)


### Features

* **capture:** archive raw page bytes for brain_perceive ([#43](https://github.com/neverprepared/mcp-phantom-brain/issues/43)) ([bad62e2](https://github.com/neverprepared/mcp-phantom-brain/commit/bad62e2646a40b2c29020bb165bcc4cd284b0a5d))


### Bug Fixes

* **entities:** prompt for principal entities, not all mentions ([#42](https://github.com/neverprepared/mcp-phantom-brain/issues/42)) ([bc5da83](https://github.com/neverprepared/mcp-phantom-brain/commit/bc5da833055d34cdb385830513b2f955005eacda))

## [2.3.2](https://github.com/neverprepared/mcp-phantom-brain/compare/v2.3.1...v2.3.2) (2026-06-22)


### Bug Fixes

* **recall:** BM25 query tokenization + LLM-driven entity extraction ([#40](https://github.com/neverprepared/mcp-phantom-brain/issues/40)) ([ee3b502](https://github.com/neverprepared/mcp-phantom-brain/commit/ee3b502ce6867cd6a47c61d1e3c4af7afbea2f39))

## [2.3.1](https://github.com/neverprepared/mcp-phantom-brain/compare/v2.3.0...v2.3.1) (2026-06-22)


### Bug Fixes

* **ingest-bulk:** default Kind+MemoryType per source; parse yaml-typed dates ([#38](https://github.com/neverprepared/mcp-phantom-brain/issues/38)) ([2d073ea](https://github.com/neverprepared/mcp-phantom-brain/commit/2d073ea9028d1a6bda829757cd24f6a5c5be53d9))

## [2.3.0](https://github.com/neverprepared/mcp-phantom-brain/compare/v2.2.1...v2.3.0) (2026-06-22)


### Features

* **memory:** structured memory classification (kind, memory_type, source, references, captured_at) ([#36](https://github.com/neverprepared/mcp-phantom-brain/issues/36)) ([e93cad2](https://github.com/neverprepared/mcp-phantom-brain/commit/e93cad28e2caf497f7a14f7f55d50f88075de0a0))

## [2.2.1](https://github.com/neverprepared/mcp-phantom-brain/compare/v2.2.0...v2.2.1) (2026-06-22)


### Bug Fixes

* **entities:** filter corpus boilerplate + numeric-prefix headings ([#34](https://github.com/neverprepared/mcp-phantom-brain/issues/34)) ([74fd135](https://github.com/neverprepared/mcp-phantom-brain/commit/74fd135629cde187f2e520adff3aa4d2841f1b30))

## [2.2.0](https://github.com/neverprepared/mcp-phantom-brain/compare/v2.1.0...v2.2.0) (2026-06-22)


### Features

* **docker:** bundle claude CLI in daemon image ([#32](https://github.com/neverprepared/mcp-phantom-brain/issues/32)) ([64b9858](https://github.com/neverprepared/mcp-phantom-brain/commit/64b98580c51adc63fc0e4d72ff1a9b2beb93d122))

## [2.1.0](https://github.com/neverprepared/mcp-phantom-brain/compare/v2.0.2...v2.1.0) (2026-06-22)


### Features

* **canonicalize:** standardise stored filenames ([#29](https://github.com/neverprepared/mcp-phantom-brain/issues/29)) ([bfc9270](https://github.com/neverprepared/mcp-phantom-brain/commit/bfc9270484dc46af42c65ae93d1d5e11436c3b8f))

## [2.0.2](https://github.com/neverprepared/mcp-phantom-brain/compare/v2.0.1...v2.0.2) (2026-06-22)


### Bug Fixes

* **osearch:** export tarball uses _index/ subdir layout ([#27](https://github.com/neverprepared/mcp-phantom-brain/issues/27)) ([8fbed31](https://github.com/neverprepared/mcp-phantom-brain/commit/8fbed31185f87aa0bcea7e9ed6360c60425a7995))

## [2.0.1](https://github.com/neverprepared/mcp-phantom-brain/compare/v2.0.0...v2.0.1) (2026-06-22)


### Bug Fixes

* **brain,cli:** make daemon-client timeout configurable ([#25](https://github.com/neverprepared/mcp-phantom-brain/issues/25)) ([cf5a2ea](https://github.com/neverprepared/mcp-phantom-brain/commit/cf5a2ea5e8e8c71fc6d87ac763deb632632f2cc2))
* **canonicalize:** dedup hash excludes frontmatter (SumBody) ([#26](https://github.com/neverprepared/mcp-phantom-brain/issues/26)) ([478b984](https://github.com/neverprepared/mcp-phantom-brain/commit/478b984a9f2cdcdf7997b725d906cd9b7b0ca90e))
* **server:** wire MinIOBackend as the AttachmentStore ([#23](https://github.com/neverprepared/mcp-phantom-brain/issues/23)) ([a425b0f](https://github.com/neverprepared/mcp-phantom-brain/commit/a425b0f83207c13019d147d032bf3463669c4bdf))

## [2.0.0](https://github.com/neverprepared/mcp-phantom-brain/compare/v1.2.1...v2.0.0) (2026-06-22)


### ⚠ BREAKING CHANGES

* Phase 6 — OpenSearch backend + per-agent SQLite caches (v2.0.0) ([#20](https://github.com/neverprepared/mcp-phantom-brain/issues/20))

### Features

* **docker:** add opensearch-dashboards to compose stack ([#22](https://github.com/neverprepared/mcp-phantom-brain/issues/22)) ([7ac2d9b](https://github.com/neverprepared/mcp-phantom-brain/commit/7ac2d9b1ab260e73c97c34cd1e97c29fc114d82f))
* **docker:** compose stack — MinIO + phantom-brain daemon together ([#15](https://github.com/neverprepared/mcp-phantom-brain/issues/15)) ([0683d9c](https://github.com/neverprepared/mcp-phantom-brain/commit/0683d9cb1f896510f3173aa8c12c0949d41ec815))
* Phase 6 — OpenSearch backend + per-agent SQLite caches (v2.0.0) ([#20](https://github.com/neverprepared/mcp-phantom-brain/issues/20)) ([80d292f](https://github.com/neverprepared/mcp-phantom-brain/commit/80d292f7172fddeea97a580ca0e671ffcae0baed))


### Bug Fixes

* **docker:** drop redundant `command:` from compose pbrainctl service ([#18](https://github.com/neverprepared/mcp-phantom-brain/issues/18)) ([245a89f](https://github.com/neverprepared/mcp-phantom-brain/commit/245a89f030a0caf4d49cf509790c65cb488e502d))
* **docker:** port-forward 9998 instead of network_mode: host ([#19](https://github.com/neverprepared/mcp-phantom-brain/issues/19)) ([2fd75c5](https://github.com/neverprepared/mcp-phantom-brain/commit/2fd75c594f0edcb36b4a5d746016ec6da77094ac))
* **docker:** ubuntu base + vendor linux/arm64 sqlite-vec ([#17](https://github.com/neverprepared/mcp-phantom-brain/issues/17)) ([be04172](https://github.com/neverprepared/mcp-phantom-brain/commit/be041723b88996e1cda4ff7df941a1438cf6dc95))
* **mcp:** dedup attach by blob SHA, not stub SHA ([#21](https://github.com/neverprepared/mcp-phantom-brain/issues/21)) ([839904b](https://github.com/neverprepared/mcp-phantom-brain/commit/839904b34ea0426db65d3469c04a9f1b887f951d))

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
