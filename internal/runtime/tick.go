// tick.go: Tick() and its step orchestration (R2, R3). Only journalctl,
// sensors and agy are exec'd (inside collect/analyze); everything here is
// in-process Go calls (C8), there is no exit-code round-tripping between
// components.
package runtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/analyze"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/collect"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/notify"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/state"
)

// Deps are the seams tests replace: collect/analyze/state/notify as
// function values, so tests need no subprocess and no network (R7).
type Deps struct {
	CollectRun  func(ctx context.Context, o collect.Options) (*facts.Facts, error)
	AnalyzeRun  func(ctx context.Context, o analyze.Options, d analyze.Deps) (*report.Report, error)
	AnalyzeDeps analyze.Deps

	StateProcess func(raw []byte) (*state.Decision, error)
	OutboxAdd    func(raw []byte) (string, error)
	OutboxTake   func() ([]state.OutboxItem, error)
	OutboxAck    func(id string) error

	NotifySend func(ctx context.Context, cfg *config.Config, r report.Report, smtpFallback bool) error
}

// DefaultDeps wires the real collect/analyze/state/notify implementations.
func DefaultDeps(cfg *config.Config, store *state.Store) Deps {
	return Deps{
		CollectRun:   collect.Run,
		AnalyzeRun:   analyze.Run,
		AnalyzeDeps:  analyze.DefaultDeps(cfg),
		StateProcess: store.Process,
		OutboxAdd:    store.OutboxAdd,
		OutboxTake:   store.OutboxTake,
		OutboxAck:    store.OutboxAck,
		NotifySend:   notify.Send,
	}
}

// TickResult is Tick's return value (R7).
type TickResult struct {
	Seq       int64
	Report    *report.Report // the document handed to notify, or the one state suppressed
	RawAlerts int
	Notified  bool
	Queued    bool // enqueued to outbox instead of delivered
	ExitCode  int  // C2 table, highest reached
	Err       error
}

func newTickLogger(cfg *config.Config) *slog.Logger {
	return newLogger(cfg)
}

// maxCode implements C2's "--once returns the highest code reached".
func maxCode(a, b int) int {
	if b > a {
		return b
	}
	return a
}

// nowFor resolves the tick's single clock read: cfg.Now if set (test
// override, C9), else the live clock, read once here, never a second
// time.Now() deeper in this package.
func nowFor(cfg *config.Config) time.Time {
	if !cfg.Now.IsZero() {
		return cfg.Now
	}
	return time.Now()
}

// Tick runs one full tick: collect, the raw-alert scan, analyze, state,
// notify, and the outbox drain (R3.2).
func Tick(ctx context.Context, cfg *config.Config, seq int64, d Deps) TickResult {
	logger := newTickLogger(cfg)
	// Housekeeping before the tick's own work, here rather than in Loop
	// so a direct --once invocation prunes exactly as the loop does (C4:
	// "before each tick").
	pruneAgyHome(cfg, logger)
	if seq == 0 {
		// R3.1: "$STATE_DIR/tick-seq ... owned exclusively by tick ...
		// read, incremented and written atomically before step 1." The
		// counter is scoped to the sentinel-tick COMMAND, not to --loop,
		// R3.1 draws no --once/--loop distinction. Loop() already
		// allocates its own seq before calling Tick, so it never passes
		// 0 here; this branch exists for a direct --once invocation.
		seq = nextTickSeq(cfg, logger)
	}
	cfg.TickSeq = seq // S.2: state reads this, runtime is the sole writer
	clock := nowFor(cfg)

	result := TickResult{Seq: seq}

	// 1. collect.
	f, err := d.CollectRun(ctx, collect.Options{Cfg: cfg, Seq: seq})
	if err != nil {
		// The analyzer-degraded marker (R3.5b) is deliberately left
		// untouched here: it records WHEN the outage started in wall-clock
		// time, and a collector hiccup neither starts nor ends an analyzer
		// outage, so it must neither bridge two separate outages together
		// nor reset a real one's clock. A flapping collector during a
		// genuine analyzer outage must not silence it.
		result.ExitCode = maxCode(result.ExitCode, 2)
		rep := CollectorUnavailable(cfg, seq, err.Error())
		if _, verr := report.Validate(mustMarshalR(rep)); verr != nil {
			logger.Error("collector fallback failed validation, replacing", "error", verr)
			rep = minimalValidationFailureAlert(cfg, seq, verr)
		}
		result.Report = rep
		deliverAndDrain(ctx, cfg, d, logger, rep, &result)
		return result
	}

	if f.Meta.Truncated {
		logger.Warn("tick facts truncated")
	}
	if len(f.Meta.CollectorErrors) > 0 {
		names := make([]string, 0, len(f.Meta.CollectorErrors))
		for _, e := range f.Meta.CollectorErrors {
			names = append(names, e.Section)
		}
		logger.Warn("tick collector errors", "sections", names)
	}

	// 1b. raw-alert scan, before analysis, dispatched immediately (design
	// principle 4): a crashing or quota-blocked agy must never delay or
	// swallow a critical kernel event.
	rawRep, n, scanFailed := scanRawAlerts(cfg, f, clock)
	if scanFailed {
		// R3.3 (amended 64e57f3): the exit code is NOT suppressed even
		// when the scan-failure NOTIFICATION is throttled by its marker
		// (rawRep may be nil below), "fails loud, never silent" is kept
		// where it cannot be muted: the exit code, the log line, and
		// sentinel health. Only the human channel is throttled.
		result.ExitCode = maxCode(result.ExitCode, 2)
	}
	if rawRep != nil {
		if _, verr := report.Validate(mustMarshalR(rawRep)); verr != nil {
			logger.Error("raw report failed validation, replacing", "error", verr)
			rawRep = minimalValidationFailureAlert(cfg, seq, verr)
		}
		result.RawAlerts = n
		if err := d.NotifySend(ctx, cfg, *rawRep, false); err != nil {
			raw, _ := json.Marshal(rawRep)
			result.ExitCode = maxCode(result.ExitCode, 4)
			queueOrLog(d, logger, &result, raw, "raw alert")
		}
	}

	// 2. analyze.
	rep, aerr := d.AnalyzeRun(ctx, analyze.Options{Cfg: cfg, Facts: f, Seq: seq}, d.AnalyzeDeps)
	if rep == nil {
		// analyze.Run returns (nil, err) ONLY on a cancelled context
		// (contracts/analyze.md §1), a shutdown is not an analyzer
		// failure and must not fabricate an ALERT. Author nothing, send
		// nothing, and return cleanly: marshaling a nil report here is
		// exactly the nil-pointer panic this check exists to prevent.
		result.Err = aerr
		return result
	}
	if aerr != nil {
		result.ExitCode = maxCode(result.ExitCode, 3)
	}

	// An analyzer outage is only worth waking someone for once it
	// persists: a single failed tick is a blip and its alert would be
	// resolved before anyone read it. Track how long the outage has
	// actually lasted in wall-clock time and stay silent below
	// DEGRADED_ALERT_AFTER by dropping the finding, state's rule 4 then
	// suppresses the message. The body is left intact so the alert, when
	// it does come, still carries this tick's raw kernel lines.
	// Deterministic paths (raw-alert, smartd, ZED) never depended on the
	// analyzer and are unaffected either way.
	// stateRep is what state.Process actually sees; it starts as an alias
	// of rep and only forks into its own copy when the hold suppresses it.
	// rep itself is never mutated: R3.8's state-failure path below sends
	// *rep verbatim as the "unfiltered" document, and if suppression had
	// mutated rep in place that path would forward the held OK/no-findings
	// version instead of the true ALERT, exactly during the window a state
	// outage most needs the real document.
	stateRep := rep
	degraded := rep.Meta != nil && rep.Meta.Degraded
	if held := analyzerHeld(cfg, logger, clock, degraded); degraded && held {
		heldRep := *rep
		heldRep.Findings = []report.Finding{}
		heldRep.Status = "OK"
		stateRep = &heldRep
	}

	// 3. marshal once, hand to state.
	raw, merr := json.Marshal(stateRep)
	if merr != nil {
		result.Err = merr
		result.ExitCode = maxCode(result.ExitCode, 1)
		return result
	}
	decision, serr := d.StateProcess(raw)
	if serr != nil {
		// S.7 / R3.8: a state failure must never lose an alert, send the
		// report unfiltered. "Unfiltered" still excludes resolved[]: on
		// every other path state.md S.3(e) replaces each entry with the
		// stored headline before a human ever sees it (the whole point of
		// the 3c078d7 key migration is that an operator never sees a raw
		// key). state never ran here, so nothing performs that
		// substitution, blank resolved[] rather than let the analyzer's
		// bare 16-hex keys reach Telegram. A dropped all-clear list on an
		// already-degraded tick is strictly better than unreadable hex,
		// and "delivery beats dedup" still holds for findings/status/body.
		unfiltered := *rep
		unfiltered.Resolved = []string{}
		unfilteredRaw, merr2 := json.Marshal(unfiltered)
		if merr2 != nil {
			unfilteredRaw = raw // defensive: marshal of a struct we just unmarshal-shaped cannot fail in practice
		}

		result.ExitCode = maxCode(result.ExitCode, 5)
		result.Report = &unfiltered
		if err := d.NotifySend(ctx, cfg, unfiltered, false); err != nil {
			result.ExitCode = maxCode(result.ExitCode, 4)
			queueOrLog(d, logger, &result, unfilteredRaw, "state-failure report")
		} else {
			result.Notified = true
		}
		drainOutbox(ctx, cfg, d, logger, &result)
		return result
	}

	result.Report = &decision.Report
	if decision.Notify {
		payload, _ := json.Marshal(decision.Report)
		if err := d.NotifySend(ctx, cfg, decision.Report, false); err != nil {
			result.ExitCode = maxCode(result.ExitCode, 4)
			queueOrLog(d, logger, &result, payload, "tick report")
		} else {
			result.Notified = true
		}
	}

	// 5. outbox drain, once per tick, last (R3.2).
	drainOutbox(ctx, cfg, d, logger, &result)

	return result
}

// deliverAndDrain sends a runtime-authored fallback (the ONLY case runtime
// itself sends outside the raw-alert path: the collector fallback, since
// there is no facts document to run the raw-alert scan or analyze against)
// and then still runs the outbox drain.
func deliverAndDrain(ctx context.Context, cfg *config.Config, d Deps, logger *slog.Logger, rep *report.Report, result *TickResult) {
	raw, _ := json.Marshal(rep)
	if err := d.NotifySend(ctx, cfg, *rep, false); err != nil {
		result.ExitCode = maxCode(result.ExitCode, 4)
		queueOrLog(d, logger, result, raw, "collector fallback")
	} else {
		result.Notified = true
	}
	drainOutbox(ctx, cfg, d, logger, result)
}

// queueOrLog attempts to enqueue raw to the outbox after a failed
// delivery. R3.8/S.7's invariant is "no alert is lost" via OutboxAdd, but
// that invariant has an unhandled edge: state.Process (S.3d) has already
// committed LastNotified/NotifyCount to active-alerts/<key>.json by the
// time this runs, so if NotifySend AND OutboxAdd both fail, the finding
// sits suppressed for the renotify window with nothing in the outbox and
// no trace anywhere, worse than an ordinary "queued, will retry"
// failure, and indistinguishable from one in the exit code before this
// fix. Neither CONTRACTS.md nor contracts/runtime.md/state.md defines a
// code for this double failure, so this reuses rc 5 (the "state failed"
// family, OutboxAdd is a state write) rather than inventing one; it
// still outranks the plain rc 4 a successful queue reports, which is the
// distinguishing signal an operator needs.
func queueOrLog(d Deps, logger *slog.Logger, result *TickResult, raw []byte, what string) {
	if _, err := d.OutboxAdd(raw); err != nil {
		logger.Error("alert lost: delivery failed and outbox enqueue also failed", "what", what, "error", err)
		result.ExitCode = maxCode(result.ExitCode, 5)
		result.Queued = false
		return
	}
	result.Queued = true
}

// drainOutbox is R3.2 step 5. A failed retry is visible in result.ExitCode
// (rc 4, "notify failed"), not only in the WARN log line, an operator
// running --once must see a stuck outbox in the exit code, the
// machine-readable signal, not only in logs they may not be tailing.
func drainOutbox(ctx context.Context, cfg *config.Config, d Deps, logger *slog.Logger, result *TickResult) {
	items, err := d.OutboxTake()
	if err != nil {
		logger.Warn("outbox take failed", "error", err)
		return
	}
	for _, item := range items {
		var rep report.Report
		if err := json.Unmarshal(item.Payload, &rep); err != nil {
			logger.Warn("outbox item undecodable, skipping", "id", item.ID)
			continue
		}
		if err := d.NotifySend(ctx, cfg, rep, item.FallbackSMTP); err != nil {
			logger.Warn("outbox retry failed", "id", item.ID, "error", err)
			result.ExitCode = maxCode(result.ExitCode, 4)
			continue
		}
		if err := d.OutboxAck(item.ID); err != nil {
			logger.Warn("outbox ack failed", "id", item.ID, "error", err)
		}
	}
}

// minimalValidationFailureAlert is R3.2's own safety net: "a document that
// fails validation is logged at ERROR and replaced by a minimal valid
// ALERT with the validation error as evidence, the system never drops an
// alert because of its own marshaling bug."
func minimalValidationFailureAlert(cfg *config.Config, seq int64, verr error) *report.Report {
	return &report.Report{
		Status:   "ALERT",
		Headline: truncRunesR("Runtime authored an invalid document", 80),
		Body:     truncRunesR("A runtime-authored report failed schema validation: "+verr.Error(), 2000),
		Findings: []report.Finding{{
			Severity: "alert", Component: "meta",
			Evidence:    truncRunesR(verr.Error(), 1000),
			Explanation: "Runtime's own document failed validation; sent as a minimal alert rather than dropped.",
		}},
		Resolved: []string{},
		Meta:     &report.Meta{Hostname: cfg.Hostname, TickSeq: seq, Raw: true},
	}
}

func mustMarshalR(r *report.Report) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		// Marshal only fails on unsupported types; report.Report contains
		// none. Treat as empty rather than panic, Validate below will
		// then reject it and the caller's safety net takes over.
		return []byte("{}")
	}
	return b
}
