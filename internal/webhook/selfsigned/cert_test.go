/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package selfsigned

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGenerate_DefaultsToOneYearValidity(t *testing.T) {
	b, err := Generate(Input{Namespace: "jaas-system"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := time.Until(b.NotAfter)
	if got < 364*24*time.Hour || got > 366*24*time.Hour {
		t.Errorf("NotAfter is %v from now, want ~1 year", got)
	}
}

func TestGenerate_HonorsCustomValidity(t *testing.T) {
	in := Input{Namespace: "jaas-system", Validity: 30 * 24 * time.Hour}
	b, err := Generate(in)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := time.Until(b.NotAfter)
	if got < 29*24*time.Hour || got > 31*24*time.Hour {
		t.Errorf("NotAfter is %v from now, want ~30 days", got)
	}
}

func TestGenerate_PinnedNotBeforeRespected(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	b, err := Generate(Input{
		Namespace: "jaas-system",
		Validity:  100 * 24 * time.Hour,
		NotBefore: start,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	wantExpiry := start.Add(100 * 24 * time.Hour)
	if !b.NotAfter.Equal(wantExpiry) {
		t.Errorf("NotAfter = %v, want %v", b.NotAfter, wantExpiry)
	}
}

func TestGenerate_RejectsEmptyNamespace(t *testing.T) {
	if _, err := Generate(Input{}); err == nil {
		t.Error("expected error for empty Namespace")
	}
}

func TestGenerate_DefaultsServiceName(t *testing.T) {
	b, err := Generate(Input{Namespace: "jaas-system"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	cert := parseCert(t, b.CertPEM)
	wantDNS := "jaas-webhook.jaas-system.svc"
	found := slices.Contains(cert.DNSNames, wantDNS)
	if !found {
		t.Errorf("DNS SANs %v missing default %q", cert.DNSNames, wantDNS)
	}
}

func TestGenerate_DNSNamesIncludeClusterLocal(t *testing.T) {
	b, err := Generate(Input{Namespace: "jaas-system", ServiceName: "jaas-webhook"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	cert := parseCert(t, b.CertPEM)
	wantBoth := []string{
		"jaas-webhook.jaas-system.svc",
		"jaas-webhook.jaas-system.svc.cluster.local",
	}
	for _, want := range wantBoth {
		found := slices.Contains(cert.DNSNames, want)
		if !found {
			t.Errorf("DNS SAN %q missing from %v", want, cert.DNSNames)
		}
	}
}

func TestGenerate_ServingCertChainsToCA(t *testing.T) {
	b, err := Generate(Input{Namespace: "ns"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	cert := parseCert(t, b.CertPEM)
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(b.CABundle) {
		t.Fatal("CABundle did not parse")
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     caPool,
		DNSName:   "jaas-webhook.ns.svc",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("serving cert does not verify against CA: %v", err)
	}
}

func TestGenerate_KeyPairLoadsAsTLS(t *testing.T) {
	b, err := Generate(Input{Namespace: "ns"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := tls.X509KeyPair(b.CertPEM, b.KeyPEM); err != nil {
		t.Errorf("tls.X509KeyPair: %v", err)
	}
}

func TestGenerate_TwoRunsProduceDifferentKeys(t *testing.T) {
	a, err := Generate(Input{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(Input{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	if string(a.KeyPEM) == string(b.KeyPEM) {
		t.Error("two generations produced identical keys (RNG broken?)")
	}
}

func TestBundle_WriteTo_WritesTLSFiles(t *testing.T) {
	dir := t.TempDir()
	b, err := Generate(Input{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.WriteTo(dir); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	for _, want := range []string{"tls.crt", "tls.key"} {
		info, err := os.Stat(filepath.Join(dir, want))
		if err != nil {
			t.Errorf("missing %s: %v", want, err)
			continue
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", want, info.Mode().Perm())
		}
	}
	crt, _ := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if !strings.HasPrefix(string(crt), "-----BEGIN CERTIFICATE-----") {
		t.Errorf("tls.crt does not look PEM-encoded")
	}
}

func TestBundle_WriteTo_NonexistentDirErrors(t *testing.T) {
	b, err := Generate(Input{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.WriteTo("/this/path/should/not/exist"); err == nil {
		t.Error("expected error writing to nonexistent dir")
	}
}

func parseCert(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("PEM decode failed")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return cert
}
