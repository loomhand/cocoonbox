#!/usr/bin/env bash
# Writes a kubeconfig for the cocoonbox-api ServiceAccount (see rbac.yaml) to stdout.
set -euo pipefail

KUBECTL=${KUBECTL:-kubectl}
SERVER=${SERVER:-$($KUBECTL config view --minify -o jsonpath='{.clusters[0].cluster.server}')}
secret() { $KUBECTL -n cocoonbox get secret cocoonbox-api-token -o jsonpath="{.data.$1}"; }
TOKEN=$(secret token | base64 -d)
CA=$(secret 'ca\.crt')

cat <<KUBECONFIG
apiVersion: v1
kind: Config
clusters:
  - name: cocoonbox
    cluster:
      server: ${SERVER}
      certificate-authority-data: ${CA}
users:
  - name: cocoonbox-api
    user:
      token: ${TOKEN}
contexts:
  - name: cocoonbox-api
    context:
      cluster: cocoonbox
      user: cocoonbox-api
      namespace: cocoonbox
current-context: cocoonbox-api
KUBECONFIG
