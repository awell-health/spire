package gatewayclient

import (
	"context"
	"net/http"
	"net/url"
)

// SendMessageInput matches the POST /api/v1/messages request body.
// To and Message are required; the rest are optional. Priority==0 is
// rewritten server-side to the 3-default, so callers can leave it zero
// when they don't care.
type SendMessageInput struct {
	To       string `json:"to"`
	Message  string `json:"message"`
	From     string `json:"from,omitempty"`
	Ref      string `json:"ref,omitempty"`
	Thread   string `json:"thread,omitempty"`
	Priority int    `json:"priority,omitempty"`
}

// ListMessages calls GET /api/v1/messages and returns the open message
// beads addressed to `to`. A blank `to` returns every open message,
// matching the gateway's unfiltered behaviour. Message beads share the
// BeadRecord shape (they're stored as type=message beads with routing
// labels).
func (c *Client) ListMessages(ctx context.Context, to string) ([]BeadRecord, error) {
	q := url.Values{}
	if to != "" {
		q.Set("to", to)
	}
	path := "/api/v1/messages"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out []BeadRecord
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SendMessage calls POST /api/v1/messages and returns the new message
// bead's ID. Required fields on in: To, Message. Empty From is filled
// in server-side with "gateway".
func (c *Client) SendMessage(ctx context.Context, in SendMessageInput) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/messages", in, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// MarkMessageRead calls POST /api/v1/messages/{id}/read, which the
// gateway implements as a bead-close on the message bead. Response body
// is discarded; only the error matters to the caller.
func (c *Client) MarkMessageRead(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/messages/"+id+"/read", nil, nil)
}
