package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// RateLimit is the throttling state parsed from the response headers.
type RateLimit struct {
	Limit     int
	Remaining int
	Reset     time.Time
}

// APIError is an OData error returned by the Admin API (e.g. a Forbidden cmdlet).
type APIError struct {
	Status   int
	Code     string `json:"code"`
	Message  string `json:"message"`
	InnerRaw json.RawMessage
}

func (e *APIError) Error() string {
	return fmt.Sprintf("adminapi: %s (%s, http %d)", e.Message, e.Code, e.Status)
}

// odataResponse is the InvokeCommand response envelope.
type odataResponse struct {
	Context  string           `json:"@odata.context"`
	NextLink string           `json:"@odata.nextLink"`
	Warnings []string         `json:"@adminapi.warnings"`
	Value    []map[string]any `json:"value"`
	Error    *struct {
		Code       string          `json:"code"`
		Message    string          `json:"message"`
		InnerError json.RawMessage `json:"innererror"`
	} `json:"error"`
}

// Result carries the objects plus per-call metadata.
type Result struct {
	Value     []map[string]any
	Warnings  []string
	RateLimit RateLimit
}

// Decode unmarshals the result Value into v (e.g. *[]models.RoleGroup). It round-
// trips through JSON, so v's fields should carry json tags matching the OData
// property names (the generated models package does).
func (r *Result) Decode(v any) error {
	b, err := json.Marshal(r.Value)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Invoke runs one cmdlet via InvokeCommand and returns all pages of results.
// params holds only the bound parameters (PowerShell semantics); generated
// request types build this map for you.
func (c *Client) Invoke(ctx context.Context, cmdlet string, params map[string]any) (*Result, error) {
	if params == nil {
		params = map[string]any{}
	}
	body, err := json.Marshal(map[string]any{
		"CmdletInput": map[string]any{"CmdletName": cmdlet, "Parameters": params},
	})
	if err != nil {
		return nil, err
	}

	out := &Result{}
	resp, err := c.postInvoke(ctx, cmdlet, body)
	if err != nil {
		return nil, err
	}
	out.RateLimit = parseRateLimit(resp.Header)
	raw, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return out, parseAPIError(resp.StatusCode, raw)
	}
	var env odataResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("adminapi: decode response: %w", err)
	}
	if env.Error != nil {
		return out, &APIError{Status: resp.StatusCode, Code: env.Error.Code, Message: env.Error.Message, InnerRaw: env.Error.InnerError}
	}
	out.Value = append(out.Value, env.Value...)
	out.Warnings = append(out.Warnings, env.Warnings...)
	// Follow @odata.nextLink pages (GET) until exhausted.
	return c.followPages(ctx, cmdlet, out, env.NextLink)
}

// followPages continues GET paging from a nextLink until exhausted.
func (c *Client) followPages(ctx context.Context, cmdlet string, out *Result, nextLink string) (*Result, error) {
	for nextLink != "" {
		pg, err := c.getPage(ctx, cmdlet, normalizeURL(nextLink))
		if err != nil {
			return out, err
		}
		out.Value = append(out.Value, pg.Value...)
		out.Warnings = append(out.Warnings, pg.Warnings...)
		nextLink = pg.NextLink
	}
	return out, nil
}

type pageResult struct {
	Value     []map[string]any
	Warnings  []string
	NextLink  string
	RateLimit RateLimit
}

func (c *Client) getPage(ctx context.Context, cmdlet, url string) (*pageResult, error) {
	token, err := c.opt.Tokens.Token(ctx, c.opt.Cloud.Resource)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.setInvokeHeaders(req, cmdlet, token)
	req.Header.Del("Content-Type")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	raw, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, parseAPIError(resp.StatusCode, raw)
	}
	var env odataResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	return &pageResult{Value: env.Value, Warnings: env.Warnings, NextLink: env.NextLink, RateLimit: parseRateLimit(resp.Header)}, nil
}

func parseAPIError(status int, raw []byte) error {
	var env odataResponse
	if json.Unmarshal(raw, &env) == nil && env.Error != nil {
		return &APIError{Status: status, Code: env.Error.Code, Message: env.Error.Message, InnerRaw: env.Error.InnerError}
	}
	return &APIError{Status: status, Code: "Unknown", Message: string(raw)}
}

func parseRateLimit(h http.Header) RateLimit {
	rl := RateLimit{}
	rl.Limit, _ = strconv.Atoi(h.Get("Rate-Limit-Limit"))
	rl.Remaining, _ = strconv.Atoi(h.Get("Rate-Limit-Remaining"))
	if v := h.Get("Rate-Limit-Reset"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			rl.Reset = t
		}
	}
	return rl
}
