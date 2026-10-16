package accounts

import (
	"context"
	"errors"

	"pricing-engine/internal/server"
)

// HubAuthorizer adapts the Service to the WebSocket hub's Authorizer.
type HubAuthorizer struct{ S *Service }

func (h HubAuthorizer) AuthenticateKey(ctx context.Context, keyID, secret string) (server.Identity, error) {
	a, err := h.S.AuthenticateKey(ctx, keyID, secret)
	return identity(a, err)
}

func (h HubAuthorizer) RedeemTicket(ctx context.Context, t string) (server.Identity, error) {
	a, err := h.S.RedeemTicket(ctx, t)
	return identity(a, err)
}

func (h HubAuthorizer) Allowed(ctx context.Context, id string) bool { return h.S.Allowed(ctx, id) }

func identity(a *Account, err error) (server.Identity, error) {
	if errors.Is(err, ErrNotVerified) {
		return server.Identity{}, server.ErrNotPermitted
	}
	if err != nil {
		return server.Identity{}, err
	}
	return server.Identity{ID: a.ID, Email: a.Email}, nil
}
