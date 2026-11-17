import { spawn } from "node:child_process";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import nextEnv from "@next/env";
import { developmentAuth } from "./dev-config.mjs";

const ui = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const root = resolve(ui, "..");
nextEnv.loadEnvConfig(ui, true);
try {
  const config = resolve(root, process.env.CV_CONFIG || "hack/config.local.yaml");
  const auth = developmentAuth(config, resolve(root, "hack/.keycloak/ui.env"));
  const child = spawn(process.execPath, [resolve(ui, "node_modules/next/dist/bin/next"), "dev", ...process.argv.slice(2)], {
    cwd: ui, stdio: "inherit", env: { ...process.env, ...auth },
  });
  for (const signal of ["SIGINT", "SIGTERM"]) process.on(signal, () => child.kill(signal));
  child.on("error", error => { console.error(error.message); process.exitCode = 1; });
  child.on("exit", (code, signal) => { process.exitCode = code ?? (signal === "SIGINT" ? 130 : 1); });
} catch (error) {
  console.error(`UI development config: ${error.message}`);
  process.exitCode = 1;
}
