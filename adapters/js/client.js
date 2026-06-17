// PromptNet JavaScript adapter (Node). Mirrors the Python client.
// Loads the .proto at runtime via @grpc/proto-loader — no codegen step.
//   npm i @grpc/grpc-js @grpc/proto-loader   (and `nats` only if you subscribe)
const path = require("path");
const grpc = require("@grpc/grpc-js");
const protoLoader = require("@grpc/proto-loader");

const PROTO = path.resolve(__dirname, "../../proto/promptnet/v1/prompt.proto");

function loadService() {
  const def = protoLoader.loadSync(PROTO, {
    keepCase: true, // preserve field names like version_hash, new_template
    longs: String,
    enums: String,
    defaults: true,
  });
  return grpc.loadPackageDefinition(def).promptnet.v1;
}

function subject(uri) {
  return "promptnet." + uri.replace("promptnet://", "").replace(/\//g, ".");
}

class PromptClient {
  constructor({ host, token, tls = false, natsUrl } = {}) {
    const pkg = loadService();
    const creds = tls ? grpc.credentials.createSsl() : grpc.credentials.createInsecure();
    this._stub = new pkg.PromptService(host, creds);
    this._meta = new grpc.Metadata();
    if (token) this._meta.add("authorization", `Bearer ${token}`);
    this._natsUrl = natsUrl;
  }

  _call(method, req) {
    return new Promise((resolve, reject) =>
      this._stub[method](req, this._meta, (err, res) => (err ? reject(err) : resolve(res)))
    );
  }

  // get fetches the served HEAD, or a pinned version when `ref` (a branch name
  // or commit hash) is given.
  get(uri, ref = "") {
    return this._call("GetPrompt", { uri, ref });
  }

  diff(uri, newTemplate) {
    return this._call("DiffPrompt", { uri, new_template: newTemplate });
  }

  publish(uri, template, slots = []) {
    return this._call("PublishPrompt", { uri, template, slots });
  }

  // subscribe registers as a subscriber: onChange(versionHash, classification)
  // fires on each republish (push). classification is the semantic diff verdict
  // (structural | localized tweak | minor edit | new | ""), so an agent can
  // auto-reload a tweak but hold a structural change. Needs `npm i nats` and
  // natsUrl set. Returns the connection.
  async subscribe(uri, onChange) {
    if (!this._natsUrl) throw new Error("set natsUrl on PromptClient to subscribe");
    const { connect, StringCodec } = require("nats");
    const nc = await connect({ servers: this._natsUrl });
    const sc = StringCodec();
    (async () => {
      for await (const m of nc.subscribe(subject(uri))) {
        let ev;
        try {
          ev = JSON.parse(sc.decode(m.data));
        } catch {
          ev = { version: sc.decode(m.data) }; // pre-0.7 bare-hash body
        }
        onChange(ev.version || "", ev.classification || "");
      }
    })();
    return nc;
  }

  close() {
    grpc.closeClient(this._stub);
  }
}

module.exports = { PromptClient };
