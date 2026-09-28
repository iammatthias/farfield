package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const me = "+15551234567"

// /undo after an append restores the post it changed; it never deletes it.
// A second /undo then reaches the post itself.
func TestUndoAfterAppendRestoresThePost(t *testing.T) {
	_, srv, feed := newTestServer(t)
	post(t, srv, "m1", me, "/feed first line")
	post(t, srv, "m2", me, "/+ second line")
	if body, _ := feed.state("post1"); !strings.Contains(body, "second line") {
		t.Fatalf("append did not land: %q", body)
	}

	post(t, srv, "m3", me, "/undo")
	body, ok := feed.state("post1")
	if !ok {
		t.Fatal("/undo after /append deleted the whole post")
	}
	if body != "first line" {
		t.Errorf("restored body = %q, want %q", body, "first line")
	}

	post(t, srv, "m4", me, "/undo")
	if _, ok := feed.state("post1"); ok {
		t.Error("second /undo did not reach the post itself")
	}
}

// /undo walks back through actions instead of retrying the same one, and runs
// out cleanly.
func TestUndoWalksBack(t *testing.T) {
	s, srv, feed := newTestServer(t)
	post(t, srv, "m1", me, "/feed one")
	post(t, srv, "m2", me, "/feed two")

	post(t, srv, "m3", me, "/undo")
	post(t, srv, "m4", me, "/undo")
	_, one := feed.state("post1")
	_, two := feed.state("post2")
	if one || two {
		t.Errorf("after two /undo: post1 live=%v post2 live=%v, want both gone", one, two)
	}
	if got := recorded(t, s, "m4"); got.Status != statusOK {
		t.Errorf("second /undo = %s (%s), want ok", got.Status, got.Reply)
	}

	post(t, srv, "m5", me, "/undo")
	if got := recorded(t, s, "m5"); !strings.Contains(got.Reply, "nothing to undo") {
		t.Errorf("third /undo reply = %q, want nothing to undo", got.Reply)
	}
}

// Once a post is undone, /append and /tags no longer aim at it.
func TestAppendSkipsAnUndonePost(t *testing.T) {
	s, srv, _ := newTestServer(t)
	post(t, srv, "m1", me, "/feed gone soon")
	post(t, srv, "m2", me, "/undo")
	post(t, srv, "m3", me, "/+ into the void")
	if got := recorded(t, s, "m3"); !strings.Contains(got.Reply, "no recent post") {
		t.Errorf("append after undo reply = %q, want no recent post", got.Reply)
	}
}

// A redelivery that arrives while the first delivery is still dispatching
// must not run the command again.
func TestRedeliveryDuringSlowCommandPostsOnce(t *testing.T) {
	_, srv, feed := newTestServer(t)
	feed.delay = 400 * time.Millisecond

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post(t, srv, "same-id", me, "/feed posted once")
		}()
	}
	wg.Wait()
	if n := feed.count(); n != 1 {
		t.Errorf("feed creates = %d, want 1", n)
	}
}

// A claim left behind by a crash expires, so the message is not silenced
// forever.
func TestStaleClaimCanBeTakenOver(t *testing.T) {
	s, _, _ := newTestServer(t)
	old := time.Now().Add(-2 * claimStale).UTC().Format(time.RFC3339)
	if ok, err := claimMessage(s.db, &Message{ID: "x", Sender: me, ReceivedAt: old}); err != nil || !ok {
		t.Fatalf("first claim = %v, %v", ok, err)
	}
	if ok, _ := claimMessage(s.db, &Message{ID: "x", Sender: me}); !ok {
		t.Error("a stale claim could not be taken over")
	}
	if ok, _ := claimMessage(s.db, &Message{ID: "x", Sender: me}); ok {
		t.Error("a fresh claim was taken over")
	}
}

// /cancel reaches a job still waiting in the queue; it never runs.
func TestCancelStopsAQueuedJob(t *testing.T) {
	s, srv, _ := newTestServer(t)
	post(t, srv, "m1", me, "please hang for a while")
	first := recorded(t, s, "m1").Ref
	// Same chat, so it queues behind the first.
	post(t, srv, "m2", me, "and then answer this")
	second := recorded(t, s, "m2").Ref

	time.Sleep(200 * time.Millisecond)
	if j, _ := getJob(s.db, second); j.Status != jobRunning || j.Result != "" {
		t.Fatalf("second job ran while the first held the chat: %+v", j)
	}

	post(t, srv, "m3", me, "/cancel "+second)
	if j := waitForJob(t, s, second); j.Status != jobCancelled || j.Result != "" {
		t.Errorf("queued job = %s result=%q, want cancelled and never run", j.Status, j.Result)
	}
	post(t, srv, "m4", me, "/cancel "+first)
	waitForJob(t, s, first)
}

// Turns in different chats still run side by side.
func TestDifferentChatsRunConcurrently(t *testing.T) {
	s, _, _ := newTestServer(t)
	a, _ := s.agent.acquire(context.Background(), "chat-a")
	defer a()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	b, err := s.agent.acquire(ctx, "chat-b")
	if err != nil {
		t.Fatalf("a second chat was blocked by the first: %v", err)
	}
	b()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if _, err := s.agent.acquire(ctx2, "chat-a"); err == nil {
		t.Error("two turns in one chat ran at once")
	}
}

// Photos with the same name, or a hostile one, each get their own file.
func TestStageAttachmentsKeepsEveryPhoto(t *testing.T) {
	atts := []attachment{
		{ID: "a", Name: "image.jpeg"},
		{ID: "b", Name: "image.jpeg"},
		{ID: "c", Name: ".."},
	}
	dl := func(_ context.Context, id string) ([]byte, error) { return []byte("bytes-" + id), nil }
	files, err := stageAttachments(context.Background(), dl, atts)
	if err != nil {
		t.Fatalf("stageAttachments: %v", err)
	}
	defer cleanupTempFiles(files)
	seen := map[string]bool{}
	for i, f := range files {
		if seen[f.Path] {
			t.Errorf("path %s used twice", f.Path)
		}
		seen[f.Path] = true
		got, err := os.ReadFile(f.Path)
		if err != nil || string(got) != "bytes-"+atts[i].ID {
			t.Errorf("file %d = %q, %v", i, got, err)
		}
		if filepath.Dir(f.Path) != f.dir {
			t.Errorf("file %d escaped the staging dir: %s", i, f.Path)
		}
	}
}

// Cutting text never splits a character.
func TestTruncateKeepsUTF8Valid(t *testing.T) {
	s := strings.Repeat("a", 39) + "🙂 and more"
	got := truncate(s, 40)
	if !utf8.ValidString(got) {
		t.Errorf("truncate produced invalid UTF-8: %q", got)
	}
	long := strings.Repeat("é", 400)
	if f := firstLine([]byte(long)); !utf8.ValidString(f) {
		t.Error("firstLine produced invalid UTF-8")
	}
}

// A failure report carries the error, not the harness's spinner.
func TestLastLinesDropsTheSpinner(t *testing.T) {
	stderr := "Working...\n404 no endpoints available\nModel blocked by guardrail\n"
	got := lastLines(stderr, 3)
	if strings.Contains(got, "Working") {
		t.Errorf("lastLines = %q, spinner leaked", got)
	}
	if !strings.Contains(got, "404") || !strings.Contains(got, "guardrail") {
		t.Errorf("lastLines = %q, lost the real error", got)
	}
}

// A message claimed by a run that died is acted on when Photon redelivers it,
// not swallowed as a duplicate.
func TestRedeliveryAfterACrashIsActedOn(t *testing.T) {
	s, srv, feed := newTestServer(t)
	old := time.Now().Add(-2 * claimStale).UTC().Format(time.RFC3339)
	if _, err := claimMessage(s.db, &Message{ID: "crashed", Sender: me, ReceivedAt: old}); err != nil {
		t.Fatal(err)
	}
	post(t, srv, "crashed", me, "/feed survived the crash")
	if feed.count() != 1 {
		t.Errorf("stale claim swallowed the redelivery: feed creates = %d", feed.count())
	}

	// And at startup, every claim is released outright.
	if _, err := claimMessage(s.db, &Message{ID: "fresh", Sender: me}); err != nil {
		t.Fatal(err)
	}
	if n, err := releaseOrphanedClaims(s.db); err != nil || n != 1 {
		t.Fatalf("releaseOrphanedClaims = %d, %v", n, err)
	}
	post(t, srv, "fresh", me, "/feed after restart")
	if feed.count() != 2 {
		t.Errorf("released claim not acted on: feed creates = %d", feed.count())
	}
}

// A line break after the command name is still the command.
func TestCommandSurvivesALineBreak(t *testing.T) {
	s, srv, feed := newTestServer(t)
	post(t, srv, "m1", me, "/feed\nfirst line\nsecond line")
	if got := recorded(t, s, "m1"); got.Route != "feed" || got.Status != statusOK {
		t.Fatalf("route=%q status=%q reply=%q", got.Route, got.Status, got.Reply)
	}
	if body, _ := feed.state("post1"); body != "first line\nsecond line" {
		t.Errorf("body = %q, want both lines intact", body)
	}
}

// An agent turn never inherits switchboard's secrets.
func TestAgentEnvIsAnAllowlist(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin", "HOME=/home/iam", "LANG=en_US.UTF-8", "LC_ALL=C",
		"FF_AGENT_MODEL=openrouter/x", "FARFIELD_FEED_KEY=ffk_scoped", "FEED_URL=http://feed:8788",
		"FEED_API_KEY=master", "PASSWORD=hunter2", "SESSION_SECRET=s",
		"SWITCHBOARD_WEBHOOK_SECRET=w", "SPECTRUM_PROJECT_SECRET=p",
		"CF_DEPLOY_HOOK_URL=https://api.cloudflare.com/hook/token", "OPENROUTER_API_KEY=sk-or",
		"SOMETHING_SECRET_URL=https://x",
	}
	got := strings.Join(agentEnv(parent), "\n")
	for _, keep := range []string{"PATH=", "HOME=", "LANG=", "LC_ALL=", "FF_AGENT_MODEL=", "FARFIELD_FEED_KEY=", "FEED_URL="} {
		if !strings.Contains(got, keep) {
			t.Errorf("dropped %s", keep)
		}
	}
	for _, leak := range []string{"FEED_API_KEY", "PASSWORD", "SESSION_SECRET", "WEBHOOK_SECRET",
		"SPECTRUM_PROJECT_SECRET", "CF_DEPLOY_HOOK_URL", "OPENROUTER_API_KEY", "SOMETHING_SECRET_URL"} {
		if strings.Contains(got, leak) {
			t.Errorf("leaked %s into the agent's environment", leak)
		}
	}
}

// End to end: the stub agent sees the allowlisted environment only.
func TestAgentTurnDoesNotSeeSecrets(t *testing.T) {
	t.Setenv("PASSWORD", "hunter2")
	t.Setenv("FARFIELD_FEED_KEY", "ffk_scoped")
	s, srv, _ := newTestServer(t)
	stub := filepath.Join(t.TempDir(), "env-agent")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nenv\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.agent.cmd = stub
	post(t, srv, "m1", me, "what is in your environment")
	job := waitForJob(t, s, recorded(t, s, "m1").Ref)
	if strings.Contains(job.Result, "hunter2") {
		t.Error("agent turn saw PASSWORD")
	}
	if !strings.Contains(job.Result, "FARFIELD_FEED_KEY=ffk_scoped") {
		t.Error("agent turn lost its scoped key")
	}
}
