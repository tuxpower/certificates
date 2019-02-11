package ct

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	ct "github.com/google/certificate-transparency-go"
	"github.com/google/certificate-transparency-go/client"
	"github.com/google/certificate-transparency-go/jsonclient"
	cttls "github.com/google/certificate-transparency-go/tls"
	ctx509 "github.com/google/certificate-transparency-go/x509"
	"github.com/pkg/errors"
)

var (
	oidExtensionCTPoison              = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 4, 3}
	oidSignedCertificateTimestampList = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 4, 2}
)

// Config represents the configuration for the certificate authority client.
type Config struct {
	URI string `json:"uri"`
	Key string `json:"key"`
}

// Validate validates the ct configuration.
func (c *Config) Validate() error {
	switch {
	case c.URI == "":
		return errors.New("ct uri cannot be empty")
	case c.Key == "":
		return errors.New("ct key cannot be empty")
	default:
		return nil
	}
}

// Client is the interfaced used to communicate with the certificate transparency logs.
type Client interface {
	GetSCTs(chain ...*x509.Certificate) (*SCT, error)
	SubmitToLogs(chain ...*x509.Certificate) error
}

type logClient interface {
	AddPreChain(ctx context.Context, chain []ct.ASN1Cert) (*ct.SignedCertificateTimestamp, error)
	AddChain(ctx context.Context, chain []ct.ASN1Cert) (*ct.SignedCertificateTimestamp, error)
}

// SCT represents a Signed Certificate Timestamp.
type SCT struct {
	LogURL string
	SCT    *ct.SignedCertificateTimestamp
}

// GetExtension returns the extension representing an SCT that will be added to
// a certificate.
func (t *SCT) GetExtension() pkix.Extension {
	val, err := cttls.Marshal(*t.SCT)
	if err != nil {
		panic(err)
	}
	value, err := cttls.Marshal(ctx509.SignedCertificateTimestampList{
		SCTList: []ctx509.SerializedSCT{
			{Val: val},
		},
	})
	if err != nil {
		panic(err)
	}
	rawValue, err := asn1.Marshal(value)
	if err != nil {
		panic(err)
	}
	return pkix.Extension{
		Id:       oidSignedCertificateTimestampList,
		Critical: false,
		Value:    rawValue,
	}
}

// AddPoisonExtension appends the ct poison extension to the given certificate.
func AddPoisonExtension(cert *x509.Certificate) {
	cert.Extensions = append(cert.Extensions, pkix.Extension{
		Id:       oidExtensionCTPoison,
		Critical: true,
	})
}

// ClientImpl is the implementation of a certificate transparency Client.
type ClientImpl struct {
	config    Config
	logClient logClient
	timeout   time.Duration
}

// New creates a new Client
func New(c Config) (*ClientImpl, error) {
	// Extract DER from key
	data, err := ioutil.ReadFile(c.Key)
	if err != nil {
		return nil, errors.Wrapf(err, "error reading %s", c.Key)
	}
	block, rest := pem.Decode(data)
	if block == nil || len(rest) > 0 {
		return nil, errors.Wrapf(err, "invalid public key %s", c.Key)
	}

	// Initialize ct client
	logClient, err := client.New(c.URI, &http.Client{}, jsonclient.Options{
		PublicKeyDER: block.Bytes,
		UserAgent:    "smallstep certificates",
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create client to %s", c.URI)
	}

	// Validate connection
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := logClient.GetSTH(ctx); err != nil {
		return nil, errors.Wrapf(err, "failed to connect to %s", c.URI)
	}
	log.Printf("connecting to CT log %s", c.URI)

	return &ClientImpl{
		config:    c,
		logClient: logClient,
		timeout:   30 * time.Second,
	}, nil
}

// GetSCTs submit the precertificate to the logs and returns the list of SCTs to
// embed into the certificate.
func (c *ClientImpl) GetSCTs(chain ...*x509.Certificate) (*SCT, error) {
	ctChain := chainFromCerts(chain)
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	sct, err := c.logClient.AddPreChain(ctx, ctChain)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get SCT from %s", c.config.URI)
	}
	return &SCT{
		LogURL: c.config.URI,
		SCT:    sct,
	}, nil
}

// SubmitToLogs submits the certificate to the certificate transparency logs.
func (c *ClientImpl) SubmitToLogs(chain ...*x509.Certificate) error {
	ctChain := chainFromCerts(chain)
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	sct, err := c.logClient.AddChain(ctx, ctChain)
	if err != nil {
		return errors.Wrapf(err, "failed submit certificate to %s", c.config.URI)
	}

	// Calculate the leaf hash
	leafEntry := ct.CreateX509MerkleTreeLeaf(ctChain[0], sct.Timestamp)
	leafHash, err := ct.LeafHashForLeaf(leafEntry)
	if err != nil {
		log.Println(err)
	}
	// Display the SCT
	fmt.Printf("LogID: %x\n", sct.LogID.KeyID[:])
	fmt.Printf("LeafHash: %x\n", leafHash)

	return nil
}

func chainFromCerts(certs []*x509.Certificate) []ct.ASN1Cert {
	var chain []ct.ASN1Cert
	for _, cert := range certs {
		chain = append(chain, ct.ASN1Cert{Data: cert.Raw})
	}
	return chain
}
