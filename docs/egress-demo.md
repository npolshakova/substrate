# Egress Demo

Run these commands from the `egress-pep-proxy-tunnel` worktree. This runbook
installs the prototype external egress path:

```text
actor TCP egress -> ateom transparent redirect -> atenet-egress agentgateway -> api.openai.com
                                      ^                    ^
                                      |                    |
                         selected by ActorTemplate   xDS from prototype-controlplane
```

`agentgateway` runs as the `atenet-egress` Deployment in `ate-system`, not inside
`ateom-gvisor`. `prototype-controlplane` owns the POC policy, serves xDS to
`atenet-egress`, and resolves gateway-side credential injection from a Kubernetes
Secret.

The sandbox `ActorTemplate` does not need native `spec.egressPolicy` for this
POC. Policy selection lives in `prototype-controlplane-policy`: the default demo
policy selects `ate-demo-sandbox/sandbox-template`, allows only
`api.openai.com:443`, and injects the `Authorization` header from the
`openai-api-key` Secret mounted into `prototype-controlplane`.

## 1. Create the gateway credential secret

Create this before restarting `prototype-controlplane` so the xDS config includes
the injected header value. The actor never receives this key.

```bash
: "${OPENAI_API_KEY:?set OPENAI_API_KEY first}"

kubectl create namespace ate-system --dry-run=client -o yaml | kubectl apply -f -
kubectl -n ate-system create secret generic openai-api-key \
  --from-literal=authorization="Bearer ${OPENAI_API_KEY}" \
  --dry-run=client -o yaml | kubectl apply -f -
```

## 2. Install Substrate with prototype egress enabled

```bash
hack/install-ate-kind.sh --enable-egress --deploy-ate-system
```

`--enable-egress` deploys both `prototype-controlplane` and the external
`atenet-egress` PEP, points `ate-api-server` at
`atenet-egress.ate-system.svc:15080`, and scopes egress tunneling to
`ate-demo-sandbox/sandbox-template`.

Check the core pieces and the external egress PEP:

```bash
kubectl -n ate-system rollout status deployment/ate-api-server-deployment --timeout=180s
kubectl -n ate-system rollout status deployment/atenet-router --timeout=180s
kubectl -n ate-system rollout status deployment/prototype-controlplane --timeout=180s
kubectl -n ate-system rollout status deployment/atenet-egress --timeout=180s
kubectl -n ate-system rollout status daemonset/atelet --timeout=180s
```

If `prototype-controlplane` or `atenet-egress` is missing, the system was
installed before the POC egress manifests were applied. Deploy the missing POC
pieces and restart ate-api so it picks up the egress tunnel env vars:

```bash
hack/install-ate-kind.sh --enable-egress \
  --create-api-server-env-vars \
  --deploy-prototype-controlplane

kubectl -n ate-system rollout restart deployment/ate-api-server-deployment
kubectl -n ate-system rollout status deployment/ate-api-server-deployment --timeout=180s
kubectl -n ate-system rollout status deployment/prototype-controlplane --timeout=180s
kubectl -n ate-system rollout status deployment/atenet-egress --timeout=180s
```

Verify the control-plane wiring:

```bash
kubectl -n ate-system get configmap ate-api-server-envvars \
  -o jsonpath='{.data.ATE_API_EGRESS_TUNNEL_GATEWAY_ADDRESS}{"\n"}{.data.ATE_API_EGRESS_TUNNEL_ACTOR_TEMPLATES}{"\n"}'

kubectl -n ate-system get configmap prototype-controlplane-policy \
  -o jsonpath='{.data.policy\.json}' | python3 -m json.tool
```

Expected values:

```text
atenet-egress.ate-system.svc:15080
ate-demo-sandbox/sandbox-template
```

If you created or changed `openai-api-key` after installing the system, restart
the xDS server and PEP:

```bash
kubectl -n ate-system rollout restart deployment/prototype-controlplane
kubectl -n ate-system rollout status deployment/prototype-controlplane --timeout=180s
kubectl -n ate-system rollout restart deployment/atenet-egress
kubectl -n ate-system rollout status deployment/atenet-egress --timeout=180s
```

## 3. Deploy the sandbox demo

```bash
kubectl -n ate-demo-sandbox delete actortemplate/sandbox-template --ignore-not-found
hack/install-ate-kind.sh --enable-egress --deploy-demo-sandbox

kubectl -n ate-demo-sandbox get workerpool sandbox-workerpool
kubectl wait --for=condition=Ready actortemplate/sandbox-template \
  -n ate-demo-sandbox --timeout=5m
```

Install the local CLI and create a fresh actor:

```bash
go install ./cmd/kubectl-ate
export PATH="$(go env GOPATH)/bin:${PATH}"
kubectl ate suspend actor egress-sandbox || true
kubectl ate delete actor egress-sandbox || true
kubectl ate create actor egress-sandbox --template ate-demo-sandbox/sandbox-template
```

Confirm the actor resumed:

```bash
kubectl ate get actor egress-sandbox
```

Port-forward the ingress router used to reach the actor:

```bash
kubectl -n ate-system port-forward svc/atenet-router 8000:80 >/tmp/atenet-router-port-forward.log 2>&1 &
PF_ROUTER_PID=$!
```

## 4. Curl the actor through the router

Basic actor reachability:

```bash
curl -sS \
  -H 'Host: egress-sandbox.actors.resources.substrate.ate.dev' \
  -H 'Content-Type: application/json' \
  http://localhost:8000/process \
  -d '{"command":["sh","-lc","echo actor-ready && uname -a"],"timeout":"10s"}'
```

Allowed egress with gateway-injected credentials:

```bash
curl -sS \
  -H 'Host: egress-sandbox.actors.resources.substrate.ate.dev' \
  -H 'Content-Type: application/json' \
  http://localhost:8000/process \
  -d '{"command":["sh","-lc","wget -S -O- --timeout=10 https://api.openai.com/v1/models 2>&1 | head -40"],"timeout":"15s"}'
```

This should return the OpenAI models response. If it returns `401`, the request
reached OpenAI but `prototype-controlplane` did not resolve/mount
`openai-api-key` before serving the xDS config.

Default-deny check against an unlisted destination:

```bash
curl -sS \
  -H 'Host: egress-sandbox.actors.resources.substrate.ate.dev' \
  -H 'Content-Type: application/json' \
  http://localhost:8000/process \
  -d '{"command":["sh","-lc","wget -S -O- --timeout=5 https://example.com 2>&1; echo exit:$?"],"timeout":"10s"}'
```

This should fail to connect or reset the connection.

## 5. Debugging

If the allowed request still returns `401`, verify the secret is mounted and the
control plane restarted after the secret was created:

```bash
kubectl -n ate-system get secret openai-api-key
kubectl -n ate-system logs deployment/prototype-controlplane --tail=100
kubectl -n ate-system rollout restart deployment/prototype-controlplane
kubectl -n ate-system rollout restart deployment/atenet-egress
```

If `example.com` succeeds, the actor was not tunneled through `atenet-egress`.
Check that ate-api has the egress target selector and recreate the actor:

```bash
kubectl -n ate-system get configmap ate-api-server-envvars \
  -o jsonpath='{.data.ATE_API_EGRESS_TUNNEL_ACTOR_TEMPLATES}{"\n"}'
kubectl -n ate-system logs deployment/ate-api-server-deployment -c ate-api-server --tail=100

kubectl ate delete actor egress-sandbox || true
kubectl ate create actor egress-sandbox --template ate-demo-sandbox/sandbox-template
```

If `atenet-egress` is not ready, inspect the agentgateway xDS client:

```bash
kubectl -n ate-system logs deployment/atenet-egress -c agentgateway --tail=200
kubectl -n ate-system logs deployment/prototype-controlplane --tail=200
```

## 6. Cleanup

```bash
kubectl ate suspend actor egress-sandbox || true
kubectl ate delete actor egress-sandbox || true
hack/install-ate.sh --delete-demo-sandbox

kill "${PF_ROUTER_PID}" 2>/dev/null || true
```
