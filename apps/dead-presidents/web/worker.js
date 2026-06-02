// worker.js -- classic Web Worker (importScripts, no ES modules so it loads
// even behind strict-MIME servers and content blockers). Holds the model and
// answers OpenAI-shaped chat requests, streaming chat.completion.chunk objects.
importScripts("./engine.js");

var gpt = null;
var COUNT = 0;

function now() {
  return self.performance && performance.now ? performance.now() : Date.now();
}
function cid() {
  return "chatcmpl-" + Date.now().toString(36) + (COUNT++).toString(36);
}

function load() {
  return Promise.all([fetch("./model.json"), fetch("./model.bin")])
    .then(function (r) {
      if (!r[0].ok || !r[1].ok) throw new Error("failed to fetch model files");
      return Promise.all([r[0].json(), r[1].arrayBuffer()]);
    })
    .then(function (a) {
      var manifest = a[0];
      gpt = new GPT(manifest, a[1]);
      self.postMessage({
        type: "ready",
        config: manifest.config,
        val_bpc: manifest.val_bpc,
        n_params: manifest.n_params,
        conditioned: !!manifest.conditioned,
        voices: manifest.voices || [],
      });
    });
}
load().catch(function (e) {
  self.postMessage({ type: "error", id: null, message: String(e) });
});

// Build a generation seed from the LATEST user message (this model has no real
// multi-turn memory -- its context is a few hundred chars -- so seeding from the
// whole transcript just fills the window and leaves no room to generate). Cap
// the seed at half the context so there is always room for output. If a `voice`
// is given (conditioned models), prefix with "<voice>: " to steer the style.
function seedFromMessages(messages, voice) {
  var text = "";
  for (var i = (messages || []).length - 1; i >= 0; i--) {
    var m = messages[i];
    if (m && m.role === "user" && m.content) {
      text = String(m.content);
      break;
    }
  }
  if (voice) text = voice + ": " + text;
  var ids = gpt.encode(text);
  var cap = Math.max(1, Math.floor(gpt.block / 2)); // always leave room to generate
  if (ids.length > cap) ids = ids.slice(ids.length - cap);
  return ids;
}

self.onmessage = function (e) {
  var m = e.data;
  if (m.type !== "chat") return;
  if (!gpt) {
    self.postMessage({
      type: "error",
      id: m.id,
      message: "model still loading",
    });
    return;
  }
  var req = m.request || {};
  var model = req.model || "dead-presidents-gpt";
  var id = cid();
  var created = Math.floor(Date.now() / 1000);
  var promptIds = seedFromMessages(req.messages, req.voice);
  var t0 = now();

  function chunk(delta, finish, usage) {
    var c = {
      id: id,
      object: "chat.completion.chunk",
      created: created,
      model: model,
      choices: [{ index: 0, delta: delta, finish_reason: finish || null }],
    };
    if (usage) c.usage = usage;
    return c;
  }

  if (req.stream) {
    self.postMessage({
      type: "chunk",
      id: m.id,
      chunk: chunk({ role: "assistant" }, null),
    });
    var res = gpt.generate({
      temperature: req.temperature,
      maxNew: req.max_tokens,
      promptIds: promptIds,
      onToken: function (ch) {
        self.postMessage({
          type: "chunk",
          id: m.id,
          chunk: chunk({ content: ch }, null),
        });
      },
    });
    var ms = now() - t0;
    var usage = {
      prompt_tokens: res.prompt_tokens,
      completion_tokens: res.completion_tokens,
      total_tokens: res.prompt_tokens + res.completion_tokens,
    };
    self.postMessage({
      type: "chunk",
      id: m.id,
      chunk: chunk({}, res.finish_reason, usage),
    });
    self.postMessage({
      type: "final",
      id: m.id,
      ms: ms,
      usage: usage,
      finish_reason: res.finish_reason,
    });
  } else {
    var r2 = gpt.generate({
      temperature: req.temperature,
      maxNew: req.max_tokens,
      promptIds: promptIds,
    });
    var ms2 = now() - t0;
    var usage2 = {
      prompt_tokens: r2.prompt_tokens,
      completion_tokens: r2.completion_tokens,
      total_tokens: r2.prompt_tokens + r2.completion_tokens,
    };
    var completion = {
      id: id,
      object: "chat.completion",
      created: created,
      model: model,
      choices: [
        {
          index: 0,
          message: { role: "assistant", content: r2.text },
          finish_reason: r2.finish_reason,
        },
      ],
      usage: usage2,
    };
    self.postMessage({
      type: "final",
      id: m.id,
      ms: ms2,
      usage: usage2,
      finish_reason: r2.finish_reason,
      completion: completion,
    });
  }
};
