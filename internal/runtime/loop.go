// loop.go: Loop(), startup preflight, agy-home seeding and its log pruning, the tick-seq
// counter, and the signal-driven interval loop (R2).
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/state"
)

// stateDirError maps to exit 69 (C2's state-dir specialization of startup
// validation).
type stateDirError struct{ path string }

func (e *stateDirError) Error() string {
	return fmt.Sprintf("%s: STATE_DIR missing or unwritable", e.path)
}

// preflightError maps to exit 78, "ERROR naming the exact path" (R2 step 2).
type preflightError struct {
	path   string
	reason string
}

func (e *preflightError) Error() string {
	return fmt.Sprintf("%s: %s", e.path, e.reason)
}

// preflight is R2's startup sequence steps 2 and 5, run once before the
// loop and once before a single --once tick.
func preflight(cfg *config.Config) error {
	info, err := os.Stat(cfg.StateDir)
	if err != nil || !info.IsDir() {
		return &stateDirError{cfg.StateDir}
	}
	probe := filepath.Join(cfg.StateDir, ".preflight-probe")
	if err := os.WriteFile(probe, []byte{}, 0o600); err != nil {
		return &stateDirError{cfg.StateDir}
	}
	os.Remove(probe)

	tmpProbe := filepath.Join(cfg.TmpDir, ".preflight-probe")
	if err := os.WriteFile(tmpProbe, []byte{}, 0o600); err != nil {
		return &preflightError{cfg.TmpDir, "not writable"}
	}
	os.Remove(tmpProbe)

	if !dirReadableNonEmpty(cfg.HostJournalDir) && !dirReadableNonEmpty(cfg.HostJournalVolatileDir) {
		return &preflightError{cfg.HostJournalDir, "neither journal directory is readable and non-empty"}
	}

	uptimePath := filepath.Join(cfg.HostProc, "uptime")
	if _, err := os.Stat(uptimePath); err != nil {
		return &preflightError{uptimePath, "not readable"}
	}

	for _, dir := range []string{"history", "active-alerts", "outbox", "raw-alerts", "deep-queue"} {
		if err := os.MkdirAll(filepath.Join(cfg.StateDir, dir), 0o700); err != nil {
			return &stateDirError{cfg.StateDir}
		}
	}
	return nil
}

func dirReadableNonEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// checkJournalReadable exists because R2's filesystem preflight
// (dirReadableNonEmpty above) only proves
// the configured journal directory exists and has files in it, it does
// NOT prove journald will hand the unprivileged sentinel user any journal
// CONTENT. A stale JOURNAL_GID, an ineffective group_add, or a wrong
// HOST_JOURNAL_DIR mount all leave that filesystem-level check green while
// journalctl itself reads nothing, and an empty journal is indistinguishable
// from a quiet system, the quietest possible failure of a component whose
// job is noticing things.
//
// Measured against real journalctl in debian:trixie-slim (the R1 runtime
// base) before writing this: "journalctl -n1 --no-pager" on an empty or
// unreadable journal exits 0 and prints "-- No entries --\n" to STDOUT, not
// stderr, so neither "exit code only" nor "stdout non-empty" discriminates
// a real record from that sentinel line. "-o json" does: an empty journal
// under -o json writes zero bytes. This function therefore queries with
// "-o json" and treats non-zero exit OR zero-byte stdout as failure.
//
// Queries HOST_JOURNAL_DIR first, then HOST_JOURNAL_VOLATILE_DIR, the
// same "at least one readable" tolerance the filesystem check above uses
// (a fresh boot can legitimately have an empty persistent journal with
// only the volatile one populated), and fails only if neither yields a
// record.
func checkJournalReadable(ctx context.Context, cfg *config.Config) error {
	timeout := cfg.SectionTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	var lastReason string
	for _, dir := range []string{cfg.HostJournalDir, cfg.HostJournalVolatileDir} {
		if dir == "" {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		cmd := exec.CommandContext(cctx, "journalctl", "-D", dir, "-n1", "-o", "json", "--no-pager")
		out, err := cmd.Output()
		cancel()
		if err != nil {
			lastReason = fmt.Sprintf("journalctl -D %s -n1 failed: %v", dir, err)
			continue
		}
		trimmed := bytes.TrimSpace(out)
		if len(trimmed) == 0 {
			lastReason = fmt.Sprintf("journalctl -D %s -n1 returned zero entries", dir)
			continue
		}
		// Defense in depth beyond "-o json": if journalctl ever falls back
		// to its human-readable "-- No entries --" sentinel line (that is
		// what a real, unreadable/empty journal prints WITHOUT -o json,
		// -o json is what makes that case zero bytes instead), a
		// non-empty-but-non-JSON line must still be rejected rather than
		// accepted as a real record.
		if !json.Valid(trimmed) {
			lastReason = fmt.Sprintf("journalctl -D %s -n1 returned non-JSON output", dir)
			continue
		}
		return nil // a real record from at least one directory
	}
	return &preflightError{
		cfg.HostJournalDir,
		lastReason + ", check JOURNAL_GID, group_add, or the /host/journal mount",
	}
}

type warner interface{ Warn(string, ...any) }

// seedAgyHome copies the mounted credential tree into $AGY_HOME/.gemini
// and points $HOME at $AGY_HOME. A missing or empty secret dir is a WARN,
// not fatal: the raw-alert path must survive without the LLM.
//
// The layout is agy's, measured on the target host rather than assumed:
//
//	~/.gemini/
//	  antigravity-cli/antigravity-oauth-token   <- the credential
//	  config/
//
// Two things follow, and the previous version got both wrong. Every
// top-level entry is a DIRECTORY, and it copied "regular files only", so
// it copied nothing while reporting nothing, because the directory was
// not empty. And agy reads the credential from $HOME/.gemini/, so a flat
// copy into $HOME would not have been found even if it had happened.
//
// Verified end to end before this was written: ~/.gemini copied into a
// fresh HOME, agy --print returned status SUCCESS with real token usage.
// contracts/runtime.md asked for exactly that measurement before relying
// on seeding, and until now nobody had run it.
func seedAgyHome(cfg *config.Config, logger warner) {
	if err := os.MkdirAll(cfg.AgyHome, 0o700); err != nil {
		logger.Warn("runtime could not create AGY_HOME", "error", err)
		return
	}
	entries, err := os.ReadDir(cfg.AgySecretDir)
	if err != nil || len(entries) == 0 {
		logger.Warn("runtime agy credentials absent, analysis will fall back")
		os.Setenv("HOME", cfg.AgyHome)
		return
	}

	dst := filepath.Join(cfg.AgyHome, ".gemini")
	if skipped, err := copyTree(cfg.AgySecretDir, dst); err != nil {
		logger.Warn("runtime could not seed agy credentials, analysis will fall back", "error", err)
	} else if len(skipped) > 0 {
		// Named, not silent: if the token was among them the operator
		// needs to know which file to make readable, and if it was not
		// then this is noise about a cache entry and says so.
		logger.Warn("runtime skipped unreadable files while seeding agy credentials",
			"count", len(skipped), "files", strings.Join(skipped, ","))
	}
	if err := writeAgyToolPolicy(dst); err != nil {
		logger.Warn("runtime could not write agy tool policy", "error", err)
	}
	os.Setenv("HOME", cfg.AgyHome)
}

// agyLogKeep is how many files survive in each of agy's log and crashes
// directories. agy writes one cli log per invocation and a crash file per
// abnormal exit, and never removes either, so on a 5-minute tick they grow
// without bound in a persistent volume. Twenty of each is several hours of
// ticks, enough to diagnose a failure that was noticed in the next report.
const agyLogKeep = 20

// agyStateEntries is what agy accumulates across conversations under
// antigravity-cli: its artifact store, conversation transcripts, knowledge
// items, implicit memory, presence markers, the summary database with its journal files and
// the prompt history. Each tick is a fresh question about fresh facts, so
// none of it is wanted, and keeping it is harmful twice over. Every entry
// is read back into the next prompt, which grew from 20k to 83k input
// tokens over a few dozen ticks. And once the store holds content the
// model starts asking for a shell to look at it (`ls -la .../brain/*/`),
// which the deny policy turns into a failed attempt. The credential, the
// installation id, settings and the binaries agy keeps beside them are not
// on this list and are never touched.
var agyStateEntries = []string{
	"brain", "conversations", "knowledge", "implicit", "presence",
	"conversation_summaries.db", "conversation_summaries.db-wal", "conversation_summaries.db-shm",
	"history.jsonl",
}

// pruneAgyHome unlinks all but the newest agyLogKeep entries in agy's log
// and crashes directories, oldest first by mtime, then removes every
// agyStateEntries entry outright. Their names carry a pid
// and a uuid rather than only a timestamp, so mtime is the ordering that
// holds for both.
//
// Every path is resolved through os.Root, which refuses any symlink that
// leaves $AGY_HOME at any component, not only the last one. agy runs as
// this uid with HOME inside the writable state volume and this repo treats
// it as untrusted. A `.gemini`, `antigravity-cli` or `log` that has become
// a symlink would otherwise turn a prune of $AGY_HOME into a prune of
// wherever the link points, sentinel's own history or the credential
// directory included (A1 write containment).
//
// No file is ever opened for reading, only listed, Lstat'ed and removed.
// Filenames enter the process, file bytes never do, which is what C7
// governs: no credential or prompt content reaches sentinel.
//
// Every failure is ignored on purpose. This is housekeeping. A directory
// that cannot be read or an entry that cannot be removed must never affect
// a tick. The removed count is logged at debug level so a wrong path or a
// changed agy layout does not become a silent no-op. A count is not
// content.
func pruneAgyHome(cfg *config.Config, logger *slog.Logger) {
	root, err := os.OpenRoot(cfg.AgyHome)
	if err != nil {
		// The most literal wrong path of all. SeedAgyHome warns on the
		// same condition earlier, this keeps the prune's own promise.
		logger.Debug("runtime agy home not pruned", "error", err)
		return
	}
	defer root.Close()
	for _, sub := range []string{"log", "crashes"} {
		dir := filepath.Join(".gemini", "antigravity-cli", sub)
		// os.Root refuses a link that leaves the root but follows one that
		// stays inside it, and a `log -> .` would make this prune list
		// antigravity-cli itself and delete the credential as the oldest
		// entry. Every component is therefore required to be a real
		// directory, checked with Lstat so a link is seen as a link.
		if err := realDirs(root, ".gemini", filepath.Join(".gemini", "antigravity-cli"), dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logger.Debug("runtime agy dir not pruned", "dir", sub, "error", err)
			continue
		} // a missing component falls through: root.Open names it in its own error
		df, err := root.Open(dir)
		if err != nil {
			// Absent, a symlink somewhere in the path, or a layout agy
			// changed under us. Said at debug level so a wrong path does
			// not become unbounded growth with no signal. A path and an
			// error are not file content. The under-cap case below stays
			// silent, it is the normal steady state.
			logger.Debug("runtime agy dir not pruned", "dir", sub, "error", err)
			continue
		}
		entries, err := df.ReadDir(-1)
		df.Close()
		if err != nil || len(entries) <= agyLogKeep {
			continue
		}
		type aged struct {
			name string
			mod  time.Time
		}
		files := make([]aged, 0, len(entries))
		for _, e := range entries {
			// Regular files only: a directory, a symlink or a socket agy
			// leaves here is neither counted against the cap nor removed.
			if !e.Type().IsRegular() {
				continue
			}
			info, err := root.Lstat(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			files = append(files, aged{e.Name(), info.ModTime()})
		}
		if len(files) <= agyLogKeep {
			continue
		}
		sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
		removed := 0
		for _, f := range files[agyLogKeep:] {
			if root.Remove(filepath.Join(dir, f.name)) == nil {
				removed++
			}
		}
		logger.Debug("runtime pruned agy files", "dir", sub, "removed", removed)
	}

	// The state entries live directly under antigravity-cli, with the same
	// two parent components guarded as above. A symlink in the entry's own
	// place is skipped rather than removed, so the prune never has an
	// opinion about where a link points. RemoveAll through os.Root stays
	// inside the home for everything below the entry.
	cli := filepath.Join(".gemini", "antigravity-cli")
	if err := realDirs(root, ".gemini", cli); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Debug("runtime agy state not cleared", "error", err)
		}
		return
	}
	removed := 0
	for _, name := range agyStateEntries {
		p := filepath.Join(cli, name)
		fi, err := root.Lstat(p)
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			logger.Debug("runtime agy state not cleared", "entry", name, "error", errNotRealDir)
			continue
		}
		if root.RemoveAll(p) == nil {
			removed++
		}
	}
	if removed > 0 {
		logger.Debug("runtime cleared agy state", "removed", removed)
	}
}

// errNotRealDir is realDirs' verdict on a component that exists but is a
// symlink or not a directory. A missing component surfaces as the Lstat
// error itself, which wraps fs.ErrNotExist.
var errNotRealDir = errors.New("a path component is a symlink or not a directory")

// realDirs returns nil when every given path, relative to root, is a
// directory reached without following a symlink at that component.
func realDirs(root *os.Root, paths ...string) error {
	for _, p := range paths {
		fi, err := root.Lstat(p)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("%s: %w", p, errNotRealDir)
		}
	}
	return nil
}

// deniedAgyTools are the tool calls the analyzer must never make.
//
// agy is an agent: mid-analysis it decides to run shell commands. In
// --print mode nobody can approve them, so the turn dies with
//
//	permission check failed for command "ls -la":
//	user denied permission to run command: ls -la
//
// and the envelope returns status ERROR with an empty response, which the
// report parser reports as invalid JSON. With these rules configured the
// same prompt returns SUCCESS and a valid report: the model answers rather
// than reaching for a shell.
//
// The security argument is the stronger one. This analyzer's input is
// attacker-controlled log text, and a tool call it can be talked into is a
// prompt injection with a shell on the end of it. The compose file already
// keeps the Telegram token out of this process for the same reason.
var deniedAgyTools = []string{"run_command(*)", "write_file(*)", "*"}

// writeAgyToolPolicy merges the deny rules into settings.json inside the
// seeded tree, preserving whatever else the operator had there.
//
// Written unconditionally on every start, unlike the rest of the seed:
// this file is policy the container owns, not state agy accumulates. If it
// depended on what the host's settings.json happened to contain, the
// container's safety would depend on the operator's desktop configuration.
func writeAgyToolPolicy(geminiDir string) error {
	path := filepath.Join(geminiDir, "antigravity-cli", "settings.json")
	settings := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		// A settings file we cannot parse is replaced rather than
		// preserved: the policy matters more than the operator's other
		// keys, and agy replaces invalid settings with defaults anyway.
		_ = json.Unmarshal(raw, &settings)
	}
	perm, _ := settings["permission"].(map[string]any)
	if perm == nil {
		perm = map[string]any{}
	}
	perm["deny"] = deniedAgyTools
	settings["permission"] = perm

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// copyTree copies src into dst recursively: directories 0700, files 0600.
// The source is a read-only mount, so nothing here writes back to it.
// Symlinks are skipped rather than followed, a credential mount is not a
// place to chase links out of.
func copyTree(src, dst string) ([]string, error) {
	var skipped []string
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case !d.Type().IsRegular():
			return nil
		default:
			// Bootstrap only. agy owns $AGY_HOME once it runs: it
			// refreshes its token there and keeps
			// conversation_summaries.db and antigravity-cli/brain/
			// there. Re-copying the host tree on every start would
			// replace a refreshed token with the older host copy,
			// where no agy runs to keep it current, and would delete
			// accumulated memory on a restart. What is missing is
			// still copied, so a first seed interrupted halfway
			// completes on the next start.
			if _, serr := os.Stat(target); serr == nil {
				return nil
			}
			// A file the container cannot read must not cost it the
			// credential. This mount is agy's whole state directory,
			// caches and a conversation database included, owned by
			// the operator with only some files carrying a group the
			// container shares. On the target host an unreadable
			// cache entry aborted the walk before the token was
			// reached.
			if cerr := copyFile(path, target); cerr != nil {
				if errors.Is(cerr, fs.ErrPermission) {
					rel, _ := filepath.Rel(src, path)
					skipped = append(skipped, rel)
					return nil
				}
				return cerr
			}
			return nil
		}
	})
	return skipped, err
}

// copyFile streams src to dst at mode 0600, creating the parent.
//
// Streamed rather than read whole: this tree is 16.6 MB across 43 files on
// the target host and 16.3 MB of that is a single bundled browser helper,
// inside a container capped at mem_limit 1g. Nothing here needs the file
// in memory, so nothing here holds it.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// nextTickSeq is R3.1: read, increment, write atomically. Missing or
// unparseable starts at 1 and WARNs.
func nextTickSeq(cfg *config.Config, logger warner) int64 {
	path := filepath.Join(cfg.StateDir, "tick-seq")
	var seq int64 = 1
	// R3.1: "Missing or unparseable ⇒ start at 1 and WARN", both cases,
	// not only the unparseable one.
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		logger.Warn("tick-seq missing, starting at 1")
	} else if n, perr := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); perr == nil {
		seq = n + 1
	} else {
		logger.Warn("tick-seq unparseable, restarting at 1")
	}

	if tmp, err := os.CreateTemp(cfg.StateDir, ".tmp-tick-seq-*"); err == nil {
		tmp.WriteString(strconv.FormatInt(seq, 10))
		tmp.Close()
		os.Chmod(tmp.Name(), 0o644)
		os.Rename(tmp.Name(), path)
	}
	return seq
}

// analyzerFailsFile holds the unix timestamp (seconds, ASCII decimal) of
// the FIRST tick in the current run of consecutive analyzer-degraded
// ticks. It lives in STATE_DIR so a restart mid-outage does not hand the
// analyzer a fresh grace period.
//
// A timestamp, not a tick count: the outage the hold measures is a
// duration ("the analyzer has been down for DEGRADED_ALERT_AFTER"), and a
// count of ticks is only a proxy for that duration, one that is exact only
// when every intervening tick actually runs on schedule. Counting also
// forced a fencepost correction (ticks land at 0, TICK_INTERVAL, ...,  so
// the Nth tick proves only (N-1)*TICK_INTERVAL of elapsed time) and made
// the collector-failure path ambiguous: does a tick where the analyzer
// never even ran advance the count, reset it, or leave it alone? Recording
// wall-clock elapsed time answers all of that at once: the collector path
// leaves this file untouched (it neither bridges nor resets anything,
// R3.5), and the hold condition is simply "has enough real time passed".
const analyzerFailsFile = "analyzer-fails"

// maxSaneAnalyzerFailAge bounds how far in the past a stored "first
// degraded tick" timestamp may credibly be. No real outage lasts this
// long; a stored value older than this is corruption (a hand edit, a
// value from an incompatible build), not a genuine multi-year incident,
// and is treated the same as an unparseable one: reset to now, WARN.
const maxSaneAnalyzerFailAge = 10 * 365 * 24 * time.Hour

// analyzerHeld records the first degraded tick of the current outage and
// reports whether the tick should still be held: `now - first <
// DEGRADED_ALERT_AFTER`. A healthy tick (degraded=false) clears the marker
// and always returns false.
//
// now is the caller's single clock read (nowFor, C9), never a second
// time.Now() here.
//
// A write failure fails OPEN, not closed: it reports "not held" instead of
// trusting a `first` it could not persist. A tick that cannot write this
// file also cannot make its `first` durable, so trusting it anyway means a
// broken disk holds a real outage silent for as long as it stays broken; a
// spurious unfiltered alert on a bad write is the safe failure mode.
func analyzerHeld(cfg *config.Config, logger warner, now time.Time, degraded bool) bool {
	path := filepath.Join(cfg.StateDir, analyzerFailsFile)
	if !degraded {
		// ENOENT is the normal case (nothing to reset, or already clean)
		// and must stay silent; any other failure means a stale marker
		// survives this healthy tick, and a later blip would ride through
		// a hold that should have started fresh. That is worth a WARN.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			logger.Warn("could not reset analyzer failure marker")
		}
		return false
	}

	first := now
	if data, err := os.ReadFile(path); err == nil {
		text := strings.TrimSpace(string(data))
		if parsed, perr := strconv.ParseInt(text, 10, 64); perr != nil {
			logger.Warn("analyzer failure marker unparseable, restarting now")
		} else {
			stored := time.Unix(parsed, 0)
			switch {
			case stored.After(now):
				// A clock that jumped backwards must not produce a
				// permanent hold: now.Sub(stored) would be negative,
				// which is "less than" any positive DEGRADED_ALERT_AFTER
				// forever.
				logger.Warn("analyzer failure marker is in the future, restarting now")
			case now.Sub(stored) > maxSaneAnalyzerFailAge:
				logger.Warn("analyzer failure marker absurdly old, restarting now")
			default:
				first = stored
			}
		}
	}

	if err := state.WriteAtomic(cfg.StateDir, analyzerFailsFile, []byte(strconv.FormatInt(first.Unix(), 10)), 0o644); err != nil {
		logger.Warn("could not persist analyzer failure marker, alert sent unfiltered instead of held")
		return false
	}
	return now.Sub(first) < cfg.DegradedAlertAfter
}

// assertBinDirReadOnly is R2 step 3: prove read_only:true took effect.
// Writable ⇒ WARN, continue, never block ticks on a lint.
func assertBinDirReadOnly(logger warner) {
	probe := "/usr/local/bin/.preflight-probe"
	if err := os.WriteFile(probe, []byte{}, 0o600); err == nil {
		os.Remove(probe)
		logger.Warn("runtime /usr/local/bin is writable, read_only:true may not be in effect")
	}
}

// StartupPreflight is R2's startup sequence steps 2-5: filesystem
// preflight (STATE_DIR, /tmp, a readable journal dir, $HOST_PROC/uptime,
// and the C4 subdirectory MkdirAlls), the /usr/local/bin read-only lint,
// and agy-home seeding. R2 requires this "once before --loop starts
// ticking, and once before the single tick in --once", step 1
// (config.Load) and step 6 (signal.NotifyContext, --loop only) are the
// caller's job. Returns the C2 exit code: 69 for a state-dir failure, 78
// for any other preflight failure, 0 on success.
func StartupPreflight(ctx context.Context, cfg *config.Config) (int, error) {
	logger := newLogger(cfg)

	if err := preflight(cfg); err != nil {
		var sderr *stateDirError
		if errors.As(err, &sderr) {
			logger.Error("STATE_DIR missing or unwritable", "path", sderr.path)
			return 69, err
		}
		var pferr *preflightError
		if errors.As(err, &pferr) {
			logger.Error("startup preflight failed", "path", pferr.path, "reason", pferr.reason)
		}
		return 78, err
	}

	// The directory-level check above proves the mount exists and is
	// listable; it does not prove journalctl can actually read journal
	// CONTENT through it. Run before the first tick so a stale
	// JOURNAL_GID fails loud at startup instead of silently reporting
	// all-clear forever.
	//
	// ctx is deliberately NOT the shutdown-signal context both callers
	// hold (Loop's sigCtx, --once's request context): wiring a
	// SIGTERM-cancellable context in here would make a real SIGTERM
	// arriving during preflight report exit 78 ("context canceled") from
	// checkJournalReadable's own exec.CommandContext, instead of the
	// exit 0 R2 contracts for a signal-driven shutdown ("--loop
	// terminates only with 0 (signal), 78 (config/mount preflight) ...").
	// checkJournalReadable already self-bounds via cfg.SectionTimeout (or
	// its own 10s default), so the accepted cost of NOT threading shutdown
	// cancellation through is a startup tail of at most that timeout,
	// comfortably inside the 15s stop_grace_period compose sets. Both
	// current callers pass context.Background() for exactly this reason;
	// ctx stays a parameter (rather than being dropped) so a future
	// caller with a different tradeoff, or a test that wants to bound the
	// whole preflight call itself, still can.
	if err := checkJournalReadable(ctx, cfg); err != nil {
		var pferr *preflightError
		if errors.As(err, &pferr) {
			logger.Error("journal not readable", "path", pferr.path, "reason", pferr.reason)
		}
		return 78, err
	}

	assertBinDirReadOnly(logger)
	seedAgyHome(cfg, logger)
	return 0, nil
}

// Loop runs StartupPreflight once, then ticks every TICK_INTERVAL until
// SIGTERM/SIGINT. It terminates only with 0 (signal), 78 (config/mount
// preflight) or 69 (state dir), every other tick failure is logged and
// the loop continues (R2).
func Loop(ctx context.Context, cfg *config.Config, d Deps) (int, error) {
	logger := newLogger(cfg)

	if code, err := StartupPreflight(context.Background(), cfg); err != nil {
		return code, err
	}

	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var ticks int64
	for {
		select {
		case <-sigCtx.Done():
			logger.Info("runtime stopped", "ticks", ticks)
			return 0, nil
		default:
		}

		seq := nextTickSeq(cfg, logger)
		// A tick already in flight gets 5s after shutdown is requested,
		// then its context is cancelled (R2 step 6).
		tickCtx, cancel := gracefulTickContext(sigCtx)
		res := Tick(tickCtx, cfg, seq, d)
		cancel()
		ticks++
		if res.ExitCode != 0 {
			logger.Warn("tick rc=" + strconv.Itoa(res.ExitCode))
		}

		select {
		case <-sigCtx.Done():
			logger.Info("runtime stopped", "ticks", ticks)
			return 0, nil
		case <-time.After(cfg.TickInterval):
		}
	}
}

// gracefulTickContext derives a tick-scoped context that survives the
// parent's cancellation for 5s (R2 step 6: "a tick already in flight gets
// 5s, then its context is cancelled"), so a slow section gets a grace
// period instead of being cut off the instant a signal arrives.
func gracefulTickContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan struct{})
	go func() {
		select {
		case <-parent.Done():
			select {
			case <-time.After(5 * time.Second):
				cancel()
			case <-stop:
			}
		case <-stop:
		}
	}()
	return ctx, func() { close(stop); cancel() }
}
