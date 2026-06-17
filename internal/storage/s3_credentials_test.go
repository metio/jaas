/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package storage

import (
	"testing"

	"github.com/minio/minio-go/v7/pkg/credentials"
)

func TestS3Credentials_StaticKeys(t *testing.T) {
	creds := s3Credentials(S3Config{AccessKeyID: "AKIA", SecretAccessKey: "secret", SessionToken: "tok"})
	v, err := creds.GetWithContext(&credentials.CredContext{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v.AccessKeyID != "AKIA" || v.SecretAccessKey != "secret" || v.SessionToken != "tok" {
		t.Errorf("static creds = %+v, want AKIA/secret/tok", v)
	}
}

// The chart mounts an existingSecret of AWS_* keys via envFrom; the empty-creds
// branch must resolve them through the env entry of the discovery chain, or the
// documented credentials path silently falls back to IAM and fails.
func TestS3Credentials_EnvChainReadsAWSEnv(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAENV")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "envsecret")
	creds := s3Credentials(S3Config{}) // not anonymous, no static key
	v, err := creds.GetWithContext(&credentials.CredContext{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v.AccessKeyID != "AKIAENV" || v.SecretAccessKey != "envsecret" {
		t.Errorf("env-chain creds = %+v, want AKIAENV/envsecret", v)
	}
}

func TestS3Credentials_Anonymous(t *testing.T) {
	if s3Credentials(S3Config{UseAnonymous: true}) != nil {
		t.Error("anonymous should yield nil credentials (unsigned requests)")
	}
}
