package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"pulse/internal/apigen"
	"pulse/internal/authn"
	"pulse/internal/authz"
	"pulse/internal/crypto"
	"pulse/internal/domain"
	"pulse/internal/entitlements"
)

// This file implements the API-key management slice (PRD-005 2, RFC-003 5): create,
// list, and revoke per-org keys for the public REST surface. The role gate is
// authz.ActionManageAPIKeys (owner/admin only). A key's own role is member or admin
// only; an owner-equivalent key is rejected (PRD-001 App A). The full secret is
// returned exactly once at creation and never stored in clear; only its SHA-256 hash
// and a non-secret prefix are stored. Every error is the localizable {code, message}
// envelope (RFC-012 / RFC-014).

// apiKeyPrefixLen is how many chars of the random part go into the stored, listable
// prefix so a key is identifiable in the list without exposing the secret.
const apiKeyPrefixLen = 6

// ListAPIKeys returns the org's keys, metadata only (PRD-005 2). The secret is never
// stored, so it is never in the response. Owner/admin only.
func (s *Server) ListAPIKeys(ctx context.Context, _ apigen.ListAPIKeysRequestObject) (apigen.ListAPIKeysResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.ListAPIKeys401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageAPIKeys, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.ListAPIKeys403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	keys, err := s.store.ListAPIKeys(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	out := make([]apigen.APIKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, apiKeyDTO(k))
	}
	return apigen.ListAPIKeys200JSONResponse(out), nil
}

// CreateAPIKey mints a key (PRD-005 2). It generates pulse_sk_<random>, stores the
// SHA-256 hash + a non-secret prefix + the role + created_by, and returns the FULL
// secret exactly once plus the key metadata. role is member or admin only; owner is
// rejected (keys are never owner-equivalent, PRD-001 App A). Owner/admin only.
func (s *Server) CreateAPIKey(ctx context.Context, req apigen.CreateAPIKeyRequestObject) (apigen.CreateAPIKeyResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.CreateAPIKey401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageAPIKeys, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.CreateAPIKey403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	// Plan gate: the Free tier has no API access (pricing.html), so it cannot create
	// keys. Paid tiers can; the read-only vs full distinction is enforced per request
	// via APIWriteAllowed, not here.
	if !entitlements.APIAccessAllowed(s.orgPlan(ctx, p.OrgID)) {
		return apigen.CreateAPIKey403JSONResponse{ForbiddenJSONResponse: forbidden("api keys are not available on your plan; upgrade to use the API")}, nil
	}
	if req.Body == nil {
		return apigen.CreateAPIKey422JSONResponse{ValidationFailedJSONResponse: validationFailed("body required")}, nil
	}
	name := strings.TrimSpace(req.Body.Name)
	if name == "" {
		return apigen.CreateAPIKey422JSONResponse{ValidationFailedJSONResponse: validationFailed("name required")}, nil
	}
	role := domain.Role(req.Body.Role)
	// A key maxes out at admin: member or admin only, never owner-equivalent
	// (PRD-001 App A, RFC-003 5.4).
	if role != domain.RoleMember && role != domain.RoleAdmin {
		return apigen.CreateAPIKey422JSONResponse{ValidationFailedJSONResponse: validationFailed("role must be member or admin")}, nil
	}

	secret, err := newAPIKeySecret()
	if err != nil {
		return nil, err
	}
	creator := p.UserID
	k := &domain.APIKey{
		OrgID:     p.OrgID,
		Name:      name,
		Prefix:    apiKeyListablePrefix(secret),
		TokenHash: crypto.HashToken(secret),
		Role:      role,
		CreatedBy: &creator,
	}
	if _, err := s.store.CreateAPIKey(ctx, k); err != nil {
		return nil, err
	}
	// Re-read the row so created_at (and any DB defaults) are populated for the DTO.
	stored, err := s.store.GetAPIKey(ctx, p.OrgID, k.ID)
	if err != nil {
		return nil, err
	}
	return apigen.CreateAPIKey201JSONResponse(apigen.APIKeyCreated{
		Key:    apiKeyDTO(stored),
		Secret: secret,
	}), nil
}

// RevokeAPIKey revokes a key (PRD-005 2). After this the key fails auth immediately:
// the store stamps revoked_at and we bust the verify cache so the next request misses
// the cache and re-reads the revoked row (RFC-003 5.3). Owner/admin only.
func (s *Server) RevokeAPIKey(ctx context.Context, req apigen.RevokeAPIKeyRequestObject) (apigen.RevokeAPIKeyResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.RevokeAPIKey401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageAPIKeys, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.RevokeAPIKey403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	keyID, err := strconv.ParseInt(req.Id, 10, 64)
	if err != nil {
		return apigen.RevokeAPIKey404JSONResponse{NotFoundJSONResponse: notFound("api key not found")}, nil
	}
	// Read the key first so we have its token_hash to bust the cache after revoke.
	key, err := s.store.GetAPIKey(ctx, p.OrgID, keyID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.RevokeAPIKey404JSONResponse{NotFoundJSONResponse: notFound("api key not found")}, nil
		}
		return nil, err
	}
	if _, err := s.store.RevokeAPIKey(ctx, p.OrgID, keyID); err != nil {
		return nil, err
	}
	// Bust the verify cache so the next request misses and sees the revoked row. A
	// no-op revoke (already revoked) still busts, which is harmless.
	if s.keys != nil {
		_ = s.keys.InvalidateAPIKey(ctx, key.TokenHash)
	}
	return apigen.RevokeAPIKey204Response{}, nil
}

// --- helpers ---

// apiKeyDTO maps a stored key to the API metadata shape. The secret is never stored,
// so it is never present here.
func apiKeyDTO(k *domain.APIKey) apigen.APIKey {
	var createdBy *string
	if k.CreatedBy != nil {
		s := strconv.FormatInt(*k.CreatedBy, 10)
		createdBy = &s
	}
	return apigen.APIKey{
		Id:         strconv.FormatInt(k.ID, 10),
		Name:       k.Name,
		Prefix:     k.Prefix,
		Role:       apigen.Role(k.Role),
		CreatedBy:  createdBy,
		CreatedAt:  k.CreatedAt,
		LastUsedAt: k.LastUsedAt,
	}
}

// newAPIKeySecret mints the raw key pulse_sk_<random> (RFC-003 5.1). It is
// high-entropy and unguessable; only its SHA-256 hash is stored, and the raw value
// is returned exactly once at creation. 32 random bytes, base64url, matches the
// refresh/invite token generators (RFC-003 1.4).
func newAPIKeySecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return authn.APIKeyPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// apiKeyListablePrefix returns the non-secret leading chars of a key, safe to store
// and list: the fixed pulse_sk_ prefix plus a few chars of the random part so a key
// is identifiable in the list without exposing the secret.
func apiKeyListablePrefix(secret string) string {
	body := strings.TrimPrefix(secret, authn.APIKeyPrefix)
	n := apiKeyPrefixLen
	if len(body) < n {
		n = len(body)
	}
	return authn.APIKeyPrefix + body[:n]
}
