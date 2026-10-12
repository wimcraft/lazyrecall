// Package resume takes a chosen session back to its agent, running directly
// in the terminal LazyRecall was invoked from (spec session-resume; design.md
// decisions 1-3 of change resume-in-current-terminal). It changes to the
// session's recorded working directory, then hands off to the agent that
// produced it by replacing the running LazyRecall process (syscall.Exec on unix,
// see exec_unix.go) or, where that is unavailable, by running the agent as a
// child and exiting with its status (see exec_fallback.go). This supersedes
// the earlier herdr-delegated design: LazyRecall cannot leave its caller's shell
// in a directory after it exits, but it can run a program in one directly,
// which is all resumption ever needed. The only write this package makes
// into a session source is putting back a transcript LazyRecall kept a copy
// of and the agent had deleted (restoreTranscript) - never overwriting, only
// ever creating the exact file the agent itself wrote.
package resume

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wimcraft/lazyrecall/internal/config"
)

// Target is the session being resumed.
type Target struct {
	Source          string
	SourceSessionID string
	CWD             *string
	GitRepoRoot     *string // offered as a fallback location if CWD is gone
	DirExists       *bool
	RepoRootExists  *bool
	Resumable       bool

	// TranscriptGone is true when the session recorded a transcript file
	// (search.Item.TranscriptPath) and that file no longer exists on disk -
	// normally the agent's own retention cleanup. There is nothing left for
	// the agent to resume, so Resume must explain that instead of starting
	// an agent that would only fail with a message saying nothing about why
	// (internal/search: TranscriptGone).
	TranscriptGone bool

	// Resume is the already-resolved argv template this session's source
	// uses to resume a session, with "{id}" standing in for the session id
	// (internal/config: a source's Resume). It is resolved by the caller
	// and passed in, exactly like ProfileEnv: resume stays a pure function
	// of its Target and never reads the config itself. agentCommand
	// substitutes "{id}" element-wise and hands the result to execAgent
	// directly - an argv vector, never a shell command string. Empty/nil
	// means the source has no configured resume command and cannot be
	// resumed.
	Resume []string

	// Model is the model the caller asked to resume with, if any
	// (lazyrecall resume --model). Empty means resume in whatever model
	// the agent would otherwise choose, which is the behaviour every
	// resume had before this option existed. When set, agentCommand
	// appends ModelTemplate (with "{model}" replaced) after the resume
	// argv; a Model with no ModelTemplate is a hard failure (Resume),
	// not a silently dropped option.
	Model string

	// ModelTemplate is the argv template a source uses to select a model,
	// with "{model}" standing in for the value (internal/config: a
	// source's Model). Resolved by the caller and passed in, like Resume.
	// Empty/nil means the source has no configured model flag.
	ModelTemplate []string

	// ProfileEnv carries environment variable overrides needed to run the
	// agent against the installation this session's own profile belongs to
	// - e.g. CLAUDE_CONFIG_DIR, for a claude session whose profile is not
	// the default installation (design.md decision 3 of change
	// fix-resume-session-identity: "internal/profile already models each
	// profile's Claude configuration root ... apply the session's profile
	// configuration to the environment"). Resolved by the caller, which
	// already knows how to map a profile name onto a config root via
	// internal/profile; empty/nil when the source's profile does not vary
	// (every source but claude, today - task 2.2, "apply only what the
	// session's profile requires").
	ProfileEnv map[string]string

	// ProfileUnresolved is true when the caller could not determine the
	// configuration this session's profile requires. Resume must report
	// this and must not start the agent: doing so could silently resume a
	// different session under the same identifier, in the wrong
	// installation (design.md decision 3's explicit warning; spec
	// session-resume, "Profile configuration cannot be determined").
	ProfileUnresolved bool
	// ProfileUnresolvedReason explains why, for the message Resume reports.
	ProfileUnresolvedReason string

	// RestoreFrom and RestoreTo are set together for a session whose own
	// transcript file is gone but which LazyRecall kept a copy of
	// (search.Item.Snapshotted): RestoreFrom is LazyRecall's copy, RestoreTo
	// the path the agent itself wrote. Before handing off, Resume copies
	// the copy back to RestoreTo, because the agent cannot resume a
	// session whose transcript is missing ("No conversation found with
	// session ID: ..."). This is the one place LazyRecall writes into
	// another tool's folder, so the write is deliberately narrow: it only
	// ever creates the exact file the agent wrote and later deleted, and
	// never overwrites an existing one. Both empty - the common case -
	// means there is nothing to restore.
	RestoreFrom string
	RestoreTo   string
}

// agentCommand is the program and argument vector that resumes a specific
// session non-interactively, built by substituting the session id into the
// source's configured argv template (t.Resume; internal/config: a source's
// Resume). When t.Model is set, the source's model template (t.ModelTemplate)
// is appended after it, with "{model}" replaced. argv[0] is conventionally
// the program's own name, matching what that program would see if invoked
// directly from a shell; the executable itself is resolved separately, via
// exec.LookPath, before anything else happens (decision 2).
//
// The result is deliberately an argv vector, never a shell command string:
// the config file is user-supplied input, and execAgent execs the program
// directly with no shell in between, so metacharacters in a template
// element (";", "&&", "|", ...) stay literal bytes of that one argument. A
// shell string built from the same template would be parsed and executed,
// turning a config file into a shell-injection surface - which is exactly
// why substitution is strings.ReplaceAll per element and nothing more.
func agentCommand(t Target) (program string, argv []string, ok bool) {
	if len(t.Resume) == 0 {
		return "", nil, false
	}
	argv = make([]string, len(t.Resume))
	for i, el := range t.Resume {
		argv[i] = strings.ReplaceAll(el, "{id}", t.SourceSessionID)
	}
	if t.Model != "" {
		for _, el := range t.ModelTemplate {
			argv = append(argv, strings.ReplaceAll(el, "{model}", t.Model))
		}
	}
	return argv[0], argv, true
}

// missingSettingMessage explains that a source cannot honour a value the
// caller asked for because its argv template for that value is empty,
// naming the field in the caller's own vocabulary and the exact config key
// to add under [sources.<source>], so the fix is copyable. Used for both a
// resume's model and a launch's id/name/model/prompt, which share the shape.
func missingSettingMessage(source, field, key string) string {
	return fmt.Sprintf("%s has no %s setting configured (%s = [...] under [sources.%s]).", source, field, key, source)
}

// LaunchTarget describes a new session to start. Unlike Target, which
// resumes a session that already exists and so inherits its working
// directory, everything here is chosen: the source, the directory, and
// optionally the session's id, name, model and first prompt.
type LaunchTarget struct {
	// Source names the agent to start; only used in failure messages.
	Source string

	// CWD is the directory the agent is started in. Required, and has no
	// fallback: a new session has no recorded working directory to offer
	// instead. When the directory is missing, Mkdir decides whether
	// LazyRecall creates it or refuses.
	CWD   string
	Mkdir bool

	// ID, Name, Model and Prompt are the optional choices; an empty one
	// contributes nothing to the argv. A non-empty one whose template in
	// Templates is empty is a hard failure, never a silently dropped
	// option - asking for a model a source cannot select must not look
	// like it worked.
	ID     string
	Name   string
	Model  string
	Prompt string

	// Templates is the source's configured argv templates (internal/config:
	// a Source). New is required; the rest are consulted only for the
	// values above that are set. Passed in resolved, exactly like
	// Target.Resume, so resume stays a pure function of its arguments and
	// never reads the config itself.
	Templates config.Source

	// ProfileEnv is the environment override for the installation this
	// launch targets, the same shape (and the same purpose) as
	// Target.ProfileEnv for a resume.
	ProfileEnv map[string]string
}

// launchCommand is the program and argument vector that starts a new
// session: t.Templates.New, then the id, name, model and prompt templates
// for whichever values are set. Like agentCommand the result is an argv
// vector, never a shell string - substitution is strings.ReplaceAll per
// element so a value cannot become a shell command no matter what it
// contains. Each template only has its own placeholder replaced, and only
// once, so a name or prompt that happens to contain "{prompt}" or "{id}"
// reaches the agent as typed instead of being substituted a second time.
func launchCommand(t LaunchTarget) (program string, argv []string) {
	tpl := t.Templates
	argv = append(argv, tpl.New...)
	add := func(elems []string, placeholder, value string) {
		if value == "" {
			return
		}
		for _, el := range elems {
			argv = append(argv, strings.ReplaceAll(el, placeholder, value))
		}
	}
	add(tpl.NewID, "{id}", t.ID)
	add(tpl.NewName, "{name}", t.Name)
	add(tpl.Model, "{model}", t.Model)
	add(tpl.Prompt, "{prompt}", t.Prompt)
	return argv[0], argv
}

// Launch starts a new session in t's source, in t's directory, and hands
// off to its agent the same way Resume does - replacing the running
// LazyRecall process, or exiting with the agent's status on the fallback.
// It never writes to another tool's data: the only side effects are
// optionally creating the requested directory and handing off to an agent.
func Launch(t LaunchTarget) Outcome {
	if len(t.Templates.New) == 0 {
		return Outcome{Failed: true, Message: fmt.Sprintf("%s has no new-session command configured (new = [...] under [sources.%s]).", t.Source, t.Source)}
	}

	// A value the caller asked for that its source cannot express is a
	// hard failure, checked before anything touches the disk: starting an
	// agent without the model or name that was requested would look like
	// success while silently ignoring the request.
	for _, opt := range []struct {
		value, field, key string
		tpl               []string
	}{
		{t.ID, "session id", "new_id", t.Templates.NewID},
		{t.Name, "name", "new_name", t.Templates.NewName},
		{t.Model, "model", "model", t.Templates.Model},
		{t.Prompt, "prompt", "prompt", t.Templates.Prompt},
	} {
		if opt.value != "" && len(opt.tpl) == 0 {
			return Outcome{Failed: true, Message: missingSettingMessage(t.Source, opt.field, opt.key)}
		}
	}

	// The directory is the one requirement a launch has no fallback for
	// (spec session-resume's "makes no other change" sibling): it is
	// either there, created on request, or the launch stops.
	if fi, err := os.Stat(t.CWD); err != nil {
		if !os.IsNotExist(err) {
			return Outcome{Failed: true, Message: fmt.Sprintf("could not check %q: %v", t.CWD, err)}
		}
		if !t.Mkdir {
			return Outcome{Failed: true, Message: fmt.Sprintf("%q does not exist; pass --mkdir to create it.", t.CWD)}
		}
		if err := os.MkdirAll(t.CWD, 0o755); err != nil {
			return Outcome{Failed: true, Message: fmt.Sprintf("could not create %q: %v", t.CWD, err)}
		}
	} else if !fi.IsDir() {
		return Outcome{Failed: true, Message: fmt.Sprintf("%q is not a directory.", t.CWD)}
	}

	program, argv := launchCommand(t)

	path, err := exec.LookPath(program)
	if err != nil {
		return Outcome{Failed: true, Message: fmt.Sprintf("%s is not installed; a session cannot be started without it.", program)}
	}

	if err := os.Chdir(t.CWD); err != nil {
		return Outcome{Failed: true, Message: fmt.Sprintf("could not change to %q: %v", t.CWD, err)}
	}

	env := mergeEnv(os.Environ(), t.ProfileEnv)
	if err := execAgent(path, argv, env); err != nil {
		return Outcome{Failed: true, Message: fmt.Sprintf("could not start %s: %v", program, err)}
	}

	// Unreachable when the agent actually started, for the same reason as
	// in Resume: execAgent either replaced this process or exited with the
	// agent's status. A safety net, not a panic.
	return Outcome{Failed: true, Message: "launch ended unexpectedly"}
}

// Outcome describes why resumption did not place the user into the session.
// Resume never returns a success value: when the agent actually starts, it
// either replaces the LazyRecall process outright or (on the child-process
// fallback) runs to completion and LazyRecall exits with its status - either way
// there is no LazyRecall left to report anything back to (spec session-resume,
// "Resumption reports success only when the agent runs"). A value is only
// ever returned along a failure path.
type Outcome struct {
	// NotResumable is true when the session has no working directory at all
	// and never can be resumed.
	NotResumable bool

	// DirectoryMissing is true when the recorded working directory no
	// longer exists.
	DirectoryMissing bool

	// TranscriptGone is true when the session's transcript file was
	// recorded but no longer exists, so the agent has nothing to resume.
	// Reported before program resolution or any working-directory change:
	// the failure belongs to the session's own history, not to the machine
	// the agent is installed on.
	TranscriptGone bool

	// AlternativeOffered is the repository root offered in place of a
	// missing working directory, when it still exists. Never a substitute
	// directory LazyRecall picked on its own - only ever this session's own
	// recorded repo root.
	AlternativeOffered *string

	// Failed is true when the agent could not be started for any other
	// reason: its program is not installed, the working directory could not
	// be entered, or starting it outright failed.
	Failed bool

	// Message is a human-readable summary of the outcome.
	Message string
}

// execAgent replaces the running process with the program at path (argv is
// its full argument vector, argv[0] included; env is the environment to run
// it under) - or, on platforms where that is unavailable, runs it as a child
// with the terminal passed through and exits this process with its status.
// It only ever returns when the agent could not be started at all: on any
// path that actually starts the agent, this process is gone by the time
// execAgent would return. A package variable so tests can substitute a
// double that records its arguments instead of truly replacing the test
// binary. Defaults to platformExec, defined per-platform in exec_unix.go
// (syscall.Exec) or exec_fallback.go (child process + os.Exit).
var execAgent = platformExec

// restoreTranscript copies LazyRecall's own copy of a session's transcript
// (from) back to the path the agent wrote it to (to), which is the one write
// LazyRecall makes into another tool's data and the only way a session whose
// agent deleted its own transcript can be resumed again ("No conversation
// found with session ID: ..."). It creates a file the agent itself had
// written and later removed, and only ever that: an already existing to -
// the transcript back, or the agent rewriting it right now - is left
// completely untouched, because LazyRecall has no way to tell a stale file
// from a live one and overwriting either could destroy history it does not
// own. to is created with O_EXCL so the check and the create cannot be raced
// apart, and a partial file is removed if the copy fails partway, so a later
// attempt sees the same state an absent file would give it.
func restoreTranscript(from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(to)
		return err
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		os.Remove(to)
		return err
	}
	if err := dst.Close(); err != nil {
		os.Remove(to)
		return err
	}
	return nil
}

// Resume places the user into t's session: restores the session's transcript
// copy if it has one (see RestoreFrom), changes to its recorded working
// directory, then hands off to the agent that produced it (design.md
// decisions 1-3; spec session-resume). The only side effects are that
// restore, which never overwrites an existing file, and handing off to
// another process.
func Resume(t Target) Outcome {
	if !t.Resumable || t.CWD == nil || *t.CWD == "" {
		return Outcome{
			NotResumable: true,
			Message:      "This session has no recorded working directory and cannot be resumed.",
		}
	}

	if t.TranscriptGone {
		return Outcome{
			TranscriptGone: true,
			Message:        "This session's transcript file no longer exists, so its agent cannot resume it. It was most likely removed by the agent's own cleanup (for Claude Code, cleanupPeriodDays in settings.json).",
		}
	}

	if t.DirExists != nil && !*t.DirExists {
		out := Outcome{DirectoryMissing: true}
		if t.GitRepoRoot != nil && *t.GitRepoRoot != "" && (t.RepoRootExists == nil || *t.RepoRootExists) {
			out.AlternativeOffered = t.GitRepoRoot
			out.Message = fmt.Sprintf("The recorded working directory %q no longer exists. Its repository root %q does; you can resume there instead.", *t.CWD, *t.GitRepoRoot)
		} else {
			out.Message = fmt.Sprintf("The recorded working directory %q no longer exists, and no related directory could be found. The session's location is gone.", *t.CWD)
		}
		return out
	}

	// A session whose profile configuration could not be determined must
	// never be started at all (design.md decision 3) - checked before
	// anything else so an unresolved profile can never race past program
	// resolution or the working-directory change below.
	if t.ProfileUnresolved {
		msg := "could not determine which installation's configuration this session's profile requires"
		if t.ProfileUnresolvedReason != "" {
			msg += ": " + t.ProfileUnresolvedReason
		}
		msg += fmt.Sprintf("; refusing to start %s, since doing so could resume the wrong session.", t.Source)
		return Outcome{Failed: true, Message: msg}
	}

	program, argv, ok := agentCommand(t)
	if !ok {
		return Outcome{Failed: true, Message: fmt.Sprintf("%q is not an agent lazyrecall knows how to resume.", t.Source)}
	}

	// A model the caller asked for that this source cannot select is a
	// hard failure before program lookup, not a resume that silently runs
	// the default model - the whole point of --model is that the user
	// chose one.
	if t.Model != "" && len(t.ModelTemplate) == 0 {
		return Outcome{Failed: true, Message: missingSettingMessage(t.Source, "model", "model")}
	}

	// Resolve the program before touching anything else (decision 2; spec
	// "makes no other change" when the program is missing) - this is the
	// one dependency resumption genuinely has, and it belongs to the agent,
	// not to LazyRecall.
	path, err := exec.LookPath(program)
	if err != nil {
		return Outcome{Failed: true, Message: fmt.Sprintf("%s is not installed; the session cannot be resumed without it.", program)}
	}

	// Restore the transcript copy, when the session has one, only after the
	// program resolved: a source with no agent installed must not leave a
	// file behind in its folder for a resume that then fails. This is also
	// why the restore sits before the chdir - it reads and writes absolute
	// paths, so the working directory is irrelevant to it, and failing the
	// restore must not have moved the process anywhere.
	if t.RestoreTo != "" {
		if err := restoreTranscript(t.RestoreFrom, t.RestoreTo); err != nil {
			return Outcome{Failed: true, Message: fmt.Sprintf("could not restore the transcript from lazyrecall's copy: %v", err)}
		}
	}

	if err := os.Chdir(*t.CWD); err != nil {
		return Outcome{Failed: true, Message: fmt.Sprintf("could not change to %q: %v", *t.CWD, err)}
	}

	env := mergeEnv(os.Environ(), t.ProfileEnv)
	if err := execAgent(path, argv, env); err != nil {
		return Outcome{Failed: true, Message: fmt.Sprintf("could not start %s: %v", program, err)}
	}

	// Unreachable when the agent actually started: execAgent either
	// replaced this process (never returning at all) or ran the agent to
	// completion and exited this process with its status. Kept as a safety
	// net rather than a panic, in case a future execAgent implementation
	// ever legitimately returns nil without one of those two things having
	// happened.
	return Outcome{Failed: true, Message: "resumption ended unexpectedly"}
}

// mergeEnv returns base with every key in overrides set to its override
// value, replacing any existing entry for that key rather than appending a
// second one alongside it. A plain append would leave two entries for the
// same variable in the environment slice; some C runtimes' getenv scans
// from the start and would still see the old value, silently defeating the
// override this function exists to apply (design.md decision 3). Returns
// base unmodified (not even a copy) when overrides is empty, which is the
// overwhelming common case (task 2.2: every source but claude today).
func mergeEnv(base []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if _, overridden := overrides[key]; overridden {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}
