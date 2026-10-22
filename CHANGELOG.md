# Changelog

All notable changes to GoClaw are documented here. For full documentation, see [docs.goclaw.sh](https://docs.goclaw.sh).

## Unreleased

### Changed

- **Bitrix24 channel migrated to imbot v2 messaging API** — outbound text now uses
  `imbot.v2.Chat.Message.send` (replacing `imbot.message.add`); bot verification/lookup
  uses `imbot.v2.Bot.list` (replacing `imbot.bot.list` + the legacy `imbot.list` fallback);
  bot teardown uses `imbot.v2.Bot.unregister` (replacing `imbot.unregister`). Bot
  registration intentionally stays on v1 `imbot.register` — v2 `imbot.v2.Bot.register`
  changes the event-delivery model (per-event handler URLs → `eventMode`), which would
  require rewriting the inbound event parser. No user-facing behavior change.

### Added

- **Per-turn model routing (`agents.other_config.model_routing`)** — an agent can now pick a
  different model for a turn *before* the turn runs, from what that turn asks for, instead of only
  reacting to a failed call. This fills a real hole: `model_fallback` is failure-driven, the media
  chains pick a backend by capability, cron/heartbeat/webhook/subagent name a model at their own
  call sites, and nothing at all read the request. No skill could close it either — SKILL.md
  metadata carries only `name` and `description`, hooks have no pre-model event, and no registered
  tool lets an agent change its own next-turn model.
  A policy is an **ordered tier list and the first match wins**, because GoClaw does not know which
  model is stronger and no provider publishes that: ranking stays the operator's decision, written
  down as list order plus thresholds (`min_runes`, `min_tokens`, `requires_code`, `requires_media`,
  `run_kinds`) rather than guessed at in a table that would be wrong quietly. Every declared
  condition must hold; a tier with only a model matches everything, which is how "always use this
  model" is expressed. Rules answer first, and the **optional classifier** — one 24-token,
  thinking-off call to a model the operator names, `Purpose: "model-routing"`, run through the same
  usage-cap preflight as the agent loop — only runs when no rule matched. It reads the tier list
  and each tier's own `hint` text and answers a tier name or `BASELINE`; nothing in the system
  invents a difficulty score. Routing rides the existing `ModelOverride`/`ProviderOverride` pair
  (the seam is now `effectiveRunModelProvider`, extracted so it can be tested), so a caller that
  already named a model wins outright and the budget pre-estimate, reasoning-effort resolution and
  provider unwrapping all see one coherent choice. That also means a routed turn leaves the
  fallback wrapper behind, exactly as every other override does — routing is a cost/quality knob,
  not a reliability one, and the docs now say so where fallback is documented.
  A tier may name another provider; that lookup is tenant-scoped (`GetForTenant(agent.TenantID, …)`
  with the usual master fallback), so one tenant's policy cannot reach another tenant's
  credentials. Every failure path — unresolvable provider, classifier timeout, unparseable answer,
  unknown tier name, usage-cap refusal — keeps the agent's configured model and logs at Debug,
  because a router that can break a conversation is worse than no router; a *failing* classifier is
  paused for a minute rather than cached as an answer, so one blip does not suppress routing for
  the whole TTL. Decisions are auditable without a schema change, in three layers: one
  `model_routing: decision` Info line naming tier/from/to/source/reason/signals, a
  `model_route:<tier>` trace tag (which is why routing happens before the trace row is written), and
  usage events, which already record the model that actually ran, so routed cost lands in the
  existing dashboard. Per-agent **process** counters round it out behind a read-only
  `WS agents.routingStats`: evaluated / routed / no-change / provider-missing, a per-tier breakdown,
  and the classifier's calls, cache hits and failures — surfaced in the card because three states
  that look identical as bare zeros are not: a policy saved but not run since the edit (updating an
  agent drops the cached loop, hence the separate `hasPolicy` and `cached` flags), a policy whose
  thresholds nothing clears, and a policy that fires and routes to the model the agent already uses.
  They reset with the loop on purpose; writing them per turn to a table was considered and rejected
  as paying DB churn for a question that only lives as long as the tuning session.
  Editing is in the web panel (Agents → agent → Advanced → *Model routing*, drag-to-order rows) —
  the desktop app has no routing card, consistent with `model_fallback` and `delivery_behavior`
  also having none there, and a desktop save preserves the key rather than clobbering it.
  `store.ValidateModelRouting` refuses the payloads that would parse but do nothing (enabled with no
  tier naming a model, >8 tiers, non-numeric thresholds, a classifier without a model) on both the
  WS and HTTP update paths, while the read path stays deliberately tolerant; `agents.create`
  validates nothing, which is pre-existing for every `other_config` key and now written down.
  Verified by unit tests only (46 new, across `internal/store`, `internal/agent`,
  `internal/gateway/methods` and the panel payload module) plus both build tags; no decision has
  ever been made against a real provider, the
  panel card has never been driven in a browser, and the classifier's answer quality is unmeasured.

- **Image generation works with URL-returning providers (Zhipu CogView, SiliconFlow, and the
  OpenAI-compatible gateways generally)** — the standard `/images/generations` path asked for
  `response_format=b64_json` and then read exactly one field, `data[0].b64_json`. Domestic
  providers ignore that request and hand back a link: CogView documents `data[].url` (and its own
  parameter table types `data` as a list of strings), SiliconFlow does not even reuse the array
  name, answering `images[].url` with a `kind` of `generated` or `original`. Every one of them
  therefore ended in `no image data in response`. `internal/tools/create_image_response.go` now
  reads all five shapes — `data[].b64_json`, `data[].url`, `data` as plain strings, `images[].url`
  (preferring `generated` over an echoed `original` input), and a `data:image/...;base64,` value
  arriving in either field — and downloads the location gateway-side, which is not optional: those
  links expire in 24 hours (Bailian, 302.AI) to 30 days (CogView), and the workspace copy is the
  only durable artifact. The fetch keeps the reference-image path's SSRF posture (validate, pin the
  resolved IP, follow a redirect only while it stays on the same host, 20 MB cap), so a hostile
  `base_url` cannot turn the gateway into a metadata-endpoint reader. Two related gaps in the same
  path are closed: a chain entry's vendor parameters now reach the request body for a documented
  whitelist (`size`/`quality` for OpenAI and Zhipu, `image_size`/`num_inference_steps`/
  `guidance_scale`/`sfw` for SiliconFlow) instead of being dropped so every answer came back
  square, and `response_format` is now model-aware and overridable: it stays `b64_json` for
  everything that accepts the field but is omitted for the `gpt-image*` / `chatgpt-image*` families,
  which reject it outright (Azure's reference says the series does not support it and always returns
  base64) — GoClaw's default OpenAI image model is one of them, so the stock OpenAI path answered
  `400 Unknown parameter: 'response_format'`. An entry with no model
  now falls back to the provider's own default rather than sending `"model": ""`. Finally, adding a
  provider in the panel is enough on its own: the image chain ends with a wildcard that appends
  every other registered **API-key** provider after the known vendors. OAuth/Codex credentials are
  never auto-tried (a person's subscription is not something to spend on a guess) and a provider
  with no default model is skipped. The panel exposes the three fields under Built-in Tools →
  create_image → chain entry → Settings for any provider registered as `openai_compat`. Tools whose
  priority list omits the wildcard (`create_video`, `read_*`) keep their closed vendor lists.


- **Official QQ bot channel** — a WebSocket-bot integration (`internal/channels/qq`)
  configured as a channel instance in the panel: credentials encrypted at rest,
  `dm_policy` / `group_policy` / `require_mention` / allow-lists per scope.
  Inbound attachments are actually downloaded (20 MB cap, SSRF checked before the
  request and on every redirect hop) and voice notes enter the transcription
  chain like other channels.

- **WeChat personal-account channel with in-panel QR activation** — the iLink
  bot flow (`internal/channels/weixin`) no longer needs a terminal: `weixin.qr.start`
  hands a PNG (base64) and status transitions back to the channel wizard,
  which polls until the scan is confirmed and then persists `bot_token` /
  `ilink_bot_id` / `ilink_user_id` encrypted and hot-loads that instance. Redirects
  during activation are pinned to `https` hosts under `weixin.qq.com`. Protocol
  limits stay explicit rather than being papered over: the account cannot be
  messaged unsolicited (cron, heartbeat and approval pushes return
  `ErrNoReplyContext`) and group inbound is dropped.

- **WeChat inbound media through the encrypted CDN, with GoClaw's own ASR fallback** —
  images, files, video and voice notes are now fetched, AES-ECB decrypted with the
  per-message key, and attached to the pipeline message; the `[图片]`-style placeholder is
  dropped for what arrived and kept for what did not. Voice keeps using iLink's own
  `voice_item.text` when the service supplies it and only spends an ASR call when it does
  not, in which case the SILK blob goes through the shared transcription chain (the same
  host SILK decoder QQ voice needs). The step is bounded — four objects per message, 25 MB
  per object, 45 s overall so a slow CDN cannot stall long-polling for every other peer —
  and the CDN **host is pinned**: `cdn_base_url` (default
  `https://novac2c.cdn.weixin.qq.com/c2c`) is the only host contacted, a peer-supplied
  `full_url` is honored only when it names that host and is otherwise rebuilt from the
  object's `encrypted_query_param`, and redirects that leave it are refused after an SSRF
  check. A key that is advertised but unusable, or bytes that do not sniff as the type the
  item claims, leave the placeholder rather than storing an unopenable file.

- **Outbound attachments and TTS on the QQ and WeChat channels** — both adapters now upload
  files instead of announcing that they cannot. QQ posts the bytes themselves
  (`file_data`, base64) to `POST /v2/{users|groups}/{id}/files` and sends the returned
  `file_info` as one `msg_type=7` message per attachment, which corrects the note in
  earlier revisions — here and in the platform docs — that a QQ-fetchable `url` was
  required. Audio is uploaded as `file_type=3` **voice**, because the media overview lists
  silk/mp3/wav/ogg and renders a voice bar for them. WeChat iLink signs an upload slot with
  `getuploadurl` — the AES key is generated by the sender and the size signed is the *padded
  ciphertext*, not the file — POSTs the AES-ECB body, and then references the object with the
  download parameter that arrives in the **`X-Encrypted-Param` response header**; the key
  travels in the message as base64 of the hex, which is the shape inbound media already
  parses, so the two directions are asserted against each other in tests. Image, video and
  file are sent there: the upload enum declares `VOICE=4` but no implementation is known to
  use it, so a spoken WeChat reply leaves as a file card rather than pretending a voice bar
  was built. On both channels media is delivered **after** the text so a small passive-reply
  budget is spent answering the human first, a failed attachment is announced
  (`[附件发送失败: …]` with a filename, never a path) instead of vanishing, and an
  attachment-only reply whose every file failed returns an error. Both are now
  media-capable, which also means `tts.auto` reaches them: previously a `[[tts]]`-tagged
  answer was delivered with the markers spelled out and no audio at all. Local paths only —
  unlike channels whose servers fetch the file, these two read bytes from disk, so a webhook
  `media_url` is refused inside the adapter and documented as such.


- **Community skill sources (ClawHub and a Git host), off by default** —
  `skills.registries.<source>.enabled` is the only way to turn a source on, so a
  gateway never makes an outbound fetch that its operator did not ask for. Both
  clients pin https (plain http only for loopback) and refuse a redirect that
  leaves the configured host, and tokens travel as secrets
  (`config_secrets` keys `skills.registries.<source>.token`, env
  `GOCLAW_SKILLS_CLAWHUB_TOKEN` / `GOCLAW_SKILLS_GITHUB_TOKEN`), never in
  config.json.
  Four endpoints — `GET /v1/skills/registry/{sources,search}`,
  `POST /v1/skills/registry/{inspect,install}` — back a new **Community** dialog on
  the Skills page: search, then read the analysis (files and sizes, executables,
  declared dependencies, dependencies missing on this host, whether it needs a
  shell, the security findings, and a SKILL.md excerpt) before installing.
  A registry install goes through exactly the guards a ZIP upload is held to, lands
  in the tenant `skills-store/<slug>/<version>/` with a `.skill-origin.json`
  sidecar, and creates **no grants** and **no dependency installs**; a skill whose
  declared binaries are absent registers `archived` rather than silently half-working.
  A content-guard finding is a hard refusal with no override flag, and a slug that
  belongs to a system skill or to another user in the tenant is refused instead of
  being repointed. Install requires owner + master scope.
  Agents get a `skill_install` tool (standard edition, and only while a source is
  enabled) built as a two-call protocol: `inspect` records a single-use, 10-minute
  proposal for the conversation and returns the analysis to relay, and `install`
  refuses without one. Turns with no human in them — cron, heartbeat, subagent, team
  task — are refused before any outbound request, as are non-owner actors and
  non-master tenants.


- **Speech-to-text on inbound voice messages** — `audio.stt` is now the real
  surface: `provider` (`openai_compat` | `elevenlabs` | `proxy`) plus
  `base_url` / `model` / `language` / `fallback` / `timeout_ms`, with
  `audio.stt.api_key` stored as a secret (`config_secrets`, or
  `GOCLAW_AUDIO_STT_API_KEY`, which wins) and never written to config.json. A
  channel transcribes only when a usable backend exists (`STTAvailable`), so an
  unconfigured gateway says so once at Debug per message instead of failing or
  spamming warnings. Feishu, Telegram, Discord and WhatsApp (opt-in, because of
  end-to-end encryption) go through `audio.Manager`; the panel gets a
  Speech-to-Text card under Config → Integrations. Providers are built after the
  config overlay, so a panel-set backend actually registers — and changing it
  still needs a restart. QQ and WeChat voice arrive as SILK: there is no pure-Go
  decoder, so the gateway looks for a host `silk_v3_decoder`, `silk_decoder` or
  `ffmpeg` (`audio.stt.silk_decoder` pins one, `off` disables the search) and
  says at startup whether it found one.


- **Embedding vector width is deployment-configurable** — the truth is the
  database's own column width (`store.EmbeddingSchemaReader`), read at startup,
  not a hard-coded constant. `goclaw migrate embed-dims --to N` re-dimensions
  every pgvector column (drops the covering HNSW indexes, clears and re-types the
  column, replays the index definitions — one transaction per column), with
  `--dry-run` listing width / embedded rows / index count first. The gateway
  refuses to start when a provider's `embedding.dimensions` disagrees with the
  column and prints the exact command, and the same operation is available from
  the panel's Embedding section (master scope, dry-run first, typed
  confirmation). An *empty* dimensions means "send no truncation parameter", so
  providers that reject it (SiliconFlow bge-m3) still work; the resolved provider
  is probed once at startup and a measured disagreement also aborts startup with
  both fixes spelled out.

- **Task detail dialog shows the full task UUID with one-click copy** — the
  short identifier (`T-015-cc8e`) carries only the last four hex characters of
  the UUID, while the agent-facing `team_tasks` tool and RPCs take the full
  UUID, so a human had no way to name a task to the lead from the dashboard.

- **Dashboard can cancel and retry stuck team tasks** — new WS RPCs
  `teams.tasks.cancel` (any task not yet completed/cancelled; optional reason is
  posted as a comment) and `teams.tasks.retry` (stale / failed / cancelled /
  in_review, and blocked tasks that nothing blocks any more; a human comment is
  required and is both posted on the task and appended to the assignment prompt).
  Task detail dialog gets Retry and Cancel buttons. Previously these transitions
  were reachable only through the lead agent's `team_tasks` tool, so a task that
  ended up blocked or stale could not be recovered from the UI (#506).

- **Behavior UX sidecar delivery overrides** — Adds sidecar-generated Quick
  Acknowledgement and Intermediate Replies with provider/model, timeout, token,
  and char caps. Effective config resolves Channel > Agent > Workspace, with
  agent overrides stored in `other_config.delivery_behavior`.

- **Built-in skill `workspace-organizing`** — closes #71. Discipline skill that
  teaches agents to keep personal, team, and delegate workspaces tidy.
  Enforces a purpose-based folder convention with two modes: flat
  (`notes/`, `data/`, `outputs/`, `scripts/`, `archive/`) for ad-hoc work
  and project (`projects/<slug>/{docs,assets,source,reports,research}/`)
  for named multi-file work. Per-agent namespacing under
  `shared/<agent_key>/` prevents collisions in team workspaces. Integrates
  pre-write discovery via `vault_search`, `memory_search`, and
  `knowledge_graph_search` to surface related files before writing and
  avoid duplicates; documents Vault scope mirroring and id-routing rules.
- **Bitrix24 channel 2-way media (file) transfer** — Inbound media downloads via
  `imbot.v2.File.download` (one-time authenticated URL) with MIME preservation for
  images, PDFs, audio, and video. Outbound uploads via `imbot.v2.File.upload` (base64).
  Shared `media_max_mb` config knob (default 20 MB) caps both directions. Requires
  `imbot` OAuth scope (no `disk` scope needed). Inbound handled by new
  `internal/channels/bitrix24/download.go`; outbound by `send_media.go`. New
  `BaseChannel.HandleMessageMedia()` method centralizes media-aware message handling.
  See `docs/05-channels-messaging.md` § 16 (Bitrix24) for configuration.

- **Skill agent manage grants** — Adds per-agent skill edit/delete grants with
  backend checks, HTTP/WS support, SQLite and PostgreSQL schema updates, and web
  dashboard controls for granting and revoking manage access.

- **Packages Update Flow (Phase 2a: pip + npm)** — closes #900 (Phase 2a). Extends
  Phase 1 update infrastructure to pip and npm package sources. `/v1/packages/updates`
  now returns mixed-source results with an `availability: {github, pip, npm}` map.
  Multi-source UI with per-source filter pills; unavailable sources (binary not on PATH
  or Lite edition) hidden automatically. apk deferred to Phase 2b.
  See `docs/packages-pip-npm.md` for command matrix, runbook, and min versions.

- **Packages Update Flow (Phase 1: GitHub binaries)** — closes #900. Proactive
  "N updates available" badge + per-row `[Update]` + `[Update All]` on the
  Runtime & Packages page. Backend endpoints under `/v1/packages/updates*`
  (master-scope). ETag-aware polling (304 responses don't burn rate limit),
  stale-while-revalidate cache, atomic two-phase `.bak` swap with rollback.
  Pre-release detection via regex + GitHub API flag; semver ordering via
  `golang.org/x/mod/semver`; non-semver tags use string-inequality fallback
  with downgrade protection. WebSocket events `package.update.*` for owner
  clients. See `docs/packages-github.md` § "Updating Installed Packages".

### Changed

- **Behavior UX simplification** — Retires user-facing Tool Status Messages and
  deterministic tool-status channel text. Show Reasoning remains separate for
  debugging/testing, while Quick Acknowledgement and Intermediate Replies are
  delivery-only sidecar messages. Legacy `block_reply` config remains readable
  as an inherited Intermediate Replies default but is no longer exposed as a
  separate Web UI control.

- **ChatGPT Subscription (OAuth)** — default model and backend-owned model catalog
  now prefer `gpt-5.5`, with reasoning metadata and context-window defaults updated
  for provider-first model selection.

### Fixed

- **Speech-to-text: the configured timeout, the provider roles, and two dialogs that
  configured nothing** — `audio.stt.timeout_ms` is now the per-transcription deadline
  (`Manager.STTTimeout(fallback)`); every channel had hard-coded 10 s, so raising it to 30 s — which
  is exactly what a long voice note needs — changed nothing, and the SILK decoder's own 15 s ceiling
  could never be reached. The startup chain log said `role=primary` for every provider because the
  role was read *after* the entry was appended to the chain, so a configured `fallback` was invisible
  in the log. Telegram was the one channel that did not check `STTAvailable`, so a gateway with no
  STT backend logged a warning per voice message instead of the documented single Debug line. The
  `proxy` provider accepted a model and a language hint and dropped them in silence (its protocol
  carries neither) — it now says so.
  **Two panels were decoys.** Built-in tools → stt → Settings saved a provider list, an ElevenLabs
  key and a proxy URL into `builtin_tools[stt].settings`, of which the only key anything reads is
  `whatsapp_enabled`: a user could configure speech recognition there and nothing would ever be
  transcribed. The desktop dialog now edits `audio.stt` itself over the same `config.get` /
  `config.patch` methods the web card uses (desktop has no Integrations screen, and channels *do* run
  in the desktop build), and the web dialog keeps the WhatsApp opt-in with a link to the real card.
  The nine dead locale entries were removed from the web tree and replaced with the pointer copy.
  `audio.stt.silk_decoder` is seeded into `system_configs` like the rest of the STT keys.
  Documentation corrected against the code: `docs/05` still claimed four channels transcribe and that
  "QQ drops inbound attachments", and its capability matrix still said QQ/WeChat cannot send media —
  all three contradicted what shipped this week. And the SILK story said the search order falls back
  to `ffmpeg`: **ffmpeg cannot decode SILK and the decoder only runs on payloads that sniff as SILK**,
  so a board with ffmpeg alone reports "decoder available" and then fails every voice note — the
  comment, the docs and the installation walkthrough now say so plainly. One investigated-and-reverted
  change: an empty audio path returning `("", nil)` looked like a swallowed error, but it is a
  deliberate no-op (another provider handed the same nothing would fail identically and only add log
  noise), so it is now covered by tests in `internal/channels/media` instead of being altered.


- **Channel reload no longer hangs after a failed channel start (#1627)** —
  `PendingHistory.StopFlusher` waited on the flush goroutine's `stopped`
  channel even when `StartFlusher` had never run. Telegram starts its flusher
  only after `getMe` and long polling succeed, so a Telegram instance whose
  `Start()` failed (network down at boot) blocked `InstanceLoader.Reload`
  forever inside `Stop()` with the loader mutex held — every later channel
  save from the dashboard queued silently behind it and only a process
  restart recovered. The same wait sat in `LoadInstanceByID`, loader `Stop`
  and the timed-out-start cleanup path. `StopFlusher` now returns immediately
  when the flusher was never started and is idempotent (`sync.Once` around
  `close(stopCh)`, so a second `Stop()` no longer panics with
  `close of closed channel`); `StartFlusher` spawns at most one loop.

- **Quick Acknowledgement generated mode** — Generated acknowledgements now use
  the sidecar delivery generator instead of always falling back to fixed
  templates. Sidecar failures stay non-blocking and fall back to templates.

- **Intermediate Replies are sidecar-generated** — Tool-call progress no longer
  appends fixed "I'll use ..." text or relies on main-pipeline assistant content.
  Visible progress is generated from bounded delivery metadata and is kept out
  of session history.

- **Multi-attachment messages no longer trigger N agent replies (#63).**
  Three coalescing surfaces hardened so a single user action produces ONE
  agent run regardless of how the platform delivers attachments:
  1. **Bus debouncer** — removed the media-bypass shortcut that fired
     immediately for any message with attachments; media now goes through
     the same per-(channel, chatID, senderID, agentID) silence window as
     text. Media-floor (`max(configured, mediaFloor)`) guarantees a
     minimum window when attachments are present so multi-file uploads
     coalesce. Dedup seed prevents the same `MessageID` from being
     buffered twice on bursty arrivals.
  2. **Web Chat debouncer** (`internal/gateway/methods/chat_debounce.go`) —
     parallel structure for `/v1/chat/completions` streaming: per-session
     buffer + media floor + Take/Discard semantics for flush/cancel
     control. Merges queued payloads at flush time (latest params win;
     text concatenated newline-separated).
  3. **Telegram album aggregator** (`internal/channels/telegram/album_aggregator.go`) —
     channel-layer coalescing for albums. Telegram delivers a media-group
     (multiple photos/videos shared as one user action) as N separate
     `Message` updates sharing a `MediaGroupID`. The aggregator buffers
     by `(chatID, MediaGroupID)` after all access gates pass, pins the
     sender on first arrival as a security tripwire, and dispatches ONE
     `processResolvedMessage` call with all members on a 500ms silence
     window. `Stop()` synchronously drains pending buffers before
     `pollCancel` so in-flight albums always publish.

  Cross-surface invariants (see CONTRIBUTING.md → "Multi-attachment
  coalescing"): no media bypass, media floor on every surface,
  drop-and-log dual caps, no `time.Timer.Reset` (use `AfterFunc` +
  `Stop`), sender pin on first arrival, post-stop pushes rejected
  with warn log.

- **Upstream critical security remediation** — hardens gateway no-token fallback,
  Feishu/Lark and Pancake webhooks, sandbox path/write handling, tenant-admin
  checks for mutable HTTP surfaces, and Lite hook schema migration verification.

- **SecureCLI runtime npm binaries** — binary discovery and credentialed exec now
  resolve tools installed under the GoClaw runtime directories, including
  `{runtimeDir}/npm-global/bin`, and support single-binary npm package aliases
  such as `openrouter-cli` exposing `orc`.

### Breaking Changes

- **Context pruning now opt-in.** Previously tool-result trimming ran by default
  for all providers; now requires explicit `contextPruning.mode: "cache-ttl"` in
  `config.agents.defaults` to enable. Matches upstream TS design and prevents
  silent prompt-cache invalidation on Anthropic.

  Migration — add to `config.json5`:
  ```json5
  agents: {
    defaults: {
      contextPruning: { mode: "cache-ttl" }
    }
  }
  ```

### New Features

- **Pancake private-reply (comment → DM).** Enables a one-time DM to commenters
  after the public reply. Stateless on GoClaw side — no DB dedup table, no
  in-memory state:
  - Config: `features.private_reply` (bool) + `private_reply_message` (text).
  - **Template variables** `{{commenter_name}}` and `{{post_title}}` with
    literal-replace semantics (pre-sanitizes `{{`/`}}` from var values to
    prevent var-in-var substitution).
  - Empty `private_reply_message` → English fallback constant.
  - **Dedup strategy**: webhook-level comment_id dedup (already in
    `comment_handler.go`) + Facebook's per-comment idempotent `private_replies`
    endpoint handle duplicates platform-side. No GoClaw state required.
  - No DB migration.

### Improvements

- **Context pruning cleanup.** Removed redundant Pass 0 (per-result 30% guard),
  deduplicated double prune call per iteration, added SanitizeHistory to
  PruneStage for broken tool_use/tool_result pair cleanup.
- **Context pruning config backfill (migration).** Agents with existing custom
  `context_pruning` config (e.g., `softTrimRatio`, `keepLastAssistants`) but
  missing a `mode` field get auto-backfilled with `mode: "cache-ttl"` to
  preserve their intent after the opt-in flip. Rows with NULL config stay
  NULL (new opt-in default applies). PG migration 51; SQLite schema v19.
- **Pancake channel metadata routing.** Whitelist in
  `internal/channels/routing_metadata.go` now preserves `post_id` and
  `display_name` across the inbound → outbound hop so the private-reply
  template variables survive the agent pipeline round-trip.

### Fixed

- **Skill grant tenant isolation.** Agent skill grants now validate both the
  skill and agent tenant scope before insert, revoke, grant listing, or
  can-manage checks. Visibility auto-promote/auto-demote updates are scoped to
  the calling tenant or system skills so one tenant cannot mutate another
  tenant's skill.

- **Agent provider switching.** Saving an agent after changing provider/model now
  handles cleared ChatGPT OAuth routing config without writing SQL NULL into
  NOT NULL JSON config columns.

## Project Status

### Implemented & Tested in Production

- **Agent management & configuration** — Create, update, delete agents via API and web dashboard. Agent types (`open` / `predefined`), agent routing, and lazy resolution all tested.
- **Telegram channel** — Full integration tested: message handling, streaming responses, rich formatting (HTML, tables, code blocks), reactions, media, chunked long messages.
- **Seed data & bootstrapping** — Auto-onboard, DB seeding, migration pipeline tested end-to-end.
- **User-scope & content files** — Per-user context files (`user_context_files`), agent-level context files (`agent_context_files`), virtual FS interceptors, per-user seeding (`SeedUserFiles`), and user-agent profile tracking all implemented and tested.
- **Core built-in tools** — File system tools (`read_file`, `write_file`, `edit_file`, `list_files`, `search`, `glob`), shell execution (`exec`), web tools (`web_search`, `web_fetch`), and session management tools tested in real agent loops.
- **Memory system** — Long-term memory with pgvector hybrid search (FTS + vector) implemented and tested with real conversations.
- **Agent loop** — Think-act-observe cycle, tool use, session history, auto-summarization, and subagent spawning tested in production.
- **WebSocket RPC protocol (v3)** — Connect handshake, chat streaming, event push all tested with web dashboard and integration tests.
- **Store layer (PostgreSQL)** — All PG stores (sessions, agents, providers, skills, cron, pairing, tracing, memory, teams) implemented and running.
- **Browser automation** — Rod/CDP integration for headless Chrome, tested in production agent workflows.
- **Lane-based scheduler** — Main/subagent/team/cron lane isolation with concurrent execution tested. Group chats support up to 3 concurrent agent runs per session with adaptive throttle and deferred session writes for history isolation.
- **Security hardening** — Rate limiting, prompt injection detection, CORS, shell deny patterns, SSRF protection, credential scrubbing all implemented and verified.
- **Web dashboard** — Channel management, agent management, pairing approval, traces & spans viewer, skills, MCP, cron, sessions, teams, and config pages all implemented and working.
- **Prompt caching** — Anthropic (explicit `cache_control`), OpenAI/MiniMax/OpenRouter (automatic). Cache metrics tracked in trace spans and displayed in web dashboard.
- **Agent delegation** — Inter-agent task delegation with permission links, sync/async modes, per-user restrictions, concurrency limits, and hybrid agent search. Tested in production.
- **Agent teams** — Team creation with lead/member roles, shared task board (create, claim, complete, search, blocked_by dependencies), team mailbox (send, broadcast, read). Tested in production.
- **Evaluate loop** — Generator-evaluator feedback cycles with configurable max rounds and pass criteria. Tested in production.
- **Delegation history** — Queryable audit trail of inter-agent delegations. Tested in production.
- **Skill system** — BM25 search, ZIP upload, SKILL.md parsing, and embedding hybrid search. Tested in production.
- **MCP integration** — stdio, SSE, and streamable-http transports with per-agent/per-user grants. Tested in production.
- **Cron scheduling** — `at`, `every`, and cron expression scheduling. Tested in production.
- **Docker sandbox** — Isolated code execution in containers. Tested in production.
- **Text-to-Speech** — OpenAI, ElevenLabs, Edge, MiniMax providers. Tested in production.
- **HTTP API** — `/v1/chat/completions`, `/v1/agents`, `/v1/skills`, etc. Tested in production. Interactive Swagger UI at `/docs`.
- **API key management** — Multi-key auth with RBAC scopes, SHA-256 hashed storage, show-once pattern, optional expiry, revocation. HTTP + WebSocket CRUD. Web UI for management.
- **Hooks system** — Event-driven hooks with command evaluators (shell exit code) and agent evaluators (delegate to reviewer). Blocking gates with auto-retry and recursion-safe evaluation.
- **Media tools** — `create_image` (DashScope, MiniMax), `create_audio` (OpenAI, ElevenLabs, MiniMax, Suno), `create_video` (MiniMax, Veo), `read_document` (Gemini File API), `read_image`, `read_audio`, `read_video`. Persistent media storage with lazy-loaded MediaRef.
- **Additional provider modes** — Claude CLI (Anthropic via stdio + MCP bridge), Codex (OpenAI gpt-5.3-codex via OAuth).
- **Google Cloud Vertex AI provider** — Enterprise GCP integration via Vertex OpenAI-compatible endpoint. OAuth2 service account auth (inline JSON or file path) with automatic token refresh, plus Application Default Credentials (ADC) for GKE/Cloud Run/Compute Engine. Regional endpoints for data residency (e.g. `asia-southeast1`, `us-central1`). Addresses [#576](https://github.com/nextlevelbuilder/goclaw/issues/576).
- **Knowledge graph** — LLM-powered entity extraction, graph traversal, force-directed visualization, and `knowledge_graph_search` agent tool.
- **Memory management** — Admin dashboard for memory documents (CRUD, semantic search, chunk/embedding details, bulk re-indexing).
- **Persistent pending messages** — Channel messages persisted to PostgreSQL with auto-compaction (LLM summarization) and monitoring dashboard.
- **Heartbeat system** — Periodic agent check-ins via HEARTBEAT.md checklists with suppress-on-OK, active hours, retry logic, and channel delivery.

### Implemented but Not Fully Tested

- **Slack** — Channel integration implemented, not yet validated with real users.
- **Other messaging channels** — Discord, Zalo OA, Zalo Personal, Feishu/Lark, WhatsApp channel adapters are implemented but have not been tested end-to-end in production. Only Telegram has been validated with real users.
- **OpenTelemetry export** — OTLP gRPC/HTTP exporter implemented (build-tag gated). In-app tracing works; external OTel export not validated in production.
- **Tailscale integration** — tsnet listener implemented (build-tag gated). Not tested in a real deployment.
- **Redis cache** — Optional distributed cache backend (build-tag gated). Not tested in production.
- **Browser pairing** — Pairing code flow implemented with CLI and web UI approval. Basic flow tested but not validated at scale.
