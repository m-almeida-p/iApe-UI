package aguibff

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/resource"
)

var _ resource.ActionGateway = (*Client)(nil)

// actionAnswer is what the BFF answers to an approval's action. Only what the Web UI shows is read.
type actionAnswer struct {
	OTPRequired bool   `json:"otp_required"`
	TransferID  string `json:"transfer_id"`
	Message     string `json:"displayMessage"`
}

// Approve implements resource.ActionGateway.
func (c *Client) Approve(ctx context.Context, card domain.Interaction) (domain.ActionResult, error) {
	got, err := c.act(ctx, card.ApprovalID, "approve", map[string]string{"run_id": card.RunID})
	if err != nil {
		return domain.ActionResult{}, err
	}
	if !got.OTPRequired {
		return domain.ActionResult{Message: got.Message}, nil
	}
	if strings.TrimSpace(got.TransferID) == "" {
		return domain.ActionResult{}, errors.New("the BFF asked for a code without transfer_id")
	}
	return domain.ActionResult{Message: got.Message, ChallengeID: got.TransferID}, nil
}

// Reject implements resource.ActionGateway.
func (c *Client) Reject(ctx context.Context, card domain.Interaction) (domain.ActionResult, error) {
	got, err := c.act(ctx, card.ApprovalID, "reject", map[string]string{"run_id": card.RunID})
	return domain.ActionResult{Message: got.Message}, err
}

// ConfirmOTP implements resource.ActionGateway.
func (c *Client) ConfirmOTP(ctx context.Context, card domain.Interaction, code string) (domain.ActionResult, error) {
	got, err := c.act(ctx, card.ApprovalID, "otp/confirm", map[string]string{
		"transfer_id": card.ChallengeID, "otp": code, "run_id": card.RunID,
	})
	return domain.ActionResult{Message: got.Message}, err
}

// ConfirmContact implements resource.ActionGateway.
func (c *Client) ConfirmContact(ctx context.Context, query string, contact domain.Contact) error {
	_, err := c.postJSON(ctx, "/bff/contacts/confirm", map[string]string{"query": query, "phone": contact.Phone, "name": contact.Name})
	return err
}

// act posts an action of an approval to the BFF. The route is built from the approval's id, never taken from the stream, and
// the id is one path segment whatever it holds.
func (c *Client) act(ctx context.Context, approvalID, action string, body map[string]string) (actionAnswer, error) {
	// An empty id leaves a hole in the route, and "." and ".." are path segments that PathEscape leaves as they are: another route.
	if strings.TrimSpace(approvalID) == "" || approvalID == "." || approvalID == ".." {
		return actionAnswer{}, fmt.Errorf("the approval has no usable id (%q)", approvalID)
	}
	return c.postJSON(ctx, "/bff/approvals/"+url.PathEscape(approvalID)+"/"+action, body)
}

// postJSON posts a JSON body to a route of the BFF as the account. A 4xx the BFF explains is the service refusing:
// *domain.ActionError.
func (c *Client) postJSON(ctx context.Context, route string, body map[string]string) (actionAnswer, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return actionAnswer{}, err
	}
	target := c.base + route
	caller := ctx
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.send(ctx, func(token string) (*http.Response, error) {
		hr, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		hr.Header.Set("Authorization", "Bearer "+token)
		hr.Header.Set("X-Yape-Account-Id", c.account)
		hr.Header.Set("Content-Type", "application/json")
		hr.Header.Set("Accept", "application/json")
		return c.http.Do(hr)
	})
	if err != nil {
		return actionAnswer{}, c.classify(caller, ctx, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return actionAnswer{}, refusal(classifyStatus(readHTTPError(resp)))
	}
	defer func() { _ = resp.Body.Close() }()
	var got actionAnswer
	if err := json.UnmarshalRead(io.LimitReader(resp.Body, maxErrorBody*16), &got); err != nil {
		return actionAnswer{}, c.classify(caller, ctx, fmt.Errorf("reading the BFF's answer: %w", err))
	}
	return got, nil
}

// refusal turns a 4xx with an explanation (NestJS puts it in "message") into the service's own words for the user. Any other
// error is returned as it is.
func refusal(err error) error {
	httpErr, ok := errors.AsType[*HTTPError](err)
	if !ok || httpErr.Status < http.StatusBadRequest || httpErr.Status >= http.StatusInternalServerError {
		return err
	}
	var body struct {
		Message any `json:"message"`
	}
	if json.Unmarshal([]byte(cmp.Or(httpErr.full, httpErr.Body)), &body) != nil {
		return err
	}
	switch m := body.Message.(type) {
	case string:
		if m != "" {
			return &domain.ActionError{Message: m}
		}
	case []any:
		parts := make([]string, 0, len(m))
		for _, p := range m {
			if s, ok := p.(string); ok {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			return &domain.ActionError{Message: strings.Join(parts, "; ")}
		}
	}
	return err
}
