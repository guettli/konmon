#!/usr/bin/env bash
# End-to-end test on a kind cluster: deploy konmon with deploy/, break a
# Deployment, and expect 'konmon check' and the running konmon to report it.
# Needs kind, kubectl, docker and Go. CI runs it; locally: hack/e2e.sh
set -euo pipefail

cluster=${KIND_CLUSTER:-konmon-e2e}
image=konmon:e2e
ctx=kind-$cluster

if ! kind get clusters | grep -qx "$cluster"; then
  kind create cluster --name "$cluster" --wait 120s
fi

echo "--- build and load the image"
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/konmon-linux-amd64 .
docker build --build-arg TARGETOS=linux --build-arg TARGETARCH=amd64 -t "$image" .
kind load docker-image "$image" --name "$cluster"
go build -o dist/konmon .

echo "--- deploy konmon with deploy/"
kubectl --context "$ctx" kustomize deploy | sed "s#ghcr.io/guettli/konmon:latest#$image#" | kubectl --context "$ctx" apply -f -
kubectl --context "$ctx" -n konmon rollout status deploy/konmon --timeout=180s

echo "--- a healthy cluster has no health findings"
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

echo "--- the running konmon recorded it"
for i in $(seq 1 30); do
  out=$(kubectl --context "$ctx" -n konmon exec deploy/konmon -- /konmon findings --db /data/konmon.db -n e2e --category health)
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
