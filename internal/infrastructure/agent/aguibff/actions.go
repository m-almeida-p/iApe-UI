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
	"regexp"
	"strings"
	"uuid"

	"iape-ui/internal/core/domain"
	"iape-ui/internal/core/port/resource"
)

var _ resource.ActionGateway = (*Client)(nil)

// actionAnswer is what the BFF answers to an approval's action. Only what the Web UI shows is read.
type actionAnswer struct {
	OTPRequired bool   `json:"otp_required"`
	TransferID  string `json:"transfer_id"`
	Message     string `json:"displayMessage"`
	// Success and Execution say whether the transfer was done. A BFF that says nothing (a reject, an older one) is taken at its word.
	Success   *bool `json:"success"`
	Execution *struct {
		Status string `json:"status"`
	} `json:"execution"`
}

// confirmed reports whether the answer does not contradict the transfer being done: "success" is not false and, when the execution
// has a status, it is SUCCESS.
func (a actionAnswer) confirmed() bool {
	if a.Success != nil && !*a.Success {
		return false
	}
	return a.Execution == nil || strings.EqualFold(strings.TrimSpace(a.Execution.Status), "SUCCESS")
}

// unconfirmed is the error of a 2xx that does not say the transfer was done. The service answered, so the transfer may have gone
// through or not: it is neither a refusal nor a request that never left, and the card stays claimed until the claim is stale.
func unconfirmed(a actionAnswer) error {
	status := ""
	if a.Execution != nil {
		status = a.Execution.Status
	}
	return fmt.Errorf("%w: the BFF did not confirm the transfer (success=%v, status=%q)", domain.ErrAgentUnavailable, a.Success != nil && *a.Success, status)
}

// Approve implements resource.ActionGateway.
func (c *Client) Approve(ctx context.Context, account uuid.UUID, card domain.Interaction) (domain.ActionResult, error) {
	got, err := c.act(ctx, account, card.ApprovalID, "approve", map[string]string{"run_id": card.RunID})
	if err != nil {
		return domain.ActionResult{}, err
	}
	if !got.OTPRequired {
		if !got.confirmed() {
			return domain.ActionResult{}, unconfirmed(got)
		}
		return domain.ActionResult{Message: got.Message}, nil
	}
	if strings.TrimSpace(got.TransferID) == "" {
		return domain.ActionResult{}, errors.New("the BFF asked for a code without transfer_id")
	}
	return domain.ActionResult{Message: got.Message, ChallengeID: got.TransferID}, nil
}

// Reject implements resource.ActionGateway.
func (c *Client) Reject(ctx context.Context, account uuid.UUID, card domain.Interaction) (domain.ActionResult, error) {
	got, err := c.act(ctx, account, card.ApprovalID, "reject", map[string]string{"run_id": card.RunID})
	return domain.ActionResult{Message: got.Message}, err
}

// ConfirmOTP implements resource.ActionGateway.
func (c *Client) ConfirmOTP(ctx context.Context, account uuid.UUID, card domain.Interaction, code string) (domain.ActionResult, error) {
	got, err := c.act(ctx, account, card.ApprovalID, "otp/confirm", map[string]string{
		"transfer_id": card.ChallengeID, "otp": code, "run_id": card.RunID,
	})
	if err != nil {
		return domain.ActionResult{}, err
	}
	if !got.confirmed() {
		return domain.ActionResult{}, unconfirmed(got)
	}
	return domain.ActionResult{Message: got.Message}, nil
}

// ConfirmContact implements resource.ActionGateway.
func (c *Client) ConfirmContact(ctx context.Context, account uuid.UUID, query string, contact domain.Contact) error {
	_, err := c.postJSON(ctx, account, "/bff/contacts/confirm", map[string]string{"query": query, "phone": contact.Phone, "name": contact.Name})
	return err
}

// act posts an action of an approval to the BFF. The route is built from the approval's id, never taken from the stream, and
// the id is one path segment whatever it holds.
func (c *Client) act(ctx context.Context, account uuid.UUID, approvalID, action string, body map[string]string) (actionAnswer, error) {
	// An empty id leaves a hole in the route, and "." and ".." are path segments that PathEscape leaves as they are: another route.
	if strings.TrimSpace(approvalID) == "" || approvalID == "." || approvalID == ".." {
		return actionAnswer{}, fmt.Errorf("the approval has no usable id (%q)", approvalID)
	}
	return c.postJSON(ctx, account, "/bff/approvals/"+url.PathEscape(approvalID)+"/"+action, body)
}

// postJSON posts a JSON body to a route of the BFF as the account of the call. A 4xx the BFF explains is the service refusing:
// *domain.ActionError.
func (c *Client) postJSON(ctx context.Context, account uuid.UUID, route string, body map[string]string) (actionAnswer, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return actionAnswer{}, err
	}
	target := c.base + route
	caller := ctx
	limit := c.actionLimit()
	ctx, cancel := withLimit(ctx, limit)
	defer cancel()
	resp, err := c.send(ctx, account, func(token string) (*http.Response, error) {
		hr, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		hr.Header.Set("Authorization", "Bearer "+token)
		hr.Header.Set("X-Yape-Account-Id", account.String())
		hr.Header.Set("Content-Type", "application/json")
		hr.Header.Set("Accept", "application/json")
		return c.http.Do(hr)
	})
	if err != nil {
		return actionAnswer{}, c.classify(caller, ctx, limit, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return actionAnswer{}, refusal(classifyStatus(readHTTPError(resp)))
	}
	defer func() { _ = resp.Body.Close() }()
	var got actionAnswer
	if err := json.UnmarshalRead(io.LimitReader(resp.Body, maxErrorBody*16), &got); err != nil {
		return actionAnswer{}, c.classify(caller, ctx, limit, fmt.Errorf("reading the BFF's answer: %w", err))
	}
	return got, nil
}

// refusal turns a 4xx with an explanation into the service's own words for the user: NestJS puts it in "message", the approvals'
// own refusals in "error". Any other error is returned as it is.
func refusal(err error) error {
	httpErr, ok := errors.AsType[*HTTPError](err)
	if !ok || httpErr.Status < http.StatusBadRequest || httpErr.Status >= http.StatusInternalServerError {
		return err
	}
	var body struct {
		Message any `json:"message"`
		Error   any `json:"error"`
	}
	if json.Unmarshal([]byte(cmp.Or(httpErr.full, httpErr.Body)), &body) != nil {
		return err
	}
	fromError := explanation(body.Error)
	if strings.EqualFold(fromError, http.StatusText(httpErr.Status)) {
		fromError = "" // NestJS names the status in "error" ("Bad Request"): that explains nothing
	}
	text := cmp.Or(explanation(body.Message), fromError)
	if text == "" {
		return err
	}
	refused := &domain.ActionError{Message: text}
	if m := alreadyResolved.FindStringSubmatch(text); m != nil {
		refused.Resolved = m[1]
	}
	return refused
}

// alreadyResolved is what the BFF says to a second answer to an approval: what the approval already is.
var alreadyResolved = regexp.MustCompile(`^approval already resolved: ([A-Za-z_]+)$`)

// explanation is the text of a "message" or an "error" of the BFF: a string, or a list of them (validation).
func explanation(v any) string {
	switch m := v.(type) {
	case string:
		return m
	case []any:
		parts := make([]string, 0, len(m))
		for _, p := range m {
			if s, ok := p.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "; ")
	}
	return ""
}
