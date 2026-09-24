package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"doppel.moe/katydid/internal/api"
	"doppel.moe/katydid/internal/importer"
	"doppel.moe/katydid/internal/library"
)

type Client struct {
	hc *http.Client
}

func Dial(socket string) *Client {
	return &Client{hc: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socket)
			},
		},
	}}
}

func (c *Client) Status() (api.StatusResponse, error) {
	var out api.StatusResponse
	return out, c.get("/status", &out)
}

func (c *Client) Albums(query library.Query) (api.AlbumsResponse, error) {
	params := []string{}
	if query.Q != "" {
		params = append(params, "q="+url.QueryEscape(query.Q))
	}
	if query.Artist != "" {
		params = append(params, "artist="+url.QueryEscape(query.Artist))
	}
	if query.Year != 0 {
		params = append(params, "year="+fmt.Sprint(query.Year))
	}
	path := "/albums"
	if len(params) > 0 {
		path += "?" + strings.Join(params, "&")
	}
	var out api.AlbumsResponse
	return out, c.get(path, &out)
}

func (c *Client) Album(id string) (library.Album, error) {
	var out library.Album
	return out, c.get("/album?id="+url.QueryEscape(id), &out)
}

func (c *Client) Scan() (api.ScanResponse, error) {
	var out api.ScanResponse
	return out, c.post("/scan", &out)
}

func (c *Client) Check() (api.CheckResponse, error) {
	var out api.CheckResponse
	return out, c.get("/check", &out)
}

func (c *Client) Import(req importer.Request) (importer.Result, error) {
	var out importer.Result
	return out, c.postJSON("/import", req, &out)
}

func (c *Client) Decide(token string, pick int, skip bool) (importer.Result, error) {
	body := struct {
		Token string `json:"token"`
		Pick  int    `json:"pick"`
		Skip  bool   `json:"skip"`
	}{Token: token, Pick: pick, Skip: skip}
	var out importer.Result
	return out, c.postJSON("/import/decide", body, &out)
}

type RetagResponse struct {
	Results []importer.RetagResult `json:"results"`
}

func (c *Client) Retag(album string, all bool, policyName string) (RetagResponse, error) {
	body := struct {
		Album  string `json:"album,omitempty"`
		All    bool   `json:"all,omitempty"`
		Policy string `json:"policy,omitempty"`
	}{Album: album, All: all, Policy: policyName}
	var out RetagResponse
	return out, c.postJSON("/retag", body, &out)
}

func (c *Client) get(path string, out any) error {
	return c.do(http.MethodGet, path, out)
}

func (c *Client) post(path string, out any) error {
	return c.do(http.MethodPost, path, out)
}

func (c *Client) postJSON(path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return c.doWithBody(http.MethodPost, path, data, out)
}

func (c *Client) do(method, path string, out any) error {
	return c.doWithBody(method, path, nil, out)
}

func (c *Client) doWithBody(method, path string, body []byte, out any) error {
	req, err := http.NewRequest(method, "http://katydid"+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody api.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errBody); err == nil && errBody.Error != "" {
			return fmt.Errorf("%s %s: %s", method, path, errBody.Error)
		}
		return fmt.Errorf("%s %s: status %s", method, path, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}
