// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package xds

import (
	"testing"

	agwapi "github.com/agentgateway/agentgateway/api"

	"github.com/agent-substrate/substrate/prototype-controlplane/api"
)

func TestBuildResourcesFromEgressPolicy(t *testing.T) {
	resources := BuildResources(&api.Snapshot{Policies: []api.EgressPolicy{{
		Namespace: "dev-agents",
		Name:      "coding-agent-egress",
		Spec: api.EgressPolicySpec{
			TargetRef: api.TargetRef{
				Kind: "ActorTemplate",
				Name: "coding-agent",
			},
			GatewayClassName: "agentgateway",
			Transparent:      true,
			DefaultAction:    api.DefaultActionDeny,
			TLSBump: &api.TLSBumpConfig{
				Enabled: true,
				DynamicCA: api.DynamicCAConfig{
					Ref: "substrate-egress-ca",
				},
			},
			Allow: []api.AllowRule{{
				Name: "openai-api",
				To: []api.Destination{{
					Host: "api.openai.com",
					Ports: []api.Port{{
						Port:     443,
						Protocol: "TCP",
					}},
				}},
				Inject: &api.CredentialInjection{Headers: []api.InjectedHeader{{
					Name:      "Authorization",
					ValueFrom: "openai-api-key",
					Value:     "Bearer test-key",
				}}},
			}},
		},
	}}})

	if got, want := len(resources), 2; got != want {
		t.Fatalf("len(resources) = %d, want %d", got, want)
	}
	if backend := resources[0].Proto.GetBackend(); backend == nil || backend.GetStatic().GetHost() != "api.openai.com" {
		t.Fatalf("first resource = %#v, want static backend for api.openai.com", resources[0].Proto)
	}
	if route := resources[1].Proto.GetRoute(); route == nil || route.GetHostnames()[0] != "api.openai.com" {
		t.Fatalf("second resource = %#v, want HTTP route for api.openai.com", resources[1].Proto)
	} else {
		if got := route.GetMatches()[0].GetPath().GetPathPrefix(); got != "/" {
			t.Fatalf("route path prefix = %q, want /", got)
		}
		policies := route.GetBackends()[0].GetBackendPolicies()
		if got, want := len(policies), 2; got != want {
			t.Fatalf("len(backend policies) = %d, want %d", got, want)
		}
		if policies[0].GetBackendTls().GetHostname() != "api.openai.com" {
			t.Fatalf("backend TLS hostname = %q, want api.openai.com", policies[0].GetBackendTls().GetHostname())
		}
		auth := policies[1].GetAuth().GetKey()
		if auth == nil || auth.GetSecret() != "test-key" {
			t.Fatalf("backend auth = %#v, want key secret without Bearer prefix", policies[1].GetAuth())
		}
		header := auth.GetAuthorizationLocation().GetHeader()
		if header == nil || header.GetName() != "authorization" || header.GetPrefix() != "Bearer " {
			t.Fatalf("backend auth location = %#v, want authorization header with Bearer prefix", auth.GetAuthorizationLocation())
		}
	}
}

func TestBuildResourcesSkipsNonTransparentOrAllowDefault(t *testing.T) {
	resources := BuildResources(&api.Snapshot{Policies: []api.EgressPolicy{
		{Spec: api.EgressPolicySpec{Transparent: false, DefaultAction: api.DefaultActionDeny}},
		{Spec: api.EgressPolicySpec{Transparent: true, DefaultAction: api.DefaultActionAllow}},
	}})
	if len(resources) != 0 {
		t.Fatalf("len(resources) = %d, want 0", len(resources))
	}
}

func TestMakeBindEnablesProxyProtocol(t *testing.T) {
	bind := makeBind(15080).Proto.GetBind()
	if bind == nil {
		t.Fatalf("makeBind returned %#v, want bind resource", bind)
	}
	if got, want := bind.GetTunnelProtocol(), agwapi.Bind_PROXY; got != want {
		t.Fatalf("tunnel protocol = %s, want %s", got, want)
	}
}
