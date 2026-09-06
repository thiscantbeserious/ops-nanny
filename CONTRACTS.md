# Component Contracts (Go)

## Conventions

Binding for every component. Where a component contract disagrees with this section, this section wins. One name, one owner, one algorithm per concern.

### C1. Module, layout, ownership

Module path: `github.com/thiscantbeserious/agentic-server-supervisor`. Go 1.25, `CGO_ENABLED=0`, stdlib only in the runtime path.

```
cmd/sentinel/main.go          subcommand dispatch + exit-code mapping (the ONLY os.Exit)
internal/config/              Config, Load(), the single env loader
internal/logging/             slog handler (C7)
internal/facts/               Facts wire types + facts.schema.json (embedded)
internal/report/              Report wire types + report.schema.json (embedded) + Validate
internal/dedup/               Key, EvidenceCore, the single normalizer
internal/journal/             journalctl exec + normalization + merge/dedup
internal/collect/             Run (tick + deep)
internal/analyze/             Run, prompt assembly, fallback, prompt/role.md (embedded)
internal/state/               Store: Process, History, Outbox{Add,Take,Ack}, Health
internal/notify/              Send, BuildPayload, Sanitize, SMTP fallback, SeedConfig
internal/runtime/             Tick, Loop, raw-alert path
test/container_test.go        build tag `container`
```

`internal/hostenv` and `internal/schema` do not exist. `go:embed` cannot escape its package directory, so embedded assets live **in** the package that owns them: `internal/facts/facts.schema.json`, `internal/report/report.schema.json`, `internal/analyze/prompt/role.md`. `supervisor/prompts/` and `supervisor/schemas/` are deleted; the image ships no prompt or schema files and `SENTINEL_HOME` is abolished (its preflight check is removed).

Single owners, no second implementation anywhere:

| Concern | Owner | Everyone else |
|---|---|---|
| dedup key | `internal/dedup` | imports `dedup.Key` |
| `/state/tick-seq` | `internal/runtime` (`tick`) | receives `seq int64` as an argument |
| `/state/outbox/` | `internal/state` | `tick` calls it; `notify` only sends |
| `/state/heartbeat` | `internal/state` | nobody else writes it |
| env parsing | `internal/config` | receives `*config.Config` |
| wire types | `internal/facts`, `internal/report` | import, never redefine |

### C2. CLI and exit codes

```
sentinel tick [--loop|--once] [--state-dir PATH]
sentinel collect [--deep zfs|smart|kernel|ras]
sentinel analyze                 # facts.json on stdin → report.json on stdout (debug)
sentinel state process|history [n]|outbox-add|outbox-take|outbox-ack <id>
sentinel notify [--dry-run] [--seed-config] [file]
sentinel health                  # compose healthcheck
sentinel --version
```

Flags: stdlib `flag.NewFlagSet(<sub>, flag.ContinueOnError)`, GNU-style `--long`, no single-dash aliases, no positional arguments except where listed. `--json` on `tick` does not exist.

One exit-code table for the whole binary (sysexits, overriding the per-contract `1`):

| Code | Meaning |
|---|---|
| 0 | success, including a suppressed tick and an empty outbox; `health` = heartbeat fresh |
| 1 | also: `health` with a stale or missing heartbeat (compose only needs non-zero; pinned so the test is written to the contract) |
| 1 | internal failure (marshal, stdout write, recovered panic) |
| 2 | `collect` failed ⇒ fallback report sent |
| 3 | `analyze` failed ⇒ fallback report sent |
| 4 | `notify` failed ⇒ payload enqueued |
| 5 | `state` failed ⇒ report sent unfiltered |
| 64 | usage error (bad flag, unknown subcommand, positional argument) |
| 65 | input is not valid JSON / not the expected shape |
| 69 | `$STATE_DIR` missing or unwritable |
| 78 | configuration error (malformed or out-of-range env var, missing required mount) |

`--once` returns the highest code reached. `--loop` returns 0 on SIGTERM/SIGINT and 78 on startup validation failure; nothing else terminates the loop. Only `main` calls `os.Exit`; every `Run` returns `(int, error)`.

### C3. Environment variables (complete; compose passes every one explicitly)

`internal/config.Load()` reads them once. Malformed, out-of-range, or non-numeric-where-numeric ⇒ **exit 78 naming the variable**, the silent-default policy of the collect/state contracts is dropped. Every duration value is parsed by `time.ParseDuration` with no rewriting of the input; every default in the table below is written in that syntax.

Ranges, binding for every numeric variable (a value outside its range is exit 78, same as a malformed one):

| Variable | Range |
|---|---|
| `TICK_INTERVAL` | 60–3600 |
| `RAW_ALERT_MAX_PRIORITY` | 0–7 (a syslog priority, C5) |
| `RAW_ALERT_MAX_LINES` | 1–20 |
| `HEARTBEAT_HOUR` | 0–23 (hour of day) |
| `RAW_ALERT_MARKER_TTL_HOURS` | 1–8760 (hours; 8760 = one year) |
| every other numeric variable | `> 0`; durations `> 0s` |
| every value that is **ever** used as a duration | additionally `<= 86400s` (24h), enforced by `Load` |

A variable's own row always wins over the catch-all row. `RAW_ALERT_MAX_PRIORITY=0` and `HEARTBEAT_HOUR=0` are therefore **legal**, priority 0 is `emerg`, the most important value in the raw-alert path, and hour 0 is midnight. Only variables falling under the catch-all row reject zero: a zero timeout, budget, history depth or outbox cap has no defined behaviour anywhere in this document. No numeric variable accepts a negative value.

These range rules apply to numeric variables only. `DEEP_ENABLED` is the `0`|`1` boolean and is validated as such, `DEEP_ENABLED=0` means disabled, not out of range.

The 24h bound exists because a second-valued variable is multiplied by `time.Second`: without an upper bound, a large value overflows `int64` nanoseconds and becomes a **negative** duration, which would make a timeout fire instantly instead of erroring. `STALE_ALERT_SEC` defaults to exactly `86400` (24h), so 24h is the ceiling, not a value below it. `RAW_ALERT_MARKER_TTL_HOURS` is a marker lifetime rather than a timeout and keeps its own row.

**`TICK_WINDOW` and `DEEP_WINDOW` are kept as both.** `Config` exposes the parsed `time.Duration` *and* the raw configured string (`TickWindowRaw`, `DeepWindowRaw`). `facts.meta.window` echoes the **raw** string, so a configured `10m` is reported as `10m` and not as `time.Duration.String()`'s `10m0s`. Re-rendering the value from the parsed duration is the "rewriting of the input" this section forbids, only in the output direction, and `meta.window` is not internal bookkeeping: it travels into the analyzer prompt and from there into text a human reads.

**The bound is on the variable, not on the Go type `Config` happens to store it in.** `STALE_ALERT_SEC`, `RENOTIFY_ALERT_SEC`, `RENOTIFY_WATCH_SEC` and `RAW_ALERT_REPEAT_SECONDS` stay `int` seconds in `Config` and only become durations in `state`/`runtime`, but `Load` bounds them at `86400` all the same. `internal/config` is the single loader (C1) and downstream components contain no env parsing and no re-validation: a rule enforced in two places is a rule that will drift out of sync, and the consumer that forgets it is the one that overflows. T5 and T6 therefore inherit already-valid values and add no bounds checks of their own.

| Name | Default | Owner |
|---|---|---|
| `TICK_INTERVAL` | `300` (60–3600, s) | runtime |
| `TICK_WINDOW` | `10m` (validated `> TICK_INTERVAL`) | collect |
| `DEEP_WINDOW` | `24h` | collect |
| `SECTION_TIMEOUT` | `10` (s) | collect |
| `JOURNAL_MAX_RECORDS` | `20000` (per journalctl query) | journal |
| `FACTS_MAX_BYTES` | `262144` | collect |
| `SERVICES_MAX_BYTES` | `65536` | collect |
| `STATE_DIR` | `/state` | all |
| `HOST_JOURNAL_DIR` | `/host/journal` | collect |
| `HOST_JOURNAL_VOLATILE_DIR` | `/host/journal-volatile` | collect |
| `HOST_PROC` | `/host/proc` | collect, notify |
| `HOST_ROOT` | `/host/root` | collect |
| `HOST_RASDAEMON` | `/host/rasdaemon` | collect |
| `SENTINEL_HOSTNAME` | resolved, see C5 | all |
| `AGY_BIN` | `agy` | analyze |
| `AGY_HOME` | `/state/agy-home` (persistent; NEVER tmpfs, a lost token refresh cannot be re-authenticated headlessly) | runtime |
| `AGY_SECRET_DIR` | `/run/secrets/agy` | runtime |
| `AGY_PRINT_TIMEOUT` | `180s` | analyze |
| `AGY_HARD_TIMEOUT` | `210s` (raised to print+30s if lower) | analyze |
| `HISTORY_N` | `5` | analyze |
| `PROMPT_MAX_BYTES` | `20000` (whole assembled prompt; agy drops larger prompts silently) | analyze |
| `HISTORY_KEEP` | `50` | state |
| `DEEP_ENABLED` | `1` | analyze |
| `DEEP_TIMEOUT` | `30s` (overrides `SECTION_TIMEOUT` for deep collects) | analyze |
| `RAW_ALERT_MAX_PRIORITY` | `2` | runtime, collect |
| `RAW_ALERT_MAX_LINES` | `20` (1–20; `findings.maxItems` is 20) | runtime, collect |
| `RAW_ALERT_REPEAT_SECONDS` | `3600` | runtime |
| `RAW_ALERT_MARKER_TTL_HOURS` | `168` | runtime |
| `DEGRADED_ALERT_AFTER` | `900` (0–86400; `0` alerts on the first failed tick) | runtime |
| `RENOTIFY_ALERT_SEC` | `3600` | state |
| `RENOTIFY_WATCH_SEC` | `21600` | state |
| `STALE_ALERT_SEC` | `86400` | state |
| `HEARTBEAT_HOUR` | `8` | state |
| `OUTBOX_MAX` | `50` | state |
| `OUTBOX_SMTP_AFTER` | `3` | state |
| `APPRISE_URL` | `http://apprise:8000` | notify |
| `APPRISE_KEY` | `sentinel` | notify |
| `APPRISE_CONFIG_FILE` | `/config/sentinel.cfg` | notify (`--seed-config` only) |
| `NOTIFY_TIMEOUT` | `15` (s) | notify |
| `NOTIFY_BODY_MAX` | `3500` (runes) | notify |
| `MAILRISE_HOST` / `MAILRISE_PORT` | `mailrise` / `8025` | notify |
| `MAILRISE_USER` / `MAILRISE_PASS` | required (`:?` in compose) | notify |
| `SENTINEL_MAIL_FROM` / `SENTINEL_MAIL_TO` | `sentinel@mailrise.xyz` | notify |
| `LOG_LEVEL` | `INFO` | all |
| `TMPDIR` | `/tmp` | analyze |
| `TZ` | `UTC` | all |
| `SENTINEL_NOW` | unset, **test-only clock override** | state, runtime |

Abolished names: `SENTINEL_HOME`, `SENTINEL_STATE`, `FACTS_PROTECT_PRIORITY` (use `RAW_ALERT_MAX_PRIORITY`), `FACTS_KEEP_CRITICAL` (use `RAW_ALERT_MAX_LINES`), `OUTBOX_FALLBACK_ATTEMPTS` (use `OUTBOX_SMTP_AFTER`), `RENOTIFY_*_SECONDS`, `TELEGRAM_BOT_TOKEN`/`TELEGRAM_CHAT_ID` (never in the sentinel container, the token stays out of the process that parses attacker-controlled log text; apprise seeds its own config volume, `--seed-config` is an ops one-shot).

### C4. Paths

Container mounts (all `:ro` except where noted): `/host/journal`, `/host/journal-volatile`, `/host/proc`, `/host/sys`, `/host/root` (rslave), `/host/rasdaemon`, `/etc/machine-id`, `/etc/os-release`, `/run/secrets/agy`; **rw:** `/state` (named volume) and `/tmp` (tmpfs). There is no `persistent/`/`volatile/` journal sublayout: both directories are queried and the results merged, de-duplicated on `(ts, message)`.

`$STATE_DIR` whitelist, nothing else may be created:

```
tick-seq  heartbeat  baseline-ports
history/  active-alerts/  outbox/  raw-alerts/  deep-queue/  agy-home/
```

`deep-queue/` is in (analyze needs it). `heartbeat-date` and `tmp/` are out. `agy-home/` is in because `$AGY_HOME` lives there: it is the **only** entry not written by sentinel itself, the agy subprocess owns its contents, and sentinel only creates the directory (`0700`) and seeds it from `$AGY_SECRET_DIR`. It must persist, because agy refreshes its OAuth token as it runs and headless mode cannot re-authenticate a lost one. Nothing in sentinel may read, parse or log anything inside it (C7: credentials). The one write is housekeeping: before each tick, `runtime` unlinks all but the newest 20 regular files, by mtime (a directory, symlink or socket there is neither counted nor removed), in `agy-home/.gemini/antigravity-cli/log/` and `.../crashes/`, because agy writes one log per invocation and one file per crash and never removes either, which on a 5-minute tick is unbounded growth in a persistent volume. It lists, `Lstat`s and removes, never opens a file for reading: filenames enter the process, file bytes never do, which is what C7 governs. Every path goes through `os.Root` rooted at `agy-home/`, so a symlink at any component (`.gemini`, `antigravity-cli`, `log`) is refused whether it points out of the volume or back into it: `os.Root` alone follows a link that stays inside, and a `log -> .` would let the prune list `antigravity-cli` itself and delete the credential as its oldest entry (A1). In the same pass it removes agy's accumulated conversation state outright: `brain/`, `conversations/`, `knowledge/`, `implicit/`, `presence/`, `conversation_summaries.db` with its `-wal` and `-shm` journals, and `history.jsonl`, all directly under `antigravity-cli/`. Each tick is a fresh question about fresh facts, and that state is read back into every prompt (input grew from 20k to 83k tokens over a few dozen ticks) and, once it holds content, makes the model ask for a shell to inspect it. An entry that is a symlink is skipped, not removed. Nothing else under `agy-home/` is touched, the credential, `installation_id`, `settings.json` and agy's own binaries included. Every failure in this pass is ignored, it must never affect a tick, and the removed count is logged at debug level so a wrong path is not a silent no-op. The liveness window is derived from the same configuration that bounds a tick, so a legal tick can never outrun it: `HEALTH_WINDOW` = `2 × TICK_INTERVAL + 8 × SECTION_TIMEOUT + DEEP_TIMEOUT + 3 × AGY_HARD_TIMEOUT + (2 + OUTBOX_MAX) × NOTIFY_TIMEOUT` (2120 s at the defaults. Every term saturating, an overflowing budget yields the maximum duration rather than a wrapped negative). The longest legal tick is 8 × `SECTION_TIMEOUT` collect + `NOTIFY_TIMEOUT` raw-alert notify + 2 × `AGY_HARD_TIMEOUT` (the triage phase: up to four attempts sharing that one budget, `contracts/analyze.md` §6 step 5, so the number of attempts does not enter the arithmetic) + `DEEP_TIMEOUT` + `AGY_HARD_TIMEOUT` (deep dive) + `NOTIFY_TIMEOUT` decision notify + `OUTBOX_MAX × NOTIFY_TIMEOUT` outbox drain, and the heartbeat gap is that plus `TICK_INTERVAL`: 80 + 15 + 420 + 30 + 210 + 15 + 750 + 300 = 1820 s at the defaults, inside 2120 s. The second `TICK_INTERVAL` is margin for the sub-second steps. A window of `3 × TICK_INTERVAL` alone was exceeded by a double agy timeout or a full drain and made `sentinel health` raise a false alarm in exactly the slow-analyzer regime the supervisor exists to survive. `heartbeat` is a single file, content `YYYY-MM-DD\n` (last heartbeat day), rewritten by `state.Process` on every tick so its mtime is the liveness marker. `sentinel health` exits 0 iff its mtime is younger than `HEALTH_WINDOW`.

Every persistent write is `os.CreateTemp(<destination dir>, ".tmp-*")` → write → `Sync` → `Close` → `os.Rename`. Dirs `0o700`, files `0o600` under `outbox/`, `0o644` elsewhere. No `/tmp` staging for state files. `/tmp` holds only analyze's prompt scratch and the materialised schema, both removed by `defer`; agy's `$HOME` is `$STATE_DIR/agy-home` (see the whitelist above), not `/tmp`.

### C5. Shared types and JSON rules

`internal/facts.Entry` is the only journal shape that leaves `internal/journal`:

```go
type Entry struct {
    TS         string  `json:"ts"`        // RFC3339 UTC, seconds
    Priority   int     `json:"priority"`  // 0..7, integer
    Identifier string  `json:"identifier"`// "-" when absent
    Unit       *string `json:"unit"`      // null when absent, always present
    Message    string  `json:"message"`
}
```

`facts.Meta.CollectorErrors` is `[]CollectorError{Section, Reason, ExitCode}`, never `[]string`. Every section is a `Section[T]` wrapper marshaling to `{"error": …}` or to its data; consumers must probe `Err` before reading data. Every section carries `truncated` and `dropped_entries`, `sensors` included.

`internal/report`:

```go
type Finding struct {
    Severity, Component, Evidence, Explanation string
    Analysis, Recommendation string `json:",omitempty"`
    Key         string `json:"key,omitempty"`         // analyze injects; nobody recomputes
    FirstSeen   int64  `json:"first_seen,omitempty"`  // epoch seconds, never RFC3339
    Occurrences int    `json:"occurrences,omitempty"` // state annotates
}
type Meta struct {
    Hostname string `json:"hostname,omitempty"`
    TickSeq  int64  `json:"tick_seq,omitempty"`
    Raw      bool   `json:"raw,omitempty"`
}
```

`report.schema.json` is one file with `additionalProperties: false`, permitting optional `meta`, `key`, `first_seen`, `occurrences` and the component value `meta`. It is normative for **every** document the system emits, raw alerts and fallbacks included. `report.Validate([]byte)` is hand-written Go (enums, rune bounds, array caps, status = highest severity) and is the runtime validator; `santhosh-tekuri/jsonschema/v6` is a **test-only** dependency that asserts `Validate` and the schema file agree. `Validate` does not use `DisallowUnknownFields`, the model's "must not emit `key`" rule is enforced by stripping, not by decode failure.

Marshaling: RFC3339 UTC with `Z` for timestamps, integers for sizes and counts (never strings), `findings`/`resolved`/`entries` never `nil` (`[]`), compact single-line JSON + `\n` on stdout, `MarshalIndent` only for files a human reads. All `maxLength` bounds count runes. `state` never emits a `status` inconsistent with its `findings`: when it suppresses, `status` becomes `OK`.

### C6. Dedup key, one algorithm

`dedup.Key(component, evidence string) string` = `hex(sha256(component + "\n" + EvidenceCore(evidence)))[:16]`. `EvidenceCore`: flatten `\n\r\t` to spaces → strip kernel monotonic stamps `[\s*\d+\.\d+]` → strip a leading syslog or ISO stamp → ASCII-only lowercase (not `strings.ToLower`) → `strings.Fields` → **mask each field**: split it on `=`, replace every part matching `^0x[0-9a-f]+$` or `^[0-9]+([.,:][0-9]+)*$` with `#`, rejoin with `=` → join the fields with a space → truncate to 200 runes.

Token-scoped on purpose: `nvme0n1`, `sda`, `zed[2914]:` survive (no `=`, and the digits are not a whole part); a rising counter `1 → 7` keeps one key.

The `=`-scoped masking is normative, not an optimization: zed evidence carries its counters as `key=value` without spaces (`eid=1841`, `cksum_errors=1`), so whole-field matching alone would give every checksum event its own key and defeat trend tracking. It is what makes the ARCHITECTURE §2.7 CKSUM benchmark and `contracts/analyze.md` test 8 (evidence differing only in `eid=` digits ⇒ identical key) reachable. `analyze` computes the key and injects it into `findings[].key`; `state` and the raw-alert path consume it. The raw-alert path uses `dedup.Key("kernel", entry.Message)`, priority is not part of the key. Nobody recomputes a key that is already set.

### C7. Logging

Every diagnostic goes to **stderr**; stdout carries machine output only (one JSON document + `\n`, or nothing). `log/slog` with a custom handler emitting exactly:

```
<RFC3339-UTC> <LEVEL> <component> <message> [k=v ...]
```

Never logged: `$AGY_SECRET_DIR` contents, `APPRISE_KEY`, `MAILRISE_PASS`, any `TELEGRAM_*` value, prompt or facts content, agy stdout, with one bounded exception: the `error` field of agy's own JSON envelope, as one line of at most 200 runes with control and line-separator characters stripped, surfaced only after the authentication check and only into the container log, never into a report (`contracts/analyze.md` §6 step 4). The same bounded text, when it names a refused tool call, is also the one piece of agy output that enters a prompt: the retry's correction quotes it to the model (§6 step 5), and it goes nowhere else. agy fails with an empty stderr and the reason in that field. Without it every crash is logged as "exit status 1" and nothing else. On an env error print the variable **name** only.

### C8. Component seams (in-process, no subprocesses between components)

```go
collect.Run(ctx, collect.Options{Cfg, DeepComponent}) (*facts.Facts, error)
analyze.Run(ctx, analyze.Options{Cfg, Facts, Seq}, analyze.Deps) (*report.Report, error)
state.Process(raw []byte) (*state.Decision, error)   // history stores the input ANNOTATED (state.md S.3b), not verbatim
notify.Send(ctx, cfg, r report.Report, smtpFallback bool) error
```

`tick` marshals the analyzer's report once and hands the bytes to `state.Process`. `analyze` builds its own fallback report (stable key, `component: "meta"`) and returns it as a valid `*report.Report` with a non-nil error; `tick` sends it through `state` unchanged and exits 3, `tick` never authors an analyzer fallback. `tick` authors only the collector fallback (`component: "meta"`, headline "Collector unavailable"). Deep context for the deep dive is `collect.Run` with `DeepComponent` set, under `DEEP_TIMEOUT`.

Outbox flow, once per tick, in this order: `state.OutboxTake()` → `notify.Send(item.Payload, item.FallbackSMTP)` → `state.OutboxAck(id)` on success. On the current report: `notify.Send` error ⇒ `state.OutboxAdd(payload)`. `notify` has no `--retry-outbox`, no envelope of its own, and never writes to `$STATE_DIR`. The queued payload is the `decision.report` document, so a retry is byte-identical. Raw alerts follow the same path, a failed raw-alert POST is queued like any other.

Notification text: `notify` sanitizes every report-derived string (`headline`, `body`, `explanation`, `analysis`, `recommendation`, `resolved[]`, `evidence`, hostname) by **stripping** `` ` `` `_` `*` `[` `]` and control characters, then adds its own markdown structure. Components therefore never put markdown in report text, the raw-alert body is plain lines `<ts> <priority-name> <message>`, no code fences, no brackets.

### C9. Tests

`go test ./...`, table-driven, stdlib `testing` only, hermetic and offline. Fixtures under `<pkg>/testdata/`, **except** where two assertions must provably share one fixture set (the validator↔schema cross-check of `contracts/analyze.md` case 12b): there the fixtures are in-code tables both assertions read, because a `testdata/` copy could drift away from the table that drives the unit test and silently stop cross-checking. `t.TempDir()` for `STATE_DIR` and `TMPDIR`; `t.Setenv` for env; the clock via `SENTINEL_NOW` or a `Config` field, never `time.Now()`. External binaries (`journalctl`, `sensors`, `agy`) are stubbed by prepending `testdata/bin` to `PATH`; HTTP by `httptest.Server`; SMTP by a `net.Listen` goroutine. Cross-package agreement is asserted, not assumed: one test proves `analyze` and `state` derive the identical key from the same evidence, one proves every emitted document validates against `report.schema.json`, one proves `facts.schema.json` **rejects** malformed facts. Tests gated on real infrastructure use `SENTINEL_CONTAINER=1`, `SENTINEL_REAL_AGY=1`, `SENTINEL_E2E=1`, or the `container` build tag, and must `t.Skip` loudly, never pass silently. CI runs `gofmt -l` (empty), `go vet ./...`, `go test -race ./...`; the Dockerfile builder stage re-runs vet and test so a red suite fails the image.


## Component contracts (split per implementation stage)

The per-component contracts live under [contracts/](contracts/), an implementer reads **this file plus only the file(s) for its TODO**:

| TODO | Read |
|---|---|
| T2 foundation | this file (C1–C9 fully specify config, CLI/exit codes, wire types, dedup, logging) |
| T3 collect | [contracts/collect.md](contracts/collect.md) |
| T4 analyze | [contracts/analyze.md](contracts/analyze.md) |
| T5 state | [contracts/state.md](contracts/state.md) |
| T6 notify + runtime | [contracts/notify.md](contracts/notify.md), [contracts/runtime.md](contracts/runtime.md) |
| T7 image/CI/install | [contracts/runtime.md](contracts/runtime.md) (Dockerfile, compose, install.sh) |

Where a component contract disagrees with this file, this file wins (see top).
