// Package scep provides common functionality for encoding and decoding
// Simple Certificate Enrolment Protocol pki messages as defined by
// https://tools.ietf.org/html/draft-gutmann-scep-02
package scep

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/micromdm/scep/cryptoutil"
	"github.com/pkg/errors"
	"go.mozilla.org/pkcs7"

	"github.com/micromdm/scep/cryptoutil/x509util"
)

// errors
var (
	errNotImplemented     = errors.New("not implemented")
	errUnknownMessageType = errors.New("unknown messageType")
)

// The MessageType attribute specifies the type of operation performed
// by the transaction.  This attribute MUST be included in all PKI
// messages.
//
// The following message types are defined:
type MessageType string

// Undefined message types are treated as an error.
const (
	CertRep    MessageType = "3"
	RenewalReq             = "17"
	UpdateReq              = "18"
	PKCSReq                = "19"
	CertPoll               = "20"
	GetCert                = "21"
	GetCRL                 = "22"
)

func (msg MessageType) String() string {
	switch msg {
	case CertRep:
		return "CertRep (3)"
	case RenewalReq:
		return "RenewalReq (17)"
	case UpdateReq:
		return "UpdateReq (18)"
	case PKCSReq:
		return "PKCSReq (19)"
	case CertPoll:
		return "CertPoll (20) "
	case GetCert:
		return "GetCert (21)"
	case GetCRL:
		return "GetCRL (22)"
	default:
		panic("scep: unknown messageType" + msg)
	}
}

// PKIStatus is a SCEP pkiStatus attribute which holds transaction status information.
// All SCEP responses MUST include a pkiStatus.
//
// The following pkiStatuses are defined:
type PKIStatus string

// Undefined pkiStatus attributes are treated as an error
const (
	SUCCESS PKIStatus = "0"
	FAILURE           = "2"
	PENDING           = "3"
)

// FailInfo is a SCEP failInfo attribute
//
// The FailInfo attribute MUST contain one of the following failure
// reasons:
type FailInfo string

//
const (
	BadAlg          FailInfo = "0"
	BadMessageCheck          = "1"
	BadRequest               = "2"
	BadTime                  = "3"
	BadCertID                = "4"
)

func (info FailInfo) String() string {
	switch info {
	case BadAlg:
		return "badAlg (0)"
	case BadMessageCheck:
		return "badMessageCheck (1)"
	case BadRequest:
		return "badRequest (2)"
	case BadTime:
		return "badTime (3)"
	case BadCertID:
		return "badCertID (4)"
	default:
		panic("scep: unknown failInfo type" + info)
	}
}

// SenderNonce is a random 16 byte number.
// A sender must include the senderNonce in each transaction to a recipient.
type SenderNonce []byte

// The RecipientNonce MUST be copied from the SenderNonce
// and included in the reply.
type RecipientNonce []byte

// The TransactionID is a text
// string generated by the client when starting a transaction. The
// client MUST generate a unique string as the transaction identifier,
// which MUST be used for all PKI messages exchanged for a given
// enrolment, encoded as a PrintableString.
type TransactionID string

// SCEP OIDs
var (
	oidSCEPmessageType    = asn1.ObjectIdentifier{2, 16, 840, 1, 113733, 1, 9, 2}
	oidSCEPpkiStatus      = asn1.ObjectIdentifier{2, 16, 840, 1, 113733, 1, 9, 3}
	oidSCEPfailInfo       = asn1.ObjectIdentifier{2, 16, 840, 1, 113733, 1, 9, 4}
	oidSCEPsenderNonce    = asn1.ObjectIdentifier{2, 16, 840, 1, 113733, 1, 9, 5}
	oidSCEPrecipientNonce = asn1.ObjectIdentifier{2, 16, 840, 1, 113733, 1, 9, 6}
	oidSCEPtransactionID  = asn1.ObjectIdentifier{2, 16, 840, 1, 113733, 1, 9, 7}
)

// WithLogger adds option logging to the SCEP operations.
func WithLogger(logger log.Logger) Option {
	return func(c *config) {
		c.logger = logger
	}
}

// WithCACerts adds option CA certificates to the SCEP operations.
// Note: This changes the verification behavior of PKCS #7 messages. If this
// option is specified, only caCerts will be used as expected signers.
func WithCACerts(caCerts []*x509.Certificate) Option {
	return func(c *config) {
		c.caCerts = caCerts
	}
}

// WithCertsSelector adds the certificates certsSelector option to the SCEP
// operations.
// This option is effective when used with NewCSRRequest function. In
// this case, only certificates selected with the certsSelector will be used
// as the PKCS #7 message recipients.
func WithCertsSelector(selector CertsSelector) Option {
	return func(c *config) {
		c.certsSelector = selector
	}
}

// Option specifies custom configuration for SCEP.
type Option func(*config)

type config struct {
	logger        log.Logger
	caCerts       []*x509.Certificate // specified if CA certificates have already been retrieved
	certsSelector CertsSelector
}

// PKIMessage defines the possible SCEP message types
type PKIMessage struct {
	TransactionID
	MessageType
	SenderNonce
	*CertRepMessage
	*CSRReqMessage

	// DER Encoded PKIMessage
	Raw []byte

	// parsed
	p7 *pkcs7.PKCS7

	// decrypted enveloped content
	pkiEnvelope []byte

	// Used to encrypt message
	Recipients []*x509.Certificate

	// Signer info
	SignerKey  *rsa.PrivateKey
	SignerCert *x509.Certificate

	logger log.Logger
}

// CertRepMessage is a type of PKIMessage
type CertRepMessage struct {
	PKIStatus
	RecipientNonce
	FailInfo

	Certificate *x509.Certificate

	degenerate []byte
}

// CSRReqMessage can be of the type PKCSReq/RenewalReq/UpdateReq
// and includes a PKCS#10 CSR request.
// The content of this message is protected
// by the recipient public key(example CA)
type CSRReqMessage struct {
	RawDecrypted []byte

	// PKCS#10 Certificate request inside the envelope
	CSR *x509.CertificateRequest

	ChallengePassword string
}

// ParsePKIMessage unmarshals a PKCS#7 signed data into a PKI message struct
func ParsePKIMessage(data []byte, opts ...Option) (*PKIMessage, error) {
	conf := &config{logger: log.NewNopLogger()}
	for _, opt := range opts {
		opt(conf)
	}

	// parse PKCS#7 signed data
	p7, err := pkcs7.Parse(data)
	if err != nil {
		return nil, err
	}

	if len(conf.caCerts) > 0 {
		// According to RFC #2315 Section 9.1, it is valid that the server sends fewer
		// certificates than necessary, if it is expected that those verifying the
		// signatures have an alternate means of obtaining necessary certificates.
		// In SCEP case, an alternate means is to use GetCaCert request.
		// Note: The https://github.com/jscep/jscep implementation logs a warning if
		// no certificates were found for signers in the PKCS #7 received from the
		// server, but the certificates obtained from GetCaCert request are still
		// used for decoding the message.
		p7.Certificates = conf.caCerts
	}

	if err := p7.Verify(); err != nil {
		return nil, err
	}

	var tID TransactionID
	if err := p7.UnmarshalSignedAttribute(oidSCEPtransactionID, &tID); err != nil {
		return nil, err
	}

	var msgType MessageType
	if err := p7.UnmarshalSignedAttribute(oidSCEPmessageType, &msgType); err != nil {
		return nil, err
	}

	msg := &PKIMessage{
		TransactionID: tID,
		MessageType:   msgType,
		Raw:           data,
		p7:            p7,
		logger:        conf.logger,
	}

	// log relevant key-values when parsing a pkiMessage.
	logKeyVals := []interface{}{
		"msg", "parsed scep pkiMessage",
		"scep_message_type", msgType,
		"transaction_id", tID,
	}
	level.Debug(msg.logger).Log(logKeyVals...)

	if err := msg.parseMessageType(); err != nil {
		return nil, err
	}

	return msg, nil
}

func (msg *PKIMessage) parseMessageType() error {
	switch msg.MessageType {
	case CertRep:
		var status PKIStatus
		if err := msg.p7.UnmarshalSignedAttribute(oidSCEPpkiStatus, &status); err != nil {
			return err
		}
		var rn RecipientNonce
		if err := msg.p7.UnmarshalSignedAttribute(oidSCEPrecipientNonce, &rn); err != nil {
			return err
		}
		if len(rn) == 0 {
			return errors.New("scep pkiMessage must include recipientNonce attribute")
		}
		cr := &CertRepMessage{
			PKIStatus:      status,
			RecipientNonce: rn,
		}
		switch status {
		case SUCCESS:
			break
		case FAILURE:
			var fi FailInfo
			if err := msg.p7.UnmarshalSignedAttribute(oidSCEPfailInfo, &fi); err != nil {
				return err
			}
			if fi == "" {
				return errors.New("scep pkiStatus FAILURE must have a failInfo attribute")
			}
			cr.FailInfo = fi
		case PENDING:
			break
		default:
			return errors.Errorf("unknown scep pkiStatus %s", status)
		}
		msg.CertRepMessage = cr
		return nil
	case PKCSReq, UpdateReq, RenewalReq:
		var sn SenderNonce
		if err := msg.p7.UnmarshalSignedAttribute(oidSCEPsenderNonce, &sn); err != nil {
			return err
		}
		if len(sn) == 0 {
			return errors.New("scep pkiMessage must include senderNonce attribute")
		}
		msg.SenderNonce = sn
		return nil
	case GetCRL, GetCert, CertPoll:
		return errNotImplemented
	default:
		return errUnknownMessageType
	}
}

// DecryptPKIEnvelope decrypts the pkcs envelopedData inside the SCEP PKIMessage
func (msg *PKIMessage) DecryptPKIEnvelope(cert *x509.Certificate, key *rsa.PrivateKey) error {
	p7, err := pkcs7.Parse(msg.p7.Content)
	if err != nil {
		return err
	}
	msg.pkiEnvelope, err = p7.Decrypt(cert, key)
	if err != nil {
		return err
	}

	logKeyVals := []interface{}{
		"msg", "decrypt pkiEnvelope",
	}
	defer func() { level.Debug(msg.logger).Log(logKeyVals...) }()

	switch msg.MessageType {
	case CertRep:
		certs, err := CACerts(msg.pkiEnvelope)
		if err != nil {
			return err
		}
		msg.CertRepMessage.Certificate = certs[0]
		logKeyVals = append(logKeyVals, "ca_certs", len(certs))
		return nil
	case PKCSReq, UpdateReq, RenewalReq:
		csr, err := x509.ParseCertificateRequest(msg.pkiEnvelope)
		if err != nil {
			return errors.Wrap(err, "parse CSR from pkiEnvelope")
		}
		// check for challengePassword
		cp, err := x509util.ParseChallengePassword(msg.pkiEnvelope)
		if err != nil {
			return errors.Wrap(err, "scep: parse challenge password in pkiEnvelope")
		}
		msg.CSRReqMessage = &CSRReqMessage{
			RawDecrypted:      msg.pkiEnvelope,
			CSR:               csr,
			ChallengePassword: cp,
		}
		logKeyVals = append(logKeyVals, "has_challenge", cp != "")
		return nil
	case GetCRL, GetCert, CertPoll:
		return errNotImplemented
	default:
		return errUnknownMessageType
	}
}

func (msg *PKIMessage) Fail(crtAuth *x509.Certificate, keyAuth *rsa.PrivateKey, info FailInfo) (*PKIMessage, error) {
	config := pkcs7.SignerInfoConfig{
		ExtraSignedAttributes: []pkcs7.Attribute{
			{
				Type:  oidSCEPtransactionID,
				Value: msg.TransactionID,
			},
			{
				Type:  oidSCEPpkiStatus,
				Value: FAILURE,
			},
			{
				Type:  oidSCEPfailInfo,
				Value: info,
			},
			{
				Type:  oidSCEPmessageType,
				Value: CertRep,
			},
			{
				Type:  oidSCEPsenderNonce,
				Value: msg.SenderNonce,
			},
			{
				Type:  oidSCEPrecipientNonce,
				Value: msg.SenderNonce,
			},
		},
	}

	sd, err := pkcs7.NewSignedData(nil)
	if err != nil {
		return nil, err
	}

	// sign the attributes
	if err := sd.AddSigner(crtAuth, keyAuth, config); err != nil {
		return nil, err
	}

	certRepBytes, err := sd.Finish()
	if err != nil {
		return nil, err
	}

	cr := &CertRepMessage{
		PKIStatus:      FAILURE,
		FailInfo:       BadRequest,
		RecipientNonce: RecipientNonce(msg.SenderNonce),
	}

	// create a CertRep message from the original
	crepMsg := &PKIMessage{
		Raw:            certRepBytes,
		TransactionID:  msg.TransactionID,
		MessageType:    CertRep,
		CertRepMessage: cr,
	}

	return crepMsg, nil

}

// Success returns a new PKIMessage with CertRep data using an already-issued certificate
func (msg *PKIMessage) Success(crtAuth *x509.Certificate, keyAuth *rsa.PrivateKey, crt *x509.Certificate) (*PKIMessage, error) {
	// check if CSRReqMessage has already been decrypted
	if msg.CSRReqMessage.CSR == nil {
		if err := msg.DecryptPKIEnvelope(crtAuth, keyAuth); err != nil {
			return nil, err
		}
	}

	// create a degenerate cert structure
	deg, err := DegenerateCertificates([]*x509.Certificate{crt})
	if err != nil {
		return nil, err
	}

	// encrypt degenerate data using the original messages recipients
	e7, err := pkcs7.Encrypt(deg, msg.p7.Certificates)
	if err != nil {
		return nil, err
	}

	// PKIMessageAttributes to be signed
	config := pkcs7.SignerInfoConfig{
		ExtraSignedAttributes: []pkcs7.Attribute{
			{
				Type:  oidSCEPtransactionID,
				Value: msg.TransactionID,
			},
			{
				Type:  oidSCEPpkiStatus,
				Value: SUCCESS,
			},
			{
				Type:  oidSCEPmessageType,
				Value: CertRep,
			},
			{
				Type:  oidSCEPsenderNonce,
				Value: msg.SenderNonce,
			},
			{
				Type:  oidSCEPrecipientNonce,
				Value: msg.SenderNonce,
			},
		},
	}

	signedData, err := pkcs7.NewSignedData(e7)
	if err != nil {
		return nil, err
	}
	// add the certificate into the signed data type
	// this cert must be added before the signedData because the recipient will expect it
	// as the first certificate in the array
	signedData.AddCertificate(crt)
	// sign the attributes
	if err := signedData.AddSigner(crtAuth, keyAuth, config); err != nil {
		return nil, err
	}

	certRepBytes, err := signedData.Finish()
	if err != nil {
		return nil, err
	}

	cr := &CertRepMessage{
		PKIStatus:      SUCCESS,
		RecipientNonce: RecipientNonce(msg.SenderNonce),
		Certificate:    crt,
		degenerate:     deg,
	}

	// create a CertRep message from the original
	crepMsg := &PKIMessage{
		Raw:            certRepBytes,
		TransactionID:  msg.TransactionID,
		MessageType:    CertRep,
		CertRepMessage: cr,
	}

	return crepMsg, nil
}

// DegenerateCertificates creates degenerate certificates pkcs#7 type
func DegenerateCertificates(certs []*x509.Certificate) ([]byte, error) {
	var buf bytes.Buffer
	for _, cert := range certs {
		buf.Write(cert.Raw)
	}
	degenerate, err := pkcs7.DegenerateCertificate(buf.Bytes())
	if err != nil {
		return nil, err
	}
	return degenerate, nil
}

// CACerts extract CA Certificate or chain from pkcs7 degenerate signed data
func CACerts(data []byte) ([]*x509.Certificate, error) {
	p7, err := pkcs7.Parse(data)
	if err != nil {
		return nil, err
	}
	return p7.Certificates, nil
}

// NewCSRRequest creates a scep PKI PKCSReq/UpdateReq message
func NewCSRRequest(csr *x509.CertificateRequest, tmpl *PKIMessage, opts ...Option) (*PKIMessage, error) {
	conf := &config{logger: log.NewNopLogger(), certsSelector: NopCertsSelector()}
	for _, opt := range opts {
		opt(conf)
	}

	derBytes := csr.Raw
	recipients := conf.certsSelector.SelectCerts(tmpl.Recipients)
	if len(recipients) < 1 {
		if len(tmpl.Recipients) >= 1 {
			// our certsSelector eliminated any CA/RA recipients
			return nil, errors.New("no selected CA/RA recipients")
		}
		return nil, errors.New("no CA/RA recipients")
	}
	e7, err := pkcs7.Encrypt(derBytes, recipients)
	if err != nil {
		return nil, err
	}

	signedData, err := pkcs7.NewSignedData(e7)
	if err != nil {
		return nil, err
	}

	// create transaction ID from public key hash
	tID, err := newTransactionID(csr.PublicKey)
	if err != nil {
		return nil, err
	}

	sn, err := newNonce()
	if err != nil {
		return nil, err
	}

	level.Debug(conf.logger).Log(
		"msg", "creating SCEP CSR request",
		"transaction_id", tID,
		"signer_cn", tmpl.SignerCert.Subject.CommonName,
	)

	// PKIMessageAttributes to be signed
	config := pkcs7.SignerInfoConfig{
		ExtraSignedAttributes: []pkcs7.Attribute{
			{
				Type:  oidSCEPtransactionID,
				Value: tID,
			},
			{
				Type:  oidSCEPmessageType,
				Value: tmpl.MessageType,
			},
			{
				Type:  oidSCEPsenderNonce,
				Value: sn,
			},
		},
	}

	// sign attributes
	if err := signedData.AddSigner(tmpl.SignerCert, tmpl.SignerKey, config); err != nil {
		return nil, err
	}

	rawPKIMessage, err := signedData.Finish()
	if err != nil {
		return nil, err
	}

	cr := &CSRReqMessage{
		CSR: csr,
	}

	newMsg := &PKIMessage{
		Raw:           rawPKIMessage,
		MessageType:   tmpl.MessageType,
		TransactionID: tID,
		SenderNonce:   sn,
		CSRReqMessage: cr,
		Recipients:    recipients,
		logger:        conf.logger,
	}

	return newMsg, nil
}

func newNonce() (SenderNonce, error) {
	size := 16
	b := make([]byte, size)
	_, err := rand.Read(b)
	if err != nil {
		return SenderNonce{}, err
	}
	return SenderNonce(b), nil
}

// use public key to create a deterministric transactionID
func newTransactionID(key crypto.PublicKey) (TransactionID, error) {
	id, err := cryptoutil.GenerateSubjectKeyID(key)
	if err != nil {
		return "", err
	}

	encHash := base64.StdEncoding.EncodeToString(id)
	return TransactionID(encHash), nil
}
