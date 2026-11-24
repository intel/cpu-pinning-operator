#!/usr/bin/env bash
# Copyright (C) 2026 Intel Corporation
# SPDX-License-Identifier: Apache-2.0
#
# Runs every gated security e2e test against the kubeconfig
# cluster, in three passes: hardened (metrics.requireAuth=true), scrape
# (requireAuth=false), destructive; then the webhook NetworkPolicy test alone with that policy
# narrowed. On exit restores the release's user-supplied Helm values exactly as
# they were before the first upgrade. It mutates the installation: run it on a
# disposable validation cluster.
# Called by `make test-validation`; DRY_RUN=1 prints the commands instead.
set -uo pipefail

# The release and namespace variables are the suite's own, so the tests see them too.
export E2E_RELEASE_NAME=${E2E_RELEASE_NAME:-cpu-pinning-operator}
export E2E_OPERATOR_NAMESPACE=${E2E_OPERATOR_NAMESPACE:-cpu-pinning-operator-system}
export E2E_OPERATOR_DEPLOYMENT=${E2E_OPERATOR_DEPLOYMENT:-$E2E_RELEASE_NAME}
RELEASE=$E2E_RELEASE_NAME NAMESPACE=$E2E_OPERATOR_NAMESPACE
CHART=${VALIDATION_CHART:-deploy/cpu-pinning-operator}
HELM=${E2E_HELM_BIN:-helm}
OUT=${ALLURE_OUTPUT_PATH:-$PWD}/validation-results
PRIOR=$OUT/prior-values.yaml
# Time budget, worst case (sized to finish within 6h): each full pass (hardened,
# scrape) is a go test -timeout of 85m plus up to 5m of `make test-e2e-sweep`; each
# filtered pass (destructive, netpol) 30m plus 5m; each of the five helm upgrades up to
# about 13m (rollout 5m, Lease 3m, BalloonsPolicy 5m). About 5h15m in all. The suite itself
# sweeps 5m before its -timeout, so a hung test still cleans up before the Go panic.
TIMEOUT=${VALIDATION_TIMEOUT:-85m}
SHORT_TIMEOUT=${VALIDATION_SHORT_TIMEOUT:-30m}
DRY_RUN=${DRY_RUN:-}

# E2E_NETWORK_POLICY is a NetworkPolicy name, not a gate: the webhook-ingress test
# runs whenever networkPolicy.webhook.from is set, so it is never set here.
GATES=(E2E_RUN_PINNING=1 E2E_MUTATING=1 E2E_RESTART=1 E2E_METRICS=1 E2E_DASHBOARDS=1
  E2E_GUARD_ACTIVE_PODS=1 E2E_DRIFT_RESTART=1 E2E_FORGED_ADMISSION=1
  E2E_RESERVE_AUTHORITY=1 E2E_CHAOS_STALE_NRT=1 E2E_WEBHOOK_CA=1 E2E_READINESS_RATE_LIMIT=1)
PLAN_TESTS=(TestForgedAdmissionBurstContainedByReservationTTL TestReserveAuthorityBurstSingleLedger
  TestReserveAuthorityCrossReplicaLedger TestReserveAuthorityFailoverAcrossBurst
  TestReserveAuthorityUnreachableDenies TestMetricsRejectsUnauthenticatedScrape
  TestWebhookPortRejectsNonAllowedPod TestWebhookTargetsNamespaceByLabel
  TestChaosWebhookFailurePolicyBlastRadius TestWebhookServingCertChainsToInjectedCABundle
  TestReadinessEndpointRateLimitsPerSource TestReadinessInitRefusesUnverifiedServer)
PLAN_TESTS+=(TestTLSListenersNegotiateApprovedProtocols TestOperatorPodsAdmittedUnderRestrictedPSA
  TestNoPersonalDataInOperatorOutputs TestTwoPoliciesPinTheirOwnWorkloads
  TestOverlappingPoliciesPickOneDeterministically)

run() { echo "+ $*"; [ -n "$DRY_RUN" ] || "$@"; }

mode=unknown
netpol= armed=
# set_auth <bool> [from]: set metrics.requireAuth and networkPolicy.webhook.from (JSON,
# default [] so every pass but the NetworkPolicy step reaches the webhook port from anywhere), then settle.
set_auth() {
  local from=${2:-[]}
  [ "$from" = "[]" ] || netpol=1
  run "$HELM" upgrade "$RELEASE" "$CHART" -n "$NAMESPACE" --reuse-values \
    --set "metrics.requireAuth=$1" --set-json "networkPolicy.webhook.from=$from" || return 1
  mode=$1
  [ "$from" = "[]" ] && netpol=
  settle
}

# restore_prior: put back the user-supplied values captured before the first upgrade,
# so a value that was unset is unset again rather than pinned to a chart default.
restore_prior() {
  run "$HELM" upgrade "$RELEASE" "$CHART" -n "$NAMESPACE" --reset-values -f "$PRIOR" || return 1
  mode=prior netpol=
  settle
}

# settle: wait for the rollout, the leader and every balloons-enabled node's BalloonsPolicy.
settle() {
  local node
  run kubectl rollout status "deploy/$E2E_OPERATOR_DEPLOYMENT" -n "$NAMESPACE" --timeout=5m || return 1
  wait_leader
  [ -n "$DRY_RUN" ] && { echo "+ wait for BalloonsPolicy node.<name> of every balloons-enabled node"; return 0; }
  for node in $(kubectl get nodes -l cpu-pinning-operator.intel.com/balloons-enabled=true -o name); do
    node=${node#node/}
    for _ in $(seq 60); do
      kubectl get balloonspolicies -A -o name 2>/dev/null | grep -q "/node\.$node$" && continue 2
      sleep 5
    done
    echo "warning: no BalloonsPolicy node.$node after 5m; continuing"
  done
}

# wait_leader: the rollout ends before a new replica takes the leader-election Lease,
# so wait (bounded) until its holder, the pod name before the first "_", is a Running,
# Ready manager pod. The Lease is named like the suite's: E2E_LEADER_LEASE.
wait_leader() {
  local lease=${E2E_LEADER_LEASE:-$E2E_OPERATOR_DEPLOYMENT} holder
  [ -n "$DRY_RUN" ] && { echo "+ wait until Lease $lease names a Running, Ready manager pod"; return; }
  for _ in $(seq 36); do
    holder=$(kubectl get lease "$lease" -n "$NAMESPACE" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null)
    holder=${holder%%_*}
    [ -n "$holder" ] && [ "$(kubectl get pod "$holder" -n "$NAMESPACE" -o \
      jsonpath='{.metadata.labels.control-plane}/{.status.phase}/{.status.conditions[?(@.type=="Ready")].status}' \
      2>/dev/null)" = controller-manager/Running/True ] && return
    sleep 5
  done
  echo "warning: Lease $lease names no Running, Ready manager pod after 3m; continuing"
}

# wait_metrics: a young replica serves no cpu_pinning_ node series for about a
# minute, so wait until every manager pod does (bounded; the suite re-scrapes too).
wait_metrics() {
  local pods pod ok
  [ -n "$DRY_RUN" ] && { echo "+ wait until every manager pod serves cpu_pinning_ node series"; return; }
  pods=$(kubectl get pods -n "$NAMESPACE" -o name \
    -l "app.kubernetes.io/instance=$RELEASE,control-plane=controller-manager")
  for _ in $(seq 36); do
    ok=1
    for pod in $pods; do
      kubectl get --raw "/api/v1/namespaces/$NAMESPACE/pods/${E2E_METRICS_SCHEME:-https}:${pod#pod/}:${E2E_METRICS_PORT:-8080}/proxy/metrics" \
        2>/dev/null | grep -q '^cpu_pinning_ht_sibling_policy' || { ok=; break; }
    done
    [ -n "$ok" ] && return
    sleep 5
  done
  echo "warning: not every manager replica serves node series after 3m; continuing"
}

# webhook_from: the networkPolicy.webhook.from JSON for the webhook NetworkPolicy test. E2E_WEBHOOK_FROM_CIDR
# (comma-separated) overrides the derived list: every control-plane node's InternalIP
# and its Calico IPIP/VXLAN tunnel address, the sources the API server dials from.
webhook_from() {
  local addrs a json=''
  if [ -n "${E2E_WEBHOOK_FROM_CIDR:-}" ]; then
    addrs=${E2E_WEBHOOK_FROM_CIDR//,/ }
  elif [ -n "$DRY_RUN" ]; then
    addrs="<control-plane-InternalIP> <calico-tunnel-addr>"
  else
    addrs=$(kubectl get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{range .items[*]}{range .status.addresses[?(@.type=="InternalIP")]}{.address} {end}{.metadata.annotations.projectcalico\.org/IPv4IPIPTunnelAddr} {.metadata.annotations.projectcalico\.org/IPv4VXLANTunnelAddr} {end}') || return 1
  fi
  for a in $addrs; do
    case $a in */*) ;; *:*) a=$a/128 ;; *) a=$a/32 ;; esac
    json+="${json:+,}{\"ipBlock\":{\"cidr\":\"$a\"}}"
  done
  [ -n "$json" ] && echo "[$json]"
}

# admission_ok: server-side dry-run create of a pod the installed policy matches, in a
# namespace it targets; true only when the webhook stamped it.
admission_ok() {
  local pol=${E2E_POLICY_NAME:-default} bpp=balloonpinningpolicies.cpu-pinning-operator.intel.com ns ctr made='' got
  [ -n "$DRY_RUN" ] && { echo "+ kubectl create --dry-run=server of a $pol-matched pod in a targeted namespace; expect pinned-by"; return 0; }
  ns=${E2E_NAMESPACE:-$(kubectl get "$bpp" "$pol" -o jsonpath='{.spec.namespaces[*]}' | tr ' ' '\n' | grep -v '[][*?]' | head -1)}
  ctr=$(kubectl get "$bpp" "$pol" -o jsonpath='{.spec.match.byContainerName[*]}' | tr ' ' '\n' | grep -v '[][*?]' | head -1)
  if [ -z "$ns" ] || [ -z "$ctr" ]; then echo "admission check: policy $pol names no exact namespace and container"; return 1; fi
  # A namespace a previous probe deleted may still be terminating: wait it out first.
  if [ "$(kubectl get namespace "$ns" -o jsonpath='{.status.phase}' 2>/dev/null)" = Terminating ] &&
    ! kubectl wait --for=delete "namespace/$ns" --timeout=3m >/dev/null; then
    echo "admission check: namespace $ns still terminating after 3m"; return 1
  fi
  kubectl get namespace "$ns" >/dev/null 2>&1 || { kubectl create namespace "$ns" >/dev/null && made=1; }
  got=$(kubectl create --dry-run=server -n "$ns" -f - \
    -o jsonpath='{.metadata.annotations.cpu-pinning-operator\.intel\.com/pinned-by}' <<YAML
apiVersion: v1
kind: Pod
metadata: {name: e2e-netpol-admission-check}
spec:
  containers:
  - name: $ctr
    image: registry.k8s.io/pause:3.9
    args: [--tensor-parallel-size, "1"]
    resources: {requests: {cpu: "1"}, limits: {cpu: "1"}}
YAML
  )
  [ -z "$made" ] || kubectl delete namespace "$ns" --wait=false >/dev/null
  echo "admission check: $ctr pod in $ns got pinned-by=${got:-<none>}"
  [ "$got" = cpu-pinning-operator ]
}

# netpol_step: the webhook NetworkPolicy test alone with networkPolicy.webhook.from set, then cleared. A failed
# admission check clears it at once and fails the webhook NetworkPolicy test; the EXIT trap restores the prior values too.
netpol_step() {
  local from log="$OUT/netpol/e2e.log"
  mkdir -p "$OUT/netpol"
  if ! from=$(webhook_from); then
    echo "--- FAIL: TestWebhookPortRejectsNonAllowedPod (no control-plane address to allow; set E2E_WEBHOOK_FROM_CIDR)" | tee "$log"
    rc=1
    return
  fi
  if ! set_auth true "$from"; then
    echo "--- FAIL: TestWebhookPortRejectsNonAllowedPod (helm upgrade with webhook.from failed)" | tee "$log"
    rc=1
  elif ! { run sleep 10; admission_ok; }; then # Calico programs the policy asynchronously
    set_auth true
    echo "--- FAIL: TestWebhookPortRejectsNonAllowedPod (admission broken with webhook.from=$from; cleared)" | tee "$log"
    rc=1
  else
    RUN_ARGS=(-run '^TestWebhookPortRejectsNonAllowedPod$')
    pass netpol
  fi
  [ -z "$netpol" ] || set_auth true || { echo "netpol: clearing networkPolicy.webhook.from FAILED"; rc=1; }
}

# sweep <name>: restore the shared objects the suite changed and delete what it
# labelled; idempotent, so a pass that cleaned up after itself costs a few seconds.
sweep() {
  mkdir -p "$OUT"
  run "${MAKE:-make}" --no-print-directory test-e2e-sweep 2>&1 | tee "$OUT/sweep-$1.log"
  [ "${PIPESTATUS[0]}" = 0 ] || { echo "sweep after $1: FAIL (see $OUT/sweep-$1.log)"; return 1; }
}

# restore: once the values are captured, put them back, run the sweep (it needs the
# operator up), then put them back again, because the sweep can write an earlier
# pass's snapshotted settings over them. Before the capture nothing has changed.
restore() {
  local ok=0
  [ -z "$armed" ] || restore_prior || ok=1
  sweep exit || ok=1
  [ -z "$armed" ] || restore_prior || ok=1
  [ "$ok" = 0 ] || exit 1
}
trap restore EXIT
trap 'exit 130' INT TERM

rc=0
RUN_ARGS=()
mkdir -p "$OUT"
if [ -n "$DRY_RUN" ]; then
  echo "+ $HELM get values $RELEASE -n $NAMESPACE -o yaml > $PRIOR"
else
  "$HELM" get values "$RELEASE" -n "$NAMESPACE" -o yaml >"$PRIOR" || { echo "cannot read the release's values; nothing changed"; exit 1; }
fi
armed=1
# pass <name> <gate>...: one suite run with its own log and Allure dir, then a sweep; fails only when the sweep does,
# and the callers then exit, so the EXIT trap retries the sweep before another pass overwrites the snapshot.
pass() {
  local name=$1 dir="$OUT/$1" timeout=$TIMEOUT
  shift
  mkdir -p "$dir"
  [ ${#RUN_ARGS[@]} -eq 0 ] || timeout=$SHORT_TIMEOUT
  run env "$@" E2E_PASS="$name" E2E_STRICT="${E2E_STRICT-1}" ALLURE_OUTPUT_PATH="$dir" \
    go test -tags e2e ./test/e2e/... -count=1 -v -timeout "$timeout" "${RUN_ARGS[@]}" 2>&1 | tee "$dir/e2e.log"
  [ "${PIPESTATUS[0]}" = 0 ] || { echo "pass $name: FAIL"; rc=1; }
  sweep "$name" || { rc=1; return 1; }
}

set_auth true || exit 1
# TLS precedes metrics auth, so the TLS policy test needs no scrape pass; the data-handling test needs the scrape.
pass hardened "${GATES[@]}" E2E_TLS_POLICY=1 E2E_POD_SECURITY=1 || exit 1
set_auth false || exit 1
wait_metrics
pass scrape "${GATES[@]}" E2E_DATA_HANDLING=1 || exit 1
set_auth true || exit 1
# The readiness TLS test corrupts the target namespace's readiness CA copy, so it runs here too.
RUN_ARGS=(-run '^(TestChaosWebhookFailurePolicyBlastRadius|TestReadinessInitRefusesUnverifiedServer)$')
pass destructive E2E_CHAOS_FAILURE_POLICY=1 E2E_READINESS_TLS=1 || exit 1

netpol_step

echo
echo "== validation summary (logs and allure-results under $OUT/<pass>/)"
never=()
for p in hardened scrape destructive netpol; do
  log="$OUT/$p/e2e.log"
  printf '%-12s pass=%s fail=%s skip=%s\n' "$p" "$(grep -c '^--- PASS' "$log")" \
    "$(grep -c '^--- FAIL' "$log")" "$(grep -c '^--- SKIP' "$log")"
  for t in "${PLAN_TESTS[@]}"; do
    v=$(grep -oE "^--- (PASS|FAIL|SKIP): $t " "$log" | cut -d' ' -f2 | tr -d :)
    [ -n "$v" ] && printf '  %-5s %s\n' "$v" "$t"
  done
done
for t in "${PLAN_TESTS[@]}"; do
  grep -qhE "^--- (PASS|FAIL): $t " "$OUT"/{hardened,scrape,destructive,netpol}/e2e.log || never+=("$t")
done
[ ${#never[@]} -eq 0 ] || echo "security tests that ran in no pass (reported, not failed): ${never[*]}"
exit $rc
