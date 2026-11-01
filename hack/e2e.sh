#!/usr/bin/env bash
# End-to-end test on a kind cluster: deploy konmon with deploy/, break a
# Deployment, and expect 'konmon check' and the running konmon to report it.
# Needs kind, kubectl, docker and Go. CI runs it; locally: hack/e2e.sh
set -euo pipefail

cluster=${KIND_CLUSTER:-konmon-e2e}
image=konmon:e2e
ctx=kind-$cluster
arch=$(go env GOARCH)

if ! kind get clusters | grep -qx "$cluster"; then
  kind create cluster --name "$cluster" --wait 120s
fi

echo "--- build and load the image"
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "dist/konmon-linux-$arch" .
docker build --build-arg TARGETOS=linux --build-arg TARGETARCH="$arch" -t "$image" .
kind load docker-image "$image" --name "$cluster"
go build -o dist/konmon .

echo "--- deploy konmon with deploy/, the image of this commit"
overlay=dist/e2e-overlay # kustomize wants a relative path to deploy/
mkdir -p "$overlay"
cat > "$overlay/kustomization.yaml" <<YAML
resources:
  - ../../deploy
images:
  - name: ghcr.io/guettli/konmon
    newName: konmon
    newTag: e2e
YAML
kubectl --context "$ctx" kustomize "$overlay" | kubectl --context "$ctx" apply -f -
kubectl --context "$ctx" -n konmon rollout status deploy/konmon --timeout=180s

echo "--- /metrics: konmon has evaluated (it is Ready, so it must have)"
pod=$(kubectl --context "$ctx" -n konmon get pod -l app=konmon -o jsonpath='{.items[0].metadata.name}')
metrics=$(kubectl --context "$ctx" get --raw "/api/v1/namespaces/konmon/pods/$pod:8080/proxy/metrics")
if ! grep -Eq '^konmon_evaluations_total [1-9]' <<<"$metrics"; then
  echo "no evaluation in /metrics:"
  echo "$metrics"
  exit 1
fi

echo "--- a healthy cluster has no health findings"
kubectl --context "$ctx" delete namespace e2e --ignore-not-found --wait # an earlier run
kubectl --context "$ctx" wait --for=condition=Ready pod --all --all-namespaces --timeout=180s
./dist/konmon check --context "$ctx" --category health

echo "--- break a Deployment"
kubectl --context "$ctx" create namespace e2e --dry-run=client -o yaml | kubectl --context "$ctx" apply -f -
kubectl --context "$ctx" -n e2e create deployment broken --image=registry.invalid/does-not-exist:1 --dry-run=client -o yaml |
  kubectl --context "$ctx" apply -f -

echo "--- konmon check reports it (exit code 1)"
for i in $(seq 1 30); do
  set +e
  out=$(./dist/konmon check --context "$ctx" -n e2e pod-container-waiting)
  code=$?
  set -e
  if [ "$code" = 1 ] && grep -q "broken" <<<"$out"; then
    echo "$out"
    break
  fi
  if [ "$i" = 30 ]; then
    echo "konmon check did not report the broken Deployment (exit $code):"
    echo "$out"
    exit 1
  fi
  sleep 5
done

echo "--- a user of namespace e2e only (view Role there) gets the same finding with -n e2e"
kubectl --context "$ctx" -n e2e create serviceaccount viewer --dry-run=client -o yaml | kubectl --context "$ctx" apply -f -
kubectl --context "$ctx" -n e2e create rolebinding viewer --clusterrole=view --serviceaccount=e2e:viewer --dry-run=client -o yaml |
  kubectl --context "$ctx" apply -f -
kubeconfig=dist/viewer.kubeconfig
kubectl --context "$ctx" config view --minify --flatten -o json |
  jq --arg token "$(kubectl --context "$ctx" -n e2e create token viewer)" \
    '.users[0].user = {token: $token}' >"$kubeconfig"
for i in $(seq 1 10); do
  set +e
  out=$(./dist/konmon check --kubeconfig "$kubeconfig" -n e2e pod-container-waiting)
  code=$?
  set -e
  if [ "$code" = 1 ] && grep -q "broken" <<<"$out" && grep -q "listed in e2e only: .*pods" <<<"$out"; then
    echo "$out"
    break
  fi
  if [ "$i" = 10 ]; then
    echo "konmon check as a namespace user did not report the broken Deployment (exit $code):"
    echo "$out"
    exit 1
  fi
  sleep 3 # the RoleBinding is new
done

echo "--- the running konmon recorded it"
for i in $(seq 1 30); do
  out=$(kubectl --context "$ctx" -n konmon exec deploy/konmon -- /konmon findings --db /data/konmon.db -n e2e --category health || true)
  if grep -q "pod-container-waiting" <<<"$out"; then
    echo "$out"
    break
  fi
  if [ "$i" = 30 ]; then
    echo "konmon run did not record the finding:"
    echo "$out"
    kubectl --context "$ctx" -n konmon logs deploy/konmon | tail -30
    exit 1
  fi
  sleep 5
done
echo "--- ok"
