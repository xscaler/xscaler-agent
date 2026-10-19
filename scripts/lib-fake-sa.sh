# shellcheck shell=bash
# Sourced by the example tests. The Kubernetes components read the in-cluster
# service account while their pipelines are built, and only dial the API
# server once started. Give them a throwaway one so configs build off-cluster.
fake_service_account() {
  local sa=/var/run/secrets/kubernetes.io/serviceaccount sudo="" tmp
  if [ ! -f "$sa/token" ]; then
    [ "$(id -u)" -eq 0 ] || sudo=sudo
    tmp=$(mktemp -d)
    openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=validate \
      -keyout "$tmp/key" -out "$tmp/ca.crt" 2>/dev/null
    $sudo mkdir -p "$sa"
    $sudo cp "$tmp/ca.crt" "$sa/ca.crt"
    echo validate | $sudo tee "$sa/token" >/dev/null
    echo xscaler | $sudo tee "$sa/namespace" >/dev/null
    rm -rf "$tmp"
  fi
  # Nothing listens here; started components log connection errors and retry.
  export KUBERNETES_SERVICE_HOST=127.0.0.1 KUBERNETES_SERVICE_PORT=6443
}
