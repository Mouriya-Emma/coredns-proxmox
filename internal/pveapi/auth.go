package pveapi

import (
	"context"
	"fmt"
	"net/http"
)

// AuthProvider authenticates requests to the Proxmox API.
type AuthProvider interface {
	Authenticate(context.Context) error
	UpdateRequest(r *http.Request)
}

// APITokenAuthProvider authenticates using a Proxmox API token.
type APITokenAuthProvider struct {
	User    string
	TokenID string
	Secret  string
}

func (a *APITokenAuthProvider) Authenticate(_ context.Context) error {
	return nil
}

func (a *APITokenAuthProvider) UpdateRequest(r *http.Request) {
	token := fmt.Sprintf("%s!%s=%s", a.User, a.TokenID, a.Secret)
	r.Header.Set("Authorization", "PVEAPIToken="+token)
}
