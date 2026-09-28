package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iammatthias/farfield/lib/store"
)

// agentFiles is the persona.
//
// Embedded and written out at boot rather than provisioned separately, so the
// voice can never drift from the binary that speaks it. Both names are written
// because the two harnesses look for different ones — omp reads AGENTS.md,
// Claude Code reads CLAUDE.md — and ff-agent may be pointed at either.
//
//go:embed agent
var agentFiles embed.FS

// agentRunner turns a message that named no command into a reply.
//
// It exists as a type rather than a method because everything about it is a
// bound: how long one turn may take, how many may run at once, when silence
// becomes rude. An agent turn is the one thing here that can take minutes,
// cost money, and outlive the request that started it.
type agentRunner struct {
	cmd      string        // ff-agent, or whatever FF_AGENT_CMD names
	stateDir string        // sessions and the workspace live here
	timeout  time.Duration // hard ceiling on one turn
	ackAfter time.Duration // silence past this is rude; say "on it"
	maxJobs  int
	enabled  bool

	db     *sql.DB
	photon *photonClient

	sem chan struct{}

	mu   sync.Mutex
	live map[string]context.CancelFunc
	// chats serializes turns within one conversation: two agent processes
	// resuming the same session history at once would interleave or lose a
	// turn. One slot per chat; different chats still run side by side.
	chats map[string]chan struct{}
}

// errQueueTimeout is a turn that never got to run.
var errQueueTimeout = errors.New("timed out waiting for a free slot")

func newAgentRunner(db *sql.DB, photon *photonClient, stateDir string) *agentRunner {
	a := &agentRunner{
		db:       db,
		photon:   photon,
		cmd:      store.Env("FF_AGENT_CMD", "ff-agent"),
		stateDir: stateDir,
		timeout:  envDuration("SWITCHBOARD_AGENT_TIMEOUT", 10*time.Minute),
		ackAfter: envDuration("SWITCHBOARD_AGENT_ACK_AFTER", 8*time.Second),
		maxJobs:  envInt("SWITCHBOARD_AGENT_MAX_JOBS", 3),
		live:     map[string]context.CancelFunc{},
		chats:    map[string]chan struct{}{},
	}
	// Absent binary is a configuration state, not a crash: switchboard still
	// answers slash commands, which is the half that must never depend on a
	// model being reachable.
	if _, err := exec.LookPath(a.cmd); err != nil {
		slog.Warn("agent disabled", "cmd", a.cmd, "err", err)
	} else {
		a.enabled = true
	}
	a.sem = make(chan struct{}, a.maxJobs)
	return a
}

// prepare writes the persona into the workspace the agent runs in.
//
// The harnesses discover instructions by walking up from the working directory,
// so the working directory is the delivery mechanism. It is deliberately NOT
// the farfield checkout: an agent sitting in the source tree would treat every
// question as an invitation to edit it.
func (a *agentRunner) prepare() error {
	ws := a.workspace()
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return err
	}
	entries, err := agentFiles.ReadDir("agent")
	if err != nil {
		return err
	}
	for _, e := range entries {
		body, err := agentFiles.ReadFile(filepath.Join("agent", e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(ws, e.Name()), body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (a *agentRunner) workspace() string { return filepath.Join(a.stateDir, "workspace") }

// sessionDir is where one conversation's history lives.
//
// Keyed by a hash of the chat rather than the chat id itself: the id is an
// Apple handle containing a phone number, and it would otherwise become a
// directory name on disk and a line in every log that touches a path.
func (a *agentRunner) sessionDir(chatGUID string) string {
	sum := sha256.Sum256([]byte(chatGUID))
	return filepath.Join(a.stateDir, "sessions", hex.EncodeToString(sum[:8]))
}

// start records a job and runs the turn in the background.
//
// Background because the caller is Photon's webhook. Holding that open for a
// model turn invites a delivery timeout and a retry, and a retried "do the
// thing" is the thing done twice. The reply goes back out over the line when it
// is ready, which is also what makes long work possible at all.
func (a *agentRunner) start(rec *Message, text string, atts []attachment) (*Job, error) {
	job := &Job{
		ID: newJobID(), MessageID: rec.ID, ChatGUID: rec.ChatGUID,
		Sender: rec.Sender, Prompt: text, Status: jobRunning,
	}
	if err := insertJob(a.db, job); err != nil {
		return nil, err
	}
	go a.run(job, atts)
	return job, nil
}

// run executes one turn. It always reaches a terminal state and always says
// something, because the alternative is a message that is never answered.
func (a *agentRunner) run(job *Job, atts []attachment) {
	// Cancellable from the moment the job exists, not from when it gets a
	// slot: a /cancel aimed at a queued job must stop it, rather than report
	// "not running" and let it run anyway once the queue clears.
	jobCtx, stop := context.WithCancel(context.Background())
	defer stop()
	a.mu.Lock()
	a.live[job.ID] = stop
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.live, job.ID)
		a.mu.Unlock()
	}()

	// Silence past ackAfter is rude; silence before it is just being quick.
	// This is the only message that is ever sent before the answer, and it is
	// sent at most once — the budget for a whole turn is two. It covers the
	// wait in the queue too: a queued turn is as silent as a slow one.
	acked := make(chan struct{})
	ackOnce := sync.OnceFunc(func() { close(acked) })
	defer ackOnce()
	go func() {
		select {
		case <-time.After(a.ackAfter):
			a.say(job.ChatGUID, "on it · "+job.ID)
		case <-acked:
		}
	}()

	// Queue rather than reject: a second message while one is running is
	// normal, and being told "too busy" by your own house is absurd. The
	// ceiling is on concurrency, not on patience.
	release, err := a.acquire(jobCtx, job.ChatGUID)
	if err != nil {
		ackOnce()
		if errors.Is(err, context.Canceled) {
			a.finish(job, jobCancelled, "", "cancelled")
			a.say(job.ChatGUID, "cancelled · "+job.ID)
			return
		}
		a.finish(job, jobFailed, "", err.Error())
		a.say(job.ChatGUID, fmt.Sprintf("✗ %s never started · %s", job.ID, err))
		return
	}
	defer release()

	ctx, cancel := context.WithTimeout(jobCtx, a.timeout)
	defer cancel()

	// Photos are fetched here rather than in the webhook: the bytes can be
	// several megabytes off a phone, and Photon is waiting on the other end of
	// that handler.
	files, err := stageAttachments(ctx, a.photon.Download, atts)
	defer cleanupTempFiles(files)

	var out string
	if err == nil {
		out, err = a.exec(ctx, job, files)
	}
	ackOnce()

	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		a.finish(job, jobCancelled, "", "cancelled")
		a.say(job.ChatGUID, "cancelled · "+job.ID)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		a.finish(job, jobFailed, "", "timed out after "+a.timeout.String())
		a.say(job.ChatGUID, fmt.Sprintf("✗ %s gave up after %s", job.ID, a.timeout))
	case err != nil:
		a.finish(job, jobFailed, "", err.Error())
		a.say(job.ChatGUID, "✗ "+firstLine([]byte(err.Error())))
	default:
		reply := strings.TrimSpace(out)
		a.finish(job, jobDone, reply, "")
		// An empty answer still gets a message. A turn that finishes in silence
		// is indistinguishable from one that never ran.
		if reply == "" {
			reply = "done · " + job.ID
		}
		a.say(job.ChatGUID, reply)
	}
}

// acquire waits for this chat's turn and then a free slot, in that order, so a
// turn queued behind its own conversation holds no slot another chat could
// use. Both waits share one deadline and end early on cancel. The returned
// func gives both back.
func (a *agentRunner) acquire(ctx context.Context, chat string) (func(), error) {
	deadline := time.NewTimer(a.timeout)
	defer deadline.Stop()

	lock := a.chatLock(chat)
	select {
	case lock <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-deadline.C:
		return nil, errQueueTimeout
	}
	select {
	case a.sem <- struct{}{}:
	case <-ctx.Done():
		<-lock
		return nil, ctx.Err()
	case <-deadline.C:
		<-lock
		return nil, errQueueTimeout
	}
	return func() { <-a.sem; <-lock }, nil
}

// chatLock returns the one-slot channel that serializes a chat's turns.
func (a *agentRunner) chatLock(chat string) chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	lock, ok := a.chats[chat]
	if !ok {
		lock = make(chan struct{}, 1)
		a.chats[chat] = lock
	}
	return lock
}

// exec runs the agent and returns its reply.
//
// stdout is the reply and nothing else; stderr is progress and diagnostics and
// is captured only to explain a failure. That split is why nothing streams into
// the thread: there is no intermediate output to leak.
func (a *agentRunner) exec(ctx context.Context, job *Job, files []namedTempFile) (string, error) {
	args := []string{"--prompt", promptWithAttachments(job.Prompt, files), "--session-dir", a.sessionDir(job.ChatGUID)}
	for _, f := range files {
		args = append(args, "--file", f.Path)
	}

	cmd := exec.CommandContext(ctx, a.cmd, args...)
	cmd.Dir = a.workspace()
	cmd.Env = agentEnv(os.Environ())
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// The agent gets its own process group, and cancelling kills the group
	// rather than the process.
	//
	// CommandContext's default is to signal the direct child only, and an
	// agent harness is a wrapper that spawns the model process underneath it
	// — so /cancel killed omp and left the actual turn running. Worse, it
	// looked like nothing happened: the orphan inherits the pipes this
	// function reads, Run waits for those to close, and the reply ("cancelled
	// · id") is only sent once Run returns. Cancelling a long turn therefore
	// hung until the orphan finished on its own. The switchboard test caught
	// it every CI run for a week — the stub's `sleep 60` outliving the `sh`
	// that owned it is the same bug in miniature.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Negative pid means the group. Setpgid above makes the child its
		// leader, so this reaches every descendant that has not left it.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// A descendant that escapes the group (its own setsid) could still hold
	// the pipes open forever. Past this much grace, stop waiting and take
	// whatever output arrived: a cancel that does not return is not a cancel.
	cmd.WaitDelay = 5 * time.Second

	started := time.Now()
	err := cmd.Run()
	slog.Info("agent turn", "job", job.ID, "dur", time.Since(started).Round(time.Second),
		"bytes", stdout.Len(), "err", err)
	if err != nil {
		detail := strings.TrimSpace(lastLines(stderr.String(), 3))
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("agent failed: %s", detail)
	}
	return stdout.String(), nil
}

// promptWithAttachments names the staged photo paths at the end of the prompt.
//
// The harness shows the model each image, but not where it lives on disk — and
// posting a texted photo means handing that file to `farfield feed --file`.
// The persona documents the `[attached: …]` line. A bare photo arrives as the
// line alone, which is how the agent tells "no caption" from a caption.
func promptWithAttachments(prompt string, files []namedTempFile) string {
	if len(files) == 0 {
		return prompt
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	note := "[attached: " + strings.Join(paths, ", ") + "]"
	if strings.TrimSpace(prompt) == "" {
		return note
	}
	return prompt + "\n\n" + note
}

// agentEnv is the environment an agent turn runs with: an allowlist, never
// switchboard's own.
//
// switchboard is started with the whole fleet .env — every app's write key,
// the admin password, the webhook and Photon secrets, the site's deploy hook.
// An agent turn is a model reading text from outside (a pasted link, a
// forwarded message), so anything in its environment is one prompt injection
// from being echoed back or used. It gets what a harness needs to run, its own
// settings, and farfield's scoped FARFIELD_* keys — minted per app, revocable
// without touching anything else. The model credential is not passed at all:
// ff-agent loads it from /etc/farfield/agent.env itself.
func agentEnv(parent []string) []string {
	base := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
		"LANG": true, "TZ": true, "TMPDIR": true, "TERM": true,
	}
	var out []string
	for _, kv := range parent {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch {
		case base[name],
			strings.HasPrefix(name, "LC_"),
			strings.HasPrefix(name, "XDG_"),
			strings.HasPrefix(name, "FF_AGENT_"),
			strings.HasPrefix(name, "FARFIELD_"),
			// Service locations are not secrets — but a URL that carries a
			// token (the site's deploy hook, a webhook) is.
			strings.HasSuffix(name, "_URL") && !strings.Contains(name, "HOOK") && !strings.Contains(name, "SECRET"):
			out = append(out, kv)
		}
	}
	return out
}

// cancel stops a running turn. Reports whether there was one to stop.
func (a *agentRunner) cancel(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if stop, ok := a.live[id]; ok {
		stop()
		return true
	}
	return false
}

func (a *agentRunner) finish(job *Job, status, result, errMsg string) {
	if err := finishJob(a.db, job.ID, status, result, errMsg); err != nil {
		slog.Error("record job outcome", "job", job.ID, "err", err)
	}
}

// say pushes a message into the thread out of band. Best effort by design: a
// failed send must not retry the work that produced it.
func (a *agentRunner) say(chatGUID, text string) {
	if a.photon == nil || chatGUID == "" || strings.TrimSpace(text) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.photon.SendText(ctx, chatGUID, text); err != nil {
		slog.Warn("agent reply failed", "err", err)
	}
}

// ── attachments ────────────────────────────────────────────────────────────

// namedTempFile is one inbound photo on disk, where the agent can open it.
type namedTempFile struct {
	Name string
	Path string
	dir  string
}

// stageAttachments writes inbound photos somewhere the agent can read them.
//
// The agent is a separate process, so bytes in memory are no use to it. They
// land in a per-turn directory that is removed when the turn ends, rather than
// accumulating photographs of someone's life in a temp directory.
func stageAttachments(ctx context.Context, dl func(context.Context, string) ([]byte, error), atts []attachment) ([]namedTempFile, error) {
	if len(atts) == 0 {
		return nil, nil
	}
	dir, err := os.MkdirTemp("", "switchboard-att-")
	if err != nil {
		return nil, err
	}
	var out []namedTempFile
	for i, a := range atts {
		data, err := dl(ctx, a.ID)
		if err != nil {
			cleanupTempFiles(out)
			return nil, fmt.Errorf("could not fetch %s: %w", displayName(a), err)
		}
		// Numbered, because two photos off a phone are routinely both
		// "image.jpeg" and the second would overwrite the first.
		path := filepath.Join(dir, fmt.Sprintf("%02d-%s", i+1, safeBase(displayName(a))))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			cleanupTempFiles(out)
			return nil, err
		}
		out = append(out, namedTempFile{Name: displayName(a), Path: path, dir: dir})
	}
	return out, nil
}

// safeBase is a name that can only ever be a file inside the staging
// directory: "." or ".." (or a bare separator) would name the directory
// itself, or its parent.
func safeBase(name string) string {
	b := filepath.Base(name)
	if b == "." || b == ".." || b == string(filepath.Separator) {
		return "attachment"
	}
	return b
}

func cleanupTempFiles(files []namedTempFile) {
	seen := map[string]bool{}
	for _, f := range files {
		if f.dir != "" && !seen[f.dir] {
			seen[f.dir] = true
			_ = os.RemoveAll(f.dir)
		}
	}
}

// ── small helpers ──────────────────────────────────────────────────────────

// newJobID is short on purpose: it is quoted back in a text message and typed
// into /cancel by someone one-handed.
func newJobID() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	}
	return hex.EncodeToString(b[:])
}

// lastLines keeps the last n lines of a failed turn's stderr that say
// something. Blank lines and the harness's progress spinner ("Working...")
// are dropped: they are noise on the way to the one line that explains the
// failure, and they were ending up in the text sent back.
func lastLines(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || isProgressLine(l) {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

// isProgressLine recognizes a harness's status spinner, which omp writes to
// stderr as "Working..." while a turn runs.
func isProgressLine(l string) bool {
	return strings.HasSuffix(l, "...") && !strings.Contains(strings.TrimSuffix(l, "..."), " ") && len(l) <= 20
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 300)
}

func envDuration(name string, fallback time.Duration) time.Duration {
	if v := store.Env(name, ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		slog.Warn("bad duration, using default", "name", name, "value", v, "default", fallback)
	}
	return fallback
}

func envInt(name string, fallback int) int {
	if v := store.Env(name, ""); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
		slog.Warn("bad integer, using default", "name", name, "value", v, "default", fallback)
	}
	return fallback
}
