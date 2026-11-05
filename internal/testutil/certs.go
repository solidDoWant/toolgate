package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// certAuthority is a throwaway CA that issues TLS certificates for tests.
type certAuthority struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

// keyPair is a PEM-encoded certificate and its private key.
type keyPair struct {
	CertPEM []byte
	KeyPEM  []byte
}

func newCertAuthority(tb testing.TB) *certAuthority {
	tb.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)

	template := &x509.Certificate{
		SerialNumber:          newSerial(tb),
		Subject:               pkix.Name{CommonName: "toolgate test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(tb, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(tb, err)

	return &certAuthority{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// issue signs a leaf certificate for commonName. Server certificates carry
// hosts as DNS or IP subject alternative names.
func (ca *certAuthority) issue(tb testing.TB, commonName string, usage x509.ExtKeyUsage, hosts ...string) keyPair {
	tb.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)

	template := &x509.Certificate{
		SerialNumber: newSerial(tb),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}

	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, host)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	require.NoError(tb, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(tb, err)

	return keyPair{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}
}

func newSerial(tb testing.TB) *big.Int {
	tb.Helper()

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	require.NoError(tb, err)

	return serial
}
