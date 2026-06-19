//go:build linux

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
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"unsafe"

	"github.com/agent-substrate/substrate/internal/proto/ateompb"
	"golang.org/x/sys/unix"
)

const proxyProtocolAuthorityTLV = 0xD0

type egressIdentity struct {
	ActorID                string
	ActorTemplateNamespace string
	ActorTemplateName      string
}

type egressTunnelOptions struct {
	Identity          egressIdentity
	GatewayAddress    string
	LocalRedirectPort uint16
}

type egressTunnel struct {
	listener net.Listener
	cancel   context.CancelFunc
	done     chan struct{}
	once     sync.Once
}

func egressIdentityFromRun(req *ateompb.RunWorkloadRequest) egressIdentity {
	return egressIdentity{
		ActorID:                req.GetActorId(),
		ActorTemplateNamespace: req.GetActorTemplateNamespace(),
		ActorTemplateName:      req.GetActorTemplateName(),
	}
}

func egressIdentityFromRestore(req *ateompb.RestoreWorkloadRequest) egressIdentity {
	return egressIdentity{
		ActorID:                req.GetActorId(),
		ActorTemplateNamespace: req.GetActorTemplateNamespace(),
		ActorTemplateName:      req.GetActorTemplateName(),
	}
}

func startEgressTunnel(ctx context.Context, opts egressTunnelOptions) (*egressTunnel, error) {
	if opts.GatewayAddress == "" {
		return nil, fmt.Errorf("egress tunnel gateway address is required")
	}
	if opts.LocalRedirectPort == 0 {
		return nil, fmt.Errorf("egress tunnel local redirect port is required")
	}

	addr := net.JoinHostPort(localhostIPv4, fmt.Sprintf("%d", opts.LocalRedirectPort))
	lc := net.ListenConfig{}
	lis, err := lc.Listen(ctx, "tcp4", addr)
	if err != nil {
		return nil, fmt.Errorf("while listening for redirected egress on %s: %w", addr, err)
	}

	t := &egressTunnel{
		listener: lis,
		cancel:   func() {},
		done:     make(chan struct{}),
	}
	tunnelCtx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	go t.acceptLoop(tunnelCtx, opts)
	return t, nil
}

func (t *egressTunnel) Close() error {
	var err error
	t.once.Do(func() {
		t.cancel()
		err = t.listener.Close()
		<-t.done
	})
	return err
}

func (t *egressTunnel) acceptLoop(ctx context.Context, opts egressTunnelOptions) {
	defer close(t.done)
	for {
		conn, err := t.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if strings.Contains(err.Error(), "use of closed network connection") {
				return
			}
			slog.WarnContext(ctx, "Failed to accept redirected egress connection", slog.Any("err", err))
			continue
		}
		go handleEgressTunnelConn(ctx, conn, opts)
	}
}

func handleEgressTunnelConn(ctx context.Context, conn net.Conn, opts egressTunnelOptions) {
	defer conn.Close()

	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		slog.WarnContext(ctx, "Redirected egress connection is not TCP")
		return
	}

	originalDst, err := originalDestination(tcpConn)
	if err != nil {
		slog.WarnContext(ctx, "Failed to read original destination for redirected egress", slog.Any("err", err))
	} else {
		slog.DebugContext(ctx, "Proxying redirected actor egress", slog.String("original-destination", originalDst), slog.String("gateway", opts.GatewayAddress))
	}

	gatewayConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", opts.GatewayAddress)
	if err != nil {
		slog.WarnContext(ctx, "Failed to connect to egress gateway", slog.Any("err", err), slog.String("gateway", opts.GatewayAddress))
		return
	}
	defer gatewayConn.Close()

	if err := writeEgressProxyProtocolHeader(gatewayConn, tcpConn, originalDst, opts.Identity); err != nil {
		slog.WarnContext(ctx, "Failed to send egress tunnel metadata to gateway", slog.Any("err", err), slog.String("original-destination", originalDst))
		return
	}

	proxyTCP(tcpConn, gatewayConn)
}

func writeEgressProxyProtocolHeader(dst io.Writer, actorConn *net.TCPConn, originalDst string, identity egressIdentity) error {
	srcAddr, ok := actorConn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("actor connection remote address is %T, want *net.TCPAddr", actorConn.RemoteAddr())
	}
	originalDstAddr, err := net.ResolveTCPAddr("tcp", originalDst)
	if err != nil {
		return fmt.Errorf("while parsing original destination %q: %w", originalDst, err)
	}

	header, err := buildProxyProtocolV2Header(srcAddr, originalDstAddr, identity)
	if err != nil {
		return err
	}
	if _, err := dst.Write(header); err != nil {
		return fmt.Errorf("while writing PROXY protocol header: %w", err)
	}
	return nil
}

func buildProxyProtocolV2Header(src, dst *net.TCPAddr, identity egressIdentity) ([]byte, error) {
	srcIP := src.IP.To4()
	if srcIP == nil {
		return nil, fmt.Errorf("source address %s is not IPv4", src.IP.String())
	}
	dstIP := dst.IP.To4()
	if dstIP == nil {
		return nil, fmt.Errorf("destination address %s is not IPv4", dst.IP.String())
	}
	if src.Port < 0 || src.Port > 65535 {
		return nil, fmt.Errorf("source port %d out of range", src.Port)
	}
	if dst.Port < 0 || dst.Port > 65535 {
		return nil, fmt.Errorf("destination port %d out of range", dst.Port)
	}

	identityURI := egressIdentitySPIFFEURI(identity)
	tlvLen := 0
	if identityURI != "" {
		if len(identityURI) > 65535 {
			return nil, fmt.Errorf("identity URI is too long for PROXY protocol TLV")
		}
		tlvLen = 3 + len(identityURI)
	}
	addrLen := 12 + tlvLen
	if addrLen > 65535 {
		return nil, fmt.Errorf("PROXY protocol address block is too long")
	}

	header := make([]byte, 16+addrLen)
	copy(header[:12], []byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A})
	header[12] = 0x21 // version 2, PROXY command.
	header[13] = 0x11 // TCP over IPv4.
	binary.BigEndian.PutUint16(header[14:16], uint16(addrLen))
	copy(header[16:20], srcIP)
	copy(header[20:24], dstIP)
	binary.BigEndian.PutUint16(header[24:26], uint16(src.Port))
	binary.BigEndian.PutUint16(header[26:28], uint16(dst.Port))
	if identityURI != "" {
		header[28] = proxyProtocolAuthorityTLV
		binary.BigEndian.PutUint16(header[29:31], uint16(len(identityURI)))
		copy(header[31:], identityURI)
	}
	return header, nil
}

func egressIdentitySPIFFEURI(identity egressIdentity) string {
	if identity.ActorTemplateNamespace == "" || identity.ActorTemplateName == "" || identity.ActorID == "" {
		return ""
	}
	return fmt.Sprintf("spiffe://substrate.local/ns/%s/sa/%s.%s", identity.ActorTemplateNamespace, identity.ActorTemplateName, identity.ActorID)
}

func originalDestination(conn *net.TCPConn) (string, error) {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return "", fmt.Errorf("while getting raw connection: %w", err)
	}

	var sockaddr unix.RawSockaddrInet4
	var sockErr error
	if err := rawConn.Control(func(fd uintptr) {
		size := uint32(unsafe.Sizeof(sockaddr))
		_, _, errno := unix.Syscall6(
			unix.SYS_GETSOCKOPT,
			fd,
			uintptr(unix.SOL_IP),
			uintptr(unix.SO_ORIGINAL_DST),
			uintptr(unsafe.Pointer(&sockaddr)),
			uintptr(unsafe.Pointer(&size)),
			0,
		)
		if errno != 0 {
			sockErr = errno
		}
	}); err != nil {
		return "", fmt.Errorf("while reading raw connection: %w", err)
	}
	if sockErr != nil {
		return "", fmt.Errorf("getsockopt SO_ORIGINAL_DST: %w", sockErr)
	}

	portBytes := (*[2]byte)(unsafe.Pointer(&sockaddr.Port))
	ip := net.IPv4(sockaddr.Addr[0], sockaddr.Addr[1], sockaddr.Addr[2], sockaddr.Addr[3])
	port := binary.BigEndian.Uint16(portBytes[:])
	return net.JoinHostPort(ip.String(), fmt.Sprintf("%d", port)), nil
}

func proxyTCP(a *net.TCPConn, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		if closer, ok := b.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		} else {
			_ = b.Close()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		_ = a.CloseWrite()
	}()
	wg.Wait()
}
