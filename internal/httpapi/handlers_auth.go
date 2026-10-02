package httpapi

import (
	"context"

	"ticket/internal/account"
	"ticket/internal/httpapi/gen"
)

// Register creates an account.
func (s *Server) Register(ctx context.Context, req gen.RegisterRequestObject) (gen.RegisterResponseObject, error) {
	if err := s.check(req.Body); err != nil {
		return nil, err
	}
	u, err := s.Accounts.Register(ctx, req.Body.Email, req.Body.Password)
	if err != nil {
		return nil, err
	}
	return gen.Register201JSONResponse{Id: u.ID, Email: u.Email, Role: u.Role, CreatedAt: u.CreatedAt}, nil
}

// Login exchanges credentials for tokens.
func (s *Server) Login(ctx context.Context, req gen.LoginRequestObject) (gen.LoginResponseObject, error) {
	if err := s.check(req.Body); err != nil {
		return nil, err
	}
	t, err := s.Accounts.Login(ctx, req.Body.Email, req.Body.Password)
	if err != nil {
		return nil, err
	}
	return gen.Login200JSONResponse(tokenPair(t)), nil
}

// RefreshTokens rotates a refresh token.
func (s *Server) RefreshTokens(ctx context.Context, req gen.RefreshTokensRequestObject) (gen.RefreshTokensResponseObject, error) {
	if err := s.check(req.Body); err != nil {
		return nil, err
	}
	t, err := s.Accounts.Refresh(ctx, req.Body.RefreshToken)
	if err != nil {
		return nil, err
	}
	return gen.RefreshTokens200JSONResponse(tokenPair(t)), nil
}

// Logout revokes a refresh token.
func (s *Server) Logout(ctx context.Context, req gen.LogoutRequestObject) (gen.LogoutResponseObject, error) {
	if err := s.check(req.Body); err != nil {
		return nil, err
	}
	if err := s.Accounts.Logout(ctx, req.Body.RefreshToken); err != nil {
		return nil, err
	}
	// Access tokens are stateless and would stay valid until they expire; if the
	// caller sent one, put its id on the revocation list for its remaining life.
	if c, ok := userFrom(ctx); ok {
		if err := s.Cache.Revoke(ctx, c.JTI, c.ExpiresAt); err != nil {
			s.Log.WarnContext(ctx, "revoking access token", "err", err)
		}
	}
	return gen.Logout204Response{}, nil
}

func tokenPair(t account.Tokens) gen.TokenPair {
	return gen.TokenPair{
		AccessToken:  t.Access,
		RefreshToken: t.Refresh,
		TokenType:    "Bearer",
		ExpiresIn:    int(t.ExpiresIn.Seconds()),
	}
}
