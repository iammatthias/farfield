package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Contract is the canonical Epochs deployment on Ethereum mainnet, controlled
// by AspectsDAO. Its interface is tiny and entirely read-only for our purposes:
//
//	currentEpochs()            view  -> uint256[12]
//	getEpochs(uint256)         pure  -> uint256[12]
//	getEpochLabels()           view  -> string[12]
//	owner()                    view  -> address
//
// The address the rebuild was originally pointed at — 0xc522…1606 — is the
// deployer wallet, not the contract: its nonce-0 CREATE produced this address.
const Contract = "0xde9f0c369Ef3692B4bF9D40803A9029a3722B9c4"

// Function selectors, the first four bytes of keccak256 over each signature.
const (
	selGetEpochs      = "f665a206" // getEpochs(uint256)
	selCurrentEpochs  = "728b15b6" // currentEpochs()   — reads block.number itself
	selGetEpochLabels = "32e394e0" // getEpochLabels()
)

// DefaultRPCs are public mainnet JSON-RPC endpoints, tried in order. Set
// EPOCHS_RPC_URL (comma-separated) to point at your own provider — public
// endpoints rate-limit and come and go, and this app makes one poll per block.
var DefaultRPCs = []string{
	"https://ethereum-rpc.publicnode.com",
	"https://eth.rpc.blxrbdn.com",
	"https://1rpc.io/eth",
	"https://eth.drpc.org",
}

// attemptTimeout bounds a single endpoint attempt. It is deliberately shorter
// than the whole-refresh budget: with failover, a hung endpoint would
// otherwise spend the entire budget before the next one is tried.
const attemptTimeout = 4 * time.Second

// endpointRecheck is how long the client sticks with a working endpoint before
// trying the configured order from the top again. Without stickiness an
// unreachable first choice would burn a failed attempt on every single poll;
// without the recheck it would never notice that choice coming back.
const endpointRecheck = 10 * time.Minute

// Client is a minimal Ethereum JSON-RPC client. It speaks only the two methods
// this app needs and encodes calldata by hand, which keeps the app free of a
// web3 dependency for what amounts to one selector and a fixed-size array.
type Client struct {
	urls []string
	http *http.Client

	// mu guards the sticky endpoint choice.
	mu       sync.Mutex
	current  string    // endpoint that last answered, tried first
	pinnedAt time.Time // when it was chosen, for endpointRecheck
}

// NewClient returns a client that fails over across urls. An empty list falls
// back to DefaultRPCs.
func NewClient(urls []string) *Client {
	if len(urls) == 0 {
		urls = DefaultRPCs
	}
	// Keep-alives matter here: the poller talks to the same endpoint every
	// slot, and a fresh TLS handshake per poll would cost more than the call.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 2
	transport.IdleConnTimeout = 90 * time.Second
	return &Client{
		urls: urls,
		http: &http.Client{Timeout: attemptTimeout, Transport: transport},
	}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Head is one consistent look at the chain: the current height and the epoch
// values that go with it, plus the epoch names when they were asked for.
type Head struct {
	Block     uint64
	Epochs    [Count]uint64
	Labels    [Count]string
	HasLabels bool
}

// Head fetches the current block and its epochs in a single JSON-RPC batch,
// optionally including the epoch names. Batching matters: the naive form is
// three sequential round trips (height, then epochs at that height, then
// labels), which on a public endpoint is most of a second of latency for data
// that could have arrived together.
//
// currentEpochs() is used rather than getEpochs(n) precisely because it reads
// block.number on-chain, so it needs no argument and can ride along in the
// same batch as eth_blockNumber instead of waiting for its answer.
func (c *Client) Head(ctx context.Context, withLabels bool) (Head, error) {
	var out Head

	items := []batchItem{
		{Method: "eth_blockNumber"},
		{Method: "eth_call", Params: []any{
			map[string]string{"to": Contract, "data": "0x" + selCurrentEpochs}, "latest"}},
	}
	if withLabels {
		items = append(items, batchItem{Method: "eth_call", Params: []any{
			map[string]string{"to": Contract, "data": "0x" + selGetEpochLabels}, "latest"}})
	}

	results, err := c.callBatch(ctx, items)
	if err != nil {
		return out, err
	}

	var blockHex string
	if err := json.Unmarshal(results[0], &blockHex); err != nil {
		return out, fmt.Errorf("eth_blockNumber: %w", err)
	}
	if out.Block, err = parseHexUint(blockHex); err != nil {
		return out, fmt.Errorf("eth_blockNumber: %w", err)
	}

	epochs, err := decodeHexReturn(results[1], decodeEpochs)
	if err != nil {
		return out, fmt.Errorf("currentEpochs: %w", err)
	}
	// eth_blockNumber and eth_call are separate sub-requests, so a provider
	// that fans them across backends — or a block landing mid-batch — can
	// answer them one height apart. The local computation is provably the
	// same function the contract runs (a test pins them together), so when
	// they disagree, recomputing from the height we are about to display
	// keeps the page self-consistent instead of showing a block whose epochs
	// belong to its neighbour.
	//
	// This is logged rather than done silently: the page's whole claim is
	// that its numbers come from the contract, so the one case where they
	// are substituted should be visible instead of having to be inferred.
	if local := Compute(out.Block); local != epochs {
		slog.Info("epochs recomputed for block consistency",
			"block", out.Block, "from_contract", epochs, "displayed", local)
		epochs = local
	}
	out.Epochs = epochs

	if withLabels {
		labels, err := decodeHexReturn(results[2], decodeLabels)
		if err != nil {
			// Names change approximately never; a failure here is not worth
			// discarding a good height and epoch reading over.
			return out, nil
		}
		out.Labels, out.HasLabels = labels, true
	}
	return out, nil
}

// decodeHexReturn unwraps a hex-string eth_call result and runs a decoder.
func decodeHexReturn[T any](raw json.RawMessage, decode func([]byte) (T, error)) (T, error) {
	var zero T
	var hexStr string
	if err := json.Unmarshal(raw, &hexStr); err != nil {
		return zero, err
	}
	b, err := hex.DecodeString(strings.TrimPrefix(hexStr, "0x"))
	if err != nil {
		return zero, err
	}
	return decode(b)
}

// batchItem is one sub-request of a JSON-RPC batch.
type batchItem struct {
	Method string
	Params []any
}

// callBatch sends items as a single JSON-RPC batch and returns their results
// in the order given. Endpoints are tried in turn until one answers with a
// complete, error-free batch.
//
// The spec does not require a server to preserve request order in the
// response array, so results are matched by id rather than position.
func (c *Client) callBatch(ctx context.Context, items []batchItem) ([]json.RawMessage, error) {
	batch := make([]rpcRequest, len(items))
	for i, it := range items {
		params := it.Params
		if params == nil {
			params = []any{}
		}
		batch[i] = rpcRequest{JSONRPC: "2.0", ID: i + 1, Method: it.Method, Params: params}
	}
	body, err := json.Marshal(batch)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for _, url := range c.order() {
		raw, err := c.post(ctx, url, body)
		if err != nil {
			lastErr = err
			continue
		}
		var responses []rpcResponse
		if err := json.Unmarshal(raw, &responses); err != nil {
			lastErr = fmt.Errorf("%s: %w", url, err)
			continue
		}
		out := make([]json.RawMessage, len(items))
		var bad error
		for _, resp := range responses {
			idx := resp.ID - 1
			if idx < 0 || idx >= len(items) {
				continue
			}
			if resp.Error != nil {
				bad = fmt.Errorf("%s: %s: rpc %d: %s",
					url, items[idx].Method, resp.Error.Code, resp.Error.Message)
				break
			}
			out[idx] = resp.Result
		}
		if bad != nil {
			lastErr = bad
			continue
		}
		// A provider that silently drops a sub-request must not look like a
		// success with a zero value.
		complete := true
		for i, r := range out {
			if len(r) == 0 || string(r) == "null" {
				lastErr = fmt.Errorf("%s: no result for %s", url, items[i].Method)
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		c.pin(url, lastErr)
		return out, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no rpc endpoints configured")
	}
	return nil, lastErr
}

// order returns the endpoints to try, preferring the one that last answered.
// The preference expires after endpointRecheck so a configured first choice
// that was down gets another look.
func (c *Client) order() []string {
	c.mu.Lock()
	current := c.current
	if current != "" && time.Since(c.pinnedAt) > endpointRecheck {
		current, c.current = "", ""
	}
	c.mu.Unlock()

	if current == "" || current == c.urls[0] {
		return c.urls
	}
	out := make([]string, 0, len(c.urls))
	out = append(out, current)
	for _, u := range c.urls {
		if u != current {
			out = append(out, u)
		}
	}
	return out
}

// pin records the endpoint that answered. A change is logged once, so an
// operator can see which provider is actually serving the page — and, when
// the configured first choice is skipped, why.
func (c *Client) pin(url string, failure error) {
	c.mu.Lock()
	changed := c.current != url
	c.current, c.pinnedAt = url, time.Now()
	c.mu.Unlock()

	if !changed {
		return
	}
	if failure != nil {
		slog.Info("rpc endpoint selected", "endpoint", url, "after_error", failure.Error())
		return
	}
	slog.Info("rpc endpoint selected", "endpoint", url)
}

// Endpoint reports the endpoint currently in use, empty before the first
// successful call. It is surfaced on /status.
func (c *Client) Endpoint() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

// post sends one request body to url and returns the response bytes.
func (c *Client) post(ctx context.Context, url string, body []byte) ([]byte, error) {
	// Bound each attempt so failover is not held up by one hung endpoint.
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// The fleet sits behind an edge that refuses bot-shaped agents; being
	// explicit here also keeps public RPC providers from silently dropping us.
	req.Header.Set("User-Agent", "farfield-epochs/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	defer resp.Body.Close()
	// Cap the read: a misbehaving endpoint should not be able to hand us an
	// unbounded body.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: http %d", url, resp.StatusCode)
	}
	return raw, nil
}

// EpochsAt calls getEpochs(blockNumber) on-chain for an arbitrary height. The
// live page does not use this — Head covers the current block in one batched
// round trip — but it is the direct expression of the contract's interface and
// the path a test uses to check the local computation against the deployment.
func (c *Client) EpochsAt(ctx context.Context, block uint64) ([Count]uint64, error) {
	var zero [Count]uint64
	// selector + one uint256 argument, left-padded to 32 bytes.
	data := selGetEpochs + fmt.Sprintf("%064x", block)
	results, err := c.callBatch(ctx, []batchItem{{Method: "eth_call", Params: []any{
		map[string]string{"to": Contract, "data": "0x" + data}, "latest"}}})
	if err != nil {
		return zero, err
	}
	return decodeHexReturn(results[0], decodeEpochs)
}

// decodeEpochs reads a uint256[12] return: a static type, so twelve 32-byte
// words with no offset header.
func decodeEpochs(ret []byte) ([Count]uint64, error) {
	var out [Count]uint64
	if len(ret) < Count*32 {
		return out, fmt.Errorf("getEpochs: short return (%d bytes)", len(ret))
	}
	for i := 0; i < Count; i++ {
		word := ret[i*32 : (i+1)*32]
		// Values are 1..11; anything wider than 8 bytes means we misread the
		// layout, so refuse it rather than silently truncating.
		for _, b := range word[:24] {
			if b != 0 {
				return out, fmt.Errorf("getEpochs: word %d overflows uint64", i)
			}
		}
		out[i] = beUint64(word[24:])
	}
	return out, nil
}

// decodeLabels reads a string[12] return.
func decodeLabels(ret []byte) ([Count]string, error) {
	var out [Count]string
	// string[12] is a fixed-size array of a dynamic type, so the return is
	// wrapped: one offset to the array, then twelve offsets (relative to the
	// array's own start), then each string as length + bytes.
	if len(ret) < 32 {
		return out, errors.New("getEpochLabels: short return")
	}
	base, err := readOffset(ret, 0, len(ret))
	if err != nil {
		return out, fmt.Errorf("getEpochLabels: array offset: %w", err)
	}
	arr := ret[base:]
	if len(arr) < Count*32 {
		return out, errors.New("getEpochLabels: short offset table")
	}
	for i := 0; i < Count; i++ {
		off, err := readOffset(arr, i*32, len(arr))
		if err != nil {
			return out, fmt.Errorf("getEpochLabels: element %d: %w", i, err)
		}
		if len(arr) < off+32 {
			return out, fmt.Errorf("getEpochLabels: element %d truncated", i)
		}
		n, err := readOffset(arr, off, len(arr))
		if err != nil {
			return out, fmt.Errorf("getEpochLabels: element %d length: %w", i, err)
		}
		start := off + 32
		if start+n > len(arr) {
			return out, fmt.Errorf("getEpochLabels: element %d overruns return data", i)
		}
		out[i] = string(arr[start : start+n])
	}
	return out, nil
}

// readOffset reads the 32-byte word at pos as a length or offset, rejecting
// anything that could not address the buffer. Bounds are enforced here so the
// decoders above cannot be walked off the end by a hostile endpoint.
func readOffset(buf []byte, pos, limit int) (int, error) {
	if pos < 0 || pos+32 > len(buf) {
		return 0, errors.New("out of range")
	}
	word := buf[pos : pos+32]
	for _, b := range word[:24] {
		if b != 0 {
			return 0, errors.New("value too large")
		}
	}
	v := beUint64(word[24:])
	if v > uint64(limit) {
		return 0, errors.New("value exceeds buffer")
	}
	return int(v), nil
}

// beUint64 reads up to 8 big-endian bytes.
func beUint64(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// parseHexUint parses a 0x-prefixed quantity.
func parseHexUint(s string) (uint64, error) {
	return strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
}
