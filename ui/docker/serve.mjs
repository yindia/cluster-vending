// The console's entrypoint in the container image (ui/Dockerfile).
//
// Next's standalone server builds request.url from its own listen address
// (http://<HOSTNAME>:<PORT>, http://0.0.0.0:3000 in a container), not from
// the Host the browser used. With login off, the RPC route compares the
// browser's Origin with that URL (src/lib/api/proxy.ts), so behind a
// port-forward or an Ingress every same-origin API call would be refused.
//
// This process listens on PORT for the world and runs the standalone server
// on loopback. For a request whose Origin names the very Host it was sent to
// (a same-origin request: what the browser does on the console's own page),
// it rewrites Origin to the inner server's origin, so the route's check
// passes. Any other Origin is passed unchanged and still refused. With login
// on (AUTH_ENABLED=true) the route checks Origin against AUTH_URL itself, so
// nothing is rewritten.
import { spawn } from "node:child_process";
import http from "node:http";

const publicPort = Number(process.env.PORT || 3000);
const publicHost = process.env.HOSTNAME || "0.0.0.0";
const innerPort = Number(process.env.CV_UI_INNER_PORT || publicPort + 1);
const innerHost = "127.0.0.1";
// Next names a loopback listener "localhost" in request.url.
const innerOrigin = `http://localhost:${innerPort}`;
const rewriteOrigin = process.env.AUTH_ENABLED !== "true";

const child = spawn(process.execPath, ["server.js"], {
  stdio: "inherit",
  env: { ...process.env, HOSTNAME: innerHost, PORT: String(innerPort) },
});
child.on("exit", (code, signal) => {
  server.close();
  process.exit(code ?? (signal ? 1 : 0));
});
for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => child.kill(signal));
}

// sameOrigin reports whether origin names the host the request was sent to
// (the first X-Forwarded-Host behind a proxy, else Host).
function sameOrigin(origin, req) {
  const forwarded = req.headers["x-forwarded-host"];
  const host = (Array.isArray(forwarded) ? forwarded[0] : forwarded)?.split(",")[0].trim() || req.headers.host;
  try {
    return Boolean(host) && new URL(origin).host === host;
  } catch {
    return false;
  }
}

const server = http.createServer((req, res) => {
  const headers = { ...req.headers };
  if (rewriteOrigin && headers.origin && sameOrigin(headers.origin, req)) {
    headers.origin = innerOrigin;
  }
  const upstream = http.request(
    { host: innerHost, port: innerPort, method: req.method, path: req.url, headers },
    (response) => {
      res.writeHead(response.statusCode ?? 502, response.headers);
      response.pipe(res);
    },
  );
  upstream.on("error", (error) => {
    if (!res.headersSent) {
      res.writeHead(502, { "Content-Type": "text/plain" });
    }
    res.end(`console server unavailable: ${error.code ?? error.message}`);
  });
  req.on("aborted", () => upstream.destroy());
  req.pipe(upstream);
});
server.keepAliveTimeout = 65_000;
server.listen(publicPort, publicHost, () => {
  console.log(`console listening on ${publicHost}:${publicPort} (Next on ${innerOrigin})`);
});
