// openai.js -- a tiny OpenAI-SDK-flavored client that runs the model in a Web
// Worker. Call shape mirrors the real thing:
//
//   const client = DeadPresidents.createClient();
//   await client.ready;
//   const stream = await client.chat.completions.create({
//     messages: [{ role: "user", content: "the state of the union" }],
//     temperature: 0.6, max_tokens: 200, stream: true,
//   });
//   for await (const chunk of stream) { ... chunk.choices[0].delta.content ... }
//
// Non-streaming returns a standard `chat.completion` object with `usage`.
(function (root) {
  "use strict";

  function makeAsyncQueue() {
    var items = [],
      waiters = [],
      done = false,
      err = null;
    return {
      push: function (v) {
        if (waiters.length) waiters.shift()({ value: v, done: false });
        else items.push(v);
      },
      close: function () {
        done = true;
        while (waiters.length)
          waiters.shift()({ value: undefined, done: true });
      },
      fail: function (e) {
        err = e;
        done = true;
        while (waiters.length) {
          var w = waiters.shift();
          w(Promise.reject(e));
        }
      },
      iterable: (function () {
        var it = {};
        it[Symbol.asyncIterator] = function () {
          return {
            next: function () {
              if (err) return Promise.reject(err);
              if (items.length)
                return Promise.resolve({ value: items.shift(), done: false });
              if (done)
                return Promise.resolve({ value: undefined, done: true });
              return new Promise(function (res) {
                waiters.push(res);
              });
            },
          };
        };
        return it;
      })(),
    };
  }

  function createClient(opts) {
    opts = opts || {};
    var worker = new Worker(opts.workerUrl || "./worker.js");
    var seq = 0;
    var handlers = {};
    var meta = null;
    var readyResolve, readyReject;
    var ready = new Promise(function (res, rej) {
      readyResolve = res;
      readyReject = rej;
    });

    worker.onmessage = function (e) {
      var m = e.data;
      if (m.type === "ready") {
        meta = m;
        readyResolve(m);
        return;
      }
      if (m.type === "error" && m.id == null) {
        readyReject(new Error(m.message));
        return;
      }
      var h = handlers[m.id];
      if (!h) return;
      if (m.type === "chunk") h.onChunk(m.chunk);
      else if (m.type === "final") {
        h.onFinal(m);
        delete handlers[m.id];
      } else if (m.type === "error") {
        h.onError(new Error(m.message));
        delete handlers[m.id];
      }
    };

    function create(params) {
      params = params || {};
      var id = "req-" + ++seq;
      var stream = !!params.stream;
      worker.postMessage({
        type: "chat",
        id: id,
        request: {
          messages: params.messages || [],
          temperature: params.temperature == null ? 0.6 : params.temperature,
          max_tokens: params.max_tokens == null ? 200 : params.max_tokens,
          stream: stream,
          model: params.model || "dead-presidents-gpt",
          voice: params.voice || null, // conditioned models: "speak as" steer
        },
      });

      if (!stream) {
        return new Promise(function (resolve, reject) {
          handlers[id] = {
            onChunk: function () {},
            onFinal: function (m) {
              resolve(m.completion);
            },
            onError: reject,
          };
        });
      }
      var q = makeAsyncQueue();
      handlers[id] = {
        onChunk: function (c) {
          q.push(c);
        },
        onFinal: function () {
          q.close();
        },
        onError: function (e) {
          q.fail(e);
        },
      };
      return Promise.resolve(q.iterable);
    }

    return {
      chat: { completions: { create: create } },
      ready: ready,
      meta: function () {
        return meta;
      },
      terminate: function () {
        worker.terminate();
      },
    };
  }

  root.DeadPresidents = { createClient: createClient };
})(typeof window !== "undefined" ? window : this);
