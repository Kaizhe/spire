package ca

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andres-erbsen/clock"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/spire/pkg/common/idutil"
	"github.com/spiffe/spire/pkg/common/jwtsvid"
	"github.com/spiffe/spire/pkg/common/telemetry"
	"github.com/spiffe/spire/proto/spire/api/node"
	"github.com/zeebo/errs"
)

const (
	// DefaultX509SVIDTTL is the TTL given to X509 SVIDs if not overridden by
	// the server config.
	DefaultX509SVIDTTL = time.Hour

	// DefaultJWTSVIDTTL is the TTL given to JWT SVIDs if a different TTL is
	// not provided in the signing request.
	DefaultJWTSVIDTTL = time.Minute * 5
)

// ServerCA is an interface for Server CAs
type ServerCA interface {
	SignX509SVID(ctx context.Context, csrDER []byte, params X509Params) ([]*x509.Certificate, error)
	SignX509CASVID(ctx context.Context, csrDER []byte, params X509Params) ([]*x509.Certificate, error)
	SignJWTSVID(ctx context.Context, jsr *node.JSR) (string, error)
}

// X509Params are parameters relevant to X509 SVID creation
type X509Params struct {
	// TTL is the desired time-to-live of the SVID. Regardless of the TTL, the
	// lifetime of the certificate will be capped to that of the signing cert.
	TTL time.Duration

	// DNSList is used to add DNS SAN's to the X509 SVID. The first entry
	// is also added as the CN. DNSList is ignored when signing CA X509 SVIDs.
	DNSList []string
}

type X509CA struct {
	// Signer is used to sign child certificates.
	Signer crypto.Signer

	// Certificate is the CA certificate.
	Certificate *x509.Certificate

	// UpstreamChain contains the CA certificate and intermediates necessary to
	// chain back to the upstream trust bundle. It is only set if the CA is
	// signed by an UpstreamCA and the upstream trust bundle *is* the SPIRE
	// trust bundle (see the upstream_bundle configurable).
	UpstreamChain []*x509.Certificate
}

type JWTKey struct {
	// The signer used to sign keys
	Signer crypto.Signer

	// Kid is the JWT key ID (i.e. "kid" claim)
	Kid string

	// NotAfter is the expiration time of the JWT key.
	NotAfter time.Time
}

type CAConfig struct {
	Log         logrus.FieldLogger
	Metrics     telemetry.Metrics
	TrustDomain url.URL
	X509SVIDTTL time.Duration
	Clock       clock.Clock
	CASubject   pkix.Name
}

type CA struct {
	c      CAConfig
	x509sn int64

	mu     sync.RWMutex
	x509CA *X509CA
	jwtKey *JWTKey

	jwtSigner *jwtsvid.Signer
}

func NewCA(config CAConfig) *CA {
	if config.X509SVIDTTL <= 0 {
		config.X509SVIDTTL = DefaultX509SVIDTTL
	}
	if config.Clock == nil {
		config.Clock = clock.New()
	}

	return &CA{
		c: config,
		jwtSigner: jwtsvid.NewSigner(jwtsvid.SignerConfig{
			Clock: config.Clock,
		}),
	}
}

func (ca *CA) X509CA() *X509CA {
	ca.mu.RLock()
	defer ca.mu.RUnlock()
	return ca.x509CA
}

func (ca *CA) SetX509CA(x509CA *X509CA) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	ca.x509CA = x509CA
}

func (ca *CA) JWTKey() *JWTKey {
	ca.mu.RLock()
	defer ca.mu.RUnlock()
	return ca.jwtKey
}

func (ca *CA) SetJWTKey(jwtKey *JWTKey) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	ca.jwtKey = jwtKey
}

func (ca *CA) SignX509SVID(ctx context.Context, csrDER []byte, params X509Params) ([]*x509.Certificate, error) {
	x509CA := ca.X509CA()
	if x509CA == nil {
		return nil, errs.New("X509 CA is not available for signing")
	}

	if params.TTL <= 0 {
		params.TTL = ca.c.X509SVIDTTL
	}

	notBefore, notAfter := ca.capLifetime(params.TTL, x509CA.Certificate.NotAfter)
	serialNumber := ca.nextSerialNumber()

	template, err := CreateX509SVIDTemplate(csrDER, ca.c.TrustDomain.Host, notBefore, notAfter, serialNumber)
	if err != nil {
		return nil, err
	}

	// for non-CA certificates, add DNS names to certificate. the first DNS
	// name is also added as the common name.
	if len(params.DNSList) > 0 {
		template.Subject.CommonName = params.DNSList[0]
		template.DNSNames = params.DNSList
	}

	cert, err := createCertificate(template, x509CA.Certificate, template.PublicKey, x509CA.Signer)
	if err != nil {
		return nil, errs.New("unable to create X509 SVID: %v", err)
	}

	spiffeID := cert.URIs[0].String()

	ca.c.Log.WithFields(logrus.Fields{
		"spiffe_id":  spiffeID,
		"expires_at": cert.NotAfter.Format(time.RFC3339),
	}).Debug("Signed X509 SVID")

	ca.c.Metrics.IncrCounterWithLabels([]string{telemetry.CA, telemetry.Sign, telemetry.X509SVID}, 1, []telemetry.Label{
		{
			Name:  telemetry.SPIFFEID,
			Value: spiffeID,
		},
	})

	return makeSVIDCertChain(x509CA, cert), nil
}

func (ca *CA) SignX509CASVID(ctx context.Context, csrDER []byte, params X509Params) ([]*x509.Certificate, error) {
	x509CA := ca.X509CA()
	if x509CA == nil {
		return nil, errs.New("X509 CA is not available for signing")
	}

	if params.TTL <= 0 {
		params.TTL = ca.c.X509SVIDTTL
	}

	notBefore, notAfter := ca.capLifetime(params.TTL, x509CA.Certificate.NotAfter)
	serialNumber := ca.nextSerialNumber()

	template, err := CreateServerCATemplate(csrDER, ca.c.TrustDomain.Host, notBefore, notAfter, serialNumber)
	if err != nil {
		return nil, err
	}
	// Don't allow the downstream server to control the subject of the CA
	// certificate.
	template.Subject = ca.c.CASubject

	cert, err := createCertificate(template, x509CA.Certificate, template.PublicKey, x509CA.Signer)
	if err != nil {
		return nil, errs.New("unable to create X509 CA SVID: %v", err)
	}

	spiffeID := cert.URIs[0].String()

	ca.c.Log.WithFields(logrus.Fields{
		"spiffe_id":  spiffeID,
		"expires_at": cert.NotAfter.Format(time.RFC3339),
	}).Debug("Signed X509 CA SVID")

	ca.c.Metrics.IncrCounterWithLabels([]string{telemetry.CA, telemetry.Sign, telemetry.X509CASVID}, 1, []telemetry.Label{
		{
			Name:  telemetry.SPIFFEID,
			Value: spiffeID,
		},
	})

	return makeSVIDCertChain(x509CA, cert), nil
}

func (ca *CA) SignJWTSVID(ctx context.Context, jsr *node.JSR) (string, error) {
	jwtKey := ca.JWTKey()
	if jwtKey == nil {
		return "", errs.New("JWT key is not available for signing")
	}

	if err := idutil.ValidateSpiffeID(jsr.SpiffeId, idutil.AllowTrustDomainWorkload(ca.c.TrustDomain.Host)); err != nil {
		return "", err
	}

	ttl := time.Duration(jsr.Ttl) * time.Second
	if ttl <= 0 {
		ttl = DefaultJWTSVIDTTL
	}
	_, expiresAt := ca.capLifetime(ttl, jwtKey.NotAfter)

	token, err := ca.jwtSigner.SignToken(jsr.SpiffeId, jsr.Audience, expiresAt, jwtKey.Signer, jwtKey.Kid)
	if err != nil {
		return "", errs.New("unable to sign JWT SVID: %v", err)
	}

	labels := []telemetry.Label{
		{
			Name:  telemetry.SPIFFEID,
			Value: jsr.SpiffeId,
		},
	}
	for _, audience := range jsr.Audience {
		labels = append(labels, telemetry.Label{
			Name:  telemetry.Audience,
			Value: audience,
		})
	}
	ca.c.Metrics.IncrCounterWithLabels([]string{telemetry.ServerCA, telemetry.Sign, telemetry.JWTSVID}, 1, labels)

	return token, nil
}

func (ca *CA) nextSerialNumber() *big.Int {
	return big.NewInt(atomic.AddInt64(&ca.x509sn, 1))
}

func (ca *CA) capLifetime(ttl time.Duration, expirationCap time.Time) (notBefore, notAfter time.Time) {
	now := ca.c.Clock.Now()
	notBefore = now.Add(-backdate)
	notAfter = now.Add(ttl)
	if notAfter.After(expirationCap) {
		notAfter = expirationCap
	}
	return notBefore, notAfter
}

func makeSVIDCertChain(x509CA *X509CA, cert *x509.Certificate) []*x509.Certificate {
	return append([]*x509.Certificate{cert}, x509CA.UpstreamChain...)
}

func createCertificate(template, parent *x509.Certificate, pub, priv interface{}) (*x509.Certificate, error) {
	certDER, err := x509.CreateCertificate(rand.Reader, template, parent, pub, priv)
	if err != nil {
		return nil, errs.New("unable to create X509 SVID: %v", err)
	}

	return x509.ParseCertificate(certDER)
}
