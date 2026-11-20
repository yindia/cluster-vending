// Refresh from the same provider version as the controller, without contacting a cluster.
import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { parse } from "yaml";

const root = fileURLToPath(new URL("../../", import.meta.url));
const providerModule = "github.com/upbound/provider-aws/v2";
const { Version: version, Dir: dir } = JSON.parse(execFileSync("go", ["list", "-m", "-json", providerModule], { cwd: root, encoding: "utf8" }));
const files = {
  s3: "s3.aws.m.upbound.io_buckets",
  rds: "rds.aws.m.upbound.io_instances",
  elasticache: "elasticache.aws.m.upbound.io_replicationgroups",
  publicAccess: "s3.aws.m.upbound.io_bucketpublicaccessblocks",
  encryption: "s3.aws.m.upbound.io_bucketserversideencryptionconfigurations",
  bucketVersioning: "s3.aws.m.upbound.io_bucketversionings",
};
const schemas = {};
for (const [service, file] of Object.entries(files)) {
  const crd = parse(readFileSync(join(dir, "package/crds", file + ".yaml"), "utf8"));
  const storage = crd.spec.versions.find(entry => entry.storage && entry.served);
  const spec = storage.schema.openAPIV3Schema.properties.spec;
  schemas[service] = {
    kind: crd.spec.names.kind, version: crd.spec.group + "/" + storage.name,
    source: "https://github.com/upbound/provider-aws/blob/" + version + "/package/crds/" + file + ".yaml",
    parameters: spec.properties.forProvider,
    envelope: Object.keys(spec.properties),
    rules: (spec["x-kubernetes-validations"] || []).map(rule => rule.message),
  };
}
writeFileSync(new URL("../src/lib/providers/aws-preview-schemas.json", import.meta.url), JSON.stringify({ version, schemas }, null, 2) + "\n");
