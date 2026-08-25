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
}

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
	// Queue rather than reject: a second message while one is running is
	// normal, and being told "too busy" by your own house is absurd. The
	// ceiling is on concurrency, not on patience.
	select {
	case a.sem <- struct{}{}:
	case <-time.After(a.timeout):
		a.finish(job, jobFailed, "", "timed out waiting for a free slot")
		return
	}
	defer func() { <-a.sem }()

	ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
	defer cancel()
	a.mu.Lock()
	a.live[job.ID] = cancel
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.live, job.ID)
		a.mu.Unlock()
	}()

	// Silence past ackAfter is rude; silence before it is just being quick.
	// This is the only message that is ever sent before the answer, and it is
	// sent at most once — the budget for a whole turn is two.
	acked := make(chan struct{})
	go func() {
		select {
		case <-time.After(a.ackAfter):
			a.say(job.ChatGUID, "on it · "+job.ID)
		case <-acked:
		}
	}()

	// Photos are fetched here rather than in the webhook: the bytes can be
	// several megabytes off a phone, and Photon is waiting on the other end of
	// that handler.
	files, err := stageAttachments(ctx, a.photon.Download, atts)
	defer cleanupTempFiles(files)

	var out string
	if err == nil {
		out, err = a.exec(ctx, job, files)
	}
	close(acked)

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

// exec runs the agent and returns its reply.
//
// stdout is the reply and nothing else; stderr is progress and diagnostics and
// is captured only to explain a failure. That split is why nothing streams into
// the thread: there is no intermediate output to leak.
func (a *agentRunner) exec(ctx context.Context, job *Job, files []namedTempFile) (string, error) {
	args := []string{"--prompt", job.Prompt, "--session-dir", a.sessionDir(job.ChatGUID)}
	for _, f := range files {
		args = append(args, "--file", f.Path)
	}

	cmd := exec.CommandContext(ctx, a.cmd, args...)
	cmd.Dir = a.workspace()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

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
	for _, a := range atts {
		data, err := dl(ctx, a.ID)
		if err != nil {
			cleanupTempFiles(out)
			return nil, fmt.Errorf("could not fetch %s: %w", displayName(a), err)
		}
		path := filepath.Join(dir, filepath.Base(displayName(a)))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			cleanupTempFiles(out)
			return nil, err
		}
		out = append(out, namedTempFile{Name: displayName(a), Path: path, dir: dir})
	}
	return out, nil
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

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
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
