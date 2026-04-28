package verisure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
)

const (
	applicationID    = "PS_PYTHON"
	defaultLoginBase = "https://automation01.verisure.com"
	fallbackLoginBase = "https://automation02.verisure.com"
)

// GraphQL queries used by the Verisure version-2 API.
const (
	queryFetchInstallations = `query fetchAllInstallations($email: String!) {
  account(email: $email) {
    installations {
      giid
      alias
      __typename
    }
    __typename
  }
}`

	queryArmState = `query ArmState($giid: String!) {
  installation(giid: $giid) {
    armState {
      statusType
      date
      name
      changedVia
      __typename
    }
    __typename
  }
}`
)

// mfaState tracks a pending MFA flow so we never send a second SMS
// while still waiting for the user to submit the first code.
type mfaState struct {
	loginBase    string
	stepupCookie string
}

// Client is a Verisure API client using the GraphQL endpoint on automation01.verisure.com.
type Client struct {
	// loginBase is the automation server we authenticated against.
	// Starts as defaultLoginBase; updated if the fallback succeeds.
	loginBase string
	email     string
	password  string
	phone     string

	http *http.Client
	jar  *cookiejar.Jar
	mu   sync.Mutex

	authed bool
	giid   string

	// pendingMFA is non-nil when an SMS has been sent and we're waiting
	// for the user to submit the code. While set, loginLocked goes straight
	// to the validate step without sending a new SMS.
	pendingMFA *mfaState

	// mfaCh receives the SMS code from the HTTP handler.
	mfaCh chan string
}

func NewClient(email, password, phone, persistedCookie string) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	c := &Client{
		loginBase: defaultLoginBase,
		email:     email,
		password:  password,
		phone:     phone,
		jar:       jar,
		mfaCh:     make(chan string, 1),
	}
	c.http = &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if persistedCookie != "" {
		// persistedCookie is stored as "|"-separated "name=value" pairs by SessionCookie().
		// (legacy: single "name=value" pair)
		restored := 0
		for _, pair := range strings.Split(persistedCookie, "|") {
			if eq := strings.Index(pair, "="); eq > 0 {
				c.setCookieOnAll(pair[:eq], pair[eq+1:])
				restored++
			}
		}
		if restored > 0 {
			c.authed = true
			slog.Debug("verisure: restored session from store", "cookies", restored)
		} else {
			slog.Warn("verisure: ignoring malformed persisted cookie")
		}
	}
	return c, nil
}

func (c *Client) SubmitMFACode(code string) {
	select {
	case c.mfaCh <- code:
	default:
	}
}

func (c *Client) SessionCookie() string {
	// Collect session cookies to persist across restarts.
	// - vs-access: short-lived JWT (10 min) — worth saving so startup
	//   works immediately if restarted within the window.
	// - vs-trust-<hash>: long-lived trust token issued by /auth/trust.
	//   The name has an account-specific hash suffix; we match by prefix.
	//   With this cookie in the jar, POST /auth/login returns a fresh
	//   vs-access without triggering MFA again.
	// - vs-refresh: refresh token at Path=/auth — save for completeness.
	// - vid: legacy session cookie, kept as fallback.
	// "no_vid_cookie" is a Verisure placeholder — skip it.
	var access, vid string
	seen := make(map[string]bool)
	var extraParts []string // vs-trust-* and vs-refresh as raw "name=value"

	for _, base := range []string{c.loginBase, defaultLoginBase, fallbackLoginBase} {
		u, _ := url.Parse(base)
		for _, cookie := range c.jar.Cookies(u) {
			if seen[cookie.Name] || cookie.Value == "" {
				continue
			}
			seen[cookie.Name] = true
			switch {
			case cookie.Name == "vs-access":
				access = cookie.Value
			case strings.HasPrefix(cookie.Name, "vs-trust") || cookie.Name == "vs-refresh":
				extraParts = append(extraParts, cookie.Name+"="+cookie.Value)
			case cookie.Name == "vid" && cookie.Value != "no_vid_cookie":
				vid = cookie.Value
			}
		}
	}

	// Build "|"-separated "name=value" pairs for NewClient to restore.
	var parts []string
	if access != "" {
		parts = append(parts, "vs-access="+access)
	}
	parts = append(parts, extraParts...)
	if len(parts) == 0 && vid != "" {
		parts = append(parts, "vid="+vid)
	}
	return strings.Join(parts, "|")
}

func (c *Client) ArmState(ctx context.Context) (ArmState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Login (including MFA) if not yet authenticated.
	if !c.authed {
		if err := c.loginLocked(ctx); err != nil {
			return ArmStateUnknown, fmt.Errorf("verisure login: %w", err)
		}
	}

	// GIID discovery is separate from auth: a transient 503/network error must
	// not clear the MFA session and force another SMS. But a 401/403 means the
	// restored session cookie is invalid — clear authed and re-login immediately.
	if c.giid == "" {
		if err := c.discoverGIIDLocked(ctx); err != nil {
			if isSessionExpired(err) {
				c.authed = false
				c.giid = ""
				if loginErr := c.loginLocked(ctx); loginErr != nil {
					return ArmStateUnknown, fmt.Errorf("verisure re-login (expired cookie): %w", loginErr)
				}
				if discErr := c.discoverGIIDLocked(ctx); discErr != nil {
					return ArmStateUnknown, fmt.Errorf("verisure installation discovery: %w", discErr)
				}
			} else {
				return ArmStateUnknown, fmt.Errorf("verisure installation discovery: %w", err)
			}
		}
	}

	state, err := c.fetchArmStateLocked(ctx)
	if err != nil {
		// Only redo the full login on a genuine session-expiry error.
		if isSessionExpired(err) {
			c.authed = false
			c.giid = ""
			if loginErr := c.loginLocked(ctx); loginErr != nil {
				return ArmStateUnknown, fmt.Errorf("verisure re-login: %w", loginErr)
			}
			if c.giid == "" {
				if discErr := c.discoverGIIDLocked(ctx); discErr != nil {
					return ArmStateUnknown, fmt.Errorf("verisure re-discovery: %w", discErr)
				}
			}
			state, err = c.fetchArmStateLocked(ctx)
		}
	}
	return state, err
}

// isSessionExpired returns true for HTTP 401/403 errors indicating the session
// cookie has expired — as opposed to transient errors which should be retried
// without a full re-login.
func isSessionExpired(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "HTTP 401") || strings.Contains(s, "HTTP 403") ||
		strings.Contains(s, "session expired")
}

func (c *Client) fetchArmStateLocked(ctx context.Context) (ArmState, error) {
	var resp graphQLArmStateResponse
	if err := c.graphql(ctx, []gqlBody{{
		OperationName: "ArmState",
		Variables:     map[string]any{"giid": c.giid},
		Query:         queryArmState,
	}}, &resp); err != nil {
		return ArmStateUnknown, err
	}
	return ArmState(resp.Data.Installation.ArmState.StatusType), nil
}

// loginLocked authenticates. If an MFA flow is already in progress
// (pendingMFA != nil), it goes straight to validate without sending a new SMS.
func (c *Client) loginLocked(ctx context.Context) error {
	slog.Info("verisure: logging in", "email", c.email)

	// If we're already mid-MFA (SMS was sent, waiting for code) skip
	// the /auth/login POST and go straight to validate.
	if c.pendingMFA != nil {
		slog.Info("verisure: resuming pending MFA flow, waiting for SMS code")
		if err := c.validateMFALocked(ctx, c.pendingMFA); err != nil {
			c.pendingMFA = nil
			return fmt.Errorf("MFA validate: %w", err)
		}
		c.loginBase = c.pendingMFA.loginBase
		c.pendingMFA = nil
		return c.finishLoginLocked(ctx)
	}

	// Normal flow: POST /auth/login, try each base until one accepts.
	var pending *mfaState
	var lastErr error

	for _, base := range []string{defaultLoginBase, fallbackLoginBase} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/auth/login", http.NoBody)
		if err != nil {
			lastErr = err
			continue
		}
		req.SetBasicAuth(c.email, c.password)
		req.Header.Set("APPLICATION_ID", applicationID)
		req.Header.Set("Accept", "application/json")
		req.ContentLength = 0

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("POST %s/auth/login: %w", base, err)
			slog.Debug("verisure: login base unreachable", "base", base, "err", lastErr)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("POST /auth/login: HTTP %d: %s", resp.StatusCode, body)
			slog.Debug("verisure: login base rejected", "base", base, "status", resp.StatusCode)
			continue
		}

		c.propagateCookies(base, resp.Cookies())

		if strings.Contains(string(body), "stepUpToken") {
			// MFA required — extract the stepup cookie.
			var stepup string
			for _, cookie := range resp.Cookies() {
				if cookie.Name == "vs-stepup" {
					stepup = cookie.Value
				}
			}
			if stepup == "" {
				lastErr = fmt.Errorf("MFA required but no vs-stepup cookie from %s", base)
				continue
			}
			pending = &mfaState{loginBase: base, stepupCookie: stepup}
		} else {
			// No MFA: session cookies already propagated above.
			slog.Info("verisure: login succeeded (no MFA)")
			c.loginBase = base
			c.requestTrustLocked(ctx, base, "")
		}
		lastErr = nil
		break
	}

	if lastErr != nil {
		return lastErr
	}

	// MFA path: send SMS once, store state, wait for code.
	if pending != nil {
		if err := c.sendSMSLocked(ctx, pending); err != nil {
			return fmt.Errorf("trigger SMS: %w", err)
		}
		// Store pending state before blocking — if the context times out,
		// the next call to loginLocked will resume the validate step.
		c.pendingMFA = pending

		if err := c.validateMFALocked(ctx, pending); err != nil {
			if strings.Contains(err.Error(), "HTTP 4") {
				c.pendingMFA = nil
			}
			return fmt.Errorf("MFA validate: %w", err)
		}
		c.loginBase = pending.loginBase
		c.pendingMFA = nil
		// Request trust token so future re-logins skip MFA.
		c.requestTrustLocked(ctx, pending.loginBase, pending.stepupCookie)
	}

	return c.finishLoginLocked(ctx)
}

// sendSMSLocked POSTs to /auth/mfa to trigger the SMS code.
func (c *Client) sendSMSLocked(ctx context.Context, state *mfaState) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		state.loginBase+"/auth/mfa", http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("APPLICATION_ID", applicationID)
	req.Header.Set("Accept", "application/json")
	req.AddCookie(&http.Cookie{Name: "vs-stepup", Value: state.stepupCookie})
	req.ContentLength = 0

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("POST /auth/mfa: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST /auth/mfa: HTTP %d", resp.StatusCode)
	}

	slog.Warn("verisure: SMS code sent — POST the code to /mfa-code to continue",
		"phone", c.phone)
	return nil
}

// validateMFALocked waits for a code on mfaCh then POSTs to /auth/mfa/validate.
func (c *Client) validateMFALocked(ctx context.Context, state *mfaState) error {
	// Drain any stale code from a previous failed attempt.
	select {
	case <-c.mfaCh:
	default:
	}

	// Block until the operator submits a code.
	var code string
	select {
	case code = <-c.mfaCh:
	case <-ctx.Done():
		return ctx.Err()
	}

	payload, _ := json.Marshal(mfaValidateRequest{Token: code, Trust: false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		state.loginBase+"/auth/mfa/validate", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("APPLICATION_ID", applicationID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.AddCookie(&http.Cookie{Name: "vs-stepup", Value: state.stepupCookie})

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("POST /auth/mfa/validate: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST /auth/mfa/validate: HTTP %d: %s", resp.StatusCode, body)
	}

	cookies := resp.Cookies()
	slog.Info("verisure: mfa/validate response", "cookies_count", len(cookies))
	for _, ck := range cookies {
		slog.Info("verisure: mfa/validate set-cookie", "name", ck.Name,
			"value_len", len(ck.Value), "path", ck.Path, "domain", ck.Domain)
	}
	c.propagateCookies(state.loginBase, cookies)
	slog.Info("verisure: MFA code accepted")
	return nil
}

// requestTrustLocked calls POST /auth/trust to obtain a long-lived trust token.
// With vs-trust in the cookie jar, future POST /auth/login calls return a fresh
// vs-access token without triggering MFA again.
// stepupCookie is the vs-stepup value from the MFA flow; pass "" on non-MFA paths.
// Errors are non-fatal — the current session still works for its 10-minute window.
func (c *Client) requestTrustLocked(ctx context.Context, loginBase, stepupCookie string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		loginBase+"/auth/trust", http.NoBody)
	if err != nil {
		slog.Warn("verisure: /auth/trust request build failed", "err", err)
		return
	}
	req.Header.Set("APPLICATION_ID", applicationID)
	req.Header.Set("Accept", "application/json")
	req.ContentLength = 0
	// Some Verisure backends require the original MFA stepup cookie for /auth/trust.
	if stepupCookie != "" {
		req.AddCookie(&http.Cookie{Name: "vs-stepup", Value: stepupCookie})
	}

	resp, err := c.http.Do(req)
	if err != nil {
		slog.Warn("verisure: /auth/trust failed", "err", err)
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("verisure: /auth/trust non-200, re-login may require MFA again",
			"status", resp.StatusCode, "body", string(body))
		return
	}
	cookies := resp.Cookies()
	slog.Info("verisure: /auth/trust response", "cookies_count", len(cookies), "body", string(body))
	for _, ck := range cookies {
		slog.Info("verisure: /auth/trust set-cookie", "name", ck.Name,
			"value_len", len(ck.Value), "path", ck.Path, "domain", ck.Domain)
	}
	c.propagateCookies(loginBase, cookies)
	slog.Info("verisure: trust token obtained — future re-logins will not require MFA")
}

// finishLoginLocked marks the session as authenticated.
// GIID discovery is intentionally deferred to ArmState so that a transient
// error on the installation endpoint does not force a full re-login (new SMS).
func (c *Client) finishLoginLocked(ctx context.Context) error {
	c.authed = true
	slog.Info("verisure: session authenticated (GIID discovery deferred)")
	return nil
}

func (c *Client) discoverGIIDLocked(ctx context.Context) error {
	if c.giid != "" {
		return nil
	}
	var resp graphQLInstallationsResponse
	if err := c.graphql(ctx, []gqlBody{{
		OperationName: "fetchAllInstallations",
		Variables:     map[string]any{"email": c.email},
		Query:         queryFetchInstallations,
	}}, &resp); err != nil {
		return err
	}
	if len(resp.Data.Account.Installations) == 0 {
		return fmt.Errorf("no installations found")
	}
	c.giid = resp.Data.Account.Installations[0].GIID
	slog.Info("verisure: discovered installation",
		"giid", c.giid,
		"alias", resp.Data.Account.Installations[0].Alias)
	return nil
}

func (c *Client) SetGIID(giid string) {
	c.mu.Lock()
	c.giid = giid
	c.mu.Unlock()
}

// gqlBody is a single GraphQL operation sent as part of a JSON array.
type gqlBody struct {
	OperationName string         `json:"operationName"`
	Variables     map[string]any `json:"variables"`
	Query         string         `json:"query"`
}

// otherBase returns the automation server that is NOT c.loginBase.
func (c *Client) otherBase() string {
	if c.loginBase == defaultLoginBase {
		return fallbackLoginBase
	}
	return defaultLoginBase
}

// graphql POSTs a GraphQL request array to the automation server.
// On SYS_00004 (backend unavailable) it retries on the other automation server,
// mirroring the failover behaviour of the python-verisure library.
func (c *Client) graphql(ctx context.Context, body []gqlBody, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	for _, base := range []string{c.loginBase, c.otherBase()} {
		respBody, err := c.graphqlOnce(ctx, base, bytes.NewReader(data))
		if err != nil {
			return err // network / HTTP error, no point retrying the other base
		}

		// GraphQL errors are returned as 200 with an "errors" key.
		if bytes.Contains(respBody, []byte(`"errors"`)) {
			// SYS_00004 means this backend shard doesn't have the account;
			// try the other automation server.
			if bytes.Contains(respBody, []byte("SYS_00004")) {
				slog.Debug("verisure: graphql SYS_00004, trying other base", "failed_base", base)
				continue
			}
			return fmt.Errorf("graphql error: %s", respBody)
		}

		// Success — remember the working base for future calls.
		if base != c.loginBase {
			slog.Info("verisure: switching to fallback graphql base", "base", base)
			c.loginBase = base
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(respBody, out)
	}

	return fmt.Errorf("graphql: SYS_00004 on all automation servers")
}

func (c *Client) graphqlOnce(ctx context.Context, base string, body *bytes.Reader) ([]byte, error) {
	body.Seek(0, 0) // rewind for retry
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/graphql", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("APPLICATION_ID", applicationID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("HTTP %d (session expired)", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, respBody)
	}
	return respBody, nil
}

func (c *Client) setCookie(base, name, value string) {
	u, _ := url.Parse(base)
	c.jar.SetCookies(u, []*http.Cookie{{Name: name, Value: value}})
}

// setCookieOnAll sets a cookie on both automation servers and www.verisure.com.
func (c *Client) setCookieOnAll(name, value string) {
	for _, base := range []string{defaultLoginBase, fallbackLoginBase, "https://www.verisure.com"} {
		c.setCookie(base, name, value)
	}
}

func (c *Client) propagateCookies(loginBase string, cookies []*http.Cookie) {
	// Normalise cookies: strip Domain so they're accepted by all automation
	// servers, and set Path="/" so they're sent to all endpoints including
	// /graphql (Verisure often returns cookies with Path=/auth which the
	// cookiejar won't send to other paths).
	normalized := make([]*http.Cookie, len(cookies))
	for i, ck := range cookies {
		cp := *ck
		cp.Domain = ""
		cp.Path = "/"
		normalized[i] = &cp
	}
	for _, base := range []string{defaultLoginBase, fallbackLoginBase, "https://www.verisure.com"} {
		u, _ := url.Parse(base)
		c.jar.SetCookies(u, normalized)
	}
}
