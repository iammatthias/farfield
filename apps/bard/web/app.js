const ASSET_VERSION = '20260602-bard-3';
const DEFAULT_RPC = 'https://ethereum-sepolia-rpc.publicnode.com';
const worker = new Worker(`./worker.js?v=${ASSET_VERSION}`, { type: 'module' });

const $ = (id) => document.getElementById(id);
const els = {
  load: $('load-btn'),
  generate: $('generate-btn'),
  abort: $('abort-btn'),
  clear: $('clear-btn'),
  prompt: $('prompt'),
  temp: $('temperature'),
  topk: $('topk'),
  topp: $('topp'),
  max: $('max-tokens'),
  seed: $('seed'),
  freq: $('freq-penalty'),
  tempVal: $('temp-val'),
  topkVal: $('topk-val'),
  toppVal: $('topp-val'),
  maxVal: $('max-val'),
  seedVal: $('seed-val'),
  freqVal: $('freq-val'),
  status: $('status'),
  progress: $('progress'),
  verified: $('verified'),
  provenance: $('provenance'),
  output: $('output'),
  name: $('m-name'),
  quant: $('m-quant'),
  vocab: $('m-vocab'),
  params: $('m-params'),
  chunks: $('m-chunks'),
  owner: $('m-owner'),
};

let loaded = false;
let streaming = false;
let currentText = '';

function syncSliderLabels() {
  els.tempVal.textContent = Number(els.temp.value).toFixed(2);
  els.topkVal.textContent = String(els.topk.value);
  els.toppVal.textContent = Number(els.topp.value).toFixed(2);
  els.maxVal.textContent = String(els.max.value);
  els.seedVal.textContent = String(els.seed.value);
  els.freqVal.textContent = Number(els.freq.value).toFixed(2);
}

function setStatus(text) {
  els.status.textContent = text;
}

function loadWeightsFromChain() {
  setStatus('starting');
  setProgress('pointers → chunks → decompress → verify → done');
  setOutput('');
  streaming = false;
  updateBusy();
  worker.postMessage({ type: 'load', rpcUrl: DEFAULT_RPC });
}

function setProgress(text) {
  els.progress.textContent = text;
}

function setOutput(text) {
  currentText = text;
  els.output.textContent = text || ' ';
}

function setModel(manifest) {
  els.name.textContent = manifest.name;
  els.quant.textContent = manifest.quantName;
  els.vocab.textContent = String(manifest.vocabSize);
  els.params.textContent = String(manifest.paramCount);
  els.chunks.textContent = String(manifest.chunkCount);
  els.owner.textContent = manifest.owner;
  els.verified.textContent = 'pending';
  els.provenance.textContent = '—';
}

function setControlsEnabled(ok) {
  loaded = ok;
  els.generate.disabled = !ok || streaming;
  els.abort.disabled = !streaming;
}

function updateBusy() {
  els.load.disabled = streaming;
  els.generate.disabled = !loaded || streaming;
  els.abort.disabled = !streaming;
}

function payload() {
  return {
    prompt: els.prompt.value,
    temperature: Number(els.temp.value),
    topK: Number(els.topk.value),
    topP: Number(els.topp.value),
    maxTokens: Number(els.max.value),
    seed: Number(els.seed.value) >>> 0,
    freqPenalty: Number(els.freq.value),
  };
}

['input', 'change'].forEach((evt) => {
  [els.temp, els.topk, els.topp, els.max, els.seed, els.freq].forEach((input) => {
    input.addEventListener(evt, syncSliderLabels);
  });
});

els.clear.addEventListener('click', () => setOutput(''));

els.load.addEventListener('click', loadWeightsFromChain);

els.generate.addEventListener('click', () => {
  if (!loaded || streaming) return;
  currentText = '';
  setOutput('');
  streaming = true;
  updateBusy();
  setStatus('generating');
  worker.postMessage({ type: 'generate', settings: payload() });
});

els.abort.addEventListener('click', () => {
  worker.postMessage({ type: 'abort' });
  setStatus('aborting');
});

worker.addEventListener('message', (event) => {
  const msg = event.data;
  if (msg.type === 'manifest') {
    setModel(msg.manifest);
    setStatus('manifest loaded');
    return;
  }
  if (msg.type === 'progress') {
    setStatus(msg.stage);
    if (msg.detail) setProgress(msg.detail);
    if (msg.manifest) setModel(msg.manifest);
    return;
  }
  if (msg.type === 'loaded') {
    loaded = true;
    streaming = false;
    setControlsEnabled(true);
    setStatus('ready');
    setProgress('done');
    els.verified.textContent = '✓ in-browser recompute matches the onchain hash';
    els.provenance.textContent = msg.provenance.hash;
    return;
  }
  if (msg.type === 'token') {
    currentText = msg.text;
    setOutput(currentText);
    return;
  }
  if (msg.type === 'done') {
    streaming = false;
    currentText = msg.text;
    setOutput(currentText);
    setStatus('done');
    updateBusy();
    return;
  }
  if (msg.type === 'error') {
    streaming = false;
    updateBusy();
    setStatus('error');
    setProgress(msg.message);
    return;
  }
});

worker.addEventListener('error', (event) => {
  streaming = false;
  updateBusy();
  setStatus('error');
  setProgress(event.message || 'worker error');
});

setOutput('Weights load on open. Generated text appears here.');
syncSliderLabels();
setControlsEnabled(false);
updateBusy();
loadWeightsFromChain();
