package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/jlbyh2o/llamaman/internal/api/middleware"
	"github.com/jlbyh2o/llamaman/internal/auth"
	"github.com/jlbyh2o/llamaman/internal/model"
	"github.com/jlbyh2o/llamaman/internal/store"
)

// The auth endpoints of DESIGN section 3.1, and the adapter that lets
// internal/auth stand behind the session gate without importing anything under
// internal/api (section 1, invariant 4: dependencies point inward).

// SessionService is everything this layer needs from internal/auth. It is
// declared here because the consumer owns the interface, and it speaks only
// domain types and strings — the cookie value, an address, a user agent — so the
// service on the other side of it never sees an *http.Request.
type SessionService interface {
	// SetupComplete reports whether `admin_account` exists — section 3's setup
	// gate.
	SetupComplete(ctx context.Context) (bool, error)
	// ResolveSession turns an `lm_session` cookie value into its row, or
	// auth.ErrNoSession. It also slides the session's idle window, which is why
	// it takes the address and user agent this request arrived with.
	ResolveSession(ctx context.Context, cookie, ip, userAgent string) (model.Session, error)
	// VerifyCSRF is the double-submit check; only internal/auth can recompute
	// the HMAC, because only it reads `sessions.csrf_secret`.
	VerifyCSRF(ctx context.Context, sessionID, cookie, header string) bool
	// AuthorizeSetup is D38: loopback may claim the daemon with no token at all,
	// every other origin must present a valid `X-Setup-Token`.
	AuthorizeSetup(ctx context.Context, loopback bool, presented, ip string) error

	// SessionState answers the public `GET /api/v1/auth/session`.
	SessionState(ctx context.Context, cookie string) (model.SessionState, error)
	// Login answers `POST /api/v1/auth/login`.
	Login(ctx context.Context, password, ip, userAgent string) (model.SessionCredential, error)
	// Logout revokes one session.
	Logout(ctx context.Context, sessionID string) error
	// ChangePassword verifies the current password, stores a new one and
	// revokes every other session.
	ChangePassword(ctx context.Context, sessionID, current, next string) error
	// Sessions lists the active sessions, marking the caller's own.
	Sessions(ctx context.Context, currentID string) ([]model.SessionInfo, error)
	// RevokeSession revokes one session by id, returning store.ErrNotFound when
	// there was nothing to revoke.
	RevokeSession(ctx context.Context, id string) error
}

// NewAuthenticator adapts a SessionService to the middleware's Authenticator.
//
// This is the ONE place an *http.Request becomes the strings a domain service
// takes: the cookie value, the client address, the user agent, the setup-token
// header, and D38's loopback question. Putting it here rather than in
// internal/auth is what keeps the import graph pointing inward — and it means
// the cookie NAMES, which section 3 fixes, are read in the same package that
// writes them.
func NewAuthenticator(svc SessionService) middleware.Authenticator {
	return authenticator{svc: svc}
}

type authenticator struct{ svc SessionService }

func (a authenticator) SetupComplete(ctx context.Context) (bool, error) {
	if a.svc == nil {
		return false, nil
	}
	return a.svc.SetupComplete(ctx)
}

func (a authenticator) Authenticate(ctx context.Context, r *http.Request) (*middleware.Session, error) {
	if a.svc == nil {
		return nil, middleware.ErrNoSession
	}
	sess, err := a.svc.ResolveSession(ctx, cookieValue(r, middleware.CookieSession),
		requestIP(r), r.UserAgent())
	if err != nil {
		if errors.Is(err, auth.ErrNoSession) {
			return nil, middleware.ErrNoSession
		}
		return nil, err
	}
	return &middleware.Session{
		ID:        sess.ID,
		CreatedAt: time.UnixMilli(sess.CreatedAt).UTC(),
		ExpiresAt: time.UnixMilli(sess.ExpiresAt).UTC(),
		IP:        deref(sess.IP),
		UserAgent: deref(sess.UserAgent),
	}, nil
}

func (a authenticator) AuthorizeSetup(ctx context.Context, r *http.Request) error {
	if a.svc == nil {
		if middleware.IsLoopback(r) {
			return nil
		}
		return middleware.ErrSetupTokenRequired
	}
	err := a.svc.AuthorizeSetup(ctx, middleware.IsLoopback(r),
		r.Header.Get(middleware.HeaderSetupToken), requestIP(r))
	if errors.Is(err, auth.ErrSetupTokenRequired) {
		return middleware.ErrSetupTokenRequired
	}
	return err
}

func (a authenticator) VerifyCSRF(ctx context.Context, s *middleware.Session, cookie, header string) bool {
	if a.svc == nil || s == nil {
		return false
	}
	return a.svc.VerifyCSRF(ctx, s.ID, cookie, header)
}

// SessionStateDTO is the body of `GET /api/v1/auth/session` (section 3.1).
//
// `claimed` and `setup_complete` are DIFFERENT questions and answering them from
// two different places is the point — the same point `GET /api/v1/meta` already
// makes in its own words. `claimed` is "`admin_account` exists": the one-time
// setup token has been burned. `setup_complete` is "the wizard's `done` step is
// complete".
//
// A host can be claimed without being complete — that is a wizard interrupted
// after the password step, which section 11.2 requires be RESUMABLE — and
// collapsing the two sends such a browser to a dashboard it is not ready for.
// This endpoint used to answer `setup_complete` with the account fact, so the
// SPA, which branches on it, abandoned every unfinished wizard the moment a
// user navigated to anything but a `/setup` URL. Both facts are on the wire
// now, and the shell reads them as the two questions they are.
type SessionStateDTO struct {
	Authenticated bool `json:"authenticated"`
	// Claimed is `admin_account` exists.
	Claimed bool `json:"claimed"`
	// SetupComplete is the wizard's `done` step is complete.
	SetupComplete bool `json:"setup_complete"`
	// ExpiresAt is null when the request carries no session.
	ExpiresAt *string `json:"expires_at"`
}

// LoginRequest is the body of `POST /api/v1/auth/login`.
type LoginRequest struct {
	Password string `json:"password"`
}

// PasswordChangeRequest is the body of `POST /api/v1/auth/password`.
type PasswordChangeRequest struct {
	Current string `json:"current"`
	Next    string `json:"next"`
}

// SessionDTO is one row of `GET /api/v1/auth/sessions`. It carries no hash and
// no secret — the id is the public half of the cookie and is what
// `DELETE /api/v1/auth/sessions/{id}` takes.
type SessionDTO struct {
	ID         string  `json:"id"`
	IP         *string `json:"ip"`
	UserAgent  *string `json:"user_agent"`
	CreatedAt  string  `json:"created_at"`
	LastSeenAt string  `json:"last_seen_at"`
	ExpiresAt  string  `json:"expires_at"`
	// Current marks the session this request arrived on.
	Current bool `json:"current"`
}

func (a *API) authRoutes() []Route {
	return []Route{
		a.sessionStateRoute(),
		a.loginRoute(),
		a.logoutRoute(),
		a.changePasswordRoute(),
		a.listSessionsRoute(),
		a.revokeSessionRoute(),
	}
}

func (a *API) sessionStateRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/auth/session",
		Auth:        AuthPublic,
		OperationID: "getAuthSession",
		Summary: "Whether this request carries a session, whether the host has been claimed, " +
			"whether the wizard has finished, and when the session expires.",
		Tag: "auth",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.sessions()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			st, err := svc.SessionState(r.Context(), cookieValue(r, middleware.CookieSession))
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			// A live session refreshes its `lm_csrf` cookie here. The cookie is
			// readable by the SPA and is the only half of the double-submit the
			// browser can lose on its own — a cleared site cookie, a session
			// cookie that outlived a browser restart — and this endpoint is the
			// one the SPA calls before every mutation path anyway.
			if st.Authenticated {
				a.refreshCSRFCookie(w, r)
			}
			// The auth service knows only the ACCOUNT fact, which is
			// `claimed`. Whether the wizard finished is the setup service's
			// answer and reaches this layer through the same MetaProvider
			// `GET /api/v1/meta` uses, so the two endpoints can never come to
			// disagree about what "set up" means.
			dto := SessionStateDTO{
				Authenticated: st.Authenticated,
				Claimed:       st.SetupComplete,
				SetupComplete: st.SetupComplete,
				ExpiresAt:     TimePtr(st.ExpiresAt),
			}
			if a.cfg.Meta != nil {
				meta, err := a.cfg.Meta.Meta(r.Context())
				if err != nil {
					WriteError(w, r, a.log, err)
					return
				}
				dto.Claimed, dto.SetupComplete = meta.Claimed, meta.SetupComplete
			}
			if err := WriteJSON(w, http.StatusOK, dto); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "The session state of this request.",
			Body:        SessionStateDTO{},
		},
	}
}

func (a *API) loginRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/auth/login",
		Auth:        AuthPublic,
		OperationID: "login",
		Summary:     "Exchange the admin password for a session cookie and a CSRF cookie.",
		Tag:         "auth",
		RequestBody: LoginRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.sessions()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			var req LoginRequest
			if err := DecodeJSON(w, r, &req); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			cred, err := svc.Login(r.Context(), req.Password, requestIP(r), r.UserAgent())
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			a.setSessionCookies(w, r, cred)
			WriteNoContent(w)
		}),
		Success: Response{
			Status:      http.StatusNoContent,
			Description: "Authenticated. `lm_session` and `lm_csrf` are set.",
		},
		Errors: []Response{
			{
				Status:      http.StatusUnauthorized,
				Description: "The password is wrong, or this host has no admin account.",
				Codes:       []model.ErrorCode{model.CodeBadCredentials},
			},
			{
				Status: http.StatusTooManyRequests,
				Description: "This address has exhausted its login attempts (SPEC section 4). " +
					"`details.retry_after_sec` says for how long.",
				Codes: []model.ErrorCode{model.CodeLockedOut},
			},
		},
	}
}

func (a *API) logoutRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/auth/logout",
		Auth:        AuthSession,
		OperationID: "logout",
		Summary:     "Revoke this session and clear its cookies.",
		Tag:         "auth",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.sessions()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			sess, _ := middleware.SessionFrom(r.Context())
			if sess == nil {
				WriteError(w, r, a.log, Errorf(http.StatusUnauthorized,
					middleware.CodeUnauthorized, "a valid admin session is required"))
				return
			}
			if err := svc.Logout(r.Context(), sess.ID); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			a.clearSessionCookies(w, r)
			WriteNoContent(w)
		}),
		Success: Response{
			Status:      http.StatusNoContent,
			Description: "The session is revoked and both cookies are cleared.",
		},
	}
}

func (a *API) changePasswordRoute() Route {
	return Route{
		Method:      http.MethodPost,
		Pattern:     BasePath + "/auth/password",
		Auth:        AuthSession,
		OperationID: "changePassword",
		Summary:     "Change the admin password; every other session is revoked.",
		Tag:         "auth",
		RequestBody: PasswordChangeRequest{},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.sessions()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			sess, _ := middleware.SessionFrom(r.Context())
			if sess == nil {
				WriteError(w, r, a.log, Errorf(http.StatusUnauthorized,
					middleware.CodeUnauthorized, "a valid admin session is required"))
				return
			}
			var req PasswordChangeRequest
			if err := DecodeJSON(w, r, &req); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			if err := svc.ChangePassword(r.Context(), sess.ID, req.Current, req.Next); err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			WriteNoContent(w)
		}),
		Success: Response{
			Status:      http.StatusNoContent,
			Description: "The password was changed and every other session was revoked.",
		},
		Errors: []Response{
			{
				Status:      http.StatusUnauthorized,
				Description: "The current password is wrong.",
				Codes:       []model.ErrorCode{model.CodeBadCredentials},
			},
			{
				Status:      http.StatusBadRequest,
				Description: "The new password does not meet the minimum length.",
				Codes:       []model.ErrorCode{model.CodePasswordInvalid},
			},
		},
	}
}

func (a *API) listSessionsRoute() Route {
	return Route{
		Method:      http.MethodGet,
		Pattern:     BasePath + "/auth/sessions",
		Auth:        AuthSession,
		OperationID: "listSessions",
		Summary:     "The active admin sessions, with address, user agent and last-seen time.",
		Tag:         "auth",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.sessions()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			sess, _ := middleware.SessionFrom(r.Context())
			current := ""
			if sess != nil {
				current = sess.ID
			}
			rows, err := svc.Sessions(r.Context(), current)
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			items := make([]SessionDTO, 0, len(rows))
			for _, v := range rows {
				items = append(items, SessionDTO{
					ID:         v.ID,
					IP:         v.IP,
					UserAgent:  v.UserAgent,
					CreatedAt:  Time(v.CreatedAt),
					LastSeenAt: Time(v.LastSeenAt),
					ExpiresAt:  Time(v.ExpiresAt),
					Current:    v.Current,
				})
			}
			if err := WriteJSON(w, http.StatusOK, NewList(items, len(items), nil)); err != nil {
				WriteError(w, r, a.log, err)
			}
		}),
		Success: Response{
			Status:      http.StatusOK,
			Description: "Every session that is neither revoked nor expired.",
			Body:        List[SessionDTO]{},
		},
	}
}

func (a *API) revokeSessionRoute() Route {
	return Route{
		Method:      http.MethodDelete,
		Pattern:     BasePath + "/auth/sessions/{id}",
		Auth:        AuthSession,
		OperationID: "revokeSession",
		Summary:     "Revoke one session — a device the operator no longer uses, or their own.",
		Tag:         "auth",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc, err := a.sessions()
			if err != nil {
				WriteError(w, r, a.log, err)
				return
			}
			id := r.PathValue("id")
			if err := svc.RevokeSession(r.Context(), id); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					WriteError(w, r, a.log, NotFound("no active session %q", id))
					return
				}
				WriteError(w, r, a.log, err)
				return
			}
			// Revoking one's own session is allowed and is simply a logout, so
			// the cookies go with it rather than leaving the browser holding a
			// credential that no longer resolves.
			if sess, ok := middleware.SessionFrom(r.Context()); ok && sess.ID == id {
				a.clearSessionCookies(w, r)
			}
			WriteNoContent(w)
		}),
		Success: Response{
			Status:      http.StatusNoContent,
			Description: "The session is revoked.",
		},
		Errors: []Response{{
			Status:      http.StatusNotFound,
			Description: "No active session has that id.",
			Codes:       []model.ErrorCode{CodeNotFound},
		}},
	}
}

// sessions returns the wired SessionService, or the 503 a daemon built without
// one answers with. It is the same shape `GET /api/v1/meta` uses for a missing
// provider: a construction gap is reported, never faked.
func (a *API) sessions() (SessionService, error) {
	if a.cfg.Sessions == nil {
		return nil, Errorf(http.StatusServiceUnavailable, CodeInternalError,
			"this daemon was built without an authentication service")
	}
	return a.cfg.Sessions, nil
}

// setSessionCookies writes the two cookies DESIGN section 3 fixes.
//
//   - `lm_session` is HttpOnly: script must never be able to read the session
//     secret, which is the whole reason the CSRF token is a second, separate
//     value rather than the session itself.
//   - `lm_csrf` is deliberately NOT HttpOnly: the SPA reads it and echoes it in
//     `X-CSRF-Token`. It holds an HMAC of the session's `csrf_secret`, never the
//     secret.
//
// Both are SameSite=Lax, Path=/, and `Secure` only when the request arrived over
// TLS — a `Secure` cookie on the plain-HTTP LAN deployment SPEC section 4
// describes would simply never be sent back, which would lock the operator out
// of their own daemon.
func (a *API) setSessionCookies(w http.ResponseWriter, r *http.Request, cred model.SessionCredential) {
	expires := time.UnixMilli(cred.ExpiresAt).UTC()
	secure := isTLS(r)

	http.SetCookie(w, &http.Cookie{
		Name:     middleware.CookieSession,
		Value:    cred.SessionCookie,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     middleware.CookieCSRF,
		Value:    cred.CSRFToken,
		Path:     "/",
		Expires:  expires,
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookies expires both cookies. MaxAge=-1 deletes them now, which is
// what a logout has to do for a browser that would otherwise keep sending a
// revoked session id on every request.
func (a *API) clearSessionCookies(w http.ResponseWriter, r *http.Request) {
	secure := isTLS(r)
	for _, name := range []string{middleware.CookieSession, middleware.CookieCSRF} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: name == middleware.CookieSession,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

// refreshCSRFCookie re-sets `lm_csrf` for the session this request carries. It is
// a no-op when the request has no session, and it never touches `lm_session`:
// the session's own lifetime is decided at login and by the idle window, not by a
// page load.
func (a *API) refreshCSRFCookie(w http.ResponseWriter, r *http.Request) {
	sess, err := a.cfg.Sessions.ResolveSession(r.Context(),
		cookieValue(r, middleware.CookieSession), requestIP(r), r.UserAgent())
	if err != nil {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     middleware.CookieCSRF,
		Value:    auth.CSRFToken(sess.ID, sess.CSRFSecret),
		Path:     "/",
		Expires:  time.UnixMilli(sess.ExpiresAt).UTC(),
		HttpOnly: false,
		Secure:   isTLS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// isTLS reports whether this request arrived over TLS, which is the only
// condition under which section 3 sets `Secure`.
func isTLS(r *http.Request) bool { return r.TLS != nil }

// cookieValue reads one cookie, returning "" when it is absent or malformed.
func cookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

// requestIP is r.RemoteAddr's host and nothing else — the same rule the rate
// limiter and the session audit columns follow. X-Forwarded-For is
// attacker-controlled on a daemon that binds a LAN address directly, and trusting
// it would let any client mint an unlimited number of lockout buckets by varying
// one header.
func requestIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
