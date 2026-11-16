import { cp, mkdir } from "node:fs/promises";

// Keep the standalone server runnable locally as well as in the Docker image.
await mkdir(".next/standalone/.next", { recursive: true });
await cp(".next/static", ".next/standalone/.next/static", { recursive: true });
await cp("public", ".next/standalone/public", { recursive: true });
