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
	"encoding/binary"
	"net"
	"testing"
)

func TestBuildProxyProtocolV2HeaderIncludesDestinationAndIdentity(t *testing.T) {
	header, err := buildProxyProtocolV2Header(
		&net.TCPAddr{IP: net.IPv4(10, 1, 2, 3), Port: 43210},
		&net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 443},
		egressIdentity{
			ActorTemplateNamespace: "ate-demo-sandbox",
			ActorTemplateName:      "sandbox-template",
			ActorID:                "egress-sandbox",
		},
	)
	if err != nil {
		t.Fatalf("buildProxyProtocolV2Header() error = %v", err)
	}

	wantSignature := []byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A}
	if got := header[:12]; string(got) != string(wantSignature) {
		t.Fatalf("signature = %v, want %v", got, wantSignature)
	}
	if got, want := header[12], byte(0x21); got != want {
		t.Fatalf("version/command = %#x, want %#x", got, want)
	}
	if got, want := header[13], byte(0x11); got != want {
		t.Fatalf("family/protocol = %#x, want %#x", got, want)
	}
	if got, want := net.IP(header[16:20]).String(), "10.1.2.3"; got != want {
		t.Fatalf("source IP = %q, want %q", got, want)
	}
	if got, want := net.IP(header[20:24]).String(), "203.0.113.7"; got != want {
		t.Fatalf("destination IP = %q, want %q", got, want)
	}
	if got, want := binary.BigEndian.Uint16(header[24:26]), uint16(43210); got != want {
		t.Fatalf("source port = %d, want %d", got, want)
	}
	if got, want := binary.BigEndian.Uint16(header[26:28]), uint16(443); got != want {
		t.Fatalf("destination port = %d, want %d", got, want)
	}
	if got, want := header[28], byte(proxyProtocolAuthorityTLV); got != want {
		t.Fatalf("TLV type = %#x, want %#x", got, want)
	}
	tlvLen := int(binary.BigEndian.Uint16(header[29:31]))
	if got, want := string(header[31:31+tlvLen]), "spiffe://substrate.local/ns/ate-demo-sandbox/sa/sandbox-template.egress-sandbox"; got != want {
		t.Fatalf("identity TLV = %q, want %q", got, want)
	}
	if got, want := int(binary.BigEndian.Uint16(header[14:16])), 12+3+tlvLen; got != want {
		t.Fatalf("address length = %d, want %d", got, want)
	}
}

func TestEgressIdentitySPIFFEURIRequiresCompleteIdentity(t *testing.T) {
	if got := egressIdentitySPIFFEURI(egressIdentity{}); got != "" {
		t.Fatalf("egressIdentitySPIFFEURI(empty) = %q, want empty", got)
	}
}
