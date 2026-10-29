package pki

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
	"net/url"
	"os"
	"time"
)

// CertificateAuthority represents an internal certificate authority.
type CertificateAuthority struct {
	Certificate *x509.Certificate
	PrivateKey  *ecdsa.PrivateKey
	CertPEM     []byte
	KeyPEM      []byte
}

// CertificateBundle holds the generated certificate, private key and CA bundle in PEM format.
type CertificateBundle struct {
	CertPEM []byte
	KeyPEM  []byte
	CAPEM   []byte
}

// NewCA creates a new self-signed Certificate Authority with an ECDSA P-256
// key. A trust domain (spiffe://<domain>) is added as URI SAN, so that SPIFFE
// consumers such as OpenZiti find the trust domain in the chain.
func NewCA(organization string, validYears int, trustDomain *url.URL) (*CertificateAuthority, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate private key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   fmt.Sprintf("%s Root CA", organization),
			Organization: []string{organization},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().AddDate(validYears, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	if trustDomain != nil {
		template.URIs = []*url.URL{trustDomain}
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("failed to create CA certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(derBytes)
	if err != nil {
		return nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	return &CertificateAuthority{
		Certificate: cert,
		PrivateKey:  priv,
		CertPEM:     certPEM,
		KeyPEM:      keyPEM,
	}, nil
}

// LoadCA loads an existing CA from PEM certificate and private key files.
func LoadCA(certPath, keyPath string) (*CertificateAuthority, error) {
	certData, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate file: %w", err)
	}
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA key file: %w", err)
	}

	certBlock, _ := pem.Decode(certData)
	if certBlock == nil {
		return nil, errors.New("invalid CA certificate PEM format")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, err
	}

	keyBlock, _ := pem.Decode(keyData)
	if keyBlock == nil {
		return nil, errors.New("invalid CA key PEM format")
	}

	privKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		// Fallback to PKCS8
		pkcs8Key, err2 := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if err2 != nil {
			return nil, fmt.Errorf("failed to parse private key: %v / %v", err, err2)
		}
		var ok bool
		privKey, ok = pkcs8Key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("only ECDSA CA private keys are supported")
		}
	}

	return &CertificateAuthority{
		Certificate: cert,
		PrivateKey:  privKey,
		CertPEM:     certData,
		KeyPEM:      keyData,
	}, nil
}
