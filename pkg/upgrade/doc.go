// Package upgrade decides automatic Kubernetes upgrades of clusters that
// follow a release channel. It is pure: it reads no cluster, store or
// cloud; the controller gathers the facts and acts on the answers.
//
// Three questions, three functions:
//
//   - NextHop: which minor a cluster moves to next. Each channel offers one
//     Kubernetes minor (written per minor in the release catalog). A cluster
//     below its channel's minor moves one minor at a time, because EKS
//     upgrades one minor at a time: a beta cluster on 1.35 with beta at 1.37
//     goes to 1.36 first, then to 1.37. A cluster at its channel's minor
//     stays; one ahead of it (it changed channel) is never downgraded and
//     waits for the channel to catch up.
//
//   - Check: whether this cluster may start that move now. Every reason it
//     may not is a blocking Gate with a message for the cluster's users:
//     manual upgrades, a failed earlier upgrade, an upgrade already
//     running, paused, on hold, not Ready, a platform component not Ready,
//     no deprecation scan for the target minor or removed APIs still in
//     use, outside the maintenance window. Notifying users before an
//     upgrade is not a gate yet: there is no notification system.
//
//   - Plan: which clusters of a channel start now. Tier 2 (least
//     sensitive) goes first, then tier 1, then tier 0. Within a tier at
//     most Policy.MaxInProgress clusters upgrade at once (tier 0: one), the
//     next start as earlier ones finish. A tier is finished when none of
//     its clusters is waiting or upgrading; the next tier starts after the
//     soak of the finished one (Policy.SoakPerTier, else Policy.Soak),
//     counted from its last cluster's end. More than Policy.MaxFailed
//     failed clusters in a tier halt the channel: nothing new starts.
//
// A cluster that its own gates block (manual, window closed, removed APIs
// in use, ...) does not hold up its tier or the next one: it is counted as
// Blocked and starts when its gate clears, even if a later tier is running
// by then. Only waiting, upgrading and failed clusters hold a rollout back.
package upgrade
