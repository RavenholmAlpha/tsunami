// Package xboardtsunami implements the private TSUNAMI node API exposed by
// the companion XBoard extension. It deliberately does not reuse XBoard's
// global server token or legacy protocol endpoints.
package xboardtsunami

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/control"
	"github.com/tsunami-protocol/tsunami/pkg/fronting"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"github.com/tsunami-protocol/tsunami/pkg/server"
	"github.com/tsunami-protocol/tsunami/pkg/transport"
)

const (
	enrollPath  = "/api/v2/server/tsunami/enroll"
	configPath  = "/api/v2/server/tsunami/config"
	usersPath   = "/api/v2/server/tsunami/users"
	reportPath  = "/api/v2/server/tsunami/report"
	admitPath   = "/api/v2/server/tsunami/device/admit"
	renewPath   = "/api/v2/server/tsunami/device/renew"
	releasePath = "/api/v2/server/tsunami/device/release"
)

// Config contains only control-plane bootstrap data. All listener and
// protocol settings are downloaded from the TSUNAMI node's XBoard record.
type Config struct {
	BaseURL         string
	NodeID          string
	EnrollmentToken string
	Credential      string
	CredentialFile  string
	SyncInterval    time.Duration
	ConfigInterval  time.Duration
	RequestTimeout  time.Duration
}

// Normalize validates bootstrap data and applies conservative poll defaults.
func (c *Config) Normalize() error {
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if c.BaseURL == "" {
		return errors.New("xboardtsunami: base_url is required")
	}
	parsed, err := url.Parse(c.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("xboardtsunami: invalid base_url %q", c.BaseURL)
	}
	c.NodeID = strings.TrimSpace(c.NodeID)
	if c.NodeID == "" {
		return errors.New("xboardtsunami: node_id is required")
	}
	if strings.TrimSpace(c.Credential) == "" && strings.TrimSpace(c.CredentialFile) == "" && strings.TrimSpace(c.EnrollmentToken) == "" {
		return errors.New("xboardtsunami: credential, credential_file, or enrollment_token is required")
	}
	if c.SyncInterval <= 0 {
		c.SyncInterval = time.Minute
	}
	if c.ConfigInterval <= 0 {
		c.ConfigInterval = c.SyncInterval
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 15 * time.Second
	}
	return nil
}

// RemoteConfig is the listener configuration served by XBoard.
type RemoteConfig struct {
	Revision        string          `json:"revision"`
	Listen          string          `json:"listen"`
	TLS             remoteTLS       `json:"tls"`
	Transport       remoteTransport `json:"transport"`
	Surge           remoteSurge     `json:"surge"`
	Fallback        string          `json:"fallback"`
	Fronting        remoteFronting  `json:"fronting"`
	PaddingScheme   string          `json:"padding_scheme"`
	AllowAll        bool            `json:"allow_all"`
	DeviceLimitMode string          `json:"device_limit_mode"`
}

type remoteTLS struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

type remoteTransport struct {
	Mode string `json:"mode"`
	Path string `json:"path"`
	Host string `json:"host"`
}

type remoteSurge struct {
	Mode           string `json:"mode"`
	MaxConnections int    `json:"max_connections"`
	Threshold      int    `json:"threshold"`
}

type remoteFronting struct {
	Enabled      bool   `json:"enabled"`
	Path         string `json:"path"`
	Secret       string `json:"secret"`
	ServerHeader string `json:"server_header"`
	SiteName     string `json:"site_name"`
	DecoyProxy   string `json:"decoy_proxy"`
}

// ToServerConfig builds a data-plane server using the dynamic user store and
// this adapter as the session admission authority.
func (c RemoteConfig) ToServerConfig(users server.UserAuthenticator, traffic control.TrafficPolicy, admission control.SessionAdmission) (server.Config, error) {
	if strings.TrimSpace(c.Listen) == "" {
		return server.Config{}, errors.New("xboardtsunami: remote config has no listen address")
	}
	if (strings.TrimSpace(c.TLS.CertFile) == "") != (strings.TrimSpace(c.TLS.KeyFile) == "") {
		return server.Config{}, errors.New("xboardtsunami: remote TLS cert_file and key_file must be supplied together")
	}

	mode := strings.ToLower(strings.TrimSpace(c.Transport.Mode))
	if mode == "" {
		mode = "raw"
	}
	if mode != "h2" && mode != "websocket" && mode != "raw" {
		return server.Config{}, fmt.Errorf("xboardtsunami: unsupported transport mode %q", c.Transport.Mode)
	}
	frontEnabled := c.Fronting.Enabled
	if mode == "raw" && frontEnabled {
		return server.Config{}, errors.New("xboardtsunami: raw transport cannot enable fronting")
	}
	if mode != "raw" && !frontEnabled {
		return server.Config{}, errors.New("xboardtsunami: h2/websocket transport requires fronting.enabled")
	}
	if frontEnabled && strings.TrimSpace(c.Fronting.Secret) == "" {
		return server.Config{}, errors.New("xboardtsunami: fronting.secret is required when fronting is enabled")
	}

	frontPath := strings.TrimSpace(c.Fronting.Path)
	if frontPath == "" {
		frontPath = strings.TrimSpace(c.Transport.Path)
	}
	if frontPath == "" {
		frontPath = fronting.DefaultPath
	}

	return server.Config{
		Listen: c.Listen,
		TLS: transport.TLSConfig{
			CertFile:   c.TLS.CertFile,
			KeyFile:    c.TLS.KeyFile,
			ALPN:       []string{"h2"},
			MinVersion: 0x0304,
		},
		TCP:              *transport.DefaultTCPConfig(),
		Authenticator:    users,
		SurgeMode:        c.Surge.Mode,
		MaxConnections:   c.Surge.MaxConnections,
		SurgeThreshold:   c.Surge.Threshold,
		FallbackAddr:     c.Fallback,
		PaddingScheme:    c.PaddingScheme,
		Traffic:          traffic,
		AllowAll:         c.AllowAll,
		SessionAdmission: admission,
		Fronting: fronting.Config{
			Enabled:      frontEnabled,
			Path:         frontPath,
			Secret:       c.Fronting.Secret,
			ServerHeader: c.Fronting.ServerHeader,
			SiteName:     c.Fronting.SiteName,
			DecoyProxy:   c.Fronting.DecoyProxy,
		},
	}, nil
}

// Adapter owns per-node credentials, ETags, panel reporting and runtime
// session lease bookkeeping.
type Adapter struct {
	config Config
	client *http.Client

	mu         sync.Mutex
	credential string
	usersETag  string
	configETag string
	active     map[string]map[string]string
	reported   map[string]struct{}
}

// New creates a TSUNAMI-specific XBoard adapter.
func New(config Config) (*Adapter, error) {
	if err := config.Normalize(); err != nil {
		return nil, err
	}
	credential := strings.TrimSpace(config.Credential)
	if credential == "" && config.CredentialFile != "" {
		data, err := os.ReadFile(config.CredentialFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("xboardtsunami: read credential file: %w", err)
		}
		credential = strings.TrimSpace(string(data))
	}
	if credential == "" && strings.TrimSpace(config.EnrollmentToken) == "" {
		return nil, errors.New("xboardtsunami: credential file is empty or missing and no enrollment_token was configured")
	}

	return &Adapter{
		config:     config,
		client:     &http.Client{Timeout: config.RequestTimeout},
		credential: credential,
		active:     make(map[string]map[string]string),
		reported:   make(map[string]struct{}),
	}, nil
}

// Config returns the normalized bootstrap configuration.
func (a *Adapter) Config() Config {
	return a.config
}

// EnsureEnrolled performs one-time enrollment only when no durable node
// credential is available locally.
func (a *Adapter) EnsureEnrolled(ctx context.Context) error {
	if a.currentCredential() != "" {
		return nil
	}
	if strings.TrimSpace(a.config.EnrollmentToken) == "" {
		return errors.New("xboardtsunami: node credential is missing and no enrollment_token was configured")
	}

	payload, err := json.Marshal(map[string]string{
		"node_id":          a.config.NodeID,
		"enrollment_token": a.config.EnrollmentToken,
	})
	if err != nil {
		return fmt.Errorf("xboardtsunami: encode enrollment request: %w", err)
	}
	req, err := a.newRequest(ctx, http.MethodPost, enrollPath, bytes.NewReader(payload), false)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	var response struct {
		NodeID     string `json:"node_id"`
		Credential string `json:"credential"`
	}
	if err := a.doJSON(req, &response); err != nil {
		return fmt.Errorf("xboardtsunami: enroll: %w", err)
	}
	if response.NodeID != a.config.NodeID || strings.TrimSpace(response.Credential) == "" {
		return errors.New("xboardtsunami: enrollment response is invalid")
	}
	if err := a.saveCredential(response.Credential); err != nil {
		return err
	}
	return nil
}

// FetchConfig retrieves data-plane configuration. unchanged is true for 304.
func (a *Adapter) FetchConfig(ctx context.Context) (*RemoteConfig, bool, error) {
	if err := a.EnsureEnrolled(ctx); err != nil {
		return nil, false, err
	}
	req, err := a.newRequest(ctx, http.MethodGet, configPath, nil, true)
	if err != nil {
		return nil, false, err
	}
	if etag := a.etag(false); etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("xboardtsunami: fetch config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, true, nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, false, responseError("xboardtsunami: fetch config", resp)
	}

	var response struct {
		Revision string       `json:"revision"`
		Config   RemoteConfig `json:"config"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&response); err != nil {
		return nil, false, fmt.Errorf("xboardtsunami: decode config: %w", err)
	}
	response.Config.Revision = response.Revision
	if response.Config.Revision == "" {
		return nil, false, errors.New("xboardtsunami: config response has no revision")
	}
	a.setETag(false, resp.Header.Get("ETag"))
	return &response.Config, false, nil
}

// FetchSnapshot retrieves the full assigned user list. unchanged is true for
// HTTP 304; a 200 response containing [] intentionally clears the local store.
func (a *Adapter) FetchSnapshot(ctx context.Context) (*control.Snapshot, bool, error) {
	if err := a.EnsureEnrolled(ctx); err != nil {
		return nil, false, err
	}
	req, err := a.newRequest(ctx, http.MethodGet, usersPath, nil, true)
	if err != nil {
		return nil, false, err
	}
	if etag := a.etag(true); etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("xboardtsunami: fetch users: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, true, nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, false, responseError("xboardtsunami: fetch users", resp)
	}

	var response struct {
		Revision string      `json:"revision"`
		Users    []panelUser `json:"users"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&response); err != nil {
		return nil, false, fmt.Errorf("xboardtsunami: decode users: %w", err)
	}
	if response.Revision == "" {
		return nil, false, errors.New("xboardtsunami: users response has no revision")
	}

	users := make([]*protocol.UserInfo, 0, len(response.Users))
	for index, raw := range response.Users {
		user, err := raw.toUserInfo()
		if err != nil {
			return nil, false, fmt.Errorf("xboardtsunami: user[%d]: %w", index, err)
		}
		users = append(users, user)
	}
	a.setETag(true, resp.Header.Get("ETag"))
	return &control.Snapshot{
		Source:    "xboard-tsunami",
		Version:   response.Revision,
		Full:      true,
		FetchedAt: time.Now(),
		Users:     users,
		Metadata: map[string]string{
			"node_id": a.config.NodeID,
		},
	}, false, nil
}

// SubmitReport reports traffic in XBoard's native user_id:[upload,download]
// shape together with the deduplicated online IP state used by the panel UI.
func (a *Adapter) SubmitReport(ctx context.Context, reportID string, deltas []control.UsageDelta) error {
	if err := a.EnsureEnrolled(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(reportID) == "" {
		return errors.New("xboardtsunami: report_id is required")
	}

	traffic := make(map[string][2]int64, len(deltas))
	for _, delta := range deltas {
		if delta.UserID == "" || (delta.UploadBytes == 0 && delta.DownloadBytes == 0) {
			continue
		}
		current := traffic[delta.UserID]
		current[0] += delta.UploadBytes
		current[1] += delta.DownloadBytes
		traffic[delta.UserID] = current
	}
	alive := a.aliveSnapshot()
	payload, err := json.Marshal(map[string]any{
		"report_id": reportID,
		"traffic":   traffic,
		"alive":     alive,
	})
	if err != nil {
		return fmt.Errorf("xboardtsunami: encode report: %w", err)
	}
	req, err := a.newRequest(ctx, http.MethodPost, reportPath, bytes.NewReader(payload), true)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	var response struct {
		Accepted bool `json:"accepted"`
	}
	if err := a.doJSON(req, &response); err != nil {
		return fmt.Errorf("xboardtsunami: submit report: %w", err)
	}
	if !response.Accepted {
		return errors.New("xboardtsunami: report was not accepted")
	}
	a.markAliveReported()
	return nil
}

// Admit implements control.SessionAdmission. Unlimited users still return a
// lease so their online IP state reaches XBoard, but do not use device slots.
func (a *Adapter) Admit(ctx context.Context, user *protocol.UserInfo, clientIP, sessionID string) (control.SessionLease, error) {
	if user == nil || user.ID == "" {
		return nil, errors.New("xboardtsunami: session has no panel user id")
	}
	if strings.TrimSpace(clientIP) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("xboardtsunami: session IP and id are required")
	}
	if err := a.EnsureEnrolled(ctx); err != nil {
		return nil, err
	}

	limited := user.MaxDevices > 0
	if limited {
		var response leaseResponse
		if err := a.postLease(ctx, admitPath, map[string]string{
			"user_id":    user.ID,
			"session_id": sessionID,
			"ip":         clientIP,
		}, &response); err != nil {
			return nil, err
		}
		if !response.Allowed {
			return nil, control.ErrAdmissionDenied
		}
	}
	a.addActive(user.ID, sessionID, clientIP)
	return &sessionLease{
		adapter:   a,
		userID:    user.ID,
		sessionID: sessionID,
		limited:   limited,
	}, nil
}

type sessionLease struct {
	adapter   *Adapter
	userID    string
	sessionID string
	limited   bool
	once      sync.Once
}

func (l *sessionLease) Renew(ctx context.Context) error {
	if l == nil || l.adapter == nil || !l.limited {
		return nil
	}
	var response leaseResponse
	if err := l.adapter.postLease(ctx, renewPath, map[string]string{
		"user_id":    l.userID,
		"session_id": l.sessionID,
	}, &response); err != nil {
		return err
	}
	if !response.Allowed {
		return control.ErrAdmissionDenied
	}
	return nil
}

func (l *sessionLease) Release(ctx context.Context) error {
	if l == nil || l.adapter == nil {
		return nil
	}
	var releaseErr error
	l.once.Do(func() {
		defer l.adapter.removeActive(l.userID, l.sessionID)
		if !l.limited {
			return
		}
		var response struct {
			Released bool `json:"released"`
		}
		releaseErr = l.adapter.postLease(ctx, releasePath, map[string]string{
			"user_id":    l.userID,
			"session_id": l.sessionID,
		}, &response)
	})
	return releaseErr
}

type leaseResponse struct {
	Allowed bool `json:"allowed"`
}

func (a *Adapter) postLease(ctx context.Context, endpoint string, payload any, output any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("xboardtsunami: encode device lease request: %w", err)
	}
	req, err := a.newRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(data), true)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := a.doJSON(req, output); err != nil {
		return fmt.Errorf("xboardtsunami: device lease request: %w", err)
	}
	return nil
}

func (a *Adapter) addActive(userID, sessionID, ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active[userID] == nil {
		a.active[userID] = make(map[string]string)
	}
	a.active[userID][sessionID] = ip
}

func (a *Adapter) removeActive(userID, sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if sessions := a.active[userID]; sessions != nil {
		delete(sessions, sessionID)
		if len(sessions) == 0 {
			delete(a.active, userID)
		}
	}
}

func (a *Adapter) aliveSnapshot() map[string][]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make(map[string][]string, len(a.active)+len(a.reported))
	for userID := range a.reported {
		result[userID] = []string{}
	}
	for userID, sessions := range a.active {
		ips := make(map[string]struct{}, len(sessions))
		for _, ip := range sessions {
			ips[ip] = struct{}{}
		}
		result[userID] = make([]string, 0, len(ips))
		for ip := range ips {
			result[userID] = append(result[userID], ip)
		}
	}
	return result
}

func (a *Adapter) markAliveReported() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reported = make(map[string]struct{}, len(a.active))
	for userID := range a.active {
		a.reported[userID] = struct{}{}
	}
}

func (a *Adapter) currentCredential() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.credential
}

func (a *Adapter) saveCredential(credential string) error {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return errors.New("xboardtsunami: empty node credential")
	}
	if a.config.CredentialFile != "" {
		dir := path.Dir(a.config.CredentialFile)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("xboardtsunami: create credential directory: %w", err)
		}
		temporary := a.config.CredentialFile + ".tmp"
		if err := os.WriteFile(temporary, []byte(credential+"\n"), 0600); err != nil {
			return fmt.Errorf("xboardtsunami: write credential: %w", err)
		}
		if err := os.Rename(temporary, a.config.CredentialFile); err != nil {
			return fmt.Errorf("xboardtsunami: persist credential: %w", err)
		}
	}
	a.mu.Lock()
	a.credential = credential
	a.mu.Unlock()
	return nil
}

func (a *Adapter) etag(users bool) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if users {
		return a.usersETag
	}
	return a.configETag
}

func (a *Adapter) setETag(users bool, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if users {
		a.usersETag = value
	} else {
		a.configETag = value
	}
}

func (a *Adapter) newRequest(ctx context.Context, method, endpoint string, body io.Reader, authenticated bool) (*http.Request, error) {
	u, err := url.Parse(a.config.BaseURL)
	if err != nil {
		return nil, err
	}
	u.Path = path.Join(u.Path, endpoint)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("xboardtsunami: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if authenticated {
		credential := a.currentCredential()
		if credential == "" {
			return nil, errors.New("xboardtsunami: missing node credential")
		}
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	return req, nil
}

func (a *Adapter) doJSON(req *http.Request, output any) error {
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return responseError("request rejected", resp)
	}
	if output == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(output); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func responseError(prefix string, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	return fmt.Errorf("%s: status=%s body=%q", prefix, response.Status, strings.TrimSpace(string(body)))
}

type panelUser struct {
	ID          jsonString `json:"id"`
	UUID        string     `json:"uuid"`
	SpeedLimit  int64      `json:"speed_limit"`
	DeviceLimit int        `json:"device_limit"`
}

func (u panelUser) toUserInfo() (*protocol.UserInfo, error) {
	id := u.ID.String()
	if id == "" {
		return nil, errors.New("missing id")
	}
	if strings.TrimSpace(u.UUID) == "" {
		return nil, errors.New("missing uuid")
	}
	hash := sha256.Sum256([]byte(u.UUID))
	return &protocol.UserInfo{
		ID:            id,
		Name:          id,
		TokenHash:     hex.EncodeToString(hash[:]),
		SpeedLimitBps: u.SpeedLimit * 1000 * 1000 / 8,
		MaxDevices:    u.DeviceLimit,
		Metadata: map[string]string{
			"panel": "xboard-tsunami",
		},
	}, nil
}

type jsonString string

func (s *jsonString) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*s = jsonString(text)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	*s = jsonString(number.String())
	return nil
}

func (s jsonString) String() string {
	return string(s)
}

// NewReportID returns a retry-safe id that is retained by the node until the
// matching traffic report has been acknowledged.
func NewReportID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
