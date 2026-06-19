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
	"fmt"
	"sort"
	"strings"

	agwapi "github.com/agentgateway/agentgateway/api"

	"github.com/agent-substrate/substrate/prototype-controlplane/api"
)

func BuildResources(snapshot *api.Snapshot) []xdsResource {
	if snapshot == nil {
		return nil
	}
	destinations := map[string]allowedDestination{}
	for _, policy := range snapshot.Policies {
		if !policy.Spec.Transparent || policy.Spec.DefaultAction != api.DefaultActionDeny {
			continue
		}
		for _, rule := range policy.Spec.Allow {
			for _, dest := range rule.To {
				for _, port := range dest.Ports {
					if !strings.EqualFold(port.Protocol, "TCP") {
						continue
					}
					key := destinationKey(dest.Host, port.Port)
					destinations[key] = allowedDestination{
						Host:             dest.Host,
						Port:             port.Port,
						Inject:           rule.Inject,
						UpstreamTLS:      dest.TLS,
						TLSBumpEnabled:   policy.Spec.TLSBump != nil && policy.Spec.TLSBump.Enabled,
						DynamicCARef:     dynamicCARef(policy.Spec.TLSBump),
						SourcePolicyName: policy.Name,
						SourcePolicyNS:   policy.Namespace,
						SourceRuleName:   rule.Name,
						GatewayClassName: policy.Spec.GatewayClassName,
						TargetRefKind:    policy.Spec.TargetRef.Kind,
						TargetRefName:    policy.Spec.TargetRef.Name,
					}
				}
			}
		}
	}

	keys := make([]string, 0, len(destinations))
	for key := range destinations {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	resources := make([]xdsResource, 0, len(keys)*2)
	for _, key := range keys {
		dest := destinations[key]
		backendKey := backendKey(dest.Host, dest.Port)
		resources = append(resources, makeDestinationBackend(backendKey, dest))
		if dest.TLSBumpEnabled {
			resources = append(resources, makeDestinationHTTPRoute(routeKey(dest.Host, dest.Port), backendKey, dest))
		} else {
			resources = append(resources, makeDestinationTCPRoute(routeKey(dest.Host, dest.Port), backendKey, dest))
		}
	}
	return resources
}

type allowedDestination struct {
	Host             string
	Port             uint32
	Inject           *api.CredentialInjection
	UpstreamTLS      *api.UpstreamTLS
	TLSBumpEnabled   bool
	DynamicCARef     string
	SourcePolicyName string
	SourcePolicyNS   string
	SourceRuleName   string
	GatewayClassName string
	TargetRefKind    string
	TargetRefName    string
}

func makeBind(port uint32) xdsResource {
	return xdsResource{
		Key: bindKey,
		Proto: &agwapi.Resource{
			Kind: &agwapi.Resource_Bind{
				Bind: &agwapi.Bind{
					Key:            bindKey,
					Port:           port,
					Protocol:       agwapi.Bind_TLS,
					TunnelProtocol: agwapi.Bind_PROXY,
				},
			},
		},
	}
}

func makeListener(gatewayNamespace, gatewayName string, tls *agwapi.TLSConfig) xdsResource {
	if tls == nil {
		tls = &agwapi.TLSConfig{
			CertificateSource: agwapi.TLSConfig_ISTIO_WORKLOAD,
			MtlsMode:          agwapi.TLSConfig_DISABLE,
		}
	}
	return xdsResource{
		Key: listenerKey,
		Proto: &agwapi.Resource{
			Kind: &agwapi.Resource_Listener{
				Listener: &agwapi.Listener{
					Key:     listenerKey,
					BindKey: bindKey,
					Name: &agwapi.ListenerName{
						GatewayName:      gatewayName,
						GatewayNamespace: gatewayNamespace,
						ListenerName:     "tcp",
					},
					Protocol: agwapi.Protocol_HTTPS,
					Tls:      tls,
				},
			},
		},
	}
}

func makeDestinationHTTPRoute(key, backendKey string, dest allowedDestination) xdsResource {
	return xdsResource{
		Key: key,
		Proto: &agwapi.Resource{
			Kind: &agwapi.Resource_Route{
				Route: &agwapi.Route{
					Key:         key,
					ListenerKey: listenerKey,
					Name: &agwapi.RouteName{
						Kind:      "EgressPolicy",
						Name:      routeResourceName(dest),
						Namespace: namespaceOrDefault(dest.SourcePolicyNS),
						RuleName:  stringPtr(dest.SourceRuleName),
					},
					Hostnames: []string{dest.Host},
					Matches: []*agwapi.RouteMatch{{
						Path: &agwapi.PathMatch{Kind: &agwapi.PathMatch_PathPrefix{PathPrefix: "/"}},
					}},
					Backends: []*agwapi.RouteBackend{{Backend: &agwapi.BackendReference{Kind: &agwapi.BackendReference_Backend{Backend: backendKey}}, Weight: 1, BackendPolicies: backendPolicies(dest)}},
				},
			},
		},
	}
}

func makeDestinationTCPRoute(key, backendKey string, dest allowedDestination) xdsResource {
	return xdsResource{
		Key: key,
		Proto: &agwapi.Resource{
			Kind: &agwapi.Resource_TcpRoute{
				TcpRoute: &agwapi.TCPRoute{
					Key:         key,
					ListenerKey: listenerKey,
					Name: &agwapi.RouteName{
						Kind:      "EgressPolicy",
						Name:      routeResourceName(dest),
						Namespace: namespaceOrDefault(dest.SourcePolicyNS),
						RuleName:  stringPtr(dest.SourceRuleName),
					},
					Hostnames: []string{dest.Host},
					Backends: []*agwapi.RouteBackend{{
						Backend: &agwapi.BackendReference{
							Kind: &agwapi.BackendReference_Backend{
								Backend: backendKey,
							},
						},
						Weight:          1,
						BackendPolicies: backendPolicies(dest),
					}},
				},
			},
		},
	}
}

func makeDestinationBackend(key string, dest allowedDestination) xdsResource {
	return xdsResource{
		Key: key,
		Proto: &agwapi.Resource{
			Kind: &agwapi.Resource_Backend{
				Backend: &agwapi.Backend{
					Key: key,
					Name: &agwapi.ResourceName{
						Name:      sanitizeName(dest.Host),
						Namespace: namespaceOrDefault(dest.SourcePolicyNS),
					},
					Kind: &agwapi.Backend_Static{
						Static: &agwapi.StaticBackend{
							Host: dest.Host,
							Port: int32(dest.Port),
						},
					},
				},
			},
		},
	}
}

func backendPolicies(dest allowedDestination) []*agwapi.BackendPolicySpec {
	var policies []*agwapi.BackendPolicySpec
	if tls := backendTLSPolicy(dest); tls != nil {
		policies = append(policies, tls)
	}
	if auth := backendAuthPolicy(dest.Inject); auth != nil {
		policies = append(policies, auth)
	}
	return policies
}

func backendTLSPolicy(dest allowedDestination) *agwapi.BackendPolicySpec {
	originate := dest.TLSBumpEnabled
	if dest.UpstreamTLS != nil && dest.UpstreamTLS.Originate {
		originate = true
	}
	if !originate {
		return nil
	}
	tls := &agwapi.BackendPolicySpec_BackendTLS{
		Verification: agwapi.BackendPolicySpec_BackendTLS_STRICT,
	}
	if dest.UpstreamTLS != nil {
		tls.Root = dest.UpstreamTLS.RootPEM
		if dest.UpstreamTLS.SNI != "" {
			tls.Hostname = stringPtr(dest.UpstreamTLS.SNI)
		}
	}
	if tls.Hostname == nil {
		tls.Hostname = stringPtr(dest.Host)
	}
	return &agwapi.BackendPolicySpec{
		Kind: &agwapi.BackendPolicySpec_BackendTls{
			BackendTls: tls,
		},
	}
}

func backendAuthPolicy(inject *api.CredentialInjection) *agwapi.BackendPolicySpec {
	if inject == nil || len(inject.Headers) == 0 {
		return nil
	}
	for _, header := range inject.Headers {
		if header.Name == "" {
			continue
		}
		value := header.Value
		if value == "" || strings.HasPrefix(value, "credential://") {
			continue
		}
		prefix := ""
		if strings.HasPrefix(strings.ToLower(value), "bearer ") {
			value = strings.TrimSpace(value[len("Bearer "):])
			prefix = "Bearer "
		}
		return &agwapi.BackendPolicySpec{
			Kind: &agwapi.BackendPolicySpec_Auth{
				Auth: &agwapi.BackendAuthPolicy{
					Kind: &agwapi.BackendAuthPolicy_Key{
						Key: &agwapi.Key{
							Secret: value,
							AuthorizationLocation: &agwapi.AuthorizationLocation{
								Kind: &agwapi.AuthorizationLocation_Header_{
									Header: &agwapi.AuthorizationLocation_Header{
										Name:   strings.ToLower(header.Name),
										Prefix: stringPtr(prefix),
									},
								},
							},
						},
					},
				},
			},
		}
	}
	return nil
}

func routeKey(host string, port uint32) string {
	return "substrate-egress/route/" + destinationKey(host, port)
}

func backendKey(host string, port uint32) string {
	return "substrate-egress/backend/" + destinationKey(host, port)
}

func destinationKey(host string, port uint32) string {
	return sanitizeName(host) + "/" + fmt.Sprintf("%d", port)
}

func dynamicCARef(cfg *api.TLSBumpConfig) string {
	if cfg == nil {
		return ""
	}
	return cfg.DynamicCA.Ref
}

func routeResourceName(dest allowedDestination) string {
	if dest.SourcePolicyName == "" {
		return sanitizeName(dest.Host)
	}
	return sanitizeName(dest.SourcePolicyName + "-" + dest.SourceRuleName)
}

func namespaceOrDefault(namespace string) string {
	if namespace == "" {
		return "default"
	}
	return namespace
}

func sanitizeName(name string) string {
	replacer := strings.NewReplacer(".", "-", "_", "-", ":", "-", "/", "-")
	return strings.Trim(replacer.Replace(strings.ToLower(name)), "-")
}

func stringPtr(s string) *string {
	return &s
}
