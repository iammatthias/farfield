// app.js -- the farfield UI glue over the OpenAI-shaped DeadPresidents client.
// The client (openai.js) runs the char-level GPT in a Web Worker; this file only
// wires the controls to it and streams the continuation into the page.
const ASSET_VERSION = '20260602-dp-1';
const client = DeadPresidents.createClient({ workerUrl: `./worker.js?v=${ASSET_VERSION}` });

const $ = (id) => document.getElementById(id);
const els = {
  seed: $('seed'),
  temp: $('temperature'),
  max: $('max-tokens'),
  tempVal: $('temp-val'),
  maxVal: $('max-val'),
  generate: $('generate-btn'),
  clear: $('clear-btn'),
  output: $('output'),
  genStat: $('gen-stat'),
  status: $('m-status'),
  params: $('m-params'),
  vocab: $('m-vocab'),
  context: $('m-context'),
  layers: $('m-layers'),
  bpc: $('m-bpc'),
};

let ready = false;
let busy = false;

function syncLabels() {
  els.tempVal.textContent = Number(els.temp.value).toFixed(2);
  els.maxVal.textContent = String(els.max.value);
}

function setStatus(text, live) {
  els.status.textContent = text;
  els.status.className = 'badge ' + (live ? 'live' : 'draft');
}

// The model is char-level and lowercases its input, so render the seed lowercased
// (dimmed) followed by the continuation — it reads as one continuous passage.
function render(seedText, continuation) {
  els.output.textContent = '';
  if (seedText) {
    const s = document.createElement('span');
    s.className = 'seed';
    s.textContent = seedText.toLowerCase();
    els.output.appendChild(s);
  }
  els.output.appendChild(document.createTextNode(continuation));
}

client.ready.then((meta) => {
  ready = true;
  els.params.textContent = Number(meta.n_params).toLocaleString();
  els.vocab.textContent = String(meta.config.vocab_size);
  els.context.textContent = meta.config.block_size + ' chars';
  els.layers.textContent = String(meta.config.n_layer);
  els.bpc.textContent = Number(meta.val_bpc).toFixed(3) + ' bpc';
  setStatus('ready', true);
  els.generate.disabled = false;
  render('', 'Ready. Edit the seed and press Continue.');
}).catch((e) => {
  setStatus('failed', false);
  render('', 'Could not load the model: ' + (e && e.message ? e.message : e));
});

async function generate() {
  if (!ready || busy) return;
  busy = true;
  els.generate.disabled = true;
  setStatus('generating', true);
  const seedText = els.seed.value;
  let continuation = '';
  render(seedText, '');
  els.genStat.textContent = '—';
  const t0 = performance.now();
  try {
    const stream = await client.chat.completions.create({
      messages: [{ role: 'user', content: seedText }],
      temperature: Number(els.temp.value),
      max_tokens: Number(els.max.value),
      stream: true,
    });
    let usage = null;
    for await (const chunk of stream) {
      const delta = chunk.choices[0].delta.content || '';
      if (delta) { continuation += delta; render(seedText, continuation); }
      if (chunk.usage) usage = chunk.usage;
    }
    const ms = Math.round(performance.now() - t0);
    const n = usage ? usage.completion_tokens : continuation.length;
    const rate = ms > 0 ? Math.round((n / ms) * 1000) : 0;
    els.genStat.textContent = `${n} chars in ${ms} ms · ${rate} char/s`;
    setStatus('ready', true);
  } catch (e) {
    setStatus('error', false);
    els.genStat.textContent = e && e.message ? e.message : String(e);
  } finally {
    busy = false;
    els.generate.disabled = false;
  }
}

['input', 'change'].forEach((evt) => {
  els.temp.addEventListener(evt, syncLabels);
  els.max.addEventListener(evt, syncLabels);
});
els.generate.addEventListener('click', generate);
els.clear.addEventListener('click', () => { els.seed.value = ''; render('', ''); els.genStat.textContent = '—'; els.seed.focus(); });
els.seed.addEventListener('keydown', (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') { e.preventDefault(); generate(); }
});

syncLabels();
setStatus('loading', false);
