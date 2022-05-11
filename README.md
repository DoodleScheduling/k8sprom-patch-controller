# k8sprom-patch-controller

[![CII Best Practices](https://bestpractices.coreinfrastructure.org/projects/5862/badge)](https://bestpractices.coreinfrastructure.org/projects/5862)
[![e2e](https://github.com/DoodleScheduling/k8sprom-patch-controller/workflows/e2e/badge.svg)](https://github.com/DoodleScheduling/k8sprom-patch-controller/actions)
[![report](https://goreportcard.com/badge/github.com/DoodleScheduling/k8sprom-patch-controller)](https://goreportcard.com/report/github.com/DoodleScheduling/k8sprom-patch-controller)
[![license](https://img.shields.io/github/license/DoodleScheduling/k8sprom-patch-controller.svg)](https://github.com/DoodleScheduling/k8sprom-patch-controller/blob/master/LICENSE)
[![release](https://img.shields.io/github/release/DoodleScheduling/k8sprom-patch-controller/all.svg)](https://github.com/DoodleScheduling/k8sprom-patch-controller/releases)

Apply patches to kubernetes resources based on prometheus queries.

## Example

```yaml
apiVersion: metrics.infra.doodle.com/v1beta1
kind: PrometheusPatchRule
metadata:
  name: annotate-namespace
spec:
  prometheus:
    address: http://prometheus-server.prometheus
  expr: |
    rate(nginx_ingress_controller_requests{exported_namespace="default"}[5m]) == 0
  for: 5m
  interval: 2m
  suspend: false
  json6902Patches:
  - target:
      version: v1
      resource: namespaces
      name: default
    patch:
    - op: add
      path: /metadata/annotations/has-ingress-traffic"
      value: "false"
```

## Details

### Prometheus expression
As soon as the given rule spec.expr evaluates to `true` the patches spec.patches get applied to the defined target `spec.patches[].target`.

### Pending state
You may define a window spec.for for which the rule will be in a pending condition similar to prometheus alerting rules.
As soon as the expression was `true` for the specified duration the patches get applied.

### Patches
Define a list of patches which needs a target selector as well as a list of JSON 6902 patch operations.
The target select requires at least the api version `version` as well as the resource group `resource` which is usually the kind in plural lowercase.

```
json6902Patches:
- target:
    version: v1
    resource: namespaces
    name: default
  patch:
  - op: add
    path: /metadata/annotations/has-ingress-traffic"
    value: "false"
```
Instead selecting a single resource you may also select multiple ones by left out the name field.
You can filter multiple onse by specifying a comma separated label select: `labelSelector: label=value,label2=value`.

### Interval
Defines in what interval the rule is evaluated.

### Suspend
The PrometheusPatchRule may be suspended setting spec.suspend to `true`. A suspended rule does not get reconciled, meaning no patches will be applied as long as the rule is suspended.

### Remove patches
By design patches are **not** removed if the defined expression evaluates to `false` and if the patches have been added before.
The way to achieve this is to create another PrometheusPatchRule which expression does the opposite as well as reverse patches.

## Installation

### Requirements
You need a running prometheus [prometheus](https://prometheus.io/) (or any compatible prometheus service like [thanos](https://thanos.io/)).

### Permission
By default both the helm chart and the kustomize default base have a cluster rolebinding to cluster-admin.
Meaning the controller is granted full admin permission on the cluster.
This is needed as patch rules can target any kind of resources.
You may disable the binding and define fine grained cluster roles accordingly.

### Helm

Please see [chart/k8sprom-patch-controller](https://github.com/DoodleScheduling/k8sprom-patch-controller/tree/master/chart/k8sprom-patch-controller) for the helm chart docs.

### Manifests/kustomize

Alternatively you may get the bundled manifests in each release to deploy it using kustomize or use them directly.

## Configure the controller

You may change base settings for the controller using env variables (or alternatively command line arguments).
Available env variables:

| Name  | Description | Default |
|-------|-------------| --------|
| `METRICS_ADDR` | The address of the metric endpoint binds to. | `:9556` |
| `PROBE_ADDR` | The address of the probe endpoints binds to. | `:9557` |
| `ENABLE_LEADER_ELECTION` | Enable leader election for controller manager. | `false` |
| `LEADER_ELECTION_NAMESPACE` | Change the leader election namespace. This is by default the same where the controller is deployed. | `` |
| `NAMESPACES` | The controller listens by default for all namespaces. This may be limited to a comma delimited list of dedicated namespaces. | `` |
| `CONCURRENT` | The number of concurrent reconcile workers.  | `2` |
| `FIELD_MANAGER` | The name of the field manager used for server side apply https://kubernetes.io/docs/reference/using-api/server-side-apply/. | `k8sprom-patch-controller` |
