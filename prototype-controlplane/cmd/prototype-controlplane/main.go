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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/agent-substrate/substrate/prototype-controlplane/api"
	"github.com/agent-substrate/substrate/prototype-controlplane/xds"
	agwapi "github.com/agentgateway/agentgateway/api"
	"google.golang.org/grpc"
)

type config struct {
	XDSListenAddr     string
	MetricsListenAddr string
	PolicyFile        string
	CredentialsDir    string
	BindPort          uint32
	GatewayNamespace  string
	GatewayName       string
	TLSCertFile       string
	TLSKeyFile        string
}

func main() {
	if err := run(); err != nil {
		slog.Error("prototype-controlplane failed", slog.Any("err", err))
		os.Exit(1)
	}
}

func run() error {
	cfg := config{
		XDSListenAddr:     envOrDefault("PROTOTYPE_CONTROLPLANE_XDS_ADDR", ":15010"),
		MetricsListenAddr: envOrDefault("PROTOTYPE_CONTROLPLANE_METRICS_ADDR", ":8080"),
		PolicyFile:        envOrDefault("PROTOTYPE_CONTROLPLANE_POLICY_FILE", "/etc/prototype-controlplane/policy.json"),
		CredentialsDir:    envOrDefault("PROTOTYPE_CONTROLPLANE_CREDENTIALS_DIR", "/etc/prototype-controlplane/credentials"),
		BindPort:          15080,
		GatewayNamespace:  envOrDefault("PROTOTYPE_CONTROLPLANE_GATEWAY_NAMESPACE", "ate-system"),
		GatewayName:       envOrDefault("PROTOTYPE_CONTROLPLANE_GATEWAY_NAME", "atenet-egress"),
		TLSCertFile:       os.Getenv("PROTOTYPE_CONTROLPLANE_TLS_CERT_FILE"),
		TLSKeyFile:        os.Getenv("PROTOTYPE_CONTROLPLANE_TLS_KEY_FILE"),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	snapshot, err := loadSnapshot(cfg.PolicyFile)
	if err != nil {
		return err
	}
	if err := resolveCredentialRefs(snapshot, cfg.CredentialsDir); err != nil {
		return err
	}
	tlsConfig, err := loadTLSConfig(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return err
	}
	source := api.NewMemorySource(snapshot)
	server := xds.NewServer(cfg.BindPort, cfg.GatewayNamespace, cfg.GatewayName, tlsConfig)
	if err := server.Initialize(ctx, source); err != nil {
		return err
	}

	grpcServer := grpc.NewServer()
	server.Register(grpcServer)

	lis, err := net.Listen("tcp", cfg.XDSListenAddr)
	if err != nil {
		return fmt.Errorf("listen xds: %w", err)
	}

	stopCh := make(chan struct{})
	defer close(stopCh)
	server.Start(stopCh)
	go server.Run(ctx, source)
	go startHealthServer(cfg.MetricsListenAddr)

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	slog.Info("prototype-controlplane serving xDS", slog.String("address", cfg.XDSListenAddr))
	if err := grpcServer.Serve(lis); err != nil {
		return fmt.Errorf("serve xds: %w", err)
	}
	return nil
}

func loadTLSConfig(certFile, keyFile string) (*agwapi.TLSConfig, error) {
	if certFile == "" && keyFile == "" {
		return nil, nil
	}
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("both TLS cert and key files are required when configuring gateway TLS")
	}
	cert, err := os.ReadFile(certFile)
	if err != nil {
		if os.IsNotExist(err) {
			if _, keyErr := os.Stat(keyFile); os.IsNotExist(keyErr) {
				slog.Warn("gateway TLS files not found; serving listener without inline TLS certificate", slog.String("cert-file", certFile), slog.String("key-file", keyFile))
				return nil, nil
			}
		}
		return nil, fmt.Errorf("read TLS cert file %q: %w", certFile, err)
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read TLS key file %q: %w", keyFile, err)
	}
	return &agwapi.TLSConfig{
		Cert:       cert,
		PrivateKey: key,
		MtlsMode:   agwapi.TLSConfig_DISABLE,
	}, nil
}

func loadSnapshot(path string) (*api.Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open policy file %q: %w", path, err)
	}
	defer f.Close()

	var snapshot api.Snapshot
	if err := json.NewDecoder(f).Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("decode policy file %q: %w", path, err)
	}
	return &snapshot, nil
}

func resolveCredentialRefs(snapshot *api.Snapshot, dir string) error {
	if snapshot == nil || dir == "" {
		return nil
	}
	for i := range snapshot.Policies {
		for j := range snapshot.Policies[i].Spec.Allow {
			inject := snapshot.Policies[i].Spec.Allow[j].Inject
			if inject == nil {
				continue
			}
			for k := range inject.Headers {
				ref := inject.Headers[k].ValueFrom
				if ref == "" {
					continue
				}
				value, ok, err := readCredentialRef(dir, ref)
				if err != nil {
					return err
				}
				if !ok {
					slog.Warn("credential ref not resolved; header injection will use unresolved placeholder", slog.String("ref", ref))
					continue
				}
				inject.Headers[k].Value = value
			}
		}
	}
	return nil
}

func readCredentialRef(dir, ref string) (string, bool, error) {
	if filepath.IsAbs(ref) || strings.Contains(ref, "..") {
		return "", false, fmt.Errorf("invalid credential ref %q", ref)
	}
	path := filepath.Join(dir, filepath.Clean(ref))
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("stat credential ref %q: %w", ref, err)
	}
	if info.IsDir() {
		path = filepath.Join(path, "authorization")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read credential ref %q: %w", ref, err)
	}
	return strings.TrimSpace(string(value)), true, nil
}

func startHealthServer(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	if err := http.ListenAndServe(addr, mux); err != nil && err != http.ErrServerClosed {
		slog.Error("health server failed", slog.Any("err", err))
	}
}

func envOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
