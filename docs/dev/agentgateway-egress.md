# agentgateway Egress PEP

This guide covers the external egress policy enforcement point used by the
`egress-pep-proxy-tunnel` worktree.

This is the PEP-proxy design:

```text
actor TCP egress
  -> ateom transparent redirect
  -> ateom mini tunnel
  -> atenet-egress agentgateway
  -> external service
```

`agentgateway` runs as the `atenet-egress` Deployment in `ate-system`.
`ateom-gvisor` does not bundle or run agentgateway in this branch. The bundled
or sidecar-style agentgateway work belongs to the separate `egress-support`
branch.

## Components

`ate-api-server` decides whether a resumed actor should use the external egress
tunnel. It passes `EgressTunnelConfig` through `atelet` to `ateom-gvisor` on
both `RunWorkload` and `RestoreWorkload`.

`ateom-gvisor` installs transparent redirect rules in the worker pod network
namespace and starts a localhost listener. Actor TCP connections are redirected
to that listener. For each redirected connection, the mini tunnel reads
`SO_ORIGINAL_DST`, connects to `atenet-egress`, sends a PROXY protocol v2 header,
then relays bytes.

`prototype-controlplane` serves agentgateway xDS for the POC policy. It owns the
external PEP resources, destination allow-list, TLS origination/interception
knobs, and gateway-side credential injection.

## Tunnel Metadata

The mini tunnel sends PROXY protocol v2 metadata before the actor stream:

* source address: the actor-side redirected TCP connection source
* destination address: the original destination from `SO_ORIGINAL_DST`
* TLV `0xD0`: actor identity as a SPIFFE-form string

The current identity string is:

```text
spiffe://substrate.local/ns/<actor_template_namespace>/sa/<actor_template_name>.<actor_id>
```

agentgateway parses TLV `0xD0` into CEL source identity fields:

```text
source.identity.trustDomain
source.identity.namespace
source.identity.serviceAccount
```

For actor `egress-sandbox` from `ate-demo-sandbox/sandbox-template`, policy CEL
can match:

```cel
source.identity.trustDomain == "substrate.local" &&
source.identity.namespace == "ate-demo-sandbox" &&
source.identity.serviceAccount == "sandbox-template.egress-sandbox"
```

This is enough for prototype enforcement in agentgateway, but it is not yet a
cryptographic actor credential. The PEP currently trusts the PROXY header from
the mini tunnel. Production enforcement should either restrict PEP reachability
to trusted worker pods or add a signed activation token/mTLS credential that the
PEP verifies before evaluating actor-aware policy.

## Setup

Create the OpenAI credential used by the demo policy. The actor never receives
this key; `prototype-controlplane` resolves it and configures agentgateway to
inject it at the PEP.

```sh
: "${OPENAI_API_KEY:?set OPENAI_API_KEY first}"

kubectl create namespace ate-system --dry-run=client -o yaml | kubectl apply -f -
kubectl -n ate-system create secret generic openai-api-key \
  --from-literal=authorization="Bearer ${OPENAI_API_KEY}" \
  --dry-run=client -o yaml | kubectl apply -f -
```

Install Substrate with prototype egress enabled:

```sh
hack/install-ate-kind.sh --enable-egress --deploy-ate-system
```

For a stock Kind cluster using JWT auth instead of PodCertificate/CTB features,
use:

```sh
hack/install-ate-kind-jwt.sh --enable-egress
```

`--enable-egress` deploys `prototype-controlplane` and `atenet-egress`, points
`ate-api-server` at `atenet-egress.ate-system.svc:15080`, and scopes egress
tunneling to the configured target ActorTemplates.

Check rollout status:

```sh
kubectl -n ate-system rollout status deployment/ate-api-server-deployment --timeout=180s
kubectl -n ate-system rollout status deployment/atenet-router --timeout=180s
kubectl -n ate-system rollout status deployment/prototype-controlplane --timeout=180s
kubectl -n ate-system rollout status deployment/atenet-egress --timeout=180s
kubectl -n ate-system rollout status daemonset/atelet --timeout=180s
```

Verify the egress wiring:

```sh
kubectl -n ate-system get configmap ate-api-server-envvars \
  -o jsonpath='{.data.ATE_API_EGRESS_TUNNEL_GATEWAY_ADDRESS}{"\n"}{.data.ATE_API_EGRESS_TUNNEL_ACTOR_TEMPLATES}{"\n"}'

kubectl -n ate-system get configmap prototype-controlplane-policy \
  -o jsonpath='{.data.policy\.json}' | python3 -m json.tool
```

Expected demo values:

```text
atenet-egress.ate-system.svc:15080
ate-demo-sandbox/sandbox-template
```

## Smoke Test

Deploy the sandbox demo and create a fresh actor:

```sh
kubectl -n ate-demo-sandbox delete actortemplate/sandbox-template --ignore-not-found
hack/install-ate-kind.sh --enable-egress --deploy-demo-sandbox

kubectl wait --for=condition=Ready actortemplate/sandbox-template \
  -n ate-demo-sandbox --timeout=5m

go install ./cmd/kubectl-ate
export PATH="$(go env GOPATH)/bin:${PATH}"

kubectl ate suspend actor egress-sandbox || true
kubectl ate delete actor egress-sandbox || true
kubectl ate create actor egress-sandbox --template ate-demo-sandbox/sandbox-template
```

Port-forward the ingress router:

```sh
kubectl -n ate-system port-forward svc/atenet-router 8000:80 >/tmp/atenet-router-port-forward.log 2>&1 &
PF_ROUTER_PID=$!
```

Check actor reachability:

```sh
curl -sS \
  -H 'Host: egress-sandbox.actors.resources.substrate.ate.dev' \
  -H 'Content-Type: application/json' \
  http://localhost:8000/process \
  -d '{"command":["sh","-lc","echo actor-ready && uname -a"],"timeout":"10s"}'
```

Allowed egress should go through `atenet-egress` and receive gateway-injected
credentials:

```sh
curl -sS \
  -H 'Host: egress-sandbox.actors.resources.substrate.ate.dev' \
  -H 'Content-Type: application/json' \
  http://localhost:8000/process \
  -d '{"command":["sh","-lc","wget -S -O- --timeout=10 https://api.openai.com/v1/models 2>&1 | head -40"],"timeout":"15s"}'
```

Default-deny should reject an unlisted destination:

```sh
curl -sS \
  -H 'Host: egress-sandbox.actors.resources.substrate.ate.dev' \
  -H 'Content-Type: application/json' \
  http://localhost:8000/process \
  -d '{"command":["sh","-lc","wget -S -O- --timeout=5 https://example.com 2>&1; echo exit:$?"],"timeout":"10s"}'
```

## Debugging

If allowed egress returns `401`, the request reached OpenAI but the PEP did not
inject the expected credential. Verify the secret and restart the xDS server and
PEP:

```sh
kubectl -n ate-system get secret openai-api-key
kubectl -n ate-system logs deployment/prototype-controlplane --tail=100
kubectl -n ate-system rollout restart deployment/prototype-controlplane
kubectl -n ate-system rollout restart deployment/atenet-egress
```

If unlisted destinations succeed, the actor probably was not tunneled through
`atenet-egress`. Check the target selector and recreate the actor:

```sh
kubectl -n ate-system get configmap ate-api-server-envvars \
  -o jsonpath='{.data.ATE_API_EGRESS_TUNNEL_ACTOR_TEMPLATES}{"\n"}'
kubectl -n ate-system logs deployment/ate-api-server-deployment -c ate-api-server --tail=100

kubectl ate delete actor egress-sandbox || true
kubectl ate create actor egress-sandbox --template ate-demo-sandbox/sandbox-template
```

Inspect PEP and xDS logs:

```sh
kubectl -n ate-system logs deployment/atenet-egress -c agentgateway --tail=200
kubectl -n ate-system logs deployment/prototype-controlplane --tail=200
```

## Cleanup

```sh
kubectl ate suspend actor egress-sandbox || true
kubectl ate delete actor egress-sandbox || true
hack/install-ate.sh --delete-demo-sandbox

kill "${PF_ROUTER_PID}" 2>/dev/null || true
```
