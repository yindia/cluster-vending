# Cluster results in S3

Status: built (write path, read path, deletion, the shard index and the `nodes` kind). Scan results (deprecated APIs, resource
recommendations, policy violations and events) move out of the Cluster
custom resource into an S3 bucket per cluster, so large clusters are not
limited by object size (etcd's 1.5 MiB, our 128 KiB snapshot cap). This
changes one rule of `docs/architecture.md`: the API server now has AWS
access, read-only, to these buckets.

## Decisions

| Question | Decision |
|---|---|
| Where the bucket lives | The cluster's own AWS account (the tenant's), created through the tenant role like every other cluster resource. |
| How it is created | provider-aws-s3, installed and activated at bootstrap with the other providers; the bucket and its settings are rendered with the cluster (`ClusterSet`) and deleted with it. |
| Who writes | The controller's scanners (deprecations, recommendations, policy), through the tenant role (`reconcile.Deps.AWSAccess`). |
| Who reads | The API server, directly from S3, through the tenant role. |
| Credentials (server and controller) | Base credentials from the AWS SDK default chain: IRSA in production, static keys in the environment locally. Optionally a source role to assume first (`aws-source-role-arn`). Then always the tenant role of the cluster's account. |
| History | Each scan writes a timestamped object and overwrites `latest`; a lifecycle rule expires history after 30 days. |
| The Cluster CR | Keeps a small summary per result kind: counts, collected time, completeness, staleness inputs and the `latest` object key. Gates that read the CR (removed APIs in use block an upgrade) read the summary. Full lists only in S3. |

## Bucket

- Name: `cv-<prefix>-<12 hex of sha256(account,region,cluster ID)>`, at most
  63 characters, lower case (globally unique, stable, no tenant data).
- Block all public access, bucket owner enforced, SSE-S3 (AES256) default
  encryption, TLS-only bucket policy, versioning off (history is explicit).
- Lifecycle: objects under `history/` expire after 30 days; incomplete
  multipart uploads after 1 day.
- Deleted with the cluster: the controller empties it (all objects under the
  cluster's prefixes) before the Bucket managed resource is deleted.

## Objects

```
latest/deprecations.json      history/deprecations/<RFC3339>.json
latest/recommendations.json   history/recommendations/<RFC3339>.json
latest/violations.json        history/violations/<RFC3339>.json
latest/violation-events.json  history/violation-events/<RFC3339>.json
```

Each is protojson of the existing contract message (the deprecation scan,
the recommendation set, `ClusterPolicySnapshot`), gzip-compressed
(`Content-Encoding: gzip`), with a bounded size (default 16 MiB
uncompressed). A write is `history` first, then `latest`; a reader reads
`latest`. The summary in the CR names the `latest` object's key and ETag,
so the server can tell a summary from a newer object.

## Server

- Reads `latest/*` with `GetObject` through the tenant role, a 5s timeout,
  the bounded size, and an in-process cache keyed by bucket, key and ETag.
- Pagination tokens keep binding to the snapshot (now the object's ETag)
  and scope.
- A missing bucket or object, denied access, or a timeout is a coverage
  notice in the response ("results unavailable: …"), never a failed call.
- Settings (flag, `CV_*` variable, file, as every other setting):
  `aws-region` (default region for the SDK), `aws-source-role-arn`
  (optional), `aws-external-id` (optional, for the tenant role). No keys in
  settings: static credentials come only from the environment (local).

## Account template

The tenant role (provisioner CloudFormation) trusts the server's principal
as well as the controller's (new parameter `ServerPrincipalArn`), and allows
the bucket actions (create, configure, delete `cv-*` buckets) and object
read/write/delete on `cv-*` buckets.

## Migration

Existing clusters get their bucket on the next pass. Until a scanner has
written to S3, the server falls back to what the CR status holds today; the
CR's full lists are dropped once a summary with an S3 key is written.

### Migration of existing clusters

On a cluster that existed before the Karpenter and results builds, the
bucket and its settings are plain management-cluster objects rendered with
the cluster: no Helm Release gate, placement value or node group waits for
them, and they are never held by a stuck chart. The per-node rows are written
only when the node group applier runs, and that waits for Karpenter's IAM
and a Ready Karpenter (see `karpenter-node-groups.md`, "Migration of existing
clusters": placement is resolved from live state, so nothing moves to a node
label that no node has yet). Reads stay bounded: one cluster's own objects
and paged node lists, never a fleet scan.

## Building blocks (what exists, and how the controller phase calls it)

The bucket, the store, the credentials and the server's read path are built;
the scanners writing to the bucket, emptying it on delete and writing the
summaries are the controller phase.

| What | Where |
|---|---|
| provider-aws-s3 (installed, activated, in the scheme, RBAC) | `pkg/crossplane/aws` (`ProviderPackages`, `managedMRDs`), `pkg/kube` |
| The bucket and its settings, rendered with the cluster | `pkg/crossplane/aws/results_bucket.go`: `ClusterPlan.ResultsBucket` (set by `cluster.RenderPlan` for clusters the platform created), needs `ClusterPlan.AccountID`; none for an imported cluster. `RenderVersion` 7 |
| Names | `ResultsBucketName(prefix, accountID, region, clusterID)`, `ClusterPlan.ResultsBucketName()`, `ResultsBucketObjectName(prefix)` (the Bucket managed resource), `ResultsBucketBelongsTo(bucket, accountID, region, clusterID)` |
| Reading and writing objects | `pkg/results` |
| S3 clients and access of an account, for the server and the controller alike | `pkg/cloud/aws` (`AccessFor`, `Client.S3`, `Config.SourceRoleARN`, `ExternalID`, `SessionPolicy`), `management.NewAWSClient` |
| The server's read path | `pkg/server/cluster` (`results.go`, `deprecations.go`, `recommendations.go`, `violations.go`), `server.NewResultsStore` |
| The summaries on the Cluster | `status.results` (`api/v1alpha1` `ClusterResultRef`), `model.Cluster.Results`, `Clusters.SetResultRef` |
| The tenant role | `pkg/cloudformation` (`ServerPrincipalArn`, the `provision-results` policy), `scripts/generate.py` |

The server never writes: its tenant-role session is limited to
`s3:GetObject` on `cv-*/*` and `s3:ListBucket` on `cv-*`
(`results.ReadOnlySessionPolicy`), whatever the role allows.

### Calling it from the controller

```go
// A client for the account, as the lookups have: own credentials, the
// source role of the settings, the tenant role (cfg.AWSAccess from
// config.LoadController; no session policy, the controller writes).
aws, err := management.NewAWSClient(ctx, cfg.AWSAccess, "")
access, err := awscloud.AccessFor(account.AccountID, account.Identity, account.ExternalID)
s3, err := aws.S3(access, cluster.Region)          // *s3.Client, a results.S3API
bucket := crossplaneaws.ResultsBucketName(prefix, account.AccountID, cluster.Region, cluster.ID)

// A scan: history, then latest. key and etag go into the summary.
key, etag, err := results.NewWriter(s3).Put(ctx, bucket, results.Deprecations, scan, time.Now())
err = store.Clusters().SetResultRef(ctx, tenantID, clusterID, model.ResultRef{
    Kind: string(results.Deprecations), Bucket: bucket, Key: key, ETag: etag,
    CollectedAt: now, Total: scan.Total, Removed: scan.Removed})   // drops the findings from the Cluster

// A cluster's deletion, after its settings are gone and before the Bucket
// object is deleted (the Bucket is never force-destroyed):
n, err := results.EmptyBucket(ctx, s3, bucket)
// then delete the Bucket managed resource crossplaneaws.ResultsBucketObjectName(prefix)
```

The messages: deprecations are a `cloudv1.ClusterDeprecations` with its
findings, recommendations a `cloudv1.ListClusterRecommendationsResponse`
(the sorted recommendations, the summary, the total), violations and
violation events a `cloudv1.ClusterPolicySnapshot` (its violations; its
events). The Bucket is not deleted until it is empty, and a bucket with
objects the platform did not write (outside `latest/` and `history/`) stays:
the provider reports it and a person looks.

### Status: the controller's write path (built)

- **Settings.** `aws-region`, `aws-source-role-arn` and `aws-external-id`
  are wired through `pkg/controller/manager` (`management.NewAWSClient`,
  `management.WithAWSClient`). `reconcile.Deps.AWSAccess` calls
  `awscloud.AccessFor`, so the controller and the API server make an
  account's access one way (the local `--skip-tenant-role` keys aside).
  `Deps.ResultsS3` makes the S3 client of that access; nil (tests) writes
  and empties nothing.
- **Writing.** `pkg/reconcile/resultsink.Sink.Stage` writes one result of a
  kind (`results.Writer.Put`: history, then latest) when the cluster's Bucket
  managed resource exists and is Ready, each call bounded to 10s. The scanner
  then records the summary with `Staged.Commit` (`SetResultRef`: counts,
  collected and attempted time, completeness, key, ETag, digest), after the
  Cluster object holds what it keeps of the scan, because recording a summary
  drops the lists the bucket now holds.
  - Deprecations: after a scan that worked and was recorded, the full
    `ClusterDeprecations` (every finding: `deprecation.Report.All`).
  - Recommendations: after a sample, the recommendations the recorded usage
    gives (`ListClusterRecommendationsResponse`, all rows, with the summary).
  - Policy: violations and violation events as two `ClusterPolicySnapshot`s,
    collected in full (`policy.Reader.Full`: lists of 500 with `continue`, the
    2-minute deadline instead of the 200-row and 128 KiB caps, marked
    truncated and incomplete when it hits). The Cluster object keeps a
    bounded copy until both results are in the bucket, then only the summaries.
- **Fallback.** No bucket, not Ready, no access, a timeout or an error: nothing
  is recorded as a summary, the scan is not failed, and the Cluster object
  keeps the data as before, with the notice "scan results are kept in the
  cluster object: ..." in the recommendation summary's message and the policy
  snapshot's notices. Deprecations have no field for a notice that does not
  mean a failed scan, so there it is logged only. An imported cluster
  without a Bucket is silent. A summary that exists keeps naming the last
  object written: after a later failure the server serves that last result,
  not the newer one in the Cluster object.
- **Write only on change.** `ResultRef.Digest` hashes the content without its
  times (`resultsink.Digest`); the same digest as the recorded summary writes
  nothing and only moves the summary's times.
- **Deletion.** In the cluster's delete path (`driveDelete`), every pass
  before the objects are deleted and while the Bucket exists, the controller
  empties the bucket with `results.EmptyBucket` through the tenant role. A
  failure is the cluster's status (`deleting; cannot empty the results bucket
  ... retrying`), the Bucket is not deleted, and the next pass tries again;
  a bucket already gone is empty. Objects outside `latest/` and `history/`
  stay and hold the bucket (a person looks), as the provider reports.
- **Sharding and the `nodes` kind (built).** `results.Nodes` is stored as one
  `cloudv1.NodeGroupNodes` per node group, `latest/nodes/<group>.json` (history
  `history/nodes/<group>/<time>.json`), and an index `latest/nodes/index.json`
  (`cloudv1.ResultIndex`: `shards: [{shard, key, etag, count, digest}]`,
  `collectedAt`, `complete`). `Writer.PutSharded(ctx, bucket, kind, shards, now,
  opts...)` writes the shards whose digest changed (a shard named `index` is
  refused: it is the index's name), deletes the shards that are gone, then the
  index; a call that changes nothing writes nothing. `Reader.Index` and
  `Reader.LatestShard` read through the same ETag cache as `Latest`. The Cluster's
  summary of the kind (`status.results`, kind `nodes`) names the index key and
  ETag, the total node count, completeness and the digest of all shards.
  `resultsink.StageSharded` is `Stage` for it (notices: "the node list is
  unavailable: ...").
- **Node rows, collected by the applier.** `pkg/reconcile/cluster` `readNodes`
  builds a row per node during the paginated Node and NodeClaim reads it already
  makes (label `karpenter.sh/nodepool` for Karpenter nodes; the default managed
  group's nodes, by `eks.amazonaws.com/nodegroup`, are group `system`), groups
  them, and `writeNodeRows` writes the shards through the sink at most once a
  minute per cluster (in memory) and only when the digest changed; an unchanged
  collection moves the summary's time only every ten minutes. Only counts stay in
  `status.nodeGroups`; a `system` status appears when its nodes were read. A
  failed or unfinished read writes what it read marked incomplete (nothing at all
  when nothing was read, so the last list stays). Without S3 or a ready bucket
  there are no rows: the counts stay and `NodeGroupStatus.nodes_notice` says why.
- **ListNodeGroupNodes.** `tenant_id, cluster_id, node_group, page_size` (default
  100, at most 500), `page_token`, `filter` (all/ready/not ready), `name_contains`
  returns `nodes, next_page_token, total, collected_at, complete, notices`. It reads
  the index, then only that group's shard (cached by ETag); the token binds to the
  shard's ETag, the filter and the search, so a new collection refuses an old token
  (InvalidArgument: start again). A bucket that cannot be read, or nothing collected,
  is a notice. A group the cluster does not have is NotFound; `system` and `platform`
  always exist.
- **Recommendation history** is written at most once an hour per cluster
  (`results.WithoutHistory` for the other writes; `latest` still follows every
  change), which bounds the 30-day history to about 720 objects a cluster.
- Not covered without a real AWS account: the tenant role's `s3:*` policy on
  `cv-*`, `AccessDenied` wording, the provider's own `BucketNotEmpty` retry,
  and Karpenter-less timing of the Bucket's `Ready`.

### Still open

- Gates that read removed APIs in use (`pkg/upgrade/gates.go`) keep reading
  the counts in `status.deprecations` (`SetResultRef` drops its findings,
  not its counts); the same counts are in the summary (`Removed`).
- Existing accounts: update the role's stack with `ServerPrincipalArn` and
  the new `provision-results` policy.
- Recommendations: one history object an hour, in memory (a restart writes one
  more); `latest` is still written on every change.

## Scale

Target: 1,000 nodes and 10,000 pods per cluster, 100 clusters. Nothing is
loaded from a workload cluster in an API call; the controller collects
during reconcile and writes to S3; the API reads S3, one bounded object or
page at a time.

| Rule | How |
|---|---|
| Collect with pagination | Every list against a workload cluster uses `limit` (500) and `continue`, with label/field selectors and only the fields needed (no full objects kept); a per-pass deadline stops a collection cleanly and marks the result incomplete. |
| No CR-era caps when writing to S3 | The 200-row / 128 KiB caps exist only for what is kept in the CR. Results written to S3 are bounded by the object size (16 MiB uncompressed per object) instead: up to ~50,000 findings or 10,000 nodes per object. |
| Shard large kinds | Kinds that grow with the cluster are sharded: nodes one object per node group (`latest/nodes/<group>.json`), recommendations one object per namespace past a threshold, with a small index object (`latest/<kind>/index.json`: shard keys, counts, ETags). The API reads the index, then only the shard a page needs. |
| Write only on change, at a bounded rate | A digest of the collected content is compared with the last write; unchanged results are not rewritten. Writes per cluster are at most one per kind per minute (nodes) or per scan interval (others). |
| CR holds counts only | The Cluster status keeps per-kind summaries (counts by phase/severity, collected time, completeness, index key and ETag) — never per-node or per-finding lists. |
| API pagination | Page size ≤ 500; tokens bind to the shard's ETag; the server keeps an ETag-keyed LRU of decoded shards (bounded memory), so paging through 1,000 nodes is a handful of S3 GETs, revalidated with If-None-Match. |
| Node group status | The applier counts nodes and NodeClaims with paginated lists (no 500-item cap), stores counts and phases in the CR, and writes the per-node rows (name, instance type, zone, capacity type, kubelet version, Ready and pressure conditions, taints count, age, NodeClaim phase) to the `nodes` shard of its group. |
