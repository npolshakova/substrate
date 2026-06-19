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
	"context"
	"fmt"

	agwapi "github.com/agentgateway/agentgateway/api"
	"github.com/agentgateway/agentgateway/controller/pkg/pluginsdk/krtutil"
	"github.com/agentgateway/agentgateway/controller/pkg/syncer/krtxds"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"istio.io/istio/pkg/kube/krt"
	workloadapi "istio.io/istio/pkg/workloadapi"

	"github.com/agent-substrate/substrate/prototype-controlplane/api"
)

const (
	bindKey     = "substrate-egress/bind"
	listenerKey = "substrate-egress/listener"
)

type xdsResource struct {
	Key   string
	Proto *agwapi.Resource
}

func (r xdsResource) ResourceName() string { return r.Key }
func (r xdsResource) Equals(other xdsResource) bool {
	return r.Key == other.Key && proto.Equal(r.Proto, other.Proto)
}
func (r xdsResource) IntoProto() *agwapi.Resource { return r.Proto }

type addressResource struct{}

func (a addressResource) ResourceName() string              { return "" }
func (a addressResource) Equals(other addressResource) bool { return true }
func (a addressResource) IntoProto() *workloadapi.Address   { return &workloadapi.Address{} }

type Server struct {
	ds              *krtxds.DiscoveryServer
	resources       krt.StaticCollection[xdsResource]
	staticResources []xdsResource
}

func NewServer(bindPort uint32, gatewayNamespace, gatewayName string, tls *agwapi.TLSConfig) *Server {
	krtopts := krtutil.KrtOptions{}
	if gatewayNamespace == "" {
		gatewayNamespace = "ate-system"
	}
	if gatewayName == "" {
		gatewayName = "atenet-egress"
	}
	staticResources := []xdsResource{
		makeBind(bindPort),
		makeListener(gatewayNamespace, gatewayName, tls),
	}
	resources := krt.NewStaticCollection[xdsResource](nil, append([]xdsResource(nil), staticResources...), krtopts.ToOptions("substrate-egress/resources")...)
	emptyAddresses := krt.NewStaticCollection[addressResource](nil, nil, krtopts.ToOptions("substrate-egress/addresses")...)
	ds := krtxds.NewDiscoveryServer(nil, nil,
		krtxds.Collection[xdsResource, *agwapi.Resource](resources, krtopts),
		krtxds.Collection[addressResource, *workloadapi.Address](emptyAddresses, krtopts),
	)

	return &Server{
		ds:              ds,
		resources:       resources,
		staticResources: staticResources,
	}
}

func (s *Server) Initialize(ctx context.Context, source api.SnapshotSource) error {
	snapshot, err := source.GetSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("get egress policy snapshot: %w", err)
	}
	s.applySnapshot(snapshot)
	return nil
}

func (s *Server) Run(ctx context.Context, source api.SnapshotSource) {
	updates := source.WatchSnapshot(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case snapshot, ok := <-updates:
			if !ok {
				return
			}
			s.applySnapshot(snapshot)
		}
	}
}

func (s *Server) Start(stop <-chan struct{}) {
	s.ds.Start(stop)
	s.ds.EnsureSynced()
}

func (s *Server) Register(srv *grpc.Server) {
	discovery.RegisterAggregatedDiscoveryServiceServer(srv, s.ds)
}

func (s *Server) applySnapshot(snapshot *api.Snapshot) {
	next := append([]xdsResource(nil), s.staticResources...)
	next = append(next, BuildResources(snapshot)...)
	s.resources.Reset(next)
}
