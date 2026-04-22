package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	opensearchlib "github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// OpenSearch implements Client against a real OpenSearch cluster.
//
// The concrete index name (messagesIndex + "-v1") and the alias
// (messagesIndex) are kept distinct so mapping changes can be rolled out
// with a reindex + alias flip instead of a client-visible migration. Every
// client method talks to the alias except EnsureIndex, which owns the
// concrete index.
type OpenSearch struct {
	api           *opensearchapi.Client
	messagesAlias string
	messagesIndex string
}

// NewOpenSearch constructs a client pointing at url. messagesAlias is the
// stable name clients read from; the concrete index is derived by suffixing
// "-v1". Bumping the suffix is a reindex, not a client change.
func NewOpenSearch(url, messagesAlias string) (*OpenSearch, error) {
	if url == "" {
		return nil, errors.New("opensearch: url is required")
	}
	if messagesAlias == "" {
		return nil, errors.New("opensearch: messages alias is required")
	}
	api, err := opensearchapi.NewClient(opensearchapi.Config{
		Client: opensearchlib.Config{
			Addresses: []string{url},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("opensearch: new client: %w", err)
	}
	return &OpenSearch{
		api:           api,
		messagesAlias: messagesAlias,
		messagesIndex: messagesAlias + "-v1",
	}, nil
}

// EnsureIndex creates the concrete index + alias if either is missing. It
// runs at startup, and is the one place the mapping is declared — every
// other method queries by alias and does not care about the concrete name.
func (o *OpenSearch) EnsureIndex(ctx context.Context) error {
	exists, err := o.api.Indices.Exists(ctx, opensearchapi.IndicesExistsReq{
		Indices: []string{o.messagesIndex},
	})
	if err != nil && !isNotFound(exists) {
		return fmt.Errorf("opensearch: exists %s: %w", o.messagesIndex, err)
	}
	if exists == nil || exists.StatusCode == http.StatusNotFound {
		body := bytes.NewReader([]byte(messagesIndexMapping))
		if _, err := o.api.Indices.Create(ctx, opensearchapi.IndicesCreateReq{
			Index: o.messagesIndex,
			Body:  body,
		}); err != nil {
			return fmt.Errorf("opensearch: create %s: %w", o.messagesIndex, err)
		}
	}

	aliasBody, err := json.Marshal(map[string]any{
		"actions": []map[string]any{
			{"add": map[string]any{"index": o.messagesIndex, "alias": o.messagesAlias}},
		},
	})
	if err != nil {
		return fmt.Errorf("opensearch: marshal alias body: %w", err)
	}
	if _, err := o.api.Aliases(ctx, opensearchapi.AliasesReq{
		Body: bytes.NewReader(aliasBody),
	}); err != nil {
		return fmt.Errorf("opensearch: add alias %s→%s: %w", o.messagesAlias, o.messagesIndex, err)
	}
	return nil
}

// IndexMessage writes one projection. _id is the outbox event UUID so
// retries are natural upserts.
func (o *OpenSearch) IndexMessage(ctx context.Context, outboxEventID uuid.UUID, doc MessageDoc) error {
	body, err := json.Marshal(map[string]any{
		"id":              doc.ID.String(),
		"channel_id":      doc.ChannelID.String(),
		"organization_id": doc.OrganizationID.String(),
		"author_id":       doc.AuthorID.String(),
		"body":            doc.Body,
		"created_at":      doc.CreatedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("opensearch: marshal doc: %w", err)
	}
	if _, err := o.api.Index(ctx, opensearchapi.IndexReq{
		Index:      o.messagesAlias,
		DocumentID: outboxEventID.String(),
		Body:       bytes.NewReader(body),
	}); err != nil {
		return fmt.Errorf("opensearch: index %s: %w", outboxEventID, err)
	}
	return nil
}

// SearchMessages runs one query. The tenant filter on organization_id is
// unconditional — callers cannot omit it.
func (o *OpenSearch) SearchMessages(ctx context.Context, q MessageQuery) (MessageResult, error) {
	if q.OrganizationID == uuid.Nil {
		return MessageResult{}, errors.New("opensearch: organization_id is required")
	}
	if q.Query == "" {
		return MessageResult{}, errors.New("opensearch: query is required")
	}

	filters := []map[string]any{
		{"term": map[string]any{"organization_id": q.OrganizationID.String()}},
	}
	if len(q.ChannelIDs) > 0 {
		ids := make([]string, 0, len(q.ChannelIDs))
		for _, id := range q.ChannelIDs {
			ids = append(ids, id.String())
		}
		filters = append(filters, map[string]any{"terms": map[string]any{"channel_id": ids}})
	}
	if !q.After.IsZero() || !q.Before.IsZero() {
		rng := map[string]any{}
		if !q.After.IsZero() {
			rng["gte"] = q.After.UTC().Format(time.RFC3339Nano)
		}
		if !q.Before.IsZero() {
			rng["lt"] = q.Before.UTC().Format(time.RFC3339Nano)
		}
		filters = append(filters, map[string]any{"range": map[string]any{"created_at": rng}})
	}

	body := map[string]any{
		"size": q.Limit,
		"query": map[string]any{
			"bool": map[string]any{
				"must":   []map[string]any{{"match": map[string]any{"body": q.Query}}},
				"filter": filters,
			},
		},
		"sort": []map[string]any{
			{"_score": map[string]any{"order": "desc"}},
			{"created_at": map[string]any{"order": "desc"}},
			{"id": map[string]any{"order": "desc"}},
		},
		"highlight": map[string]any{
			"fields": map[string]any{
				"body": map[string]any{
					"pre_tags":            []string{"**"},
					"post_tags":           []string{"**"},
					"fragment_size":       160,
					"number_of_fragments": 1,
				},
			},
		},
	}
	if len(q.CursorValues) > 0 {
		body["search_after"] = q.CursorValues
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return MessageResult{}, fmt.Errorf("opensearch: marshal search body: %w", err)
	}
	resp, err := o.api.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{o.messagesAlias},
		Body:    bytes.NewReader(raw),
	})
	if err != nil {
		return MessageResult{}, fmt.Errorf("opensearch: search: %w", err)
	}

	out := MessageResult{Hits: make([]MessageHit, 0, len(resp.Hits.Hits))}
	for _, h := range resp.Hits.Hits {
		var src struct {
			ID             string `json:"id"`
			ChannelID      string `json:"channel_id"`
			OrganizationID string `json:"organization_id"`
			AuthorID       string `json:"author_id"`
			Body           string `json:"body"`
			CreatedAt      string `json:"created_at"`
		}
		if err := json.Unmarshal(h.Source, &src); err != nil {
			return MessageResult{}, fmt.Errorf("opensearch: unmarshal hit: %w", err)
		}
		doc, err := docFromSource(src)
		if err != nil {
			return MessageResult{}, fmt.Errorf("opensearch: build hit: %w", err)
		}
		snippet := ""
		if frags, ok := h.Highlight["body"]; ok && len(frags) > 0 {
			snippet = frags[0]
		}
		out.Hits = append(out.Hits, MessageHit{
			Doc:        doc,
			Snippet:    snippet,
			Score:      h.Score,
			SortValues: append([]interface{}{}, h.Sort...),
		})
	}
	if len(out.Hits) == q.Limit && q.Limit > 0 {
		out.NextCursor = out.Hits[len(out.Hits)-1].SortValues
	}
	return out, nil
}

// docFromSource parses string-typed ids and timestamp back into the typed
// MessageDoc the rest of the system uses.
func docFromSource(src struct {
	ID             string `json:"id"`
	ChannelID      string `json:"channel_id"`
	OrganizationID string `json:"organization_id"`
	AuthorID       string `json:"author_id"`
	Body           string `json:"body"`
	CreatedAt      string `json:"created_at"`
},
) (MessageDoc, error) {
	id, err := uuid.Parse(src.ID)
	if err != nil {
		return MessageDoc{}, fmt.Errorf("id: %w", err)
	}
	cid, err := uuid.Parse(src.ChannelID)
	if err != nil {
		return MessageDoc{}, fmt.Errorf("channel_id: %w", err)
	}
	oid, err := uuid.Parse(src.OrganizationID)
	if err != nil {
		return MessageDoc{}, fmt.Errorf("organization_id: %w", err)
	}
	aid, err := uuid.Parse(src.AuthorID)
	if err != nil {
		return MessageDoc{}, fmt.Errorf("author_id: %w", err)
	}
	ts, err := time.Parse(time.RFC3339Nano, src.CreatedAt)
	if err != nil {
		return MessageDoc{}, fmt.Errorf("created_at: %w", err)
	}
	return MessageDoc{
		ID:             id,
		ChannelID:      cid,
		OrganizationID: oid,
		AuthorID:       aid,
		Body:           src.Body,
		CreatedAt:      ts,
	}, nil
}

func isNotFound(resp *opensearchlib.Response) bool {
	return resp != nil && resp.StatusCode == http.StatusNotFound
}

// messagesIndexMapping is the concrete mapping the alias initially points
// at. Fields are keyword-typed for exact-match filters (tenant, channel,
// author, id); body is text-analyzed with the standard analyzer; created_at
// is a native date for range queries + sort.
const messagesIndexMapping = `{
  "settings": {
    "number_of_shards": 1,
    "number_of_replicas": 0
  },
  "mappings": {
    "dynamic": "strict",
    "properties": {
      "id":              {"type": "keyword"},
      "channel_id":      {"type": "keyword"},
      "organization_id": {"type": "keyword"},
      "author_id":       {"type": "keyword"},
      "body":            {"type": "text"},
      "created_at":      {"type": "date"}
    }
  }
}`
