package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rpcStub is a fake JSON-RPC endpoint. It answers a batch by dispatching each
// sub-request to handle, and can shuffle the response order to prove results
// are matched by id rather than position.
type rpcStub struct {
	handle  func(method string, params []any) (any, *rpcError)
	shuffle bool
	delay   time.Duration
	calls   atomic.Int64
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *rpcStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		if s.delay > 0 {
			time.Sleep(s.delay)
		}
		var batch []struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			http.Error(w, "bad batch", http.StatusBadRequest)
			return
		}
		type out struct {
			JSONRPC string    `json:"jsonrpc"`
			ID      int       `json:"id"`
			Result  any       `json:"result,omitempty"`
			Error   *rpcError `json:"error,omitempty"`
		}
		responses := make([]out, 0, len(batch))
		for _, req := range batch {
			res, rerr := s.handle(req.Method, req.Params)
			responses = append(responses, out{JSONRPC: "2.0", ID: req.ID, Result: res, Error: rerr})
		}
		if s.shuffle {
			for i, j := 0, len(responses)-1; i < j; i, j = i+1, j-1 {
				responses[i], responses[j] = responses[j], responses[i]
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(responses)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fixtureHex returns a captured contract return as its 0x string.
func fixtureHex(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.TrimSpace(string(raw))
}

// epochsHexFor encodes a uint256[12] the way the contract would.
func epochsHexFor(block uint64) string {
	var b strings.Builder
	b.WriteString("0x")
	for _, v := range Compute(block) {
		fmt.Fprintf(&b, "%064x", v)
	}
	return b.String()
}

func TestHeadBatchesInOneRoundTrip(t *testing.T) {
	const block = 25648214
	stub := &rpcStub{handle: func(method string, params []any) (any, *rpcError) {
		switch method {
		case "eth_blockNumber":
			return fmt.Sprintf("0x%x", block), nil
		case "eth_call":
			data := params[0].(map[string]any)["data"].(string)
			switch {
			case strings.HasSuffix(data, selCurrentEpochs):
				return epochsHexFor(block), nil
			case strings.HasSuffix(data, selGetEpochLabels):
				return fixtureHex(t, "testdata/getEpochLabels.hex"), nil
			}
		}
		return nil, &rpcError{Code: -32601, Message: "unexpected " + method}
	}}
	srv := stub.server(t)
	client := NewClient([]string{srv.URL})

	head, err := client.Head(context.Background(), true)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head.Block != block {
		t.Errorf("block = %d, want %d", head.Block, block)
	}
	if head.Epochs != Compute(block) {
		t.Errorf("epochs = %v, want %v", head.Epochs, Compute(block))
	}
	if !head.HasLabels || head.Labels != DefaultLabels {
		t.Errorf("labels = %v (has=%v)", head.Labels, head.HasLabels)
	}
	// The whole point of batching: height, epochs and names in one request.
	if n := stub.calls.Load(); n != 1 {
		t.Errorf("made %d HTTP requests, want 1", n)
	}
}

// TestHeadMatchesResultsByID proves the decoder does not rely on the server
// echoing the batch back in request order, which the spec does not require.
func TestHeadMatchesResultsByID(t *testing.T) {
	const block = 19487171
	stub := &rpcStub{shuffle: true, handle: func(method string, params []any) (any, *rpcError) {
		if method == "eth_blockNumber" {
			return fmt.Sprintf("0x%x", block), nil
		}
		return epochsHexFor(block), nil
	}}
	client := NewClient([]string{stub.server(t).URL})

	head, err := client.Head(context.Background(), false)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head.Block != block {
		t.Errorf("block = %d, want %d — results matched by position, not id", head.Block, block)
	}
	if head.Epochs != Compute(block) {
		t.Errorf("epochs = %v, want %v", head.Epochs, Compute(block))
	}
}

// TestHeadKeepsBlockAndEpochsConsistent covers a provider answering the two
// sub-requests one block apart. The page must never show a height whose epochs
// belong to a different height.
func TestHeadKeepsBlockAndEpochsConsistent(t *testing.T) {
	const reported = 25648214
	stub := &rpcStub{handle: func(method string, params []any) (any, *rpcError) {
		if method == "eth_blockNumber" {
			return fmt.Sprintf("0x%x", reported), nil
		}
		// Epochs for the *next* block — a boundary crossed mid-batch.
		return epochsHexFor(reported + 1), nil
	}}
	client := NewClient([]string{stub.server(t).URL})

	head, err := client.Head(context.Background(), false)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head.Block != reported {
		t.Fatalf("block = %d, want %d", head.Block, reported)
	}
	if want := Compute(reported); head.Epochs != want {
		t.Errorf("epochs = %v, want %v (must match the displayed block)", head.Epochs, want)
	}
}

// TestHeadFailsOverAndRejectsPartialBatches checks that a dead endpoint, an
// endpoint erroring one sub-request, and one dropping a sub-request all move
// on to the next endpoint rather than yielding a half-populated reading.
func TestHeadFailsOverAndRejectsPartialBatches(t *testing.T) {
	const block = 15537394

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer broken.Close()

	erroring := (&rpcStub{handle: func(method string, params []any) (any, *rpcError) {
		if method == "eth_blockNumber" {
			return fmt.Sprintf("0x%x", block), nil
		}
		return nil, &rpcError{Code: -32000, Message: "execution reverted"}
	}}).server(t)

	// Answers only the first sub-request; the second comes back null.
	partial := (&rpcStub{handle: func(method string, params []any) (any, *rpcError) {
		if method == "eth_blockNumber" {
			return fmt.Sprintf("0x%x", block), nil
		}
		return nil, nil
	}}).server(t)

	good := (&rpcStub{handle: func(method string, params []any) (any, *rpcError) {
		if method == "eth_blockNumber" {
			return fmt.Sprintf("0x%x", block), nil
		}
		return epochsHexFor(block), nil
	}}).server(t)

	client := NewClient([]string{broken.URL, erroring.URL, partial.URL, good.URL})
	head, err := client.Head(context.Background(), false)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head.Block != block || head.Epochs != Compute(block) {
		t.Errorf("got block %d epochs %v", head.Block, head.Epochs)
	}

	// With every endpoint bad, the error must surface rather than a zero value.
	failing := NewClient([]string{broken.URL, erroring.URL, partial.URL})
	if _, err := failing.Head(context.Background(), false); err == nil {
		t.Error("Head succeeded with no working endpoint")
	}
}

// TestRefreshFetchesLabelsOnceThenStops guards the polling cost: the epoch
// names must not be re-requested on every slot forever.
func TestRefreshFetchesLabelsOnce(t *testing.T) {
	var labelReads atomic.Int64
	const block = 25648214
	stub := &rpcStub{handle: func(method string, params []any) (any, *rpcError) {
		if method == "eth_blockNumber" {
			return fmt.Sprintf("0x%x", block), nil
		}
		data := params[0].(map[string]any)["data"].(string)
		if strings.HasSuffix(data, selGetEpochLabels) {
			labelReads.Add(1)
			return fixtureHex(t, "testdata/getEpochLabels.hex"), nil
		}
		return epochsHexFor(block), nil
	}}
	state := NewState(NewClient([]string{stub.server(t).URL}), nil)

	for i := 0; i < 5; i++ {
		if err := state.Refresh(context.Background()); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	if n := labelReads.Load(); n != 1 {
		t.Errorf("read epoch labels %d times over 5 polls, want 1", n)
	}
	r, ok := state.Current()
	if !ok || r.Labels != DefaultLabels {
		t.Errorf("labels not retained across polls: %v", r.Labels)
	}
}

// TestWaitReleasesOnFirstReading covers the cold-start path: a request that
// arrives before the first poll lands should get real numbers, not "Loading".
func TestWaitReleasesOnFirstReading(t *testing.T) {
	const block = 25648214
	stub := &rpcStub{delay: 150 * time.Millisecond,
		handle: func(method string, params []any) (any, *rpcError) {
			if method == "eth_blockNumber" {
				return fmt.Sprintf("0x%x", block), nil
			}
			return epochsHexFor(block), nil
		}}
	state := NewState(NewClient([]string{stub.server(t).URL}), nil)

	if _, ok := state.Current(); ok {
		t.Fatal("a fresh state should have no reading")
	}
	go func() { _ = state.Refresh(context.Background()) }()

	start := time.Now()
	reading, ok := state.Wait(context.Background(), 3*time.Second)
	if !ok {
		t.Fatal("Wait timed out before the first reading")
	}
	if reading.Block != block {
		t.Errorf("block = %d, want %d", reading.Block, block)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Wait took %v; it should release as soon as the poll lands", elapsed)
	}

	// Once warm, Wait must not block at all.
	start = time.Now()
	if _, ok := state.Wait(context.Background(), 3*time.Second); !ok {
		t.Fatal("Wait failed on a warm state")
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("warm Wait blocked for %v", elapsed)
	}
}

// TestWaitGivesUpWhenChainIsUnreachable keeps the loading fallback: the page
// must not hang when there is nothing to show.
func TestWaitGivesUpWhenChainIsUnreachable(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer dead.Close()

	state := NewState(NewClient([]string{dead.URL}), nil)
	start := time.Now()
	if _, ok := state.Wait(context.Background(), 200*time.Millisecond); ok {
		t.Error("Wait reported a reading with no working endpoint")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Wait blocked for %v past its budget", elapsed)
	}
}

// TestFailoverIsStickyAcrossPolls covers the cost of an unreachable first
// choice. Without stickiness the client would retry the dead endpoint — and
// pay its timeout — on every single poll, forever.
func TestFailoverIsStickyAcrossPolls(t *testing.T) {
	const block = 25648214
	var deadHits atomic.Int64
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadHits.Add(1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer dead.Close()

	stub := &rpcStub{handle: func(method string, params []any) (any, *rpcError) {
		if method == "eth_blockNumber" {
			return fmt.Sprintf("0x%x", block), nil
		}
		return epochsHexFor(block), nil
	}}
	good := stub.server(t)

	client := NewClient([]string{dead.URL, good.URL})
	for i := 0; i < 6; i++ {
		if _, err := client.Head(context.Background(), false); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}

	// Only the first poll should have touched the dead endpoint.
	if n := deadHits.Load(); n != 1 {
		t.Errorf("hit the dead endpoint %d times over 6 polls, want 1", n)
	}
	if got := client.Endpoint(); got != good.URL {
		t.Errorf("Endpoint() = %q, want the working endpoint %q", got, good.URL)
	}
	if n := stub.calls.Load(); n != 6 {
		t.Errorf("working endpoint served %d of 6 polls", n)
	}
}
