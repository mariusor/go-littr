package app

import (
	"context"
	"fmt"
	pub "github.com/go-ap/activitypub"
	"github.com/go-ap/errors"
	"github.com/go-chi/chi"
	"github.com/gorilla/csrf"
	"github.com/mariusor/littr.go/internal/config"
	"github.com/mariusor/littr.go/internal/log"
	"golang.org/x/oauth2"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const (
	sessionName           = "_s"
	csrfName              = "_c"
	sessionsCookieBackend = "cookie"
	sessionsFSBackend     = "fs"
)

type handler struct {
	conf    appConfig
	v       *view
	storage *repository
	logger  log.Logger
	infoFn  CtxLogFn
	errFn   CtxLogFn
}

var defaultAccount = AnonymousAccount

type appConfig struct {
	config.Configuration
	BaseURL         string
	SessionKeys     [][]byte
	SessionsBackend string
	SessionsPath    string
	Logger          log.Logger
}

var defaultLogFn = func(string, ...interface{}) {}
var defaultCtxLogFn = func(c ...log.Ctx) LogFn { return defaultLogFn }

func hideString(s string) string {
	l := len(s)

	if l <= 3 {
		return "***"
	}
	ss := strings.Repeat("*", l-3)

	return ss + s[l-3:]
}

func Init(c appConfig) (*handler, error) {
	var err error

	h := new(handler)

	h.infoFn = defaultCtxLogFn
	h.errFn = defaultCtxLogFn
	if c.Logger != nil {
		h.infoFn = func(ctx ...log.Ctx) LogFn {
			return c.Logger.WithContext(ctx...).Infof
		}
		h.errFn = func(ctx ...log.Ctx) LogFn {
			return c.Logger.WithContext(ctx...).Errorf
		}
		h.logger = c.Logger
	}

	if c.SessionsBackend = os.Getenv("SESSIONS_BACKEND"); c.SessionsBackend == "" {
		c.SessionsBackend = sessionsFSBackend
	}
	if c.SessionsPath = os.Getenv("SESSIONS_PATH"); c.SessionsPath == "" {
		c.SessionsPath = os.TempDir()
	}
	c.SessionsBackend = strings.ToLower(c.SessionsBackend)
	c.SessionKeys = loadEnvSessionKeys()
	h.conf = c

	h.storage = ActivityPubService(c)

	provider := "fedbox"
	config := GetOauth2Config(provider, h.conf.BaseURL)
	if len(config.ClientID) > 0 {
		oIRI := actors.IRI(pub.IRI(h.storage.BaseURL)).AddPath(config.ClientID)
		oauth, err := h.storage.fedbox.Actor(context.Background(), oIRI)
		if err != nil {
			h.conf.UserCreatingEnabled = false
			h.errFn()("Failed to load actor: %s", err)
		}
		if oauth != nil {
			h.storage.app = new(Account)
			h.storage.app.FromActivityPub(oauth)

			handle := h.storage.app.Handle
			tok, err := config.PasswordCredentialsToken(context.Background(), handle, config.ClientSecret)
			ctx := log.Ctx{
				"handle":      handle,
				"provider":    provider,
				"client":      config.ClientID,
				"pw":          hideString(config.ClientSecret),
				"authURL":     config.Endpoint.AuthURL,
				"tokURL":      config.Endpoint.TokenURL,
				"redirectURL": config.RedirectURL,
			}
			if err != nil {
				h.conf.UserCreatingEnabled = false
				h.errFn(log.Ctx{"err": err}, ctx)("Failed to authenticate client")
			} else {
				if tok == nil {
					h.conf.UserCreatingEnabled = false
					h.errFn(ctx)("Failed to load a valid OAuth2 token for client")
				}
				h.storage.app.Metadata.OAuth.Provider = provider
				h.storage.app.Metadata.OAuth.Token = tok
				h.infoFn(ctx, log.Ctx{
					"token":   hideString(tok.AccessToken),
					"type":    tok.TokenType,
					"refresh": hideString(tok.RefreshToken),
				})("Loaded valid OAuth2 token for client")

			}
		}
	} else {
		h.conf.UserCreatingEnabled = false
		h.errFn(log.Ctx{"conf": config})("Failed to load OAuth2 ClientID")
	}
	h.v, err = ViewInit(h.conf, h.infoFn, h.errFn)
	if err != nil {
		h.errFn(log.Ctx{"err": err})("Error initializing view")
	}
	return h, err
}

type headerEl struct {
	IsCurrent bool
	Icon      string
	Auth      bool
	Name      string
	URL       string
}

func loggedAccount(r *http.Request) *Account {
	if acct := ContextAccount(r.Context()); acct != nil {
		return acct
	}
	return &defaultAccount
}

// HandleCallback serves /auth/{provider}/callback request
func (h *handler) HandleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	provider := chi.URLParam(r, "provider")
	providerErr := q["error"]
	if providerErr != nil {
		errDescriptions := q["error_description"]
		var errs = make([]error, 1)
		errs[0] = errors.Errorf("Error for provider %q:\n", provider)
		for _, errDesc := range errDescriptions {
			errs = append(errs, errors.Errorf(errDesc))
		}
		h.v.HandleErrors(w, r, errs...)
		return
	}
	code := q.Get("code")
	state := q.Get("state")
	if len(code) == 0 {
		h.v.HandleErrors(w, r, errors.Forbiddenf("%s error: Empty authentication token", provider))
		return
	}

	conf := GetOauth2Config(provider, h.conf.BaseURL)
	tok, err := conf.Exchange(r.Context(), code)
	if err != nil {
		h.errFn(log.Ctx{"err": err})("Unable to load token")
		h.v.HandleErrors(w, r, err)
		return
	}

	account := h.v.loadCurrentAccountFromSession(w, r)
	account.Metadata.OAuth = OAuth{
		State:    state,
		Code:     code,
		Provider: provider,
		Token:    tok,
	}

	if err := h.v.saveAccountToSession(w, r, account); err == nil {
		if strings.ToLower(provider) != "local" {
			h.v.addFlashMessage(Success, w, r, fmt.Sprintf("Login successful with %s", provider))
		} else {
			h.v.addFlashMessage(Success, w, r, "Login successful")
		}
	} else {
		h.errFn()("Unable to save account to session")
		//h.v.addFlashMessage(Success, w, r, fmt.Sprintf("Login successful with %s", provider))
	}
	h.v.Redirect(w, r, "/", http.StatusFound)
}

func GetOauth2Config(provider string, localBaseURL string) oauth2.Config {
	var config oauth2.Config
	switch strings.ToLower(provider) {
	case "github":
		config = oauth2.Config{
			ClientID:     os.Getenv("GITHUB_KEY"),
			ClientSecret: os.Getenv("GITHUB_SECRET"),
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://github.com/login/oauth/authorize",
				TokenURL: "https://github.com/login/oauth/access_token",
			},
		}
	case "gitlab":
		config = oauth2.Config{
			ClientID:     os.Getenv("GITLAB_KEY"),
			ClientSecret: os.Getenv("GITLAB_SECRET"),
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://gitlab.com/login/oauth/authorize",
				TokenURL: "https://gitlab.com/login/oauth/access_token",
			},
		}
	case "facebook":
		config = oauth2.Config{
			ClientID:     os.Getenv("FACEBOOK_KEY"),
			ClientSecret: os.Getenv("FACEBOOK_SECRET"),
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://graph.facebook.com/oauth/authorize",
				TokenURL: "https://graph.facebook.com/oauth/access_token",
			},
		}
	case "google":
		config = oauth2.Config{
			ClientID:     os.Getenv("GOOGLE_KEY"),
			ClientSecret: os.Getenv("GOOGLE_SECRET"),
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://accounts.google.com/o/oauth2/auth", // access_type=offline
				TokenURL: "https://accounts.google.com/o/oauth2/token",
			},
		}
	case "fedbox":
		fallthrough
	default:
		apiURL := os.Getenv("API_URL")
		config = oauth2.Config{
			ClientID:     os.Getenv("OAUTH2_KEY"),
			ClientSecret: os.Getenv("OAUTH2_SECRET"),
			Endpoint: oauth2.Endpoint{
				AuthURL:  fmt.Sprintf("%s/oauth/authorize", apiURL),
				TokenURL: fmt.Sprintf("%s/oauth/token", apiURL),
			},
		}
	}
	confOauth2URL := os.Getenv("OAUTH2_URL")
	if u, err := url.Parse(confOauth2URL); err != nil || u.Host == "" {
		config.RedirectURL = fmt.Sprintf("%s/auth/%s/callback", localBaseURL, provider)
	}
	return config
}

func isInverted(r *http.Request) bool {
	cookies := r.Cookies()
	for _, c := range cookies {
		if c.Name == "inverted" {
			return true
		}
	}
	return false
}

func (v *view) saveAccountToSession(w http.ResponseWriter, r *http.Request, a Account) error {
	if !v.s.enabled || w == nil || r == nil {
		return nil
	}
	s, err := v.s.get(w, r)
	if err != nil {
		return err
	}
	s.Values[SessionUserKey] = a
	return s.Save(r, w)
}

func (v *view) loadCurrentAccountFromSession(w http.ResponseWriter, r *http.Request) Account {
	if !v.s.enabled || w == nil || r == nil {
		return defaultAccount
	}
	acc := defaultAccount
	s, err := v.s.get(w, r)
	if err != nil {
		return defaultAccount
	}
	// load the current account from the session or setting it to anonymous
	raw, ok := s.Values[SessionUserKey]
	if !ok {
		v.errFn(log.Ctx{"sess": s.Values})("no account data saved to session")
	} else if acc, ok = raw.(Account); !ok {
		v.errFn(log.Ctx{"sess": s.Values})("invalid account in session")
	} else {
		v.infoFn(log.Ctx{
			"handle": acc.Handle,
			"hash":   acc.Hash,
		})("loaded account from session")
	}
	return acc
}

func (h *handler) SetSecurityHeaders(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if !h.conf.Env.IsDev() && h.conf.Secure {
			w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
		}
		h.v.SetCSP(ContextModel(r.Context()), w)
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Xss-Protection", "1; mode=block")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func loadAccountData(a *Account, b Account) {
	if !HashesEqual(a.Hash, b.Hash) {
		return
	}
	if len(a.Email) == 0 && len(b.Email) > 0 {
		a.Email = b.Email
	}
	if len(a.Handle) == 0 && len(b.Handle) > 0 {
		a.Handle = b.Handle
	}
	if a.CreatedAt.IsZero() && !b.CreatedAt.IsZero() {
		a.CreatedAt = b.CreatedAt
	}
	if a.CreatedBy == nil && b.CreatedBy != nil {
		a.CreatedBy = b.CreatedBy
	}
	if a.UpdatedAt.IsZero() && !b.UpdatedAt.IsZero() {
		a.UpdatedAt = b.UpdatedAt
	}
	if a.Metadata == nil && b.Metadata != nil {
		a.Metadata = b.Metadata
	}
	if a.pub == nil && b.pub != nil {
		a.pub = b.pub
	}
	if len(a.Votes) == 0 && len(b.Votes) > 0 {
		a.Votes = b.Votes
	}
	if len(a.Followers) == 0 && len(b.Followers) > 0 {
		a.Followers = b.Followers
	}
	if len(a.Following) == 0 && len(b.Following) > 0 {
		a.Following = b.Following
	}
	if len(a.Blocked) == 0 && len(b.Blocked) > 0 {
		a.Blocked = b.Blocked
	}
	if len(a.Ignored) == 0 && len(b.Ignored) > 0 {
		a.Ignored = b.Ignored
	}
	if a.Parent == nil && b.Parent != nil {
		a.Parent = b.Parent
	}
	if len(a.Children) == 0 && len(b.Children) > 0 {
		a.Children = b.Children
	}
}

func (h *handler) LoadSession(next http.Handler) http.Handler {
	if !h.conf.SessionsEnabled {
		return next
	}
	fn := func(w http.ResponseWriter, r *http.Request) {
		h.storage.WithAccount(nil)
		acc := AnonymousAccount
		clearCookie := true
		if h.v != nil {
			acc = h.v.loadCurrentAccountFromSession(w, r)
			clearCookie = false
		}
		if acc.IsLogged() {
			f := &Filters{
				Name: CompStrs{EqualsString(acc.Handle)},
				//IRI:  CompStrs{EqualsString(acc.Hash.String())},
				Type: ActivityTypesFilter(ValidActorTypes...),
			}
			ctx := context.Background()
			ltx := log.Ctx{
				"handle": acc.Handle,
				"hash":   acc.Hash,
			}
			if accounts, err := h.storage.accounts(ctx, f); err != nil {
				ctxtErr(next, w, r, err)
			} else {
				if len(accounts) == 0 {
					err := errors.NotFoundf("no accounts found for %v", f)
					h.infoFn(ltx)("Error: %s", err)
				}
				if !accounts[0].IsValid() {
					ctxtErr(next, w, r, errors.NotFoundf("Not found"))
				}
				loadAccountData(&acc, accounts[0])
			}

			h.storage.WithAccount(&acc)
			var err error
			if len(acc.Followers) == 0 {
				// TODO(marius): this needs to be moved to where we're handling all Inbox activities, not on page load
				acc, err = h.storage.loadAccountsFollowers(ctx, acc)
				if err != nil {
					h.infoFn(ltx)("Error: %s", err)
				}
			}
			if len(acc.Following) == 0 {
				acc, err = h.storage.loadAccountsFollowing(ctx, acc)
				if err != nil {
					h.infoFn(ltx)("Error: %s", err)
				}
			}
			if len(acc.Blocked) == 0 || len(acc.Ignored) == 0 {
				acc, err = h.storage.loadAccountsBlockedIgnored(ctx, acc)
				if err != nil {
					h.infoFn(ltx)("Error: %s", err)
				}
			}
			if len(acc.Votes) == 0 {
				var items ItemCollection
				if cursor := ContextCursor(r.Context()); cursor != nil {
					items = cursor.items.Items()
				}
				h.storage.loadAccountVotes(ctx, &acc, items)
				acc, err = h.storage.loadAccountsOutbox(ctx, acc)
				if err != nil {
					h.infoFn(ltx)("Error: %s", err)
				}
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), LoggedAccountCtxtKey, &acc))
		if clearCookie {
			clearSessionCookie(w, r)
		} else if h.v != nil {
			if err := h.v.saveAccountToSession(w, r, acc); err != nil {
				h.errFn(log.Ctx{
					"err":    err,
					"handle": acc.Handle,
					"hash":   acc.Hash,
				})("unable to save account to session")
			}
		}
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (h handler) NeedsSessions(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if !h.conf.SessionsEnabled {
			h.v.HandleErrors(w, r, errors.NotFoundf("sessions are disabled"))
			return
		}
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

// HandleAbout serves /about request
// It's something Mastodon compatible servers should show
func (h *handler) HandleAbout(w http.ResponseWriter, r *http.Request) {
	m := &aboutModel{Title: "About"}

	repo := h.storage
	info, err := repo.LoadInfo()
	if err != nil {
		h.v.HandleErrors(w, r, errors.NewNotValid(err, "oops!"))
		return
	}
	m.Desc.Description = info.Description

	h.v.RenderTemplate(r, w, m.Template(), m)
}

func httpErrorResponse(e error) int {
	if errors.IsBadRequest(e) {
		return http.StatusBadRequest
	}
	if errors.IsForbidden(e) {
		return http.StatusForbidden
	}
	if errors.IsNotSupported(e) {
		return http.StatusHTTPVersionNotSupported
	}
	if errors.IsMethodNotAllowed(e) {
		return http.StatusMethodNotAllowed
	}
	if errors.IsNotFound(e) {
		return http.StatusNotFound
	}
	if errors.IsNotImplemented(e) {
		return http.StatusNotImplemented
	}
	if errors.IsUnauthorized(e) {
		return http.StatusUnauthorized
	}
	if errors.IsTimeout(e) {
		return http.StatusGatewayTimeout
	}
	if errors.IsNotValid(e) {
		return http.StatusInternalServerError
	}
	return http.StatusInternalServerError
}

func loadEnvSessionKeys() [][]byte {
	keys := make([][]byte, 0)
	if authKey := os.Getenv("SESS_AUTH_KEY"); len(authKey) >= 16 {
		keys = append(keys, []byte(authKey[:16]))
	}
	if encKey := os.Getenv("SESS_ENC_KEY"); len(encKey) >= 16 {
		keys = append(keys, []byte(encKey[:16]))
	}
	return keys
}

func (h *handler) ErrorHandler(errs ...error) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		h.v.HandleErrors(w, r, errs...)
	}
	return http.HandlerFunc(fn)
}

func (h handler) CSRF(next http.Handler) http.Handler {
	opts := []csrf.Option{
		csrf.CookieName(csrfName),
		csrf.FieldName(csrfName),
		csrf.Secure(h.conf.Env.IsProd()),
		csrf.ErrorHandler(h.ErrorHandler(errors.Forbiddenf("Invalid request token"))),
	}
	var authKey []byte
	if len(h.conf.SessionKeys) > 0 {
		authKey = h.conf.SessionKeys[0]
	} else {
		if h.conf.Env.IsProd() {
			h.errFn()("Invalid CSRF auth key")
		}
		// TODO(marius): WTF is this?
		authKey = []byte{0x1, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x1}
	}
	return csrf.Protect(authKey, opts...)(next)
}
