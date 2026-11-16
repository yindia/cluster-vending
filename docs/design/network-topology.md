# Network architecture diagram

The network detail page uses the existing React Flow renderer and Astryx theme.
No cloud infrastructure, Crossplane composition or provisioning behavior is
changed. Kubernetes-backed, tenant-scoped observations remain the source of truth.

## Pipeline

1. `pkg/reconcile/network/architecture.go` projects an allowlist of network-owned
   Crossplane facts into generated `NetworkStatus.architecture` contracts. This is
   a read path, with no AWS SDK calls or writes. It includes explicit references,
   observed cloud IDs, route table inline routes, destinations and route targets.
2. `network-infrastructure.ts` validates endpoints, deduplicates evidence and
   resolves canonical AWS IDs (managed object IDs for pending or ambiguous IDs).
3. `network-architecture-graph.ts` constructs semantic nodes and typed
   relationships: `contains`, `associated-with`, `routes-to`, `attached-to`, and
   `uses-network`. Containment is drawn exclusively as nesting. Complete route
   and association objects remain inspectable even when not individual cards.
4. `network-architecture-layout.ts` calculates hierarchical geometry from child
   dimensions and connection counts. The scoped AWS account contains Region; Region contains VPC; VPC contains AZs;
   AZs contain subnets; only explicit subnet placement puts a NAT in a subnet.
   Route tables remain VPC children in lanes alongside associated subnet rows.
   Shared tables use the median associated AZ lane and are never duplicated.
5. `network-edge-routing.ts` assigns deterministic ports and routes orthogonal
   connectors around cards, headers and unrelated containers. Labels reserve
   their own horizontal path segments, with measured text boxes. Empty association
   labels reserve no space. Routing failures are explicit diagnostics, not silently
   missing relationships or fallback lines through resources.

ELK layered layout was evaluated against the required compound hierarchy and
fixed-port support ([ELK documentation](https://eclipse.dev/elk/reference/algorithms/org-eclipse-elk-layered.html)).
Unrestricted graph layering conflicts with AWS placement when private route tables
point into public-subnet NATs, which themselves use public route tables. This
implementation retains topology-aware hierarchical packing, association-based
ordering and a separate obstacle-aware rectilinear router. Spacing is a shared
layout policy driven by dimensions and edge degree, not topology-specific offsets.

## Evidence and accuracy

Configured and observed relationships remain distinct when they disagree. A
configured route is not proof of reachability. Route composition must not pair an
observed table with an unrelated configured target during drift. Unknown placement
is shown as unknown, not guessed from names or AZ proximity.

Explicit endpoint, peering and transit-gateway IDs outside the owned snapshot are
marked `reference_only`. Such targets have no inferred readiness, location or
ownership. Local routes are drawn only when the provider reports a local gateway;
VPC CIDR alone is insufficient. Non-default CIDRs and prefix lists are retained.

A logical NAT-to-IGW path requires all three facts: the NAT's actual public subnet,
that subnet's associated route table, and its default route to the IGW. A private
subnet can target any reported NAT, including another AZ. Cross-AZ dependencies
are marked separately from resource health.

## Compact presentation and inspection

- AWS account, Region, VPC, AZ and public/private subnet boundaries establish location.
  Account name and its explicit 12-digit cloud ID identify ownership; the ID is
  copyable. Account context is omitted when tenant/bootstrap ownership does not match.
  Colored headers fill their container; measured title bounds reserve safe routing
  space. Content dimensions and connection counts determine compact spacing.
- The duplicate topology inventory table is removed; Resources remains the full
  inventory. Resource names expose their full value on hover and inspection.
- Logical NAT-to-IGW paths appear on selection, avoiding duplicate default paths.
- An optional security overlay reports explicit public-IP associations, IGW routes,
  cross-AZ dependencies and incomplete route observations. It does not equate an
  IGW route with public exposure or claim to evaluate unreported SG/NACL rules.
- Route tables are adjacent to their users, outside subnet and AZ containers.
- Association links are subtle, dashed and unlabeled; route links are directional
  and destination-labeled. Logical egress is distinguished from a provider reference.
- The default view focuses on internet and external destinations. Local VPC
  routes remain in every table inspector; no local-route control is shown.
- Equivalent table/target/destination/evidence paths share one visible connection;
  full route identities remain in the underlying graph and inspector.
- Elastic IPs with one unambiguous NAT owner are compact inspectable attachments.
  Their cloud and managed identities remain independent. Ambiguous allocations
  remain standalone; no duplicate address is fabricated.
- Associated cluster links are in the header, not disconnected cards below the
  region. No node-group placement is inferred from network membership.
- Selecting a subnet traces its table and actual egress. Selecting a table shows
  its associated subnets; selecting a NAT shows every table that targets it.
  The inspector retains destination, evidence, identifier and status details.
- Pan, zoom, fit and fullscreen remain available. Large networks prioritize
  readable cards over fitting the entire graph at an unreadable scale.

## Validation

`network-topology-fixtures.ts` generates isolated synthetic protobuf fixtures A–I:
2/1, 2/2, 3/1, 3/3, 3/2 mixed, no NAT, multiple private subnets, shared route table,
and partial provisioning. No fixture is installed in a live API or cluster.

Model and independent geometry tests check actual targets, containment, deduplication,
stable ordering, parent bounds, sibling collisions, port geometry, card/container
intersections and label collisions. Browser tests use the existing server with
mocked read-only RPCs to exercise inspection, headers, fullscreen, theme and mobile
behavior and capture representative screenshots for visual inspection.
