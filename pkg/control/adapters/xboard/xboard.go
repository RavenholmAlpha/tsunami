// Package xboard provides a configurable XBoard-compatible control-plane adapter.
package xboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/control"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

const (
	defaultUsersPath   = "/api/v1/server/tsunami/users"
	defaultTrafficPath = "/api/v1/server/tsunami/traffic"
)

// Config defines the panel API contract. Paths and authentication headers are
// configurable so a private XBoard fork does not require a protocol change.
type Config struct {
	BaseURL             string        `json:"base_url" yaml:"base-url"`
	APIKey              string        `json:"api_key" yaml:"api-key"`
	NodeID              string        `json:"node_id" yaml:"node-id"`
	UsersPath           string        `json:"users_path" yaml:"users-path"`
	TrafficPath         string        `json:"traffic_path" yaml:"traffic-path"`
	AuthorizationHeader string        `json:"authorization_header" yaml:"authorization-header"`
	AuthorizationPrefix string        `json:"authorization_prefix" yaml:"authorization-prefix"`
	SyncInterval        time.Duration `json:"sync_interval" yaml:"sync-interval"`
	RequestTimeout      time.Duration `json:"request_timeout" yaml:"request-timeout"`
}

// Normalize fills conservative defaults and validates the required fields.
func (c *Config) Normalize() error {
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if c.BaseURL == "" {
		return fmt.Errorf("xboard: base_url is required")
	}
	parsed, err := url.Parse(c.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("xboard: invalid base_url %q", c.BaseURL)
	}
	if c.APIKey == "" {
		return fmt.Errorf("xboard: api_key is required")
	}
	if c.NodeID == "" {
		return fmt.Errorf("xboard: node_id is required")
	}
	if c.UsersPath == "" {
		c.UsersPath = defaultUsersPath
	}
	if c.TrafficPath == "" {
		c.TrafficPath = defaultTrafficPath
	}
	useDefaultAuthorizationHeader := c.AuthorizationHeader == ""
	if useDefaultAuthorizationHeader {
		c.AuthorizationHeader = "Authorization"
	}
	if useDefaultAuthorizationHeader && c.AuthorizationPrefix == "" {
		c.AuthorizationPrefix = "Bearer "
	}
	if c.SyncInterval <= 0 {
		c.SyncInterval = time.Minute
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 15 * time.Second
	}
	return nil
}

// Adapter fetches users and submits traffic for one TSUNAMI node.
type Adapter struct {
	config Config
	client *http.Client
}

// New creates an XBoard adapter. The configured paths are intentionally part
// of runtime configuration because private panels commonly expose custom APIs.
func New(config Config) (*Adapter, error) {
	if err := config.Normalize(); err != nil {
		return nil, err
	}
	return &Adapter{
		config: config,
		client: &http.Client{Timeout: config.RequestTimeout},
	}, nil
}

// Name identifies this control-plane adapter.
func (a *Adapter) Name() string {
	return "xboard"
}

// Config returns a normalized copy of the adapter configuration.
func (a *Adapter) Config() Config {
	return a.config
}

// FetchSnapshot retrieves and normalizes the current assigned users.
func (a *Adapter) FetchSnapshot(ctx context.Context) (*control.Snapshot, error) {
	var response usersResponse
	if err := a.getJSON(ctx, a.config.UsersPath, &response); err != nil {
		return nil, err
	}
	if !response.Success() {
		return nil, fmt.Errorf("xboard: users request rejected: %s", response.Message())
	}

	users := make([]*protocol.UserInfo, 0, len(response.Users()))
	for index, raw := range response.Users() {
		user, err := raw.toUserInfo()
		if err != nil {
			return nil, fmt.Errorf("xboard: user[%d]: %w", index, err)
		}
		users = append(users, user)
	}

	return &control.Snapshot{
		Source:    a.Name(),
		Version:   response.Version(),
		Full:      true,
		FetchedAt: time.Now(),
		Users:     users,
		Metadata: map[string]string{
			"node_id": a.config.NodeID,
		},
	}, nil
}

// SubmitUsage reports a batch of accepted traffic deltas. A successful 2xx
// response is the acknowledgement used by the caller before clearing deltas.
func (a *Adapter) SubmitUsage(ctx context.Context, deltas []control.UsageDelta) error {
	if len(deltas) == 0 {
		return nil
	}
	payload := usageRequest{
		NodeID:  a.config.NodeID,
		Traffic: make([]usageEntry, 0, len(deltas)),
	}
	for _, delta := range deltas {
		if delta.UserID == "" {
			continue
		}
		payload.Traffic = append(payload.Traffic, usageEntry{
			UserID:   delta.UserID,
			Upload:   delta.UploadBytes,
			Download: delta.DownloadBytes,
		})
	}
	if len(payload.Traffic) == 0 {
		return nil
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("xboard: encode usage: %w", err)
	}
	req, err := a.newRequest(ctx, http.MethodPost, a.config.TrafficPath, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("xboard: submit usage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return responseError("xboard: submit usage", resp)
	}

	var result apiResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil && err != io.EOF {
		return fmt.Errorf("xboard: decode usage response: %w", err)
	}
	if !result.Success() {
		return fmt.Errorf("xboard: usage request rejected: %s", result.Message())
	}
	return nil
}

func (a *Adapter) getJSON(ctx context.Context, endpoint string, output any) error {
	req, err := a.newRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("xboard: request %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return responseError("xboard: fetch users", resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(output); err != nil {
		return fmt.Errorf("xboard: decode users response: %w", err)
	}
	return nil
}

func (a *Adapter) newRequest(ctx context.Context, method, endpoint string, body io.Reader) (*http.Request, error) {
	u, err := url.Parse(a.config.BaseURL)
	if err != nil {
		return nil, err
	}
	u.Path = path.Join(u.Path, endpoint)
	if strings.HasSuffix(endpoint, "/") {
		u.Path += "/"
	}
	query := u.Query()
	query.Set("node_id", a.config.NodeID)
	u.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("xboard: build request: %w", err)
	}
	req.Header.Set(a.config.AuthorizationHeader, a.config.AuthorizationPrefix+a.config.APIKey)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func responseError(prefix string, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	return fmt.Errorf("%s: status=%s body=%q", prefix, response.Status, strings.TrimSpace(string(body)))
}

type apiResponse struct {
	Status       string `json:"status"`
	SuccessValue *bool  `json:"success"`
	MessageValue string `json:"message"`
}

func (r apiResponse) Success() bool {
	if r.SuccessValue != nil {
		return *r.SuccessValue
	}
	return r.Status == "" || strings.EqualFold(r.Status, "success") || strings.EqualFold(r.Status, "ok")
}

func (r apiResponse) Message() string {
	if r.MessageValue != "" {
		return r.MessageValue
	}
	if r.Status != "" {
		return r.Status
	}
	return "unspecified panel error"
}

type usersResponse struct {
	apiResponse
	Data struct {
		Version string      `json:"version"`
		Users   []panelUser `json:"users"`
	} `json:"data"`
	UsersValue   []panelUser `json:"users"`
	VersionValue string      `json:"version"`
}

func (r usersResponse) Users() []panelUser {
	if r.Data.Users != nil {
		return r.Data.Users
	}
	return r.UsersValue
}

func (r usersResponse) Version() string {
	if r.Data.Version != "" {
		return r.Data.Version
	}
	return r.VersionValue
}

type panelUser struct {
	ID             jsonString `json:"id"`
	UUID           string     `json:"uuid"`
	Email          string     `json:"email"`
	Name           string     `json:"name"`
	Password       string     `json:"password"`
	Token          string     `json:"token"`
	TokenHash      string     `json:"token_hash"`
	Enabled        *bool      `json:"enabled"`
	Disabled       bool       `json:"disabled"`
	ExpiredAt      jsonTime   `json:"expired_at"`
	ExpiresAt      jsonTime   `json:"expires_at"`
	SpeedLimit     int64      `json:"speed_limit"`
	SpeedLimitBps  int64      `json:"speed_limit_bps"`
	Bandwidth      int        `json:"bandwidth"`
	QuotaBytes     int64      `json:"quota_bytes"`
	TransferEnable int64      `json:"transfer_enable"`
	Upload         int64      `json:"u"`
	Download       int64      `json:"d"`
	UsedUpload     int64      `json:"used_upload_bytes"`
	UsedDownload   int64      `json:"used_download_bytes"`
	MaxSessions    int        `json:"max_sessions"`
	MaxDevices     int        `json:"max_devices"`
}

func (u panelUser) toUserInfo() (*protocol.UserInfo, error) {
	id := u.ID.String()
	if id == "" {
		id = u.UUID
	}
	if id == "" {
		return nil, fmt.Errorf("missing id")
	}

	name := firstNonEmpty(u.Name, u.Email, id)
	tokenHash := strings.TrimSpace(u.TokenHash)
	password := strings.TrimSpace(u.Token)
	if password == "" {
		password = strings.TrimSpace(u.Password)
	}
	if tokenHash == "" && password == "" {
		return nil, fmt.Errorf("missing token_hash, token, or password")
	}

	expiresAt := u.ExpiresAt.Time()
	if expiresAt.IsZero() {
		expiresAt = u.ExpiredAt.Time()
	}
	quota := u.QuotaBytes
	if quota == 0 {
		quota = u.TransferEnable
	}
	speed := u.SpeedLimitBps
	if speed == 0 {
		speed = u.SpeedLimit
	}
	disabled := u.Disabled || (u.Enabled != nil && !*u.Enabled)

	return &protocol.UserInfo{
		ID:                id,
		Name:              name,
		Password:          password,
		TokenHash:         tokenHash,
		Disabled:          disabled,
		ExpiresAt:         expiresAt,
		Bandwidth:         u.Bandwidth,
		SpeedLimitBps:     speed,
		QuotaBytes:        quota,
		UsedUploadBytes:   firstNonZero(u.UsedUpload, u.Upload),
		UsedDownloadBytes: firstNonZero(u.UsedDownload, u.Download),
		MaxSessions:       u.MaxSessions,
		MaxDevices:        u.MaxDevices,
		Metadata: map[string]string{
			"panel": "xboard",
		},
	}, nil
}

type usageRequest struct {
	NodeID  string       `json:"node_id"`
	Traffic []usageEntry `json:"traffic"`
}

type usageEntry struct {
	UserID   string `json:"user_id"`
	Upload   int64  `json:"u"`
	Download int64  `json:"d"`
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

func (s jsonString) String() string { return string(s) }

type jsonTime struct{ value time.Time }

func (t *jsonTime) UnmarshalJSON(data []byte) error {
	if string(data) == "null" || string(data) == "\"\"" || string(data) == "0" || string(data) == "\"0\"" {
		t.value = time.Time{}
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err == nil {
		seconds, err := strconv.ParseInt(number.String(), 10, 64)
		if err == nil {
			t.value = time.Unix(seconds, 0)
			return nil
		}
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	if seconds, err := strconv.ParseInt(text, 10, 64); err == nil {
		t.value = time.Unix(seconds, 0)
		return nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05"} {
		parsed, err := time.Parse(layout, text)
		if err == nil {
			t.value = parsed
			return nil
		}
	}
	return fmt.Errorf("invalid timestamp %q", text)
}

func (t jsonTime) Time() time.Time { return t.value }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonZero(first, second int64) int64 {
	if first != 0 {
		return first
	}
	return second
}
