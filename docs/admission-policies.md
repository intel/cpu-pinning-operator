<!--
Copyright (C) 2026 Intel Corporation
SPDX-License-Identifier: Apache-2.0
-->

# Restricting who may change cluster-wide pinning config

This operator's two CRDs are **cluster-scoped** and drive CPU pinning for the
whole fleet:

- `BalloonsClusterConfig` (`bcc`) — the node-global singleton (reserved pool,
  `cpuAllocation`, per-node overrides, …).
- `BalloonPinningPolicy` (`bpp`) — per-workload placement policies (match, intent,
  capacity check, drift policy, …).

Editing either changes placement for **every tenant on the cluster**, so in a
multi-tenant environment mutation should be limited to platform administrators.
This page explains why the built-in controls are not sufficient on their own and
ships example admission policies — for both Kyverno and Gatekeeper/OPA — that
close the gap.

## The gap

Two mechanisms guard these CRDs, and neither restricts *who* may write
cluster-wide config:

1. **CEL singleton validation.** `BalloonsClusterConfig` carries a CEL rule
   (`self.metadata.name == 'cluster'`) so only one object can exist. That stops
   a *second* config, but it says nothing about *who* may create or edit the one
   named `cluster`. `BalloonPinningPolicy` has no such limit — any number can
   exist.
2. **RBAC verbs.** RBAC *can* restrict these verbs to a specific group by
   binding a role only to that group. The practical gap is leakage: the CRDs
   are cluster-scoped, so only a `ClusterRoleBinding` can grant their verbs (a
   namespaced `RoleBinding` cannot), but an over-broad or aggregated `ClusterRole`
   bound that way can reach unintended subjects — and any principal that ends up
   with them can rewrite cluster-wide pinning behavior. Admission policy is a defense-in-depth layer that re-checks the
   requesting identity regardless of how the verbs were granted.

The result: any principal granted (or inheriting) the verbs cluster-wide can change
placement for the whole fleet. Admission policy is the layer that can enforce
"only platform admins", keyed on the requesting identity — which is exactly what
these examples do.

## What the examples do

Both engines deny `create`/`update`/`patch` on `BalloonsClusterConfig` and
`BalloonPinningPolicy` unless the requester is in a **platform-admin group**, and
both exempt the operator's own ServiceAccount.

The operator never writes either spec: it only updates their `status`, so the
exemption matters only if you extend a policy to match the `status` subresource.
The chart-shipped `cluster` and `default` objects are written by whoever runs
`helm install` or `helm upgrade`, so **that identity must be in the allowed group**,
or the next `helm upgrade` is denied.

The manifests are **opt-in references** for platform teams. They are deliberately
**not** installed by the Helm chart — you choose an engine, customize the allowed
principals, and apply them yourself.

| Engine | Manifests |
|---|---|
| Kyverno | [`deploy/examples/kyverno/restrict-bcc-bpp-mutation.yaml`](../deploy/examples/kyverno/restrict-bcc-bpp-mutation.yaml) |
| Gatekeeper / OPA | [`deploy/examples/gatekeeper/constrainttemplate-restrict-bcc-bpp-mutation.yaml`](../deploy/examples/gatekeeper/constrainttemplate-restrict-bcc-bpp-mutation.yaml), [`deploy/examples/gatekeeper/constraint-restrict-bcc-bpp-mutation.yaml`](../deploy/examples/gatekeeper/constraint-restrict-bcc-bpp-mutation.yaml) |

## Customize before applying

Both examples use `platform-admins` as a **placeholder group** — you MUST replace
it with your real platform-admin identity (an OIDC group, a
`system:serviceaccount:...` name, etc.) or the policy will lock everyone out
except the operator ServiceAccount.

The operator ServiceAccount exemption matches the **default install**
(`cpu-pinning-operator` in namespace `cpu-pinning-operator-system`). If you set
`fullnameOverride`, `serviceAccount.name`, or a different release namespace,
update the exempted principal to match:

- Kyverno — the `exclude.any[].subjects` `ServiceAccount` entry.
- Gatekeeper — the `allowedUsers` entry
  (`system:serviceaccount:<namespace>:<serviceaccount>`).

## Applying the Kyverno policy

Requires [Kyverno](https://kyverno.io) running in the cluster.

```bash
# 1. Edit deploy/examples/kyverno/restrict-bcc-bpp-mutation.yaml:
#    - replace the group 'platform-admins' with your platform-admin group
#    - adjust the exempted ServiceAccount name/namespace if you overrode the install
# 2. Apply:
kubectl apply -f deploy/examples/kyverno/restrict-bcc-bpp-mutation.yaml
# 3. Verify it took (Ready):
kubectl get clusterpolicy restrict-bcc-bpp-mutation
```

Test that a non-admin is blocked (run as / impersonating a tenant admin):

```bash
# RBAC layer — qualify with the CRD group (bare name checks the core group and
# misreports), and impersonate the tenant admin you want to test.
kubectl auth can-i create balloonpinningpolicies.cpu-pinning-operator.intel.com \
  --as=alice --as-group=tenant-admins
# Admission layer — expect: denied by the ClusterPolicy.
kubectl --as=alice --as-group=tenant-admins apply \
  -f docs/examples/policy-vllm.yaml                    # blocked by Kyverno
```

## Applying the Gatekeeper / OPA policy

Requires [Gatekeeper](https://open-policy-agent.github.io/gatekeeper/) running in
the cluster. Apply the **ConstraintTemplate first** — Gatekeeper registers the
`RestrictPinningConfigMutation` kind from it, and the Constraint cannot be
created until that CRD exists.

```bash
# 1. Apply the template.
kubectl apply -f deploy/examples/gatekeeper/constrainttemplate-restrict-bcc-bpp-mutation.yaml
# 2. Edit deploy/examples/gatekeeper/constraint-restrict-bcc-bpp-mutation.yaml:
#    - replace 'platform-admins' in allowedGroups with your platform-admin group
#    - adjust the allowedUsers ServiceAccount entry if you overrode the install
# 3. Apply the constraint.
kubectl apply -f deploy/examples/gatekeeper/constraint-restrict-bcc-bpp-mutation.yaml
# 4. Verify the constraint is enforced (no template/ingestion errors):
kubectl get restrictpinningconfigmutation restrict-bcc-bpp-mutation -o yaml
```

## Notes and limitations

- These policies gate **admission** (create/update/patch). They complement, and
  do not replace, tight RBAC — keep the write verbs on these CRDs off tenant
  roles as well (defense in depth).
- `delete` is intentionally not restricted by these examples; add the operation
  to the match block if your threat model requires it.
- Keep the ServiceAccount exemption if you extend the match to the `status`
  subresource, or the operator cannot report status. The Helm installer identity
  must stay allowed, or chart upgrades fail.
