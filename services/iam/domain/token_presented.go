package domain

import (
	"strings"

	"github.com/kfirgollan/NebiusMocker/pkg/auth"
)

// Presented describes, unverified, the credential a token exchange request
// presents: what the audit event of a refused exchange names. Its fields are
// attacker-controlled when the exchange is refused; it holds no signature or
// secret, except Token, which the audit log masks.
type Presented struct {
	// KeyID, Algorithm, Issuer and Subject are the kid and alg headers and
	// the iss and sub claims of the presented JWT: the service account JWT
	// (subject_token), or the external issuer's JWT of a federated exchange
	// (actor_token).
	KeyID     string
	Algorithm string
	Issuer    string
	Subject   string
	// ServiceAccountID is the service account the exchange is for.
	ServiceAccountID string
	// CredentialID is the resource the exchange presented: the auth public
	// key (kid) of a service account JWT, else the service account.
	CredentialID string
	// Token is the presented token: the JWT, or the actor's access token of
	// an impersonation.
	Token string
}

// PresentedBy describes the credential req presents, without verifying it.
func PresentedBy(req ExchangeRequest) Presented {
	var p Presented
	jwtToken := false
	switch req.SubjectTokenType {
	case TokenTypeJWT:
		p.Token, jwtToken = req.SubjectToken, true
	case TokenTypeSubjectIdentifier:
		p.Token, jwtToken = req.ActorToken, req.ActorTokenType == TokenTypeJWT
		if strings.HasPrefix(req.SubjectToken, KindServiceAccount+"-") {
			p.ServiceAccountID = req.SubjectToken
		}
	}
	if h, ok := auth.PeekJWT(p.Token); ok && jwtToken {
		p.KeyID, p.Algorithm, p.Issuer, p.Subject = h.KeyID, h.Algorithm, h.Issuer, h.Subject
	}
	if req.SubjectTokenType == TokenTypeJWT {
		for _, id := range []string{p.Subject, p.Issuer} {
			if p.ServiceAccountID == "" && strings.HasPrefix(id, KindServiceAccount+"-") {
				p.ServiceAccountID = id
			}
		}
		if strings.HasPrefix(p.KeyID, KindAuthPublicKey+"-") {
			p.CredentialID = p.KeyID
		}
	}
	if p.CredentialID == "" {
		p.CredentialID = p.ServiceAccountID
	}
	return p
}
