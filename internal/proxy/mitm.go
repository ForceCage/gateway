package proxy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"sync"
	"time"
)

// CertAuthority mints short-lived leaf certificates on demand, signed by a CA
// that the agent's runtime is configured to trust. This is what lets the forward
// proxy terminate (MITM) the agent's TLS tunnel so the engine can inspect the
// plaintext request. Generated leaf certs are cached per host.
type CertAuthority struct {
	caCert *x509.Certificate
	caKey  *rsa.PrivateKey
	caPEM  []byte

	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

// NewCertAuthority creates a CA from PEM-encoded cert and key bytes.
func NewCertAuthority(certPEM, keyPEM []byte) (*CertAuthority, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load CA key pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}
	key, ok := pair.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("CA key is not RSA")
	}
	return &CertAuthority{
		caCert: leaf,
		caKey:  key,
		caPEM:  certPEM,
		cache:  make(map[string]*tls.Certificate),
	}, nil
}

// LoadCertAuthority reads the CA cert and key from the given file paths.
func LoadCertAuthority(certPath, keyPath string) (*CertAuthority, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read CA key: %w", err)
	}
	return NewCertAuthority(certPEM, keyPEM)
}

// GenerateCertAuthority creates a brand-new self-signed CA. Returns the authority
// plus the PEM-encoded cert and key so callers can persist them (the same CA must
// be reused across restarts, and its cert must be installed into the agent's
// trust store). Intended for dev/bootstrap; production should mount a managed CA.
func GenerateCertAuthority(commonName string) (ca *CertAuthority, certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"ForceCage"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	ca, err = NewCertAuthority(certPEM, keyPEM)
	if err != nil {
		return nil, nil, nil, err
	}
	return ca, certPEM, keyPEM, nil
}

// CACertPEM returns the PEM-encoded CA certificate (for trust-store installation).
func (ca *CertAuthority) CACertPEM() []byte { return ca.caPEM }

// LeafFor returns a TLS certificate valid for host, signed by the CA, minting and
// caching a new one if necessary.
func (ca *CertAuthority) LeafFor(host string) (*tls.Certificate, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if c, ok := ca.cache[host]; ok {
		return c, nil
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.caCert, &key.PublicKey, ca.caKey)
	if err != nil {
		return nil, err
	}
	leaf := &tls.Certificate{
		Certificate: [][]byte{der, ca.caCert.Raw},
		PrivateKey:  key,
	}
	ca.cache[host] = leaf
	return leaf, nil
}
