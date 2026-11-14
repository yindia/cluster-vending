# Components: the `component` command

A component is something the platform runs on a cluster from a Helm chart:
the AWS Load Balancer Controller, Karpenter, Kyverno, the GPU operator. The
platform team keeps the list (the catalog, [catalog.md](catalog.md)); this
page is how a tenant reads it and turns components on.

> **How it gets installed.** `enable` checks and stores the component,
> `Pending`. The cluster's next pass installs it, after the platform's own
> components: it waits for what it requires (`kyverno-policies` for the
> platform's `kyverno`), then its chart is installed at the version of
> the cluster's release, with your values under the platform's. Its
> status says `Installing`, then `Ready` or `Failed` with why. See
> [how it works](#how-a-component-is-installed).

## Who manages a component

| Managed by | What it means | The tenant can |
|---|---|---|
| platform | Installed with every cluster, with fixed values. | see it, nothing else |
| tenant | Enabled per cluster by the tenant. | enable, change its values, disable |

Enabling, changing or disabling a platform component is refused:
`permission_denied: component karpenter is installed with the cluster by
the platform; it cannot be enabled, changed or disabled`.

A component's version is never the tenant's choice: it is the one the
cluster's release gives it. There is no `--version`.

## The flow

```sh
# 1. What is there
cluster-vending component catalog --tenant acme
cluster-vending component catalog --tenant acme --cluster <cluster-id>   # with the versions that cluster would get

# 2. The values file to start from (YAML, with the platform's comments)
cluster-vending component values kyverno-policies > policies.yaml
$EDITOR policies.yaml

# 3. Check, then enable
cluster-vending component enable kyverno-policies --cluster <cluster-id> --tenant acme -f policies.yaml --validate-only
cluster-vending component enable kyverno-policies --cluster <cluster-id> --tenant acme -f policies.yaml

# 4. What is on the cluster
cluster-vending component list --cluster <cluster-id> --tenant acme

# 5. Change it later, then remove it
cluster-vending component edit <component-id> --tenant acme
cluster-vending component disable <component-id> --tenant acme
```

`enable` prints the component with its `id`. Every later command takes
that ID, as network and cluster commands do; `list` shows it.

## Commands

All take `--tenant` (except `values`), `--server` and `-o yaml|json`;
`catalog` and `list` print a table unless `-o` says otherwise.

| Command | What it does |
|---|---|
| `component catalog [--cluster <cluster-id>]` | Every component: name, who manages it, whether it takes values, what it requires. With `--cluster`, the version that cluster's release gives each. A bundle (Istio) is one entry; its parts are not listed. |
| `component values <name>` | Prints the component's values file as the platform wrote it. Refused for a component that takes no values. |
| `component enable <name> --cluster <cluster-id> [-f values.yaml] [--validate-only] [--wait]` | Enables the component on the cluster. Without `-f` it runs with the defaults. Running the same command again returns the same component. |
| `component update <id> -f values.yaml [--validate-only] [--etag <etag>] [--wait]` | Replaces the component's values with the file: a key left out goes back to its default. |
| `component edit <id>` | Opens the stored values in `$VISUAL`, else `$EDITOR`, else `vi`, and stores them when they changed. With no stored values it opens the commented values file. |
| `component get <id> [--values]` | Shows the component. `--values` prints only its stored values, as YAML to edit and send back with `update -f`. |
| `component disable <id> [--etag <etag>] [--wait]` | Removes the component from its cluster. Refused while another enabled component requires it. |
| `component list [--cluster <cluster-id>]` | With `--cluster`: everything on the cluster, by name. Without: the tenant's components on all its clusters, a page at a time. |

### `catalog`

```
NAME                          MANAGED BY  CONFIGURABLE  REQUIRES  DESCRIPTION
aws-load-balancer-controller  platform    no            -         Provisions ALBs and NLBs.
gpu-operator                  tenant      yes           -         NVIDIA GPU operator.
kyverno                       platform    no            -         Policy engine.
kyverno-policies              tenant      yes           kyverno   Pod Security Standards as Kyverno policies.
```

### `list --cluster`

Three kinds of rows: what the platform installed (no ID, read-only), what
the tenant enabled, and what the tenant could enable (`not enabled`).

```
ID                                    NAME              MANAGED BY  VERSION  PHASE    SINCE                 MESSAGE
-                                     karpenter         platform    1.15.1   Ready    2026-11-13T04:00:00Z  -
-                                     kyverno           platform    3.10.0   Ready    2026-11-13T04:02:10Z  -
01a1d0a7-0000-7000-8000-000000000001  kyverno-policies  tenant      3.10.0   Pending  2026-11-13T05:10:00Z  accepted; the platform installs it with the cluster's next pass
-                                     gpu-operator      tenant      -        -        -                     not enabled
```

Why a platform component is not ready (its chart, Helm's state, the last
error) is `cluster components <cluster-id>`.

## Values

- **Only your edits are stored.** A key you leave as it is in the values
  file is dropped before the component is stored, so a later change to the
  platform's file still reaches you. `--validate-only` shows what would be
  stored:

  ```
  valid. 1 value differs from the defaults and would be stored:
    validationFailureAction: Enforce
  ```

- **An update replaces.** `update -f` and `edit` send the whole set of your
  values: a key that is no longer in the file goes back to its default.
- **What is refused**, with every problem in one answer:

  ```
  invalid_argument: only the platform sets failurePolicy; replicas: is not an accepted value
  ```

  A key the platform set (`failurePolicy` here) is locked; anything else
  must be allowed by the component's schema.

### `edit`

1. The editor opens on your stored values, or, when you stored none, on
   the component's values file with its comments.
2. Save and close. Nothing changed: `no changes`. Else the values are
   checked, then stored.
3. Refused: the reason is printed and, on a terminal, you are asked `Edit
   again? [Y/n]`; the editor opens on what you typed. Off a terminal (a
   pipe, CI) the command ends with the error.

The file is a temporary one only you can read, removed when the command
ends. The editor is the program in your own `$VISUAL` or `$EDITOR`, run
without a shell (`code --wait` works).

## Other answers

| Answer | Why |
|---|---|
| `not_found: component x is not in the catalog` | No such component, or its definition is not valid. |
| `failed_precondition: enable kyverno first` | It requires a component that is not on the cluster. A platform component always is. |
| `failed_precondition: the cluster is PROVISIONING; enable components once it is RUNNING` | |
| `failed_precondition: x is not offered on Kubernetes 1.37 in release r` | The cluster's release has no version for it. |
| `failed_precondition: remove the cert-manager add-on first: it conflicts with component x` | |
| `already_exists: component x is already enabled on cluster c` | One of each per cluster. |
| `invalid_argument: version cannot be set` | The version comes from the cluster's release. |
| `aborted: component ... changed since it was read` | The `--etag` you gave is not the current one. |

## How a component is installed

| Step | What happens |
|---|---|
| `enable` | Checked and stored `Pending`. On the cluster's next pass it is planned like a platform component: the release's version and locked values, your values under the platform's. It waits for what it requires, then its IAM role (if any) and its chart are applied: `Installing`, then `Ready` once Helm reports the chart deployed. |
| `update` | New values make a new generation; the next pass upgrades the chart, and `status.observedGeneration` follows. The same values again change nothing. |
| `disable` | Marked `Deleting`. The next passes uninstall its chart, then remove its role, then the component. A component another enabled one requires cannot be disabled. |
| cluster delete | Every tenant component is removed first, then the platform's, then the cluster. |

- **`--wait`** follows the component until it is `Ready` (or `Failed`,
  an error), or gone after a `disable`.
- **Nothing is installed** while the cluster is imported and not taken
  over, taken over with the platform components kept off, paused, or not
  running yet: the component stays `Pending` and says why.
- **A stuck Helm release** is recovered as a platform component's is
  (at most three times per version; `status.recoveries`).
- **Ready means Helm finished**, as for platform components: that the
  workloads run is not checked.
- **Placeholders** in `status.effectiveValues` (`${cluster.name}`) are
  shown as written; they are filled in in what is installed.

The design, the checks and the API are in
[design/component-usage.md](design/component-usage.md).
