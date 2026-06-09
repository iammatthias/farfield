const CONTRACT = '0xBA2D82930b2F74B1319Fd326bdF43b567Ac03720';
const CHAIN_ID = 11155111;
const RPC_DEFAULT = 'https://ethereum-sepolia-rpc.publicnode.com';
const SELECTORS = {
  config: '0x79502c55',
  chunkCount: '0xf91f0937',
  getChunks: '0x67207738',
  artifactHash: '0x0b312fb0',
  isSealed: '0x631f9852',
  owner: '0x8da5cb5b',
};

const decoder = new TextDecoder();
let model = null;
let abortRequested = false;

function progress(stage, detail, extra = {}) {
  postMessage({ type: 'progress', stage, detail, ...extra });
}

function strip0x(hex) {
  return hex.startsWith('0x') ? hex.slice(2) : hex;
}

function hexToBytes(hex) {
  const clean = strip0x(hex);
  const out = new Uint8Array(clean.length / 2);
  for (let i = 0; i < out.length; i++) {
    out[i] = parseInt(clean.slice(i * 2, i * 2 + 2), 16);
  }
  return out;
}

function bytesToHex(bytes) {
  let out = '';
  for (const byte of bytes) out += byte.toString(16).padStart(2, '0');
  return out;
}

function concatBytes(chunks) {
  let total = 0;
  for (const chunk of chunks) total += chunk.length;
  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.length;
  }
  return out;
}

function bigWord(bytes, offset) {
  let value = 0n;
  for (let i = 0; i < 32; i++) value = (value << 8n) | BigInt(bytes[offset + i]);
  return value;
}

function wordAt(bytes, index) {
  return bytes.subarray(index * 32, index * 32 + 32);
}

function wordNumber(bytes, index) {
  return Number(bigWord(bytes, index * 32));
}

function readAddressFromWord(word) {
  return '0x' + bytesToHex(word.subarray(12));
}

function decodeUtf8(bytes) {
  return decoder.decode(bytes);
}

function quantName(quant) {
  if (quant === 0) return 'fp32';
  if (quant === 1) return 'int8';
  if (quant === 2) return 'int4';
  return `unknown(${quant})`;
}

const RPC_TIMEOUT_MS = 15000;
let rpcId = 0;

function rpcCall(rpcUrl, method, params = []) {
  rpcId += 1;
  return fetch(rpcUrl, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ jsonrpc: '2.0', id: rpcId, method, params }),
    signal: AbortSignal.timeout(RPC_TIMEOUT_MS),
  }).then(
    async (res) => {
      if (!res.ok) {
        throw new Error(`RPC ${method} failed: ${res.status} ${res.statusText}`);
      }
      const payload = await res.json();
      if (payload.error) {
        throw new Error(payload.error.message || `RPC ${method} failed`);
      }
      return payload.result;
    },
    (err) => {
      if (err && (err.name === 'TimeoutError' || err.name === 'AbortError')) {
        throw new Error(`RPC ${method} timed out after ${RPC_TIMEOUT_MS / 1000}s — hit "Reload weights" to retry`);
      }
      throw new Error(`RPC ${method} failed: ${err && err.message ? err.message : err} — hit "Reload weights" to retry`);
    },
  );
}

function ethCall(rpcUrl, to, data) {
  return rpcCall(rpcUrl, 'eth_call', [{ to, data }, 'latest']);
}

function ethGetCode(rpcUrl, address) {
  return rpcCall(rpcUrl, 'eth_getCode', [address, 'latest']);
}

function decodeConfigResult(resultHex) {
  const bytes = hexToBytes(resultHex);

  // Live `config()` comes back as a bytes blob whose payload is laid out as:
  // quant, vocabSize, paramCount, maxChunkBytes, artifactHash, nameLength, name.
  // The outer two words are the standard dynamic-bytes wrapper.
  if (bytes.length >= 288) {
    const payload = bytes.subarray(64);
    const quant = payload[31];
    const vocabSize = Number(bigWord(payload, 32));
    const paramCount = Number(bigWord(payload, 64));
    const maxChunkBytes = Number(bigWord(payload, 96));
    const artifactHash = '0x' + bytesToHex(payload.subarray(128, 160));
    const nameLength = Number(bigWord(payload, 160));
    const name = decodeUtf8(payload.subarray(192, 192 + nameLength));
    if (nameLength > 0 && nameLength <= 64 && quant <= 2 && vocabSize > 0) {
      return { name, quant, vocabSize, paramCount, maxChunkBytes, artifactHash };
    }
  }

  // Fallback for a standard ABI tuple encoding.
  const nameOffset = Number(bigWord(bytes, 0));
  const quant = bytes[32 + 31];
  const vocabSize = Number(bigWord(bytes, 64));
  const paramCount = Number(bigWord(bytes, 96));
  const maxChunkBytes = Number(bigWord(bytes, 128));
  const artifactHash = '0x' + bytesToHex(bytes.subarray(160, 192));
  const nameLength = Number(bigWord(bytes, nameOffset));
  const name = decodeUtf8(bytes.subarray(nameOffset + 32, nameOffset + 32 + nameLength));
  return { name, quant, vocabSize, paramCount, maxChunkBytes, artifactHash };
}

function decodeBoolResult(resultHex) {
  const bytes = hexToBytes(resultHex);
  return bytes[31] === 1;
}

function decodeAddressArray(resultHex) {
  const bytes = hexToBytes(resultHex);
  const offset = Number(bigWord(bytes, 0));
  const length = Number(bigWord(bytes, offset));
  const out = [];
  for (let i = 0; i < length; i++) {
    out.push(readAddressFromWord(wordAt(bytes, offset / 32 + 1 + i)));
  }
  return out;
}

function decodeBytes32(resultHex) {
  return '0x' + strip0x(resultHex).slice(0, 64);
}

async function loadManifest(rpcUrl) {
  const chainIdHex = await rpcCall(rpcUrl, 'eth_chainId');
  if (Number.parseInt(chainIdHex, 16) !== CHAIN_ID) {
    throw new Error(`wrong chain: expected Sepolia ${CHAIN_ID}, got ${Number.parseInt(chainIdHex, 16)}`);
  }
  const [configResult, chunkCountResult, chunksResult, artifactResult, sealedResult, ownerResult] = await Promise.all([
    ethCall(rpcUrl, CONTRACT, SELECTORS.config),
    ethCall(rpcUrl, CONTRACT, SELECTORS.chunkCount),
    ethCall(rpcUrl, CONTRACT, SELECTORS.getChunks),
    ethCall(rpcUrl, CONTRACT, SELECTORS.artifactHash),
    ethCall(rpcUrl, CONTRACT, SELECTORS.isSealed),
    ethCall(rpcUrl, CONTRACT, SELECTORS.owner),
  ]);
  const config = decodeConfigResult(configResult);
  const chunkCount = wordNumber(hexToBytes(chunkCountResult), 0);
  const chunks = decodeAddressArray(chunksResult);
  const artifactHash = decodeBytes32(artifactResult);
  const isSealed = decodeBoolResult(sealedResult);
  const owner = readAddressFromWord(wordAt(hexToBytes(ownerResult), 0));
  if (!isSealed) throw new Error('contract is not sealed');
  if (chunkCount !== chunks.length) {
    throw new Error(`chunk count mismatch: contract says ${chunkCount}, getChunks() returned ${chunks.length}`);
  }
  // artifactHash() is the source of truth for the sealed artifact.
  return {
    rpcUrl,
    chainId: CHAIN_ID,
    contract: CONTRACT,
    ...config,
    quantName: quantName(config.quant),
    chunkCount,
    chunks,
    artifactHash,
    owner,
    isSealed,
  };
}

// Client-side cache of the verified gzip artifact, keyed by the onchain
// artifact hash. A hit skips all 29 eth_getCode chunk fetches; the SHA-256
// verification below still runs over the bytes either way, so the trust model
// is unchanged. The Cache API is missing outside secure contexts, so every
// helper degrades to a no-op.
const WEIGHT_CACHE = 'bard-weights';

function artifactCacheUrl(artifactHash) {
  return '/artifact/' + strip0x(artifactHash).toLowerCase();
}

async function readCachedArtifact(artifactHash) {
  if (typeof caches === 'undefined') return null;
  try {
    const cache = await caches.open(WEIGHT_CACHE);
    const res = await cache.match(artifactCacheUrl(artifactHash));
    if (!res) return null;
    return new Uint8Array(await res.arrayBuffer());
  } catch {
    return null;
  }
}

async function writeCachedArtifact(artifactHash, gzBytes) {
  if (typeof caches === 'undefined') return;
  try {
    const cache = await caches.open(WEIGHT_CACHE);
    await cache.put(artifactCacheUrl(artifactHash), new Response(gzBytes, { headers: { 'content-type': 'application/gzip' } }));
  } catch {
    // The cache is a best-effort accelerator; loading must not fail on it.
  }
}

async function deleteCachedArtifact(artifactHash) {
  if (typeof caches === 'undefined') return;
  try {
    const cache = await caches.open(WEIGHT_CACHE);
    await cache.delete(artifactCacheUrl(artifactHash));
  } catch {
    // Best effort, as above.
  }
}

// Fetch all chunk pointers with a bounded number of RPC calls in flight,
// placing each result by index so chunk order is preserved.
const CHUNK_CONCURRENCY = 6;

async function fetchChunks(rpcUrl, manifest) {
  const results = new Array(manifest.chunks.length);
  let nextIndex = 0;
  let fetched = 0;
  const lane = async () => {
    while (nextIndex < manifest.chunks.length) {
      if (abortRequested) throw new Error('aborted');
      const i = nextIndex++;
      const address = manifest.chunks[i];
      const codeHex = await ethGetCode(rpcUrl, address);
      const code = hexToBytes(codeHex);
      if (code.length === 0) throw new Error(`empty pointer code at ${address}`);
      if (code[0] !== 0x00) {
        throw new Error(`pointer ${address} does not start with STOP`);
      }
      results[i] = code.subarray(1);
      fetched += 1;
      progress('chunks', `${fetched}/${manifest.chunkCount} ${address}`, { manifest: manifestSummary(manifest) });
    }
  };
  await Promise.all(Array.from({ length: Math.min(CHUNK_CONCURRENCY, manifest.chunks.length) }, lane));
  return results;
}

async function decompressAndVerify(gzBytes, manifest) {
  progress('decompress', `gzip ${gzBytes.length} bytes`, { manifest: manifestSummary(manifest) });
  const stream = new Blob([gzBytes]).stream().pipeThrough(new DecompressionStream('gzip'));
  const decompressed = new Uint8Array(await new Response(stream).arrayBuffer());
  progress('verify', `sha-256 ${decompressed.length} bytes`, { manifest: manifestSummary(manifest) });

  const hashBuf = await crypto.subtle.digest('SHA-256', decompressed);
  const hashHex = bytesToHex(new Uint8Array(hashBuf));
  if (hashHex.toLowerCase() !== manifest.artifactHash.slice(2).toLowerCase()) {
    throw new Error(`artifact hash mismatch: expected ${manifest.artifactHash}, got 0x${hashHex}`);
  }
  return { decompressed, hashHex };
}

async function loadWeights(rpcUrl) {
  const manifest = await loadManifest(rpcUrl);
  postMessage({ type: 'manifest', manifest: manifestSummary(manifest) });

  let gzBytes = await readCachedArtifact(manifest.artifactHash);
  let fromCache = gzBytes !== null;
  let verified = null;
  if (fromCache) {
    progress('chunks', 'cached artifact found — skipping chunk fetches', { manifest: manifestSummary(manifest) });
    try {
      verified = await decompressAndVerify(gzBytes, manifest);
    } catch {
      // A stale or corrupt cache entry must never brick the load: evict it
      // and fall through to a fresh fetch from the chain.
      await deleteCachedArtifact(manifest.artifactHash);
      fromCache = false;
      verified = null;
    }
  }
  if (!verified) {
    progress('pointers', `found ${manifest.chunkCount} pointers`, { manifest: manifestSummary(manifest) });
    const chunks = await fetchChunks(rpcUrl, manifest);
    gzBytes = concatBytes(chunks);
    verified = await decompressAndVerify(gzBytes, manifest);
  }
  const { decompressed, hashHex } = verified;

  model = parseArtifact(decompressed);
  if (model.vocabSize !== manifest.vocabSize) throw new Error('header vocab size does not match manifest');
  if (model.paramCount !== manifest.paramCount) throw new Error('header param count does not match manifest');
  if (model.quant !== manifest.quant) throw new Error('header quant does not match manifest');
  if (model.vocab.length !== manifest.vocabSize - 1) throw new Error('vocab length mismatch');

  // Only store after a successful chain load + full verification.
  if (!fromCache) await writeCachedArtifact(manifest.artifactHash, gzBytes);

  postMessage({
    type: 'loaded',
    provenance: {
      hash: `0x${hashHex}`,
      artifactHash: manifest.artifactHash,
      chunkCount: manifest.chunkCount,
      gzBytes: gzBytes.length,
      layers: model.nLayer,
      embd: model.nEmbd,
      vocabSize: model.vocabSize,
    },
  });
}

function manifestSummary(manifest) {
  return {
    name: manifest.name,
    quantName: manifest.quantName,
    vocabSize: manifest.vocabSize,
    paramCount: manifest.paramCount,
    chunkCount: manifest.chunkCount,
    owner: manifest.owner,
  };
}

function parseArtifact(bytes) {
  const dv = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  if (decoder.decode(bytes.subarray(0, 4)) !== 'OCM1') {
    throw new Error('bad artifact magic');
  }
  const formatVersion = bytes[4];
  if (formatVersion !== 1) throw new Error(`unsupported artifact version ${formatVersion}`);
  const quant = bytes[5];
  const nLayer = bytes[6];
  const nHead = bytes[7];
  const nEmbd = dv.getUint16(8, true);
  const blockSize = dv.getUint16(10, true);
  const vocabSize = dv.getUint16(12, true);
  const ffnDim = dv.getUint16(14, true);
  const vocabByteLen = dv.getUint32(16, true);
  const vocab = decodeUtf8(bytes.subarray(20, 20 + vocabByteLen));
  let offset = 20 + vocabByteLen;
  const tensors = {};
  let paramCount = 0;
  const specs = [
    ['wte', vocabSize, nEmbd],
    ['wpe', blockSize, nEmbd],
  ];
  for (let i = 0; i < nLayer; i++) {
    specs.push([`layer${i}.attn_wq`, nEmbd, nEmbd]);
    specs.push([`layer${i}.attn_wk`, nEmbd, nEmbd]);
    specs.push([`layer${i}.attn_wv`, nEmbd, nEmbd]);
    specs.push([`layer${i}.attn_wo`, nEmbd, nEmbd]);
    specs.push([`layer${i}.mlp_fc1`, ffnDim, nEmbd]);
    specs.push([`layer${i}.mlp_fc2`, nEmbd, ffnDim]);
  }
  specs.push(['lm_head', vocabSize, nEmbd]);

  for (const [name, rows, cols] of specs) {
    paramCount += rows * cols;
    const result = readTensor(bytes, dv, offset, rows, cols, quant);
    tensors[name] = { shape: [rows, cols], data: result.data };
    offset = result.offset;
  }
  if (offset !== bytes.length) {
    throw new Error(`artifact has ${bytes.length - offset} trailing bytes`);
  }
  return { quant, nLayer, nHead, nEmbd, blockSize, vocabSize, ffnDim, paramCount, vocab, vocabByteLen, tensors, bosId: vocabSize - 1 };
}

function readTensor(bytes, dv, offset, rows, cols, quant) {
  const count = rows * cols;
  const scale = dv.getFloat32(offset, true);
  offset += 4;
  const data = new Float32Array(count);
  if (quant === 0) {
    for (let i = 0; i < count; i++) {
      data[i] = dv.getFloat32(offset + i * 4, true);
    }
    offset += count * 4;
    return { data, offset };
  }
  if (quant === 1) {
    for (let i = 0; i < count; i++) {
      data[i] = dv.getInt8(offset + i) * scale;
    }
    offset += count;
    return { data, offset };
  }
  if (quant === 2) {
    const packed = bytes.subarray(offset, offset + Math.ceil(count / 2));
    for (let i = 0; i < count; i++) {
      const byte = packed[i >> 1];
      const nibble = (i & 1) === 0 ? (byte & 0x0f) : ((byte >> 4) & 0x0f);
      const signed = nibble < 8 ? nibble : nibble - 16;
      data[i] = signed * scale;
    }
    offset += packed.length;
    return { data, offset };
  }
  throw new Error(`unsupported quant ${quant}`);
}

function encodePrompt(prompt, vocab) {
  const ids = [];
  for (const ch of prompt) {
    const idx = vocab.indexOf(ch);
    if (idx >= 0) ids.push(idx);
  }
  return ids;
}

function decodeToken(id, vocab) {
  return id >= 0 && id < vocab.length ? vocab[id] : '';
}

function rmsnormRows(x, rows, dim) {
  const out = new Float32Array(x.length);
  for (let r = 0; r < rows; r++) {
    let sum = 0;
    const base = r * dim;
    for (let i = 0; i < dim; i++) {
      const v = x[base + i];
      sum += v * v;
    }
    const scale = 1 / Math.sqrt(sum / dim + 1e-5);
    for (let i = 0; i < dim; i++) out[base + i] = x[base + i] * scale;
  }
  return out;
}

function linearRows(input, weight, rows, inputDim, outputDim) {
  const out = new Float32Array(rows * outputDim);
  for (let r = 0; r < rows; r++) {
    const inBase = r * inputDim;
    const outBase = r * outputDim;
    for (let o = 0; o < outputDim; o++) {
      let sum = 0;
      const wBase = o * inputDim;
      for (let i = 0; i < inputDim; i++) {
        sum += input[inBase + i] * weight[wBase + i];
      }
      out[outBase + o] = sum;
    }
  }
  return out;
}

function reluInPlace(arr) {
  for (let i = 0; i < arr.length; i++) {
    if (arr[i] < 0) arr[i] = 0;
  }
}

function attention(q, k, v, rows, dim, heads) {
  const headDim = dim / heads;
  const scale = 1 / Math.sqrt(headDim);
  const out = new Float32Array(rows * dim);
  const scores = new Float64Array(rows);
  for (let h = 0; h < heads; h++) {
    const headBase = h * headDim;
    for (let i = 0; i < rows; i++) {
      let max = -Infinity;
      const qi = i * dim + headBase;
      for (let j = 0; j <= i; j++) {
        let dot = 0;
        const kj = j * dim + headBase;
        for (let d = 0; d < headDim; d++) dot += q[qi + d] * k[kj + d];
        const score = dot * scale;
        scores[j] = score;
        if (score > max) max = score;
      }
      let denom = 0;
      for (let j = 0; j <= i; j++) {
        const e = Math.exp(scores[j] - max);
        scores[j] = e;
        denom += e;
      }
      const oi = i * dim + headBase;
      for (let d = 0; d < headDim; d++) {
        let sum = 0;
        for (let j = 0; j <= i; j++) {
          sum += (scores[j] / denom) * v[j * dim + headBase + d];
        }
        out[oi + d] = sum;
      }
    }
  }
  return out;
}

function forward(tokens, m) {
  const T = tokens.length;
  const D = m.nEmbd;
  const H = m.nHead;
  const vocab = m.vocabSize;
  const x = new Float32Array(T * D);
  const wte = m.tensors.wte.data;
  const wpe = m.tensors.wpe.data;
  for (let t = 0; t < T; t++) {
    const tok = tokens[t];
    const xBase = t * D;
    const wteBase = tok * D;
    const wpeBase = t * D;
    for (let d = 0; d < D; d++) x[xBase + d] = wte[wteBase + d] + wpe[wpeBase + d];
  }

  for (let layer = 0; layer < m.nLayer; layer++) {
    const prefix = `layer${layer}.`;
    const h = rmsnormRows(x, T, D);
    const q = linearRows(h, m.tensors[`${prefix}attn_wq`].data, T, D, D);
    const k = linearRows(h, m.tensors[`${prefix}attn_wk`].data, T, D, D);
    const v = linearRows(h, m.tensors[`${prefix}attn_wv`].data, T, D, D);
    const o = attention(q, k, v, T, D, H);
    const proj = linearRows(o, m.tensors[`${prefix}attn_wo`].data, T, D, D);
    for (let i = 0; i < x.length; i++) x[i] += proj[i];

    const h2 = rmsnormRows(x, T, D);
    const mlp1 = linearRows(h2, m.tensors[`${prefix}mlp_fc1`].data, T, D, m.ffnDim);
    reluInPlace(mlp1);
    const mlp2 = linearRows(mlp1, m.tensors[`${prefix}mlp_fc2`].data, T, m.ffnDim, D);
    for (let i = 0; i < x.length; i++) x[i] += mlp2[i];
  }

  // Only the last position is ever sampled, so the final rmsnorm + lm_head
  // matmul run on that single row instead of all T. Both ops are row-local,
  // so the last row's floats are bit-identical to the full-T computation.
  const xf = rmsnormRows(x.subarray((T - 1) * D), 1, D);
  return linearRows(xf, m.tensors.lm_head.data, 1, D, vocab);
}

function argmax(values) {
  let best = 0;
  for (let i = 1; i < values.length; i++) {
    if (values[i] > values[best]) best = i;
  }
  return best;
}

function softmax(values) {
  let max = -Infinity;
  for (const v of values) if (v > max) max = v;
  const probs = new Float64Array(values.length);
  let sum = 0;
  for (let i = 0; i < values.length; i++) {
    const e = Math.exp(values[i] - max);
    probs[i] = e;
    sum += e;
  }
  if (sum === 0) return probs;
  for (let i = 0; i < probs.length; i++) probs[i] /= sum;
  return probs;
}

function sample(logits, settings, counts, rng, bosId) {
  const adjusted = new Float64Array(logits.length);
  for (let i = 0; i < logits.length; i++) adjusted[i] = logits[i] - settings.freqPenalty * counts[i];
  if (settings.temperature <= 0) return argmax(adjusted);
  for (let i = 0; i < adjusted.length; i++) adjusted[i] /= settings.temperature;
  if (settings.topK > 0 && settings.topK < adjusted.length) {
    const ranked = Array.from(adjusted.entries()).sort((a, b) => b[1] - a[1]);
    const keep = new Set(ranked.slice(0, settings.topK).map(([i]) => i));
    for (let i = 0; i < adjusted.length; i++) if (!keep.has(i)) adjusted[i] = -Infinity;
  }
  let probs = softmax(adjusted);
  if (settings.topP < 1) {
    const ranked = Array.from(probs.entries()).sort((a, b) => b[1] - a[1]);
    const keep = new Set([ranked[0][0]]);
    let cum = ranked[0][1];
    for (let i = 1; i < ranked.length; i++) {
      if (cum >= settings.topP) break;
      keep.add(ranked[i][0]);
      cum += ranked[i][1];
    }
    let total = 0;
    const next = new Float64Array(probs.length);
    for (const idx of keep) {
      next[idx] = probs[idx];
      total += next[idx];
    }
    if (total > 0) {
      for (let i = 0; i < next.length; i++) next[i] /= total;
      probs = next;
    }
  }
  let draw = rng();
  let accum = 0;
  for (let i = 0; i < probs.length; i++) {
    accum += probs[i];
    if (draw <= accum) return i;
  }
  return probs.length - 1;
}

function mulberry32(seed) {
  let a = seed >>> 0;
  return function rng() {
    a += 0x6D2B79F5;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

async function pause() {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

async function generate(settings) {
  if (!model) throw new Error('load weights first');
  abortRequested = false;
  const rng = mulberry32(settings.seed >>> 0);
  const counts = new Uint32Array(model.vocabSize);
  const promptTokens = encodePrompt(settings.prompt, model.vocab);
  const tokens = [model.bosId, ...promptTokens];
  let text = '';
  for (let step = 0; step < settings.maxTokens; step++) {
    if (abortRequested) break;
    const window = tokens.slice(-model.blockSize);
    const logits = forward(window, model); // logits for the last position only
    const tok = sample(logits, settings, counts, rng, model.bosId);
    if (tok === model.bosId) break;
    tokens.push(tok);
    counts[tok] += 1;
    text += decodeToken(tok, model.vocab);
    postMessage({ type: 'token', text });
    if ((step & 3) === 3) await pause();
  }
  postMessage({ type: 'done', text });
}

let loadInFlight = false;

onmessage = async (event) => {
  const msg = event.data || {};
  try {
    if (msg.type === 'load') {
      if (loadInFlight) return; // never interleave two chain loads
      abortRequested = false;
      const rpcUrl = (msg.rpcUrl || RPC_DEFAULT).trim() || RPC_DEFAULT;
      progress('pointers', 'fetching manifest');
      loadInFlight = true;
      try {
        await loadWeights(rpcUrl);
      } finally {
        loadInFlight = false;
      }
      return;
    }
    if (msg.type === 'generate') {
      abortRequested = false;
      progress('sampling', 'starting generation');
      await generate(msg.settings);
      return;
    }
    if (msg.type === 'abort') {
      abortRequested = true;
      progress('aborting', 'requested');
    }
  } catch (err) {
    postMessage({ type: 'error', message: err && err.message ? err.message : String(err) });
  }
};
