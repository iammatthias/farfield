// engine.js -- pure-JS forward pass + sampling for the dead-presidents GPT.
// (Renamed from gpt.js so privacy/ad blockers don't eat a script named "gpt".)
//
// A direct port of the NumPy forward in gpt_fast.py: token+position embeddings,
// pre-norm residual blocks (RMSNorm -> multi-head causal attention -> RMSNorm ->
// MLP), and a tied output head. Plain script (no ES modules) so it loads via a
// classic Worker's importScripts() in the browser and via require() in Node.
// Inference only -- no autograd.
//
// Generation is KV-cached: _step computes a single token per call, appending
// its k/v to a per-layer cache (see generate), so each new character costs
// O(context) work instead of a full-context recompute and a sentence streams
// in well under a second.

(function (root) {
  "use strict";
  var RMS_EPS = 1e-5;

  function GPT(manifest, buffer) {
    var c = manifest.config;
    this.cfg = c;
    this.C = c.n_embd;
    this.L = c.n_layer;
    this.H = c.n_head;
    this.block = c.block_size;
    this.vocab = c.vocab_size;
    this.hs = this.C / this.H;
    this.bos = manifest.bos;
    this.itos = Array.from(manifest.chars);
    this.stoi = {};
    for (var si = 0; si < this.itos.length; si++) this.stoi[this.itos[si]] = si;

    var f = new Float32Array(buffer);
    this.t = {};
    for (var i = 0; i < manifest.tensors.length; i++) {
      var tn = manifest.tensors[i];
      this.t[tn.name] = f.subarray(tn.offset, tn.offset + tn.length);
    }
  }

  // X: (T, nIn) row-major; W: (nOut, nIn) row-major -> out (T, nOut)
  GPT.prototype._matmul = function (X, W, T, nIn, nOut) {
    var out = new Float32Array(T * nOut);
    for (var r = 0; r < T; r++) {
      var xr = r * nIn;
      for (var o = 0; o < nOut; o++) {
        var wr = o * nIn,
          s = 0;
        for (var i = 0; i < nIn; i++) s += X[xr + i] * W[wr + i];
        out[r * nOut + o] = s;
      }
    }
    return out;
  };

  GPT.prototype._rmsnorm = function (X, gain, T, C) {
    var out = new Float32Array(T * C);
    for (var r = 0; r < T; r++) {
      var base = r * C,
        ms = 0;
      for (var i = 0; i < C; i++) {
        var v = X[base + i];
        ms += v * v;
      }
      var inv = 1 / Math.sqrt(ms / C + RMS_EPS);
      for (var j = 0; j < C; j++) out[base + j] = X[base + j] * inv * gain[j];
    }
    return out;
  };

  GPT.prototype._sample = function (logits, temperature, rng) {
    var i;
    if (temperature <= 0) {
      var best = 0;
      for (i = 1; i < logits.length; i++)
        if (logits[i] > logits[best]) best = i;
      return best;
    }
    var mx = -Infinity;
    for (i = 0; i < logits.length; i++) if (logits[i] > mx) mx = logits[i];
    var probs = new Float32Array(logits.length),
      sum = 0;
    for (i = 0; i < logits.length; i++) {
      probs[i] = Math.exp((logits[i] - mx) / temperature);
      sum += probs[i];
    }
    var r = rng() * sum,
      cum = 0;
    for (i = 0; i < probs.length; i++) {
      cum += probs[i];
      if (r <= cum) return i;
    }
    return probs.length - 1;
  };

  // Encode a string to token ids the model knows (lowercase; chars outside the
  // 33-char vocabulary are dropped, whitespace folds to a space). Used to seed
  // generation from a user prompt.
  GPT.prototype.encode = function (str) {
    var s = String(str).toLowerCase(),
      ids = [],
      sp = this.stoi[" "];
    for (var i = 0; i < s.length; i++) {
      var id = this.stoi[s[i]];
      if (id !== undefined) ids.push(id);
      else if (sp !== undefined && /\s/.test(s[i])) ids.push(sp);
    }
    return ids;
  };

  // Single autoregressive step with a KV-cache: compute logits for `token` at
  // absolute position `pos`, appending its k/v to the per-layer cache and
  // attending over all cached positions 0..pos. O(context) per token instead of
  // the O(context^2) full recompute, so generation stays sub-second even at
  // block_size 256 and many layers. Mathematically identical to running the
  // full forward pass over the whole context and taking the last position.
  GPT.prototype._step = function (token, pos, cache) {
    var C = this.C,
      L = this.L,
      H = this.H,
      hs = this.hs,
      t = this.t;
    var scale = 1 / Math.sqrt(hs),
      x = new Float32Array(C),
      c,
      h,
      s,
      d;
    for (c = 0; c < C; c++) x[c] = t.wte[token * C + c] + t.wpe[pos * C + c];

    for (var l = 0; l < L; l++) {
      var n1 = this._rmsnorm(x, t["ln1_" + l], 1, C);
      var q = this._matmul(n1, t["wq_" + l], 1, C, C);
      var K = cache.K[l],
        V = cache.V[l];
      K.set(this._matmul(n1, t["wk_" + l], 1, C, C), pos * C);
      V.set(this._matmul(n1, t["wv_" + l], 1, C, C), pos * C);

      var ctx = new Float32Array(C);
      for (h = 0; h < H; h++) {
        var lo = h * hs,
          scores = new Float32Array(pos + 1),
          mx = -Infinity;
        for (s = 0; s <= pos; s++) {
          var dot = 0;
          for (d = 0; d < hs; d++) dot += q[lo + d] * K[s * C + lo + d];
          dot *= scale;
          scores[s] = dot;
          if (dot > mx) mx = dot;
        }
        var sum = 0;
        for (s = 0; s <= pos; s++) {
          scores[s] = Math.exp(scores[s] - mx);
          sum += scores[s];
        }
        for (d = 0; d < hs; d++) {
          var acc = 0;
          for (s = 0; s <= pos; s++) acc += scores[s] * V[s * C + lo + d];
          ctx[lo + d] = acc / sum;
        }
      }
      var attnOut = this._matmul(ctx, t["wo_" + l], 1, C, C);
      for (c = 0; c < C; c++) x[c] += attnOut[c];

      var n2 = this._rmsnorm(x, t["ln2_" + l], 1, C);
      var h1 = this._matmul(n2, t["fc_" + l], 1, C, 4 * C);
      for (var hi = 0; hi < h1.length; hi++) if (h1[hi] < 0) h1[hi] = 0;
      var mlpOut = this._matmul(h1, t["proj_" + l], 1, 4 * C, C);
      for (c = 0; c < C; c++) x[c] += mlpOut[c];
    }

    var xf = this._rmsnorm(x, t.lnf, 1, C);
    var logits = new Float32Array(this.vocab);
    for (var vi = 0; vi < this.vocab; vi++) {
      var wr = vi * C,
        sl = 0;
      for (c = 0; c < C; c++) sl += t.wte[wr + c] * xf[c];
      logits[vi] = sl;
    }
    return logits;
  };

  // Autoregressively generate a continuation, KV-cached. opts.promptIds seeds
  // the context (after BOS); onToken(char) fires per character. The total
  // sequence (BOS + prompt + output) is capped at block_size. Returns
  // {text, finish_reason, prompt_tokens, completion_tokens}.
  GPT.prototype.generate = function (opts) {
    opts = opts || {};
    var temperature = opts.temperature == null ? 0.6 : opts.temperature;
    var maxNew = opts.maxNew == null ? 200 : opts.maxNew;
    var rng = opts.rng || Math.random;
    var onToken = opts.onToken || null;
    var prompt = (opts.promptIds || []).slice(0, this.block - 1);

    var cache = { K: [], V: [] };
    for (var l = 0; l < this.L; l++) {
      cache.K.push(new Float32Array(this.block * this.C));
      cache.V.push(new Float32Array(this.block * this.C));
    }

    var ids = [this.bos].concat(prompt);
    var pos = 0,
      logits = null;
    for (var i = 0; i < ids.length && pos < this.block; i++) {
      logits = this._step(ids[i], pos, cache);
      pos++;
    }

    var outStr = "",
      finish = "length";
    while (outStr.length < maxNew && pos < this.block) {
      var nxt = this._sample(logits, temperature, rng);
      if (nxt === this.bos) {
        finish = "stop";
        break;
      }
      var ch = this.itos[nxt];
      outStr += ch;
      if (onToken) onToken(ch);
      logits = this._step(nxt, pos, cache);
      pos++;
    }
    return {
      text: outStr,
      finish_reason: finish,
      prompt_tokens: prompt.length + 1, // +1 for BOS
      completion_tokens: outStr.length,
    };
  };

  // Expose for: classic Worker (self), browser (window), and Node (module.exports).
  if (typeof self !== "undefined") self.GPT = GPT;
  if (root) root.GPT = GPT;
  if (typeof module !== "undefined" && module.exports)
    module.exports = { GPT: GPT };
})(typeof globalThis !== "undefined" ? globalThis : this);
