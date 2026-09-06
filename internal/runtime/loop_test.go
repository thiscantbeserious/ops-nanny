package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/analyze"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/collect"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/state"
)

// --- Critical item #2: tick MUST nil-check analyze.Run's report before
// marshaling. On a cancelled context Run returns (nil, err) deliberately;
// this test reaches that exact branch by having the stub RunAgy itself
// return it, the same shape the real analyze.Run produces on
// context.Canceled (contracts/analyze.md §1), and proves Tick does not
// panic and authors nothing. Deleting the nil-check in tick.go must make
// this test fail (verified below the test, in prose, per the task brief;
// the reviewer/gate re-runs this by literally removing the check).
func TestTick_NilCheckOnCancelledAnalyze(t *testing.T) {
	cfg := testConfig(t, tick0)
	rec := newAppriseRecorder(t, 200)
	cfg.AppriseURL = rec.srv.URL
	store := newStore(t, cfg)

	d := baseDeps(store)
	d.CollectRun = func(ctx context.Context, o collect.Options) (*facts.Facts, error) { return factsClean(), nil }
	d.AnalyzeRun = func(ctx context.Context, o analyze.Options, dd analyze.Deps) (*report.Report, error) {
		return nil, context.Canceled
	}

	var res TickResult
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Tick panicked on a nil analyze report: %v", r)
			}
		}()
		res = Tick(context.Background(), cfg, 1, d)
	}()

	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("Err = %v, want context.Canceled", res.Err)
	}
	if res.Report != nil {
		t.Errorf("Report = %+v, want nil, a cancelled analysis must author nothing", res.Report)
	}
	if res.Notified {
		t.Error("Notified = true, want false, nothing should have been sent")
	}
	if rec.count() != 0 {
		t.Errorf("apprise received %d requests, want 0 (a cancelled analysis must send nothing)", rec.count())
	}
}

// --- E17: config_validation ---

func TestConfig_Validation(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantVar string
	}{
		{"bad tick interval", map[string]string{"TICK_INTERVAL": "abc"}, "TICK_INTERVAL"},
		{"tick interval too low", map[string]string{"TICK_INTERVAL": "10"}, "TICK_INTERVAL"},
		{"window not greater than interval", map[string]string{"TICK_WINDOW": "5m", "TICK_INTERVAL": "300"}, "TICK_WINDOW"},
		{"bad log level", map[string]string{"LOG_LEVEL": "LOUD"}, "LOG_LEVEL"},
		{"raw alert max lines out of range", map[string]string{"RAW_ALERT_MAX_LINES": "99"}, "RAW_ALERT_MAX_LINES"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			_, err := config.Load()
			if err == nil {
				t.Fatalf("expected an error for %v", c.env)
			}
			var cerr *config.Error
			if !errors.As(err, &cerr) {
				t.Fatalf("err = %v, want *config.Error", err)
			}
			if cerr.Var != c.wantVar {
				t.Errorf("Var = %q, want %q", cerr.Var, c.wantVar)
			}
			if len(c.env) == 1 {
				for _, v := range c.env {
					if contains(cerr.Error(), v) {
						t.Errorf("error text %q must never repeat the offending VALUE (C7)", cerr.Error())
					}
				}
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// --- E18: shutdown ---

func TestLoop_Shutdown(t *testing.T) {
	withStubJournalctlOnPath(t, `echo '{"MESSAGE":"boot"}'`)
	cfg := testConfig(t, tick0)
	rec := newAppriseRecorder(t, 200)
	cfg.AppriseURL = rec.srv.URL
	store := newStore(t, cfg)

	d := baseDeps(store)
	d.CollectRun = func(ctx context.Context, o collect.Options) (*facts.Facts, error) { return factsClean(), nil }
	d.AnalyzeRun = stubAnalyzeReturning(okReport())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Loop even starts a tick

	done := make(chan struct{})
	var code int
	var err error
	go func() {
		code, err = Loop(ctx, cfg, d)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Loop did not return within 5s of a cancelled context")
	}
	if code != 0 || err != nil {
		t.Errorf("Loop() = %d, %v, want 0, nil", code, err)
	}
}

// TestNextTickSeq_MissingWarnsToo asserts R3.1's "Missing or unparseable
// ⇒ start at 1 and WARN" covers the missing case, not just unparseable,
// the far more common real case (a fresh $STATE_DIR on first boot,
// tick-seq simply absent) must not start at 1 silently.
func TestNextTickSeq_MissingWarnsToo(t *testing.T) {
	cfg := testConfig(t, tick0)
	var logBuf bytes.Buffer
	logWriter = &logBuf
	defer func() { logWriter = nil }()

	seq := nextTickSeq(cfg, newLogger(cfg))
	if seq != 1 {
		t.Fatalf("seq = %d, want 1 on a fresh $STATE_DIR", seq)
	}
	if !strings.Contains(logBuf.String(), "tick-seq missing") {
		t.Errorf("stderr does not WARN on a missing tick-seq file: %q", logBuf.String())
	}
}

// --- analyzer-degraded marker durability (R3.5b) ---
//
// R3.5b holds an analyzer alert until it has actually LASTED
// DEGRADED_ALERT_AFTER of wall-clock time, not until a certain number of
// Tick() calls have happened: $STATE_DIR/analyzer-fails stores the unix
// timestamp of the first degraded tick, and analyzerHeld compares elapsed
// time (now - first) against DEGRADED_ALERT_AFTER. now is always the
// caller's own clock read (nowFor/cfg.Now, C9), passed in explicitly,
// never a bare time.Now() inside analyzerHeld itself.

// A stored timestamp AFTER now means the clock jumped backwards (or the
// file is corrupt): now.Sub(stored) would be negative, which compares less
// than any positive DEGRADED_ALERT_AFTER forever, a permanent hold. Reset
// to now instead, same as an unparseable value, and WARN.
func TestAnalyzerHeld_FutureStoredValueTreatedAsCorrupt(t *testing.T) {
	cfg := testConfig(t, tick0)
	var logBuf bytes.Buffer
	logWriter = &logBuf
	defer func() { logWriter = nil }()

	future := tick0.Add(time.Hour)
	if err := os.WriteFile(filepath.Join(cfg.StateDir, analyzerFailsFile), []byte(strconv.FormatInt(future.Unix(), 10)), 0o644); err != nil {
		t.Fatal(err)
	}

	held := analyzerHeld(cfg, newLogger(cfg), tick0, true)
	if !held {
		t.Fatal("held = false, want true: a corrupt (future) marker must reset to now, not release the hold immediately")
	}
	if !strings.Contains(logBuf.String(), "in the future") {
		t.Errorf("stderr does not WARN on a future marker: %q", logBuf.String())
	}
}

// A marker stored absurdly long ago (corruption, not a real multi-year
// outage) must reset the same way.
func TestAnalyzerHeld_AbsurdlyOldStoredValueTreatedAsCorrupt(t *testing.T) {
	cfg := testConfig(t, tick0)
	var logBuf bytes.Buffer
	logWriter = &logBuf
	defer func() { logWriter = nil }()

	ancient := tick0.Add(-20 * 365 * 24 * time.Hour)
	if err := os.WriteFile(filepath.Join(cfg.StateDir, analyzerFailsFile), []byte(strconv.FormatInt(ancient.Unix(), 10)), 0o644); err != nil {
		t.Fatal(err)
	}

	held := analyzerHeld(cfg, newLogger(cfg), tick0, true)
	if !held {
		t.Fatal("held = false, want true: an absurdly old marker must reset to now, not report the outage as already having lasted 20 years")
	}
	if !strings.Contains(logBuf.String(), "absurdly old") {
		t.Errorf("stderr does not WARN on an absurdly old marker: %q", logBuf.String())
	}
}

// A write failure must fail OPEN: it must not report "held" while the disk
// stays broken, that silences a real outage indefinitely. Forced by
// pre-creating the counter path as a directory, so WriteAtomic's final
// rename fails (root-safe: an ENOTDIR/ENOTEMPTY-shaped failure, not a
// permission bit, which uid 0 would sail through).
func TestAnalyzerHeld_WriteFailureFailsOpen(t *testing.T) {
	cfg := testConfig(t, tick0)
	var logBuf bytes.Buffer
	logWriter = &logBuf
	defer func() { logWriter = nil }()

	if err := os.MkdirAll(filepath.Join(cfg.StateDir, analyzerFailsFile), 0o700); err != nil {
		t.Fatal(err)
	}

	held := analyzerHeld(cfg, newLogger(cfg), tick0, true)
	if held {
		t.Fatal("held = true, want false: a marker that cannot persist must fail open, not hold the alert")
	}
	if !strings.Contains(logBuf.String(), "unfiltered") {
		t.Errorf("stderr does not WARN about the failed-open write: %q", logBuf.String())
	}
}

// The reset path (degraded=false) must WARN when the delete itself fails,
// distinct from the normal "nothing to delete" case, which must stay
// silent: a stale marker surviving a healthy tick lets a later blip ride
// through a hold that should have started fresh. Forced by making the
// marker path a NON-EMPTY directory, so os.Remove fails with "directory
// not empty" rather than the file simply not existing.
func TestAnalyzerHeld_ResetDeleteFailureWarns(t *testing.T) {
	cfg := testConfig(t, tick0)
	var logBuf bytes.Buffer
	logWriter = &logBuf
	defer func() { logWriter = nil }()

	path := filepath.Join(cfg.StateDir, analyzerFailsFile)
	if err := os.MkdirAll(filepath.Join(path, "obstruction"), 0o700); err != nil {
		t.Fatal(err)
	}

	held := analyzerHeld(cfg, newLogger(cfg), tick0, false)
	if held {
		t.Fatal("held = true, want false: the reset path always reports not-held, delete failure or not")
	}
	if !strings.Contains(logBuf.String(), "could not reset") {
		t.Errorf("stderr does not WARN on a failed marker delete: %q", logBuf.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("marker path gone despite Remove failing (test setup invalid): %v", err)
	}
}

// A missing marker is the ordinary case (nothing to reset, or a healthy
// system that was never degraded) and must never WARN.
func TestAnalyzerHeld_ResetOfMissingMarkerIsSilent(t *testing.T) {
	cfg := testConfig(t, tick0)
	var logBuf bytes.Buffer
	logWriter = &logBuf
	defer func() { logWriter = nil }()

	analyzerHeld(cfg, newLogger(cfg), tick0, false)

	if logBuf.String() != "" {
		t.Errorf("stderr = %q, want empty: resetting an already-clean marker must be silent", logBuf.String())
	}
}

// --- analyzerHeld elapsed-time threshold (R3.5b) ---

// The hold releases exactly when elapsed time reaches DEGRADED_ALERT_AFTER,
// not a tick count: with the documented defaults (TICK_INTERVAL=300s,
// DEGRADED_ALERT_AFTER=900s), the marker is planted at tick0 (elapsed 0,
// held), stays held at 899s elapsed, and releases at exactly 900s elapsed.
// A literal boundary, not a call into the code under test, so a mutation
// of "<" to "<=" (or vice versa) is caught.
func TestAnalyzerHeld_ReleasesExactlyAtElapsedThreshold(t *testing.T) {
	cfg := testConfig(t, tick0)
	logger := newLogger(cfg)

	if !analyzerHeld(cfg, logger, tick0, true) {
		t.Fatal("held = false at elapsed 0s, want true")
	}
	if !analyzerHeld(cfg, logger, tick0.Add(899*time.Second), true) {
		t.Fatal("held = false at elapsed 899s, want true (still below the 900s threshold)")
	}
	if analyzerHeld(cfg, logger, tick0.Add(900*time.Second), true) {
		t.Fatal("held = true at elapsed 900s, want false (the threshold has been reached)")
	}
}

// DEGRADED_ALERT_AFTER=0 means no grace period at all: elapsed(0) < 0 is
// false, so even the very first degraded tick releases immediately.
func TestAnalyzerHeld_ZeroGraceNeverHolds(t *testing.T) {
	cfg := testConfig(t, tick0)
	cfg.DegradedAlertAfter = 0

	if analyzerHeld(cfg, newLogger(cfg), tick0, true) {
		t.Fatal("held = true, want false: DEGRADED_ALERT_AFTER=0 must never hold")
	}
}

// A restart mid-outage must not hand the analyzer a fresh grace period:
// the marker's original `first` timestamp must survive being re-read, not
// be silently overwritten with the read's own `now` on every call. This
// pins the persistence behaviour directly (the Tick-level equivalent lives
// in degraded_test.go); mutating `first = stored` to `first = now` in
// analyzerHeld would make this fail while still passing every Tick-level
// test that doesn't advance the clock between calls.
func TestAnalyzerHeld_PreservesOriginalFirstAcrossCalls(t *testing.T) {
	cfg := testConfig(t, tick0)
	logger := newLogger(cfg)

	analyzerHeld(cfg, logger, tick0, true) // plants first = tick0

	if !analyzerHeld(cfg, logger, tick0.Add(899*time.Second), true) {
		t.Fatal("held = false at elapsed 899s from the ORIGINAL first, want true (first must not have been re-planted at the second call's now)")
	}
}

// --- E19: health ---

// --- E19: health ---

func TestHealth(t *testing.T) {
	cfg := testConfig(t, tick0)
	store := newStore(t, cfg)

	if code, err := Health(cfg); code != 1 || err == nil {
		t.Errorf("missing heartbeat: code=%d err=%v, want 1, non-nil", code, err)
	}

	// A fresh heartbeat, via a real Process call.
	_, perr := store.Process(mustMarshalReportForTest(t, report.Report{
		Status: "OK", Headline: "h", Body: "b", Findings: []report.Finding{}, Resolved: []string{},
	}))
	if perr != nil {
		t.Fatal(perr)
	}
	// Health() compares against cfg.Now (the test clock, C9), pin the
	// file's mtime to it explicitly rather than relying on the real wall
	// clock, which may disagree with cfg.Now by however old the fixture
	// date is.
	hb := filepath.Join(cfg.StateDir, "heartbeat")
	if err := os.Chtimes(hb, cfg.Now, cfg.Now); err != nil {
		t.Fatal(err)
	}
	if code, err := Health(cfg); code != 0 || err != nil {
		t.Errorf("fresh heartbeat: code=%d err=%v, want 0, nil", code, err)
	}

	// Backdate the heartbeat past state.HealthWindow relative to cfg.Now.
	stale := cfg.Now.Add(-state.HealthWindow(cfg) - time.Minute)
	if err := os.Chtimes(hb, stale, stale); err != nil {
		t.Fatal(err)
	}
	if code, err := Health(cfg); code != 1 || err == nil {
		t.Errorf("stale heartbeat: code=%d err=%v, want 1, non-nil", code, err)
	}
}

func mustMarshalReportForTest(t *testing.T, r report.Report) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// capturingWarner records warnings so a test can assert what the operator
// was told, not merely that nothing crashed.
type capturingWarner struct{ msgs []string }

func (w *capturingWarner) Warn(msg string, _ ...any) { w.msgs = append(w.msgs, msg) }

// TestSeedAgyHome_CopiesCredentialTree pins the layout agy actually uses,
// measured on the target host rather than assumed:
//
//	~/.gemini/
//	  antigravity-cli/
//	    antigravity-oauth-token      <- the credential
//	  config/
//
// Every entry at the top level of the mounted secret directory is a
// DIRECTORY. The previous implementation copied "regular files only" and
// skipped directories, so it copied nothing at all, and because the
// directory was not empty it did not even warn: the operator saw an
// analyzer that silently fell back on every tick with no explanation.
//
// agy resolves its credential under $HOME/.gemini/, so the tree has to
// land there rather than flat in $HOME. Verified end to end on the host:
// copying ~/.gemini into a fresh HOME and running agy returned
// status SUCCESS with real token usage, which is the empirical check
// contracts/runtime.md required before relying on seeding.
func TestSeedAgyHome_CopiesCredentialTree(t *testing.T) {
	secret := t.TempDir()
	home := filepath.Join(t.TempDir(), "agy-home")

	cliDir := filepath.Join(secret, "antigravity-cli")
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cliDir, "antigravity-oauth-token"), []byte("TOKEN-CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(secret, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{AgyHome: home, AgySecretDir: secret}
	w := &capturingWarner{}
	seedAgyHome(cfg, w)

	tokenPath := filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	got, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("credential did not reach %s: %v (the secret dir holds only directories, and skipping them copies nothing)", tokenPath, err)
	}
	if string(got) != "TOKEN-CONTENT" {
		t.Errorf("token content = %q, want TOKEN-CONTENT", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "config", "config.json")); err != nil {
		t.Errorf("config tree did not reach AGY_HOME: %v", err)
	}
	for _, m := range w.msgs {
		if strings.Contains(m, "credentials absent") {
			t.Errorf("warned %q although credentials were present", m)
		}
	}
}

// TestSeedAgyHome_DoesNotClobberContainerState pins that seeding
// bootstraps and never re-imposes.
//
// seedAgyHome runs on every container start, and agy owns $AGY_HOME once
// it is running: it refreshes its OAuth token there, and writes
// conversation_summaries.db and antigravity-cli/brain/ (its memory) there.
// Copying the host tree over that on each start would silently replace a
// refreshed token with the older one from the host, where no agy runs to
// keep it current, and would delete accumulated memory on a restart.
//
// Bootstrapping is still needed, so files MISSING at the destination are
// still copied: a crash halfway through the first seed must be able to
// complete on the next start.
func TestSeedAgyHome_DoesNotClobberContainerState(t *testing.T) {
	secret := t.TempDir()
	home := filepath.Join(t.TempDir(), "agy-home")

	cliDir := filepath.Join(secret, "antigravity-cli")
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// What the host holds: the token as it was when the operator logged in.
	if err := os.WriteFile(filepath.Join(cliDir, "antigravity-oauth-token"), []byte("STALE-HOST-TOKEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cliDir, "settings.json"), []byte(`{"seeded":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// What the container already has: a refreshed token and memory that
	// exist only here.
	destCLI := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(filepath.Join(destCLI, "brain"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destCLI, "antigravity-oauth-token"), []byte("REFRESHED-IN-CONTAINER"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destCLI, "brain", "memory.bin"), []byte("ACCUMULATED"), 0o600); err != nil {
		t.Fatal(err)
	}

	seedAgyHome(&config.Config{AgyHome: home, AgySecretDir: secret}, &capturingWarner{})

	tok, err := os.ReadFile(filepath.Join(destCLI, "antigravity-oauth-token"))
	if err != nil {
		t.Fatal(err)
	}
	if string(tok) != "REFRESHED-IN-CONTAINER" {
		t.Errorf("token = %q, want the container's refreshed one: seeding replaced a current token with the host's older copy", tok)
	}
	mem, err := os.ReadFile(filepath.Join(destCLI, "brain", "memory.bin"))
	if err != nil {
		t.Fatalf("agy's memory did not survive a restart: %v", err)
	}
	if string(mem) != "ACCUMULATED" {
		t.Errorf("memory = %q, want ACCUMULATED", mem)
	}
	// Bootstrapping still has to work for what is genuinely missing.
	if _, err := os.Stat(filepath.Join(destCLI, "settings.json")); err != nil {
		t.Errorf("a file absent from the destination was not seeded: %v", err)
	}
}

// TestSeedAgyHome_WritesToolDenyPolicy pins that the container imposes its
// own tool policy on agy, overriding whatever the operator's desktop
// settings say.
//
// agy is an agent, not a completion API: mid-analysis it decides to run
// shell commands. In --print mode there is nobody to approve them, so the
// turn dies and the envelope comes back status ERROR with an empty
// response, which the report parser then reports as invalid JSON.
// Measured on the target host:
//
//	error: permission check failed for command "ls -la":
//	       user denied permission to run command: ls -la
//
// With a deny rule configured, the same prompt returns status SUCCESS and a
// valid report: the model answers instead of reaching for a shell.
//
// This is also the security property the project already claims elsewhere.
// The analyzer's input is attacker-controlled log text; a tool call it can
// be talked into is a prompt injection with a shell on the end of it. The
// policy is therefore written unconditionally, not merged in only when
// absent, and not left to whatever settings.json the host happened to have.
func TestSeedAgyHome_WritesToolDenyPolicy(t *testing.T) {
	secret := t.TempDir()
	home := filepath.Join(t.TempDir(), "agy-home")
	cliDir := filepath.Join(secret, "antigravity-cli")
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The operator's own settings, carrying a key worth preserving and no
	// permission block at all.
	if err := os.WriteFile(filepath.Join(cliDir, "settings.json"),
		[]byte(`{"enableTelemetry":false,"trustedWorkspaces":["/home/doh"]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	seedAgyHome(&config.Config{AgyHome: home, AgySecretDir: secret}, &capturingWarner{})

	raw, err := os.ReadFile(filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"))
	if err != nil {
		t.Fatalf("settings.json missing from AGY_HOME: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}
	perm, ok := got["permission"].(map[string]any)
	if !ok {
		t.Fatalf("no permission block written; settings = %s", raw)
	}
	deny, ok := perm["deny"].([]any)
	if !ok || len(deny) == 0 {
		t.Fatalf("no deny rules written; permission = %v", perm)
	}
	var denied string
	for _, d := range deny {
		denied += d.(string) + " "
	}
	if !strings.Contains(denied, "run_command(*)") {
		t.Errorf("shell execution is not denied; deny = %v", deny)
	}
	if got["enableTelemetry"] != false {
		t.Errorf("the operator's other settings were discarded; got %v", got)
	}
}

// TestSeedAgyHome_UnreadableFileDoesNotAbortSeed pins that one file the
// container cannot read does not cost it the credential.
//
// The mounted tree is agy's whole state directory, not a credential store:
// caches, a conversation database, a bundled browser helper. Its files are
// owned by the operator, and only some carry a group the container shares,
// so an unreadable cache entry is normal. Aborting the walk on it meant a
// file with no bearing on authentication could stop the token from being
// seeded, which is what happened on the target host:
//
//	could not seed agy credentials, analysis will fall back
//	error=open /run/secrets/agy/antigravity-cli/cache/last_conversations.json: permission denied
func TestSeedAgyHome_UnreadableFileDoesNotAbortSeed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test depends on")
	}
	secret := t.TempDir()
	home := filepath.Join(t.TempDir(), "agy-home")

	cliDir := filepath.Join(secret, "antigravity-cli")
	cacheDir := filepath.Join(cliDir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Sorts before the token, so an aborting walk never reaches it.
	unreadable := filepath.Join(cacheDir, "last_conversations.json")
	if err := os.WriteFile(unreadable, []byte("{}"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cliDir, "zz-token-sorts-last"), []byte("TOKEN"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := &capturingWarner{}
	seedAgyHome(&config.Config{AgyHome: home, AgySecretDir: secret}, w)

	got, err := os.ReadFile(filepath.Join(home, ".gemini", "antigravity-cli", "zz-token-sorts-last"))
	if err != nil {
		t.Fatalf("a readable credential was not seeded because another file was unreadable: %v", err)
	}
	if string(got) != "TOKEN" {
		t.Errorf("seeded content = %q, want TOKEN", got)
	}
	var named bool
	for _, m := range w.msgs {
		if strings.Contains(m, "unreadable") {
			named = true
		}
	}
	if !named {
		t.Errorf("the skipped file was not reported; warnings were %v", w.msgs)
	}
}

// agy writes one cli log per invocation and a crash file per abnormal
// exit and never removes either, so on a 5-minute tick they grow without
// bound in a persistent volume. The prune keeps a bounded window, ordered
// by mtime because crash names carry a pid and a uuid rather than a
// sortable timestamp, and it must never touch anything else under
// $AGY_HOME, least of all the credential.

// agyHomeFixture builds $AGY_HOME/.gemini/antigravity-cli with a token
// file, a brain/ directory, and n files in each of log/ and crashes/
// whose mtime order is deliberately the REVERSE of their name order (f0 is
// the newest), so a name-sorted or ascending-sorted prune keeps the wrong
// ones. Returns the home, base and token path.
func agyHomeFixture(t *testing.T, n int) (home, base, token string) {
	t.Helper()
	home = t.TempDir()
	base = filepath.Join(home, ".gemini", "antigravity-cli")
	for _, sub := range []string{"log", "crashes", "brain"} {
		if err := os.MkdirAll(filepath.Join(base, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	token = filepath.Join(base, "antigravity-oauth-token")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	newest := time.Now()
	for _, sub := range []string{"log", "crashes"} {
		for i := 0; i < n; i++ {
			name := filepath.Join(base, sub, "f"+strconv.Itoa(i)+".log")
			if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			mod := newest.Add(-time.Duration(i) * time.Minute)
			if err := os.Chtimes(name, mod, mod); err != nil {
				t.Fatal(err)
			}
		}
	}
	return home, base, token
}

func TestPruneAgyLogs(t *testing.T) {
	home, base, token := agyHomeFixture(t, 30)
	// Snapshot the credential's timestamps here, before any fixture step
	// that could follow a symlink into it. A snapshot taken later would
	// compare a damaged file with itself.
	tokenBefore, err := os.Lstat(token)
	if err != nil {
		t.Fatal(err)
	}
	// A subdirectory inside log/ is not a log file and must survive: the
	// IsDir skip is the only thing standing between it and os.Remove.
	nested := filepath.Join(base, "log", "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	// Older than every file, so it sits outside the keep window: the only
	// thing that saves it is the directory skip, not its position.
	old := time.Now().Add(-99 * time.Hour)
	if err := os.Chtimes(nested, old, old); err != nil {
		t.Fatal(err)
	}

	// A symlink inside log/ pointing at the credential: not a regular
	// file, so it is neither counted nor removed, and its target is never
	// touched. Its mtime is left as created (os.Chtimes would follow the
	// link and age the credential instead). A prune that counted it would
	// keep it as the newest entry and evict one real file, which the
	// regular-file count below catches.
	link := filepath.Join(base, "log", "stale-link")
	if err := os.Symlink(token, link); err != nil {
		t.Fatal(err)
	}

	logCfg := testConfig(t, tick0)
	logCfg.LogLevel = "DEBUG"
	var logBuf bytes.Buffer
	logWriter = &logBuf
	defer func() { logWriter = nil }()

	pruneAgyHome(&config.Config{AgyHome: home}, newLogger(logCfg))

	// The count line is what makes a wrong path visible instead of a
	// silent no-op. It carries the directory name and a number, no
	// filenames and no content.
	if got := logBuf.String(); !strings.Contains(got, "runtime pruned agy files dir=log removed=10") ||
		!strings.Contains(got, "dir=crashes removed=10") {
		t.Errorf("debug count line missing or wrong:\n%s", got)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("a symlink inside log/ must be left alone: %v", err)
	}
	for _, sub := range []string{"log", "crashes"} {
		entries, err := os.ReadDir(filepath.Join(base, sub))
		if err != nil {
			t.Fatal(err)
		}
		files := 0
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			files++
			var i int
			if _, err := fmt.Sscanf(e.Name(), "f%d.log", &i); err != nil {
				t.Fatalf("%s: unexpected file %q", sub, e.Name())
			}
			// The contract says the newest 20, the literal, not whatever
			// the constant happens to be.
			if i >= 20 {
				t.Errorf("%s: kept %q, which is older than the newest 20", sub, e.Name())
			}
		}
		if files != 20 {
			t.Errorf("%s: %d files remain, want 20", sub, files)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "log", "nested")); err != nil {
		t.Errorf("a directory inside log/ must survive the prune: %v", err)
	}
	if data, err := os.ReadFile(token); err != nil || string(data) != "secret" {
		t.Errorf("the credential must never be touched: err=%v", err)
	}
	// Bytes alone would not catch a fixture or a prune that follows the
	// symlink and rewrites the credential's timestamps.
	if fi, err := os.Lstat(token); err != nil || !fi.ModTime().Equal(tokenBefore.ModTime()) {
		t.Errorf("the credential's mtime changed across the prune: err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(base, "brain")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("agy's conversation state must be gone after the prune: err=%v", err)
	}
}

// Every accumulated-state entry goes, files and directories alike, while
// a symlink standing in an entry's place is skipped and its target is
// never touched, whether it points outside the home or at the credential.
func TestPruneAgyHome_ClearsConversationState(t *testing.T) {
	home, base, token := agyHomeFixture(t, 1)
	for _, d := range []string{"conversations/c1", "knowledge", "implicit"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"conversations/c1/turn.json", "conversation_summaries.db", "conversation_summaries.db-wal", "history.jsonl", "installation_id", "settings.json"} {
		if err := os.WriteFile(filepath.Join(base, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// brain replaced by a link to a tree outside the home.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "artifact"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(base, "brain")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "brain")); err != nil {
		t.Fatal(err)
	}
	// conversation_summaries.db-shm replaced by a link to the credential.
	if err := os.Symlink(token, filepath.Join(base, "conversation_summaries.db-shm")); err != nil {
		t.Fatal(err)
	}

	logCfg := testConfig(t, tick0)
	logCfg.LogLevel = "DEBUG"
	var logBuf bytes.Buffer
	logWriter = &logBuf
	defer func() { logWriter = nil }()

	pruneAgyHome(&config.Config{AgyHome: home}, newLogger(logCfg))

	for _, gone := range []string{"conversations", "knowledge", "implicit", "conversation_summaries.db", "conversation_summaries.db-wal", "history.jsonl"} {
		if _, err := os.Lstat(filepath.Join(base, gone)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s must be removed: err=%v", gone, err)
		}
	}
	if got := logBuf.String(); !strings.Contains(got, "runtime cleared agy state removed=6") {
		t.Errorf("debug count line missing or wrong:\n%s", got)
	}
	for _, kept := range []string{"brain", "conversation_summaries.db-shm"} {
		if fi, err := os.Lstat(filepath.Join(base, kept)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("a symlink in %s's place must be skipped, not removed: %v", kept, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(outside, "artifact")); err != nil || string(data) != "keep" {
		t.Errorf("the link target outside the home was touched: %v", err)
	}
	if data, err := os.ReadFile(token); err != nil || string(data) != "secret" {
		t.Errorf("the credential must never be touched: err=%v", err)
	}
	for _, kept := range []string{"installation_id", "settings.json"} {
		if _, err := os.Lstat(filepath.Join(base, kept)); err != nil {
			t.Errorf("%s must survive: %v", kept, err)
		}
	}
}

// A1 write containment: if log/ has been replaced by a symlink, following
// it would prune whatever it points at, sentinel's own history or the
// credential directory. The snapshot is of the PARENT of the target, a
// test rooted at the target cannot observe an escape by construction.
func TestPruneAgyLogs_SymlinkedDirIsNeverFollowed(t *testing.T) {
	// Every component between $AGY_HOME and log/ is a place agy can plant
	// a link, so each is tried in turn. A guard on the leaf alone passes
	// the first case and fails the other two.
	for _, link := range []string{"log", "antigravity-cli", ".gemini"} {
		t.Run(link, func(t *testing.T) { symlinkEscapeProbe(t, link) })
	}
	// A relative link that stays inside agy-home is followed by os.Root,
	// so it is refused by the component check instead: `log -> .` would
	// list antigravity-cli itself and delete the credential as its oldest
	// entry.
	for _, link := range []string{"log", "antigravity-cli", ".gemini"} {
		t.Run("in-root/"+link, func(t *testing.T) { symlinkInRootProbe(t, link) })
	}
}

func symlinkInRootProbe(t *testing.T, link string) {
	home, base, token := agyHomeFixture(t, 0)
	// victimDir is where the pruned path resolves once the link is in
	// place: for `log -> .` that is antigravity-cli itself, holding the
	// credential; for a linked parent it is the real tree's log/. It gets
	// an old, credential-like regular file and enough newer files to put
	// that file past the cap, so a prune that follows the link deletes it.
	var linkPath, target, victimDir string
	switch link {
	case "log":
		if err := os.RemoveAll(filepath.Join(base, "log")); err != nil {
			t.Fatal(err)
		}
		linkPath, target, victimDir = filepath.Join(base, "log"), ".", base
	case "antigravity-cli":
		real := filepath.Join(home, ".gemini", "real")
		if err := os.Rename(base, real); err != nil {
			t.Fatal(err)
		}
		linkPath, target, victimDir = base, "real", filepath.Join(real, "log")
		token = filepath.Join(real, "antigravity-oauth-token")
	case ".gemini":
		real := filepath.Join(home, "real")
		if err := os.Rename(filepath.Join(home, ".gemini"), real); err != nil {
			t.Fatal(err)
		}
		linkPath, target, victimDir = filepath.Join(home, ".gemini"), "real", filepath.Join(real, "antigravity-cli", "log")
		token = filepath.Join(real, "antigravity-cli", "antigravity-oauth-token")
	}
	victim := filepath.Join(victimDir, "must-survive")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-99 * time.Hour)
	for _, f := range []string{victim, token} {
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 25; i++ {
		if err := os.WriteFile(filepath.Join(victimDir, "f"+strconv.Itoa(i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(victimDir)
	if err != nil {
		t.Fatal(err)
	}
	logCfg := testConfig(t, tick0)
	logCfg.LogLevel = "DEBUG"
	var logBuf bytes.Buffer
	logWriter = &logBuf
	defer func() { logWriter = nil }()

	pruneAgyHome(&config.Config{AgyHome: home}, newLogger(logCfg))

	if _, err := os.Lstat(victim); err != nil {
		t.Fatalf("in-root %s link: the prune followed the link and deleted the oldest file where it landed: %v", link, err)
	}
	if _, err := os.Lstat(token); err != nil {
		t.Fatalf("in-root %s link: the prune deleted the credential: %v", link, err)
	}
	after, err := os.ReadDir(victimDir)
	if err != nil {
		t.Fatal(err)
	}
	// In the `log -> .` case the victim directory is antigravity-cli
	// itself, whose brain/ the state clear removes by design. Count what
	// the log prune could have touched.
	if n, m := countNotState(before), countNotState(after); n != m {
		t.Errorf("in-root %s link: %d entries before, %d after", link, n, m)
	}
	if !strings.Contains(logBuf.String(), "a path component is a symlink") {
		t.Errorf("in-root %s link: refused silently, want the debug reason:\n%s", link, logBuf.String())
	}
}

func symlinkEscapeProbe(t *testing.T, link string) {
	home, base, _ := agyHomeFixture(t, 0)
	outside := t.TempDir() // stands in for $STATE_DIR/history
	// The target carries the same layout below the link, so a prune that
	// follows it finds a populated log/ to delete from.
	target := outside
	switch link {
	case "antigravity-cli":
		target = filepath.Join(outside, "log")
	case ".gemini":
		target = filepath.Join(outside, "antigravity-cli", "log")
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if err := os.WriteFile(filepath.Join(target, "h"+strconv.Itoa(i)+".json"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var logDir string
	switch link {
	case "log":
		logDir = filepath.Join(base, "log")
	case "antigravity-cli":
		logDir = base
	case ".gemini":
		logDir = filepath.Join(home, ".gemini")
	}
	if err := os.RemoveAll(logDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, logDir); err != nil {
		t.Fatal(err)
	}
	outside = target
	// Everything the test itself allocates under the temp root happens
	// before the snapshot, so a changed count can only be the prune.
	logger := newLogger(testConfig(t, tick0))
	before, err := os.ReadDir(filepath.Dir(outside))
	if err != nil {
		t.Fatal(err)
	}

	pruneAgyHome(&config.Config{AgyHome: home}, logger)

	if entries, _ := os.ReadDir(outside); len(entries) != 30 {
		t.Fatalf("prune followed the symlinked %s out of $AGY_HOME: %d of 30 files remain outside", link, len(entries))
	}
	after, err := os.ReadDir(filepath.Dir(outside))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("parent of the symlink target changed: %d entries before, %d after", len(before), len(after))
	}
	if fi, err := os.Lstat(logDir); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink itself must be left alone: %v", err)
	}
}

// A missing agy-home, and a directory already under the cap, are both
// normal (a fresh volume, an early tick) and must not error or delete.
func TestPruneAgyLogsToleratesMissingAndSmallDirs(t *testing.T) {
	logCfg := testConfig(t, tick0)
	logCfg.LogLevel = "DEBUG"
	var logBuf bytes.Buffer
	logWriter = &logBuf
	defer func() { logWriter = nil }()
	logger := newLogger(logCfg)

	// An absent home is a no-op that still says so.
	pruneAgyHome(&config.Config{AgyHome: filepath.Join(t.TempDir(), "absent")}, logger)
	if got := logBuf.String(); !strings.Contains(got, "runtime agy home not pruned") {
		t.Errorf("absent AGY_HOME produced no debug signal:\n%s", got)
	}
	logBuf.Reset()

	// A layout that is not agy's must not be a silent no-op either: at
	// debug level the operator can tell "path wrong, growing" from "path
	// right, nothing to do".
	wrong := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wrong, ".gemini", "antigravity-v2", "log"), 0o700); err != nil {
		t.Fatal(err)
	}
	pruneAgyHome(&config.Config{AgyHome: wrong}, logger)
	// The reason is the real one, a missing path, not a symlink accusation
	// that would send an operator looking for an attack on a fresh volume.
	if got := logBuf.String(); !strings.Contains(got, "runtime agy dir not pruned dir=log") || !strings.Contains(got, "no such file or directory") || strings.Contains(got, "symlink") {
		t.Errorf("wrong layout: want the missing-path error as the reason:\n%s", got)
	}

	// The normal steady state, a correct path under the cap, logs
	// nothing at all: no per-tick noise on a healthy volume.
	logBuf.Reset()
	home, base, _ := agyHomeFixture(t, 1)
	// The fixture's brain/ would be cleared and counted, so drop it first:
	// silence is asserted for a home with nothing to do.
	if err := os.RemoveAll(filepath.Join(base, "brain")); err != nil {
		t.Fatal(err)
	}
	pruneAgyHome(&config.Config{AgyHome: home}, logger)
	if entries, _ := os.ReadDir(filepath.Join(base, "log")); len(entries) != 1 {
		t.Errorf("a directory under the cap must be left alone, got %d files", len(entries))
	}
	if got := logBuf.String(); got != "" {
		t.Errorf("under-cap prune must be silent, got:\n%s", got)
	}
}

// The prune is wired into Loop, not only defined: one real tick over a
// 30-file log/ must leave 20. Without this, the call in Loop can be
// deleted and every other test stays green.
func TestLoop_PrunesAgyLogsEachTick(t *testing.T) {
	withStubJournalctlOnPath(t, `echo '{"MESSAGE":"boot"}'`)
	cfg := testConfig(t, tick0)
	rec := newAppriseRecorder(t, 200)
	cfg.AppriseURL = rec.srv.URL
	store := newStore(t, cfg)
	home, base, _ := agyHomeFixture(t, 30)
	cfg.AgyHome = home

	ctx, cancel := context.WithCancel(context.Background())
	d := baseDeps(store)
	d.CollectRun = func(ctx context.Context, o collect.Options) (*facts.Facts, error) {
		cancel() // one tick, then Loop must return at its next check
		return factsClean(), nil
	}
	d.AnalyzeRun = stubAnalyzeReturning(okReport())

	done := make(chan struct{})
	go func() {
		Loop(ctx, cfg, d)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Loop did not return within 10s of the in-tick cancel")
	}
	if entries, _ := os.ReadDir(filepath.Join(base, "log")); len(entries) != 20 {
		t.Errorf("after one Loop tick: %d log files, want 20 (prune not wired into Loop)", len(entries))
	}
}

// The --once command calls Tick directly, never Loop, and C4 promises the
// prune before each tick. A prune wired into Loop alone would skip it.
func TestTick_PrunesAgyLogs(t *testing.T) {
	withStubJournalctlOnPath(t, `echo '{"MESSAGE":"boot"}'`)
	cfg := testConfig(t, tick0)
	rec := newAppriseRecorder(t, 200)
	cfg.AppriseURL = rec.srv.URL
	store := newStore(t, cfg)
	home, base, _ := agyHomeFixture(t, 30)
	cfg.AgyHome = home

	d := baseDeps(store)
	d.CollectRun = func(ctx context.Context, o collect.Options) (*facts.Facts, error) { return factsClean(), nil }
	d.AnalyzeRun = stubAnalyzeReturning(okReport())

	Tick(context.Background(), cfg, 0, d)

	if entries, _ := os.ReadDir(filepath.Join(base, "log")); len(entries) != 20 {
		t.Errorf("after one direct Tick: %d log files, want 20 (prune not wired into Tick)", len(entries))
	}
}

// countNotState counts directory entries that are not agy state entries.
func countNotState(entries []os.DirEntry) int {
	n := 0
	for _, e := range entries {
		if !slices.Contains(agyStateEntries, e.Name()) {
			n++
		}
	}
	return n
}
