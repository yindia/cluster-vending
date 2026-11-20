// Package helmrel is the controller's side of reliable Helm installs and
// upgrades (docs/design/helm-reliability.md): what it knows about a
// provider-helm Release without touching a cluster. The digest of a
// Release's spec, the class of its errors, the backoff between attempts and
// the state machine that turns what a Release shows into "pause it",
// "unpause it" or "give up". It does no I/O: callers read the Release, ask
// Step what to do and do it.
package helmrel
