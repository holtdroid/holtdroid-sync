package slacksync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// slack-go drops the files of forwarded messages (attachments[].files), so
// history and replies are decoded here.
type message struct {
	slack.Msg
	Attachments []attachment `json:"attachments,omitempty"`
}

type attachment struct {
	slack.Attachment
	Files []slack.File `json:"files,omitempty"`
}

type messagePage struct {
	Messages []message `json:"messages"`
	HasMore  bool      `json:"has_more"`
	Meta     struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

func (s *Syncer) pages(ctx context.Context, method string, form url.Values) ([]message, error) {
	form.Set("limit", "200")
	var out []message
	for {
		var page messagePage
		_, err := retry(ctx, s.log, func() (struct{}, error) {
			return struct{}{}, s.call(ctx, method, form, &page)
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", method, err)
		}
		out = append(out, page.Messages...)
		if !page.HasMore || page.Meta.NextCursor == "" {
			return out, nil
		}
		form.Set("cursor", page.Meta.NextCursor)
	}
}

func (s *Syncer) call(ctx context.Context, method string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.APIURL+method, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return &slack.RateLimitedError{RetryAfter: time.Duration(secs) * time.Second}
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	var status struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return err
	}
	if !status.OK {
		return errors.New(status.Error)
	}
	return json.Unmarshal(raw, out)
}
