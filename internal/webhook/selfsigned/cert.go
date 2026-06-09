/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

// Package selfsigned generates a CA + serving cert for the operator's
// validating webhook without depending on cert-manager. Pairs with
// VWCPatcher (in this package's caller) to inject the CA bundle into
// the ValidatingWebhookConfiguration so the apiserver trusts the
// operator's TLS handshake.
//
// The certs are valid for [Generate.Validity] (default 1 year). Pods
// regenerate on every restart, so operators get rotation for free
// across rollouts. Clusters that want shorter-lived material or
// cross-pod-shared certs should use cert-manager instead.
package selfsigned

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Input captures the per-cluster knobs Generate consumes. ServiceName and
// Namespace produce the standard DNS SANs an apiserver expects when
// dialing a Service-fronted webhook:
//
//	<service>.<namespace>.svc
//	<service>.<namespace>.svc.cluster.local
type Input struct {
	// ServiceName is the Service the webhook is reachable through.
	// Defaults to "jaas-webhook" when empty.
	ServiceName string

	// Namespace is the Service's namespace. Required.
	Namespace string

	// Validity is how long the issued cert is valid for. Defaults to
	// 365 days. The CA is generated with the same validity so the
	// chain stays consistent.
	Validity time.Duration

	// NotBefore lets tests pin the cert's start time. Production
	// passes the zero value, which Generate fills from time.Now().
	NotBefore time.Time
}

// Bundle holds the PEM-encoded material a self-signed install needs:
// the CA bundle (for apiserver trust), and the serving cert+key
// (for the webhook HTTPS server).
type Bundle struct {
	// CABundle is the CA cert in PEM form. Inject into every
	// ValidatingWebhookConfiguration.webhooks[*].clientConfig.caBundle
	// so the apiserver trusts the serving cert.
	CABundle []byte

	// CertPEM is the serving cert in PEM form. Controller-runtime
	// reads this from <cert-dir>/tls.crt.
	CertPEM []byte

	// KeyPEM is the serving key in PEM form. Controller-runtime reads
	// this from <cert-dir>/tls.key.
	KeyPEM []byte

	// NotAfter is the serving cert's expiry. Callers schedule rotation
	// against this — typically at NotAfter - 30 days.
	NotAfter time.Time
}

// Generate produces a new CA + serving cert pair signed by the CA. ECDSA
// P-256 keys keep the bundle small (~1 KB total PEM) compared to RSA
// without losing FIPS-acceptable security.
func Generate(in Input) (*Bundle, error) {
	if in.Namespace == "" {
		return nil, errors.New("selfsigned: Namespace is required")
	}
	if in.ServiceName == "" {
		in.ServiceName = "jaas-webhook"
	}
	if in.Validity == 0 {
		in.Validity = 365 * 24 * time.Hour
	}
	notBefore := in.NotBefore
	if notBefore.IsZero() {
		notBefore = time.Now().Add(-1 * time.Minute) // small skew tolerance
	}
	notAfter := notBefore.Add(in.Validity)

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("selfsigned: ca key: %w", err)
	}
	caSerial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "jaas-webhook-ca"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("selfsigned: sign ca: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("selfsigned: parse ca: %w", err)
	}

	servingKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("selfsigned: serving key: %w", err)
	}
	servingSerial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	dnsNames := []string{
		in.ServiceName + "." + in.Namespace + ".svc",
		in.ServiceName + "." + in.Namespace + ".svc.cluster.local",
	}
	servingTmpl := &x509.Certificate{
		SerialNumber: servingSerial,
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
	}
	servingDER, err := x509.CreateCertificate(rand.Reader, servingTmpl, caCert, &servingKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("selfsigned: sign serving: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(servingKey)
	if err != nil {
		return nil, fmt.Errorf("selfsigned: marshal serving key: %w", err)
	}

	return &Bundle{
		CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		CertPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: servingDER}),
		KeyPEM:   pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		NotAfter: notAfter,
	}, nil
}

// WriteTo materializes tls.key and tls.crt under dir with 0o600
// permissions. controller-runtime's webhook server reads these
// filenames out of the box. dir must already exist; the operator
// mounts an emptyDir on top in the chart.
//
// Key first, then cert. certwatcher's fsnotify hook fires on the
// cert file's write event; by the time it reloads and re-reads both
// files from disk, the matching key is guaranteed to be present.
// Writing the cert first would open a sub-millisecond window where
// certwatcher could load the new cert paired with the old key —
// TLS handshakes fail with "key does not match certificate" until
// the second write completes.
func (b *Bundle) WriteTo(dir string) error {
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), b.KeyPEM, 0o600); err != nil {
		return fmt.Errorf("selfsigned: write tls.key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), b.CertPEM, 0o600); err != nil {
		return fmt.Errorf("selfsigned: write tls.crt: %w", err)
	}
	return nil
}

func randomSerial() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, fmt.Errorf("selfsigned: serial: %w", err)
	}
	return n, nil
}
