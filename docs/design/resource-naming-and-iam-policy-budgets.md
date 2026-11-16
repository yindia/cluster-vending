# Platform resource naming, cluster-name limits and cloud uniqueness

Status: proposed implementation plan. This document does not change runtime naming, rename existing resources, or apply infrastructure changes.

## Problem and evidence

The Karpenter managed policy for `inference-ap-northeast-1-dev` expanded to 6,153 characters, exceeding IAM's 6,144-character document limit. AWS rejected the policy, so the dependent RolePolicyAttachment was never created. Removing optional IAM statement IDs reduces this instance to 5,479 characters without changing its permissions. That is an immediate repair, not a general capacity strategy.

The suffix on the Policy resource name is not itself counted in its policy document. The document grows because the real cluster name appears repeatedly in tag condition keys, tag values, EKS ARNs, and generated role ARNs. A short policy object name alone cannot solve this problem.

Current source has partial protection:

- `pkg/crossplane/aws/network.go`: `NetworkPrefix` returns the complete human name plus an eight-hex-character identity hash. `ClusterPrefix` delegates to it.
- `pkg/crossplane/aws/component_iam.go`: component IAM bases truncate and hash against a 64-character role-name budget. Attachments and policies then append different suffixes to that base.
- `pkg/crossplane/aws/platform_components.go` and `usage.go`: Helm managed objects and Usage objects have separate truncation/hash helpers.
- `pkg/component/cloud.go`: `ValidateCloudResources` checks the policy template's size. `ExpandPolicy` substitutes real values but does not recheck the resulting size.
- `pkg/component/platform.go`: `planIAM` forwards that expanded policy to the renderer.

These are two separate budgets: the external name of a resource and the content of a generated document. Both need validation before a managed-resource write.

## Decision

Adopt a **32-character maximum for newly created cluster names**, short resource-purpose suffixes, one naming allocator used by every generated resource, and an atomic account-wide cluster-name reservation. These rules apply to cluster infrastructure, add-ons, components, Helm resources, access resources and supporting Kubernetes objects. Network-owned resources use the same allocator with their own name budget. IAM document sizing is an additional check, not the scope of this proposal.

The existing API/CRD maximum is 40 characters. Reducing it to 32 is a proposed platform contract, not an AWS EKS restriction. Pairing that limit with bounded suffixes makes infrastructure names predictable. A lower parent-name limit alone still cannot guarantee arbitrary future component suffixes, Helm-generated child names or IAM document sizes; final rendered validation remains mandatory.

Do not replace the real EKS name with a shortened alias inside discovery tags, IAM conditions, or cluster ARNs: those values must match the actual cluster. Do not add wildcard permissions to make a document smaller.

## Cluster-name contract and the length calculation

For new cluster creation, require 1–32 ASCII characters, starting with a lowercase letter, containing lowercase letters, digits and hyphens, and ending with a letter or digit. Keep `displayName` independently mutable at its existing 128-character limit. Cluster name remains immutable.

For a common generated-name profile with a 63-character bound:

```text
<cluster hint, up to 32>-<96-bit identity token, 20>-<purpose, up to 9>
32 + 1 + 20 + 1 + 9 = 63
```

This is a platform budget, not a claim that all Kubernetes or AWS resources have a 63-character limit. A tighter profile reserves the same identity token and purpose, then shortens the cluster hint further. The real EKS name remains the full accepted cluster name. Fixed paths, compound external IDs and tags receive their own validation.

The 32-character recommendation is conditional on the complete render audit below passing. Before enabling the contract, CI must render every supported kind and chart at maximum input lengths and verify its real constraints. If a chart cannot accept a bounded release/fullname override, give it a more compact profile or reject that catalog entry; do not silently shorten the real EKS name or shrink the global limit for one uncontrolled chart.

Use a registry of short purpose codes instead of concatenated descriptions. Examples: `cp-r` for control-plane role, `node-r` for node role, `ng` for default node group, `lt` for launch template, `kc` for kubeconfig, `oidc`, `kms`, `logs`, `karp-r` and `karp-p`. These are proposed codes, not changes to existing names. Component/group identity is part of the token, so short codes do not have to carry a complete component name. Validate registry collisions and bound user/catalog-supplied logical keys through the allocator.

Audit and shorten nested suffix chains such as `access-<principal hash>-<policy hash>` using one token over the entire semantic identity. Do not stack two or three unrelated hashes and descriptive suffixes. Preserve separate names for distinct access scopes, revisions and ownership boundaries.

## Account-wide duplicate prevention

Current management storage only guarantees a unique Cluster object name within a tenant namespace. Its `clusterKeys` reserve request IDs, not account/name pairs. Two tenants connected to the same AWS account can therefore bypass tenant-local uniqueness. A list-then-create lookup cannot fix concurrent requests.

Adopt the requested stronger platform rule: **a cluster name is unique throughout the canonical cloud account**, across tenants, network selections, account registrations and regions. AWS itself permits reuse across regions; the account-wide rule is an intentional platform policy. Preserve the existing tenant-local uniqueness rule for now, since Cluster object names and several lookup paths rely on it. Relaxing that existing restriction would be a separate identity migration.

1. Resolve the network's account to a verified provider identity, such as AWS partition plus the actual 12-digit account ID. Never key uniqueness by a tenant's CloudAccount CR UUID, display name or credential alias. Two registrations pointing to the same account must share the reservation scope.
2. Reserve `(provider, partition, canonical account identity, normalized cluster name)` atomically in a platform-owned management namespace using a Kubernetes object create. Hash the canonical tuple into its object name, retain the full tuple for collision checks, and record tenant, cluster UUID, request UUID, lifecycle phase and immutable owner UID once created. No database is needed. Reuse the existing claim concept, but not its tenant-local namespace or age-only abandonment behavior.
3. A competing create/import gets `ALREADY_EXISTS` with an account-scoped message. Respect tenant authorization: do not reveal the other tenant's name, cluster ID or resources. Replaying the same request resumes its reservation safely; reusing the request ID with different input is rejected.
4. Keep the reservation through provisioning failure, deletion and asynchronous AWS cleanup. Release it only after the resource's absence and cleanup are confirmed. A timeout alone does not prove a name is free. A crash after reservation but before CR creation must be recoverable using recorded intent and conditional updates, not by racing another holder.
5. Create, import, takeover, native CR reconciliation and retry paths must enforce the same rule before cloud writes. Direct Kubernetes users can submit intent, but cannot bypass provisioning ownership checks. Use admission validation for immediate feedback where feasible; do not claim a list-based webhook offers atomic cross-object uniqueness. Restrict manual claim writes through RBAC.
6. Backfill reservations for existing managed and imported clusters first. Detect and report existing cross-region duplicates or duplicate registrations; grandfather existing owners without renaming or deleting them, and forbid new competing allocations. Externally created AWS clusters require discovery in the requested region and, to enforce the stronger whole-account policy, an inventory across the account's enabled/accessible regions. Report inaccessible regions and fail closed for an unverified new allocation. External actors can still race the platform: handle provider conflicts without adoption.

Examples: same account/name under two tenants is rejected; same account/name in another region is rejected by platform policy; the same name in a different account can be accepted under different tenants. Within one tenant, the existing management-object restriction still rejects duplicate names even across accounts.

## 1. Allocate bounded resource names

Introduce a provider-neutral identity/naming package, with AWS and Kubernetes constraint profiles registered by their adapters. It must distinguish:

1. User-facing display name: unrestricted by the shortest child-resource budget.
2. Actual external identity: EKS name, IAM role name, policy name, and similar provider identifiers.
3. Management object name: Kubernetes object identity, which can differ from the external name.

For new generated child resources, allocate a compact name from an immutable resource ID and a semantic role. The format is `<hint>-<identity>-<purpose>` with optional omission of the hint for tighter constraints. The hint is captured at allocation and never recalculated from a later display-name edit. The identity token is derived from the complete immutable owner ID, provider scope, resource kind, component, and logical resource key using a canonical, length-delimited encoding.

Use a 96-bit token encoded as 20 lowercase base32 characters for the initial profile, rather than eight hex characters. At 100,000 distinct allocations the approximate birthday collision probability for 96 bits is 6.3e-20. This is collision resistance, not a uniqueness proof: conflicting ownership must still fail closed. Never adopt an existing resource merely because its shortened name matches.

Allocate each final name with its own constraint profile. Reserve delimiters, token, purpose, and any revision suffix before truncating the hint. Do not generate an unconstrained base and keep appending suffixes. Validate the completed name's allowed characters, length, scope, and reserved prefixes. Truncate ASCII-normalized hints, not arbitrary UTF-8 bytes.

Initial constraint registry:

| Resource | Constraint to encode | Scope / handling |
| --- | --- | --- |
| IAM Role | 64 characters and IAM character rules | Account-wide, including cross-region allocations; case-insensitive uniqueness |
| Customer managed Policy | 128 characters and IAM character rules | Account-wide identity; separately budget document content |
| InstanceProfile | 128 characters and IAM character rules | Account-wide identity |
| Kubernetes managed object | Actual CRD/name constraints | Namespace or cluster scope; do not assume every object is a 63-character DNS label |
| Helm release | Chart/release-name constraints | Separate Helm release identity from its management object's name |
| Other generated AWS resources | Provider-specific limits | Audit node groups, launch templates, log groups, aliases, networking and access resources before enabling the profile |

The audit must include every object returned by `ClusterResources`, `NetworkResources`, `ComponentIAMResources`, platform/tenant Helm rendering and import/takeover rendering. Check management names, provider `forProvider` names, external-name annotations, tag keys/values, label values, Secret/ProviderConfig references and chart-generated children separately. Include EKS add-ons, Pod Identity associations, SG/rules, routes/associations, NAT/EIP/subnets, flow logs, KMS aliases, log groups, access entries/policy associations, Usages and cleanup Jobs. Not every one has a user-selected AWS name; validate the identity representation it actually uses.

Preserve externally required names such as `/aws/eks/<actual-cluster-name>/cluster` and imported names. Validate them; do not silently transform them. A typed allocation map must feed all references, policy variables, inventory, Usage dependencies, import/takeover paths, and deletion logic. No caller should reconstruct identities by string concatenation or prefix stripping.

Store the immutable naming profile on the owning management CR at creation. Missing profile means legacy naming, including objects without a profile that have not yet provisioned. Persist exceptional/imported external identities in the existing Kubernetes inventory. Use no database. A renderer-version increase must not implicitly change the naming profile.

## 2. Compile and validate policies after expansion

Create one policy compilation path for platform and tenant-managed components:

1. Parse and validate the identity-policy document and supported placeholders.
2. Expand real cluster names, provider partition, account, region, OIDC identities, and allocated role ARNs. Unresolved late-bound values remain Pending; they must not be assumed to fit.
3. Serialize compact JSON while preserving statement semantics. Offer optional Sid omission for platform-owned IAM identity policies; do not apply this blindly to resource policies or externally supplied documents.
4. Check the exact expanded document against IAM's character set and size rules. JSON serialization must preserve whitespace inside string values; do not implement minification with a whitespace-removal regular expression. Qualify counting against AWS, including escaping and non-ASCII input.
5. Validate the role trust document separately against its own effective quota, including OIDC host/path and service-account lists.
6. Calculate per-role attachment counts and account-wide managed-policy requirements, including imported/external attachments and temporary rollout overlap where observable.

AWS's hard managed-policy limit is 6,144 characters. Use a proposed 5,500-character catalog target to retain headroom; this target is internal policy, not an AWS limit. Catalog CI should require an explicit reviewed exception for a supported worst-case expansion above the target. Runtime may accept documents up to and including 6,144, but must reject larger documents before creating or updating their managed resources.

Use the same pure validation functions in API create/update/plan and controller reconciliation. Check what is known synchronously; clearly report deferred checks that require observed identities. A rejected component plan must not write a subset of its IAM changes. Do not block observation or unrelated healthy components.

Example diagnostic:

```text
IAMPolicyTooLarge: component karpenter, group controller,
expanded size 6153, limit 6144.
Policy cannot be applied. Publish smaller named policy groups.
```

## 3. Support stable, explicitly named policy groups

Extend component cloud-resource definitions with an optional collection of `{name, document}` managed policies. Illustrative names are `compute`, `instance-profile`, and `discovery`; final group boundaries must follow a permissions review. Preserve the existing singular `policy` field as a backward-compatible logical group. Reject ambiguous simultaneous singular/plural input unless the conversion contract explicitly defines it.

Each named group becomes a separately validated managed Policy and RolePolicyAttachment. Group names are immutable logical keys used for inventory, naming, references, and deletion dependencies. Never identify a group by list position, current byte size, or a hash of its entire document: ordinary permission updates should not rename it.

Prefer these reviewed groups over runtime automatic packing. Packing can reshuffle statements between policies when a placeholder grows, creating attachment churn and unsafe transitions. If one named group's expanded document still exceeds the hard limit, fail clearly; never split an individual statement or weaken its conditions automatically.

For Karpenter, begin with compact statements and use CI to determine whether the 5,500-character worst-case target requires multiple groups. Keep statement permissions intact, including explicit Deny semantics and policy-level fields. Verify the union of generated statements against the reviewed source document; do not infer equivalence merely from a matching action list.

Before an attachment-count-changing rollout, evaluate the effective quota and all existing attachments. Current AWS documentation lists 20 managed policies per role by default, adjustable to 25; do not hard-code the historical default of 10. Quota reads may be unavailable: report that uncertainty rather than claiming capacity is verified. At account scale, splitting policies multiplies managed-policy consumption across clusters. Default to per-cluster ownership; shared policies require a separate lifecycle/security design.

Migration of an existing single policy to groups must preserve the current role identity. Pin the old group until every replacement Policy and attachment is Ready, then retire the old attachment and policy through explicit dependency ordering. During additive migrations, overlap must not introduce permissions beyond the reviewed target. Permission revocations require an explicit staged transition and plan because a broad old policy would continue to authorize actions during overlap. If overlap cannot fit the effective quota, stop with a migration plan rather than detaching working permissions first.

## 4. Preserve existing installations and rollout gates

- Existing clusters, networks, adopted resources, and managed object names keep the legacy profile. No automatic resource replacement on controller upgrade.
- Existing cluster names of 33–40 characters remain valid for get/update/delete and unchanged-name reconciliation. The new 32-character maximum applies to new create requests. Do not simply tighten the top-level CRD CEL rule and make old resources impossible to update: define transition-aware validation or a compatible API version and test metadata/finalizer/status operations on grandfathered objects. Import of a longer existing name needs an explicit legacy/import contract, not silent truncation.
- New resources on a legacy owner continue to use its recorded profile. A move to the new naming profile is an explicit migration with a complete replacement/adoption plan.
- Naming profile and policy grouping are separate decisions. Existing clusters can receive smaller or grouped policies without renaming their EKS cluster or role.
- Bind catalog/renderer changes to an immutable release artifact. The current release content pins component versions/values but must also identify the IAM definition/compiler revision that produced cloud-resource plans. Merely updating a mutable ComponentDefinition or bumping the controller binary is not sufficient release isolation.
- Follow existing channel, network tier, maintenance, freeze, and rollout gates. Show create/update/replace/delete and temporary attachment changes in plan output.
- Reconcile from the pinned definition/compiler contract, not the newest embedded source. An older supported release must remain reproducible while a newer controller is running.
- Unsupported pinned profiles produce an explicit compatibility condition; never silently substitute new identities or policy rules.

## 5. Status and operations

Surface the first failed dependency on its owning component, as implemented for the present Karpenter diagnostic. Distinguish invalid rendered plans from AWS-reported reconciliation errors. Emit events on transitions, not every retry. Include resource kind/name, measured budget, limit and remediation without dumping full policy documents or credentials.

Add bounded-label metrics for validation failures by provider/resource kind/reason and aggregate budget utilization. Do not put cluster IDs, names, policy contents or ARNs into metric labels. UI/CLI should display the provider's root cause and the same plan diagnostics.

## Delivery order and reviewable results

1. **Complete naming audit:** enumerate every rendered identity and chart child, shorten the purpose registry, and establish passing length/charset/reference tests at the proposed 32-character cluster limit.
2. **Creation contract and allocator:** enforce the new limit consistently in IDL/RPC/CLI/UI/native CR handling, preserve existing longer names, and implement per-kind allocation behind an immutable naming profile. Prove legacy plans are byte-for-byte stable.
3. **Account reservations:** backfill existing inventory, handle duplicates explicitly, then enable atomic reservations on all provisioning/import/takeover paths with concurrency and recovery tests.
4. **Policy budgets and named groups:** implement post-expansion validation; update CRD/IDL/Go/TypeScript contracts where exposed, plan output, compiler, renderer, inventory and deletion ordering. Convert Karpenter if its reviewed budget requires it. Retain the current Sid-only repair as a narrow fix.
5. **Release contract:** pin naming/compiler/catalog revisions and validate a mixed fleet of legacy/new resources on different release channels. Complete this before enabling changed semantics on real clusters.
6. **Canary rollout:** exercise new allocations and policy migrations in an isolated account, then normal tier/channel rollout. Rollback keeps allocated identities; assess IAM permission differences explicitly before reverting documents.

## Acceptance tests

- Minimum and maximum allowed cluster/component names; identical truncated prefixes; multiple tenants, accounts, regions and provider partitions; repeated reconcile/restart; display-name changes.
- New names of 32 characters succeed and 33 characters fail consistently through UI, CLI, RPC and native provisioning. Existing 33–40-character names still permit normal update, freeze, finalizer/status patches and deletion.
- Concurrent same-account/name creates across tenants/regions/duplicate account registrations have one owner. Same request retries, interrupted creates, import/takeover, delete-and-recreate, unauthorized conflict inspection and inaccessible-region discovery behave as specified.
- Every final name fits its actual constraint; semantic references resolve; ownership collisions fail closed; imported names are preserved; legacy golden plans are unchanged.
- Expanded managed policies at 6,143, 6,144 and 6,145 characters; large node-role ARNs and IAM paths; long OIDC issuers/service-account sets; escaped values and meaningful whitespace in JSON strings.
- All catalog policies expanded against each supported maximum-length variable, not just Karpenter. CI fails before shipping an unsupported budget.
- Named groups remain stable on reordering, insertion and permission edits; their permission union matches the reviewed source; no automatic statement fragmentation or wildcard broadening.
- Migration under insufficient attachment/account quota, AWS partial failure, restart, deletion, freeze, and rollback. Existing access remains available only where consistent with the approved permission transition.
- Frozen resources receive no new writes, old pinned releases remain reproducible, and plan output reports all replacements and removal actions.

## Sources

Verified 2026-11-16:

- [AWS IAM and STS quotas](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_iam-quotas.html): document/name limits, attachment and account quotas, and whitespace rules.
- [IAM Sid element](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_elements_sid.html): optional identifiers for IAM statements; service-specific resource policies can differ.
- [IAM policy validation checks](https://docs.aws.amazon.com/IAM/latest/UserGuide/access-analyzer-reference-policy-checks.html): policy-size diagnostics and validation guidance.
- [EKS CreateCluster API](https://docs.aws.amazon.com/eks/latest/APIReference/API_CreateCluster.html): AWS's own name limit is 100 characters, with uniqueness scoped to account and region; the proposed 32-character/account-wide rules are stricter platform policy.
