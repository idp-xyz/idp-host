package cmiclient

import "context"

// ReadRequest is the body of POST /v1/context/read (idp.telos.cmi 1.7.0; docs/adr/kb8-cmi-api.md): the blocks of one document
// at a place, in document order, with Around blocks on each side.
type ReadRequest struct {
	DocumentID         string   `json:"documentId"`
	Locator            *Locator `json:"locator,omitempty"`
	Around             *int     `json:"around,omitempty"`
	Budget             *Budget  `json:"budget,omitempty"`
	Purpose            string   `json:"purpose"`
	AsOf               *AsOf    `json:"asOf,omitempty"`
	SensitivityCeiling string   `json:"sensitivityCeiling,omitempty"`
}

// FindRequest is the body of POST /v1/context/find: the blocks of one document that hold the text.
type FindRequest struct {
	DocumentID         string  `json:"documentId"`
	Text               string  `json:"text"`
	MaxHits            int     `json:"maxHits,omitempty"`
	Budget             *Budget `json:"budget,omitempty"`
	Purpose            string  `json:"purpose"`
	AsOf               *AsOf   `json:"asOf,omitempty"`
	SensitivityCeiling string  `json:"sensitivityCeiling,omitempty"`
}

// Budget bounds a package.
type Budget struct {
	MaxTokens int `json:"maxTokens,omitempty"`
	MaxItems  int `json:"maxItems,omitempty"`
}

// Read asks the CMI for the blocks of a document at a place, as the holder of the bearer.
func (c *Client) Read(ctx context.Context, bearer string, req ReadRequest) (*Package, error) {
	return c.post(ctx, bearer, "/v1/context/read", req)
}

// Find asks the CMI for the blocks of a document that hold a text, as the holder of the bearer.
func (c *Client) Find(ctx context.Context, bearer string, req FindRequest) (*Package, error) {
	return c.post(ctx, bearer, "/v1/context/find", req)
}
