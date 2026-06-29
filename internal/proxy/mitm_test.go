package proxy

import (
	"crypto/x509"
	"testing"
)

func TestCertAuthority_LeafChainsToCA(t *testing.T) {
	ca, certPEM, _, err := GenerateCertAuthority("Test CA")
	if err != nil {
		t.Fatal(err)
	}
	if len(certPEM) == 0 {
		t.Fatal("expected CA cert PEM")
	}

	leaf, err := ca.LeafFor("api.openai.com")
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Subject.CommonName != "api.openai.com" {
		t.Fatalf("CN = %q, want api.openai.com", parsed.Subject.CommonName)
	}

	// Leaf must verify against the CA.
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("failed to add CA to pool")
	}
	if _, err := parsed.Verify(x509.VerifyOptions{Roots: roots, DNSName: "api.openai.com"}); err != nil {
		t.Fatalf("leaf did not verify against CA: %v", err)
	}
}

func TestCertAuthority_CachesLeaf(t *testing.T) {
	ca, _, _, _ := GenerateCertAuthority("Test CA")
	a, _ := ca.LeafFor("api.openai.com")
	b, _ := ca.LeafFor("api.openai.com")
	if a != b {
		t.Fatal("expected cached leaf to be reused")
	}
}
