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

package api

import "context"

type DefaultAction string

const (
	DefaultActionDeny  DefaultAction = "Deny"
	DefaultActionAllow DefaultAction = "Allow"
)

type EgressPolicy struct {
	Namespace string
	Name      string
	Spec      EgressPolicySpec
}

type EgressPolicySpec struct {
	TargetRef        TargetRef
	GatewayClassName string
	Transparent      bool
	DefaultAction    DefaultAction
	TLSBump          *TLSBumpConfig
	Allow            []AllowRule
}

type TLSBumpConfig struct {
	Enabled   bool
	DynamicCA DynamicCAConfig
}

type DynamicCAConfig struct {
	// Ref names the control-plane CA material the PEP uses to mint per-host
	// certificates. The raw CA key never appears in actor-visible config.
	Ref string
}

type TargetRef struct {
	Kind string
	Name string
}

type AllowRule struct {
	Name string
	To   []Destination
	// Inject applies only when this rule and destination match.
	Inject *CredentialInjection
}

type Destination struct {
	Host string
	// Ports is an explicit allow list. With DefaultActionDeny, destinations not
	// present here produce no xDS route and are denied by absence.
	Ports []Port
	TLS   *UpstreamTLS
}

type Port struct {
	Port     uint32
	Protocol string
}

type UpstreamTLS struct {
	Originate bool
	SNI       string
	RootPEM   []byte
}

type CredentialInjection struct {
	Headers []InjectedHeader
}

type InjectedHeader struct {
	Name      string
	ValueFrom string
	Value     string
}

type Snapshot struct {
	Policies []EgressPolicy
}

type SnapshotSource interface {
	GetSnapshot(ctx context.Context) (*Snapshot, error)
	WatchSnapshot(ctx context.Context) <-chan *Snapshot
}
